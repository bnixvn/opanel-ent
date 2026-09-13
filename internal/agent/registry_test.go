package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type echoIn struct {
	Text string `json:"text"`
	N    int    `json:"n"`
}

func (e *echoIn) Validate() error {
	if e.Text == "" {
		return errors.New("text must not be empty")
	}
	return nil
}

type echoOut struct {
	Echo string `json:"echo"`
}

func newTestRegistry() *Registry {
	r := NewRegistry()
	Register(r, "echo", 1, func(_ context.Context, in echoIn) (echoOut, error) {
		return echoOut{Echo: strings.Repeat(in.Text, max(in.N, 1))}, nil
	})
	Register(r, "boom", 1, func(context.Context, struct{}) (struct{}, error) {
		return struct{}{}, errors.New("handler exploded")
	})
	Register(r, "nope", 1, func(context.Context, struct{}) (struct{}, error) {
		return struct{}{}, &DeniedError{Reason: "not allowed here"}
	})
	return r
}

func call(t *testing.T, r *Registry, action string, version int, payload string) (any, error) {
	t.Helper()
	h, err := r.lookup(action, version)
	if err != nil {
		return nil, err
	}
	return h.fn(context.Background(), json.RawMessage(payload))
}

func TestRegisterAndDispatch(t *testing.T) {
	r := newTestRegistry()
	out, err := call(t, r, "echo", 1, `{"text":"ab","n":3}`)
	if err != nil {
		t.Fatalf("echo: %v", err)
	}
	if got := out.(echoOut).Echo; got != "ababab" {
		t.Fatalf("echo = %q, want %q", got, "ababab")
	}
}

func TestUnknownActionAndVersion(t *testing.T) {
	r := newTestRegistry()
	if _, err := call(t, r, "missing", 1, `{}`); err == nil {
		t.Fatal("unknown action was accepted")
	}
	if _, err := call(t, r, "echo", 99, `{"text":"a"}`); err == nil {
		t.Fatal("version mismatch was accepted")
	}
	// Version 0 means "whatever the agent has", used by tools that do not
	// pin a contract.
	if _, err := call(t, r, "echo", 0, `{"text":"a"}`); err != nil {
		t.Fatalf("version 0 should match any version: %v", err)
	}
}

func TestUnknownFieldIsRejected(t *testing.T) {
	// A misspelled field must fail loudly. Silently ignoring it would let a
	// caller believe it passed an argument that never arrived.
	r := newTestRegistry()
	_, err := call(t, r, "echo", 1, `{"text":"a","txet":"typo"}`)
	var pe *PayloadError
	if !errors.As(err, &pe) {
		t.Fatalf("got %v, want PayloadError", err)
	}
}

func TestValidatorRuns(t *testing.T) {
	r := newTestRegistry()
	_, err := call(t, r, "echo", 1, `{"text":""}`)
	var pe *PayloadError
	if !errors.As(err, &pe) {
		t.Fatalf("got %v, want PayloadError from Validate", err)
	}
	if !strings.Contains(err.Error(), "text must not be empty") {
		t.Fatalf("validator message lost: %v", err)
	}
}

func TestErrorClassification(t *testing.T) {
	r := newTestRegistry()

	_, err := call(t, r, "nope", 1, `{}`)
	var de *DeniedError
	if !errors.As(err, &de) {
		t.Fatalf("got %v, want DeniedError", err)
	}

	_, err = call(t, r, "boom", 1, `{}`)
	if err == nil || errors.As(err, &de) {
		t.Fatalf("a plain handler error must not be classified as denied: %v", err)
	}
}

func TestDuplicateRegistrationPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("registering the same action twice should panic at startup")
		}
	}()
	r := NewRegistry()
	Register(r, "dup", 1, func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil })
	Register(r, "dup", 1, func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil })
}

func TestSortedNames(t *testing.T) {
	r := newTestRegistry()
	names := r.SortedNames()
	if len(names) != 3 {
		t.Fatalf("got %d actions, want 3", len(names))
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("names are not sorted: %v", names)
		}
	}
}

func TestReadRequestRejectsGarbage(t *testing.T) {
	for name, in := range map[string]string{
		"empty":     "",
		"not json":  "hello\n",
		"no action": `{"version":1}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readRequest(strings.NewReader(in)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestReadRequestAcceptsValid(t *testing.T) {
	req, err := readRequest(strings.NewReader(`{"action":"ping","version":1}` + "\n"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if req.Action != "ping" || req.Version != 1 {
		t.Fatalf("decoded %+v", req)
	}
}
