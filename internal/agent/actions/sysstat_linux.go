package actions

import "golang.org/x/sys/unix"

// diskUsage returns the size of the filesystem holding path, and how much of
// it is free, both in bytes.
//
// Free rather than available: this is the operator's view of their own
// server, and the root reserve is space they can actually reach.
func diskUsage(path string) (total, free uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return st.Blocks * bs, st.Bfree * bs, nil
}
