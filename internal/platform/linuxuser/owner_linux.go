package linuxuser

import (
	"io/fs"
	"syscall"
)

// ownerOf reports the uid and gid a file belongs to.
//
// Split into a build-tagged file because syscall.Stat_t does not exist off
// Unix, and a package that will not compile on a developer's laptop is a
// package whose tests only run in CI -- which is where they are least useful.
func ownerOf(fi fs.FileInfo) (uid, gid int64, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int64(st.Uid), int64(st.Gid), true
}
