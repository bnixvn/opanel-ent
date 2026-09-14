//go:build !linux

// A pseudo-terminal here is Linux ioctls and nothing else. The agent only
// ever runs on Linux; this exists so the package -- and everything that
// imports it, which is most of the panel -- still compiles on a developer's
// machine. Tests that only run in CI are tests nobody runs.
package pty

import (
	"errors"
	"os"
)

// Terminal is the unsupported stand-in.
//
// Master is a real *os.File, always nil, because callers read and write it:
// a placeholder type without those methods would compile here and break
// every package that uses this one.
type Terminal struct{ Master *os.File }

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

// Start always fails off Linux, so nothing ever holds a Terminal here.
func Start(Options) (*Terminal, error) { return nil, errUnsupported }

// Resize always fails off Linux.
func (t *Terminal) Resize(uint16, uint16) error { return errUnsupported }

// Close does nothing off Linux.
func (t *Terminal) Close() {}
