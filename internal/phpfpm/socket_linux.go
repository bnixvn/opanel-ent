//go:build linux

package phpfpm

import (
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// socketsMatch reports whether every pool's socket is there and owned by the
// account the pool file names.
//
// Comparing the file on disk to the file the panel would write is not enough
// to know the running server matches it: a master started from an earlier
// version of the same path keeps serving a socket that the current file would
// have made differently. The symptom is a site answering 503 with "Permission
// denied" on a socket that looks perfectly right in the configuration -- so
// what gets checked is the socket itself, not the text describing it.
func socketsMatch(pools []Pool) bool {
	for _, pool := range pools {
		info, err := os.Stat(pool.Socket)
		if err != nil {
			return false
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		u, err := user.Lookup(pool.SocketOwner)
		if err != nil {
			return false
		}
		if u.Uid != strconv.FormatUint(uint64(st.Uid), 10) {
			return false
		}
	}
	return true
}
