package ui

import (
	"context"
	"runtime"
	"strings"
	"time"

	"github.com/ironpark/ggui"

	"github.com/ironpark/gvx-desktop/internal/registry"
)

// Searching a registry as the user types: a search waits until the typing
// pauses, a newer one calls off the one before it, and an answer that
// comes after its question was replaced is dropped.
const (
	searchDelay   = 300 * time.Millisecond
	searchTimeout = 10 * time.Second
	searchLimit   = 25
	tagLimit      = 24
	searchKept    = 64 // answers remembered, so that going back asks nothing
)

type searchKey struct {
	source registry.Source
	query  string
}

// search is the state of the pull dialog's search that the screen does not
// show; the model's Search signals are what it does.
type search struct {
	gen      int // of the newest question
	timer    *time.Timer
	cancel   context.CancelFunc
	picked   string // the repository chosen from the results, which typing a tag after keeps
	tagsGen  int
	tagsStop context.CancelFunc
	kept     map[searchKey][]registry.Result
	order    []searchKey
}

// SearchSource is the registry the pull dialog searches.
func (m *Model) SearchSource() registry.Source {
	if registry.Source(ggui.Peek(m.Settings).SearchRegistry) == registry.Quay {
		return registry.Quay
	}
	return registry.DockerHub
}

// repoOf is a reference without its tag or digest.
func repoOf(ref string) string {
	ref = strings.TrimSpace(ref)
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return ref
}

// SearchTyped follows what the pull dialog's field holds: it searches for
// it once the typing has paused, or clears the results when it is a
// reference to pull as it stands. It runs on the UI thread.
func (m *Model) SearchTyped() {
	if m.Offline {
		return
	}
	s := &m.search
	typed := strings.TrimSpace(ggui.Peek(m.PullRef))
	// A tag typed or chosen after the picked repository leaves the list be.
	if s.picked != "" && repoOf(typed) == s.picked && ggui.Peek(m.PullOpen) {
		return
	}
	s.picked = ""
	s.gen++
	gen := s.gen
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	m.clearTags()
	if !ggui.Peek(m.PullOpen) || ggui.Peek(m.Pulling) || !registry.Searchable(typed) {
		m.SearchResults.Set(nil)
		m.SearchFor.Set("")
		m.SearchBusy.Set(false)
		m.SearchErr.Set("")
		return
	}
	key := searchKey{m.SearchSource(), strings.ToLower(typed)}
	if kept, ok := s.kept[key]; ok {
		m.SearchResults.Set(kept)
		m.SearchFor.Set(typed)
		m.SearchBusy.Set(false)
		m.SearchErr.Set("")
		return
	}
	// What was found stays on the screen until the next answer replaces it.
	m.SearchBusy.Set(true)
	s.timer = time.AfterFunc(searchDelay, func() {
		ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
		m.post(func() {
			if gen != s.gen {
				cancel()
				return
			}
			s.cancel = cancel
			go func() {
				defer cancel()
				found, err := registry.Search(ctx, key.source, typed, searchLimit)
				m.post(func() {
					if gen != s.gen {
						return
					}
					s.cancel = nil
					m.SearchBusy.Set(false)
					if err != nil {
						m.SearchErr.Set(err.Error())
						return
					}
					m.keep(key, found)
					m.SearchErr.Set("")
					m.SearchResults.Set(found)
					m.SearchFor.Set(typed)
				})
			}()
		})
	})
}

func (m *Model) keep(key searchKey, found []registry.Result) {
	s := &m.search
	if s.kept == nil {
		s.kept = map[searchKey][]registry.Result{}
	}
	if _, ok := s.kept[key]; !ok {
		s.order = append(s.order, key)
		if len(s.order) > searchKept {
			delete(s.kept, s.order[0])
			s.order = s.order[1:]
		}
	}
	s.kept[key] = found
}

func (m *Model) clearTags() {
	s := &m.search
	s.tagsGen++
	if s.tagsStop != nil {
		s.tagsStop()
		s.tagsStop = nil
	}
	m.SearchTags.Set(nil)
	m.SearchTagsOf.Set("")
	m.SearchTagsBusy.Set(false)
}

// PickResult puts a repository the search found in the field, and lists
// its tags to choose from.
func (m *Model) PickResult(r registry.Result) {
	s := &m.search
	ref := r.Ref()
	m.clearTags()
	s.picked = ref
	m.PullRef.Set(ref)
	m.SearchTagsOf.Set(ref)
	m.SearchTagsBusy.Set(true)
	gen := s.tagsGen
	ctx, cancel := context.WithTimeout(context.Background(), searchTimeout)
	s.tagsStop = cancel
	go func() {
		defer cancel()
		tags, err := registry.Tags(ctx, r, runtime.GOARCH, tagLimit)
		m.post(func() {
			if gen != s.tagsGen {
				return
			}
			s.tagsStop = nil
			m.SearchTagsBusy.Set(false)
			if err == nil {
				m.SearchTags.Set(tags)
			}
		})
	}()
}

// PickTag completes the picked repository with one of its tags.
func (m *Model) PickTag(tag string) {
	if ref := ggui.Peek(m.SearchTagsOf); ref != "" {
		m.PullRef.Set(ref + ":" + tag)
	}
}
