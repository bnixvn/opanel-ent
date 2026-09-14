//go:build !linux

// A pseudo-terminal here is Linux ioctls and nothing else. The agent only
// ever runs on Linux; this exists so the package still builds on a
// developer's machine.
package pty

import "errors"

// Terminal is the unsupported stand-in.
type Terminal struct{ Master *nothing }

type nothing struct{}

// Options mirrors the Linux definition so callers compile.
type Options struct {
	Path   string
	Args   []string
	Dir    string
	Env    []string
	UID    uint32
	GID    uint32
	Groups []uint32
	Cols   uint16
	Rows   uint16
}

var errUnsupported = errors.New("pty: only supported on Linux")

// Start always fails off Linux.
func Start(Options) (*Terminal, error) { return nil, errUnsupported }

// Resize always fails off Linux.
func (t *Terminal) Resize(uint16, uint16) error { return errUnsupported }

// Wait always fails off Linux.
func (t *Terminal) Wait() error { return errUnsupported }

// Close does nothing off Linux.
func (t *Terminal) Close() {}
