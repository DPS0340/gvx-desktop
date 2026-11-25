package uiblock

import (
	"context"
	"errors"
	"time"
)

// ErrGone ends a stream whose block went.
var ErrGone = errors.New("the block went")

// DefaultFollowInterval is the minimum interval between coalesced stream updates.
const DefaultFollowInterval = 100 * time.Millisecond

// View is a block as a stream sends it, at a moment: its tree, how it
// stands, and a channel closed when any of it next changes.
type View struct {
	Tree              *Node
	Title, State, Err string
	Chrome            string
	Stale             bool
	Changed           <-chan struct{}
	Gone              bool
}

func (v View) sameAs(o View) bool {
	return v.Title == o.Title && v.State == o.State && v.Err == o.Err &&
		v.Chrome == o.Chrome && v.Stale == o.Stale
}

// Update is one coalesced tree or patch frame emitted by Follow.
type Update struct {
	Seq    uint64 `json:"seq,omitempty"`
	Tree   *Node  `json:"tree,omitempty"`
	Ops    []*Op  `json:"ops,omitempty"`
	Title  string `json:"title,omitempty"`
	State  string `json:"state,omitempty"`
	Error  string `json:"error,omitempty"`
	Chrome string `json:"chrome,omitempty"`
	Stale  bool   `json:"stale,omitempty"`
	Gone   bool   `json:"gone,omitempty"`
}

// Follow sends a view's tree whole, then the ops from each tree to the
// next, no more often than every (defaulting to DefaultFollowInterval when
// <= 0), so that a burst of changes goes as one. It ends when ctx does,
// send fails, or the view goes, after it says so.
func Follow(ctx context.Context, every time.Duration, view func() View, send func(Update) error) error {
	if every <= 0 {
		every = DefaultFollowInterval
	}
	var last View
	var seq uint64
	var sent time.Time
	for {
		v := view()
		upd := Update{
			Title: v.Title, State: v.State, Error: v.Err,
			Chrome: v.Chrome, Stale: v.Stale, Gone: v.Gone,
		}
		switch {
		case seq == 0 || last.Tree == nil && v.Tree != nil:
			upd.Tree = v.Tree
		case v.Tree != nil && v.Tree != last.Tree:
			upd.Ops = Diff(last.Tree, v.Tree)
		}
		if seq == 0 || upd.Tree != nil || len(upd.Ops) > 0 || !v.sameAs(last) || v.Gone || (last.Tree != nil && v.Tree == nil) {
			seq++
			upd.Seq = seq
			if err := send(upd); err != nil {
				return err
			}
			last, sent = v, time.Now()
		}
		if v.Gone {
			return ErrGone
		}
		select {
		case <-v.Changed:
		case <-ctx.Done():
			return ctx.Err()
		}
		if wait := every - time.Since(sent); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
		}
	}
}
