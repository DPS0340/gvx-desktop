package extsdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
)

type gestureCtxKey struct{}

// WithGestureID returns a child context carrying the given X-GVX-Gesture-ID token.
func WithGestureID(ctx context.Context, gestureID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if gestureID == "" {
		return ctx
	}
	return context.WithValue(ctx, gestureCtxKey{}, gestureID)
}

// GestureIDFromContext extracts the X-GVX-Gesture-ID token from ctx, or returns "".
func GestureIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(gestureCtxKey{}).(string)
	return v
}

// Context is the base SDK context passed to lifecycle callbacks and host calls.
// It implements context.Context cleanly so gesture tokens propagate automatically.
type Context struct {
	context.Context
	Host       *Host
	ExtID      string
	DataDir    string
	DockerHost string
	Locale     string
	notifyFn   func(viewIDs ...string)
}

// NotifyChanged requests re-rendering of the named views (or all views if none are specified).
func (c *Context) NotifyChanged(viewIDs ...string) {
	if c != nil && c.notifyFn != nil {
		c.notifyFn(viewIDs...)
	}
}

// GestureID returns the X-GVX-Gesture-ID carried by this context, if any.
func (c *Context) GestureID() string {
	return GestureIDFromContext(c)
}

// ViewContext is passed to View.Render.
type ViewContext struct {
	*Context
	ViewID string
}

// NotifyChanged marks this view as needing a re-render.
func (v *ViewContext) NotifyChanged() {
	if v == nil || v.Context == nil {
		return
	}
	if v.ViewID != "" {
		v.Context.NotifyChanged(v.ViewID)
		return
	}
	v.Context.NotifyChanged()
}

// EventContext is passed to View.On handlers when a UI event occurs.
// It intentionally avoids defining a Value(string) method so context.Context.Value(any) any is preserved.
type EventContext struct {
	*Context
	ViewID   string
	NodeID   string
	Kind     string
	RawValue string
	Args     []json.RawMessage
	values   map[string]string
	keyMap   map[string]string
}

// NotifyChanged marks the originating view as needing a re-render.
func (e *EventContext) NotifyChanged() {
	if e == nil || e.Context == nil {
		return
	}
	if e.ViewID != "" {
		e.Context.NotifyChanged(e.ViewID)
		return
	}
	e.Context.NotifyChanged()
}

// GestureID returns the X-GVX-Gesture-ID associated with this user interaction.
func (e *EventContext) GestureID() string {
	return GestureIDFromContext(e)
}

// EventValue returns the raw event value string sent by the frontend.
func (e *EventContext) EventValue() string {
	if e == nil {
		return ""
	}
	return e.RawValue
}

// FormValue looks up a form field value by full node ID (e.g. "/name") or unique trailing key ("name").
func (e *EventContext) FormValue(key string) string {
	if e == nil || key == "" {
		return ""
	}
	if v, ok := e.values[key]; ok {
		return v
	}
	if key[0] != '/' {
		if v, ok := e.values["/"+key]; ok {
			return v
		}
	}
	if v, ok := e.keyMap[key]; ok {
		return v
	}
	return ""
}

// Field is an alias for FormValue.
func (e *EventContext) Field(key string) string {
	return e.FormValue(key)
}

// FormValues returns a shallow copy of all form field values keyed by node ID.
func (e *EventContext) FormValues() map[string]string {
	if e == nil || len(e.values) == 0 {
		return nil
	}
	return maps.Clone(e.values)
}

// Arg unmarshals the i-th handler argument into dst.
func (e *EventContext) Arg(i int, dst any) error {
	if e == nil || i < 0 || i >= len(e.Args) {
		return fmt.Errorf("extsdk: argument index %d out of range", i)
	}
	if dst == nil {
		return errors.New("extsdk: nil destination")
	}
	return json.Unmarshal(e.Args[i], dst)
}

// ArgString returns the i-th argument as a string (or "" if out of range or not a string).
func (e *EventContext) ArgString(i int) string {
	var s string
	if err := e.Arg(i, &s); err != nil {
		return ""
	}
	return s
}

// CommandContext is passed to App.Command handlers.
type CommandContext struct {
	*Context
	CommandID string
}

// GestureID returns the X-GVX-Gesture-ID associated with this command invocation.
func (c *CommandContext) GestureID() string {
	return GestureIDFromContext(c)
}

// ToolContext is passed to App.Tool handlers.
type ToolContext struct {
	*Context
	ToolID  string
	RawArgs json.RawMessage
}

// DecodeArgs unmarshals RawArgs into dst.
func (t *ToolContext) DecodeArgs(dst any) error {
	if t == nil || len(t.RawArgs) == 0 || string(t.RawArgs) == "null" {
		return nil
	}
	if dst == nil {
		return errors.New("extsdk: nil destination")
	}
	return json.Unmarshal(t.RawArgs, dst)
}
