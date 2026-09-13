//go:build linux

package agent

import (
	"errors"
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// Credential identifies the process on the other end of a unix socket.
type Credential struct {
	UID uint32
	GID uint32
	PID int32
}

// peerCredential reads SO_PEERCRED from the connection.
//
// The kernel fills these values in at connect time from the calling process's
// real identity. A client cannot set or spoof them, which is what makes the
// socket a genuine authentication boundary rather than a convention.
func peerCredential(conn net.Conn) (Credential, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return Credential{}, errors.New("agent: connection is not a unix socket")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return Credential{}, fmt.Errorf("agent: raw conn: %w", err)
	}

	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return Credential{}, fmt.Errorf("agent: control fd: %w", err)
	}
	if credErr != nil {
		return Credential{}, fmt.Errorf("agent: getsockopt SO_PEERCRED: %w", credErr)
	}
	return Credential{UID: cred.Uid, GID: cred.Gid, PID: cred.Pid}, nil
}
