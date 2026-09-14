//go:build !linux

package linuxuser

import "io/fs"

// ownerOf has no answer off Linux. Reporting "unknown" rather than a guess
// means RepairHome leaves ownership alone and only corrects the mode, which
// is the right thing to do on a platform that does not have this concept.
func ownerOf(fs.FileInfo) (uid, gid int64, ok bool) { return 0, 0, false }
