//go:build !linux

package actions

import "errors"

// diskUsage has no meaning off Linux; the agent only ever runs there. It
// exists so the package still builds on a developer's machine.
func diskUsage(string) (total, free uint64, err error) {
	return 0, 0, errors.New("not supported on this platform")
}
