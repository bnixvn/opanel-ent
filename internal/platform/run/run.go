// Package run executes external commands.
//
// Every entry point takes an argv slice. There is deliberately no helper that
// accepts a command line as one string: the absence of a shell is what makes
// caller-supplied values (domain names, usernames, package names) safe to
// pass through without quoting rules.
package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Result is the outcome of one command.
type Result struct {
	Args     []string
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
}

// Failed reports a non-zero exit.
func (r Result) Failed() bool { return r.ExitCode != 0 }

// Output returns stdout, or stderr when stdout is empty. Useful for tools
// that report on the wrong stream.
func (r Result) Output() string {
	if s := strings.TrimSpace(r.Stdout); s != "" {
		return s
	}
	return strings.TrimSpace(r.Stderr)
}

// Error describes a command that could not be run or exited non-zero.
type Error struct {
	Result
	err error
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("run %s: exit %d", strings.Join(e.Args, " "), e.ExitCode)
	if out := strings.TrimSpace(e.Stderr); out != "" {
		msg += ": " + firstLine(out)
	}
	if e.err != nil {
		msg += ": " + e.err.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.err }

// Option adjusts one invocation.
type Option func(*options)

type options struct {
	timeout time.Duration
	stdin   string
	// stdinFrom streams input instead of holding it in memory, for the cases
	// where the input is a database dump rather than a short string.
	stdinFrom io.Reader
	env       []string
	dir       string
	// okCodes lists exit codes to treat as success besides 0. dnf uses 100
	// for "updates available", which is information rather than failure.
	okCodes []int
}

// Timeout bounds the command. Default 60s.
func Timeout(d time.Duration) Option { return func(o *options) { o.timeout = d } }

// Stdin feeds data to the command.
func Stdin(s string) Option { return func(o *options) { o.stdin = s } }

// StdinFrom streams input to the command.
//
// Separate from Stdin because the caller that needs it is feeding a database
// dump out of a tar archive: holding that in a string would mean the whole
// dump in memory, twice, at the moment a migration is already filling the
// server.
func StdinFrom(r io.Reader) Option { return func(o *options) { o.stdinFrom = r } }

// Env adds environment variables, each as "NAME=value". They are appended to
// the process environment, so a name that is already there is overridden.
func Env(vars ...string) Option { return func(o *options) { o.env = append(o.env, vars...) } }

// Dir runs the command in a directory. Used where a vendor's installer
// expects to be run from its own unpacked tree and reads files beside itself.
func Dir(d string) Option { return func(o *options) { o.dir = d } }

// AllowExit marks extra exit codes as success.
func AllowExit(codes ...int) Option {
	return func(o *options) { o.okCodes = append(o.okCodes, codes...) }
}

// Cmd runs argv and returns its result. A non-zero exit that is not listed by
// AllowExit is returned as *Error, with the Result still populated so callers
// can inspect the output of a failure.
func Cmd(ctx context.Context, argv []string, opts ...Option) (Result, error) {
	if len(argv) == 0 {
		return Result{}, errors.New("run: empty argv")
	}
	o := options{timeout: 60 * time.Second}
	for _, fn := range opts {
		fn(&o)
	}

	ctx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = o.dir
	if len(o.env) > 0 {
		cmd.Env = append(cmd.Environ(), o.env...)
	}
	switch {
	case o.stdinFrom != nil:
		cmd.Stdin = o.stdinFrom
	case o.stdin != "":
		cmd.Stdin = strings.NewReader(o.stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	res := Result{
		Args:     argv,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start),
	}

	var ee *exec.ExitError
	switch {
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
	default:
		res.ExitCode = -1
		return res, &Error{Result: res, err: err}
	}

	if res.ExitCode != 0 {
		for _, c := range o.okCodes {
			if res.ExitCode == c {
				return res, nil
			}
		}
		if ctx.Err() != nil {
			return res, &Error{Result: res, err: ctx.Err()}
		}
		return res, &Error{Result: res}
	}
	return res, nil
}

// Look reports whether a binary exists on PATH.
func Look(bin string) (string, bool) {
	p, err := exec.LookPath(bin)
	return p, err == nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
