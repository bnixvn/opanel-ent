// Package pty opens a pseudo-terminal and runs a process on it.
//
// Hand-rolled rather than pulled from a package: it is four ioctls, it is
// the only place in the panel that needs one, and a dependency that runs as
// root is a dependency somebody has to keep reading.
package pty

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// Terminal is a running process attached to a pseudo-terminal.
type Terminal struct {
	// Master is the side the panel reads from and writes to. Reading it
	// yields what the program printed; writing to it is what the program
	// sees as somebody typing.
	Master *os.File
	cmd    *exec.Cmd
	tty    *os.File
}

// Options describe the process to start.
type Options struct {
	Path string
	Args []string
	Dir  string
	Env  []string
	UID  uint32
	GID  uint32
	// Groups are the supplementary groups. Passing an empty slice is not the
	// same as passing none: nil leaves the child with root's supplementary
	// groups, which is the one mistake in this file worth being loud about.
	Groups []uint32
	Cols   uint16
	Rows   uint16
}

// Start opens a pty and runs the process on it.
func Start(opts Options) (*Terminal, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("pty: open /dev/ptmx: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = master.Close()
		}
	}()

	// Unlock the slave before asking for its number; the kernel refuses to
	// open a locked one.
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		return nil, fmt.Errorf("pty: unlock: %w", err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		return nil, fmt.Errorf("pty: get slave number: %w", err)
	}
	name := fmt.Sprintf("/dev/pts/%d", n)
	tty, err := os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("pty: open %s: %w", name, err)
	}
	defer func() {
		if !ok {
			_ = tty.Close()
		}
	}()

	// The terminal the program sees must belong to the user running in it,
	// or anything that opens /dev/tty -- sudo, ssh, less -- fails.
	if err := tty.Chown(int(opts.UID), int(opts.GID)); err != nil {
		return nil, fmt.Errorf("pty: chown %s: %w", name, err)
	}

	cmd := exec.Command(opts.Path, opts.Args...)
	cmd.Dir = opts.Dir
	cmd.Env = opts.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// A session of its own, with this pty as its controlling terminal:
		// without both, job control does not work and Ctrl-C kills nothing.
		Setsid:  true,
		Setctty: true,
		Credential: &syscall.Credential{
			Uid:    opts.UID,
			Gid:    opts.GID,
			Groups: opts.Groups,
		},
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("pty: start %s: %w", opts.Path, err)
	}

	t := &Terminal{Master: master, cmd: cmd, tty: tty}
	if opts.Cols > 0 && opts.Rows > 0 {
		_ = t.Resize(opts.Cols, opts.Rows)
	}
	ok = true
	return t, nil
}

// Resize tells the program the window changed. Without this a full-screen
// program draws for the size it was told at startup and nothing else.
func (t *Terminal) Resize(cols, rows uint16) error {
	return unix.IoctlSetWinsize(int(t.Master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{
		Row: rows,
		Col: cols,
	})
}

// Wait blocks until the process exits.
func (t *Terminal) Wait() error { return t.cmd.Wait() }

// Close kills the process and releases both ends.
//
// The signal goes to the process group, not the process: the shell is the
// only thing this package started, but whatever the shell started is what is
// actually holding the terminal open.
func (t *Terminal) Close() {
	if t.cmd.Process != nil {
		_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGHUP)
		_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
	}
	_ = t.tty.Close()
	_ = t.Master.Close()
}
