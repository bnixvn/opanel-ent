package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// handler is the type-erased form of a registered action.
type handler struct {
	version int
	// timeout overrides the server's default budget. Zero means use it.
	timeout time.Duration
	fn      func(context.Context, json.RawMessage) (any, error)
}

// Registry holds the actions an agent will serve. Actions are registered at
// startup and never change, so lookups need no lock beyond construction.
type Registry struct {
	mu      sync.RWMutex
	actions map[string]handler
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{actions: make(map[string]handler)}
}

// Register adds a typed action. In and Out are ordinary structs; the registry
// handles JSON decoding and encoding, so a handler never sees raw bytes and
// cannot forget to validate the shape of its input.
//
// It panics on a duplicate name: that is a programming error discovered at
// startup, not a runtime condition.
func Register[In, Out any](r *Registry, name string, version int, fn func(context.Context, In) (Out, error)) {
	register(r, name, version, 0, fn)
}

// RegisterSlow is Register for an action that legitimately takes longer than
// the server's default budget -- archiving a customer's whole home directory,
// restoring one, dumping a large database. Without it the only ways to allow
// those are to raise the budget for every action, which lets a wedged handler
// hold a slot for an hour, or to build a job queue, which is a lot of
// machinery for a handful of operations.
func RegisterSlow[In, Out any](r *Registry, name string, version int, timeout time.Duration,
	fn func(context.Context, In) (Out, error)) {
	register(r, name, version, timeout, fn)
}

func register[In, Out any](r *Registry, name string, version int, timeout time.Duration,
	fn func(context.Context, In) (Out, error)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.actions[name]; dup {
		panic("agent: action registered twice: " + name)
	}
	r.actions[name] = handler{
		version: version,
		timeout: timeout,
		fn: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in In
			if len(raw) > 0 {
				dec := json.NewDecoder(bytes.NewReader(raw))
				dec.DisallowUnknownFields() // a typo in a field name must fail, not be ignored
				if err := dec.Decode(&in); err != nil {
					return nil, &PayloadError{Err: err}
				}
			}
			if v, ok := any(&in).(Validator); ok {
				if err := v.Validate(); err != nil {
					return nil, &PayloadError{Err: err}
				}
			}
			return fn(ctx, in)
		},
	}
}

// Validator lets an input struct check itself. Implement it on the pointer
// receiver; the registry calls it after decoding and before the handler runs.
type Validator interface {
	Validate() error
}

// PayloadError marks a caller-side input problem so the server can answer
// with CodeBadPayload rather than CodeInternal.
type PayloadError struct{ Err error }

func (e *PayloadError) Error() string { return "invalid payload: " + e.Err.Error() }
func (e *PayloadError) Unwrap() error { return e.Err }

// DeniedError marks a refusal by policy rather than a failure.
type DeniedError struct{ Reason string }

func (e *DeniedError) Error() string { return "denied: " + e.Reason }

// Budget returns how long the named action may run, falling back to def for
// an action that did not ask for more (and for one that does not exist -- the
// lookup that follows reports that properly).
func (r *Registry) Budget(name string, def time.Duration) time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if h, ok := r.actions[name]; ok && h.timeout > 0 {
		return h.timeout
	}
	return def
}

// lookup resolves an action and checks the requested version.
func (r *Registry) lookup(name string, version int) (handler, error) {
	r.mu.RLock()
	h, ok := r.actions[name]
	r.mu.RUnlock()
	if !ok {
		return handler{}, ErrUnknownAction{Name: name}
	}
	// Zero means "whatever the agent has", for opanelctl and for anybody
	// holding a socket open by hand. Every call from the panel pins a
	// version, which is where the guarantee is needed and where it applies.
	if version != 0 && version != h.version {
		return handler{}, ErrVersionMismatch{Name: name, Has: h.version, Wanted: version}
	}
	return h, nil
}

// ErrUnknownAction and ErrVersionMismatch are distinct types because the
// caller has to tell them apart to pick a code, and it used to guess: it
// reported version_mismatch whenever the request carried a version, which
// every real request does. A mistyped action name therefore came back as a
// version problem, which is the one answer guaranteed to send somebody
// looking in the wrong place.
type ErrUnknownAction struct{ Name string }

func (e ErrUnknownAction) Error() string { return fmt.Sprintf("unknown action %q", e.Name) }

// ErrVersionMismatch means the action exists but not at the version asked for.
type ErrVersionMismatch struct {
	Name   string
	Has    int
	Wanted int
}

func (e ErrVersionMismatch) Error() string {
	return fmt.Sprintf("action %q is version %d, caller asked for %d", e.Name, e.Has, e.Wanted)
}

// Actions lists registered action names with their versions, sorted. Used by
// opanelctl to show what an agent build supports.
func (r *Registry) Actions() map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]int, len(r.actions))
	for k, v := range r.actions {
		out[k] = v.version
	}
	return out
}
