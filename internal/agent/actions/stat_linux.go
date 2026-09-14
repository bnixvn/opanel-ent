//go:build linux

package actions

import (
	"os"
	"syscall"
)

// statUID returns the owning uid of a stat result. Split by platform because
// the field only exists in the Unix syscall structure; the panel builds on a
// developer's machine too, and a build that only works on the target host is
// a build nobody runs the tests on.
func statUID(info os.FileInfo) (uint32, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}
