//go:build !linux

package agent

import (
	"errors"
	"net"
)

// Credential identifies the process on the other end of a unix socket.
type Credential struct {
	UID uint32
	GID uint32
	PID int32
}

// peerCredential is unavailable off Linux. The agent only ever runs on the
// target host; this stub exists so the tree still builds on a workstation for
// editing and for running the platform-independent tests.
func peerCredential(net.Conn) (Credential, error) {
	return Credential{}, errors.New("agent: peer credentials require Linux")
}
