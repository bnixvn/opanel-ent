package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

// handler is the type-erased form of a registered action.
type handler struct {
	version int
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
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.actions[name]; dup {
		panic("agent: action registered twice: " + name)
	}
	r.actions[name] = handler{
		version: version,
		fn: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var in In
			if len(raw) > 0 {
				dec := json.NewDecoder(bytes.NewReader(raw))
				dec.DisallowUnknownFields() // a typo in a field name must fail, not be ignored
				if err := dec.Decode(&in); err != nil {
					return nil, &PayloadError{err: err}
				}
			}
			if v, ok := any(&in).(Validator); ok {
				if err := v.Validate(); err != nil {
					return nil, &PayloadError{err: err}
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
type PayloadError struct{ err error }

func (e *PayloadError) Error() string { return "invalid payload: " + e.err.Error() }
func (e *PayloadError) Unwrap() error { return e.err }

// DeniedError marks a refusal by policy rather than a failure.
type DeniedError struct{ Reason string }

func (e *DeniedError) Error() string { return "denied: " + e.Reason }

// lookup resolves an action and checks the requested version.
func (r *Registry) lookup(name string, version int) (handler, error) {
	r.mu.RLock()
	h, ok := r.actions[name]
	r.mu.RUnlock()
	if !ok {
		return handler{}, fmt.Errorf("unknown action %q", name)
	}
	if version != 0 && version != h.version {
		return handler{}, fmt.Errorf("action %q is version %d, caller asked for %d",
			name, h.version, version)
	}
	return h, nil
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

// SortedNames returns action names in a stable order.
func (r *Registry) SortedNames() []string {
	m := r.Actions()
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
