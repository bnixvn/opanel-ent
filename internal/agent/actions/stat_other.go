//go:build !linux

package actions

import "os"

// statUID has no answer off Unix; listings simply omit the owner column.
func statUID(os.FileInfo) (uint32, bool) { return 0, false }
