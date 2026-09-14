// Package termproto is the wire format between the panel and the agent's
// terminal socket.
//
// The agent's ordinary socket is one request, one response, one connection.
// A terminal is neither: it is a stream that lives for minutes and carries
// traffic both ways at once. Rather than teach the action protocol about
// streams -- and give every action a way to hold a connection open -- the
// agent listens on a second socket that speaks only this.
package termproto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Hello is the first line the panel sends, as JSON with a newline after it.
type Hello struct {
	// User is the Linux account the shell runs as. The agent decides whether
	// it is allowed to, and the panel's opinion is not the one that counts.
	User string `json:"user"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// Ready is the agent's reply to Hello, also one JSON line.
type Ready struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Frame types. After the handshake every message is a frame.
const (
	// FrameData carries terminal bytes, in either direction.
	FrameData byte = 0
	// FrameResize carries four bytes: cols then rows, big-endian. Panel to
	// agent only.
	FrameResize byte = 1
	// FrameExit carries the process's exit status as a UTF-8 string. Agent
	// to panel only, and always the last frame.
	FrameExit byte = 2
)

// MaxFrame bounds one frame's payload. A terminal never legitimately sends a
// megabyte in one go, and without a ceiling a bad length is an allocation of
// whatever the sender felt like.
const MaxFrame = 1 << 20

// ErrFrameTooBig means the sender declared a payload past MaxFrame.
var ErrFrameTooBig = errors.New("termproto: frame too large")

// WriteFrame writes one frame: a type byte, a four-byte big-endian length,
// then the payload.
func WriteFrame(w io.Writer, kind byte, payload []byte) error {
	if len(payload) > MaxFrame {
		return ErrFrameTooBig
	}
	var head [5]byte
	head[0] = kind
	binary.BigEndian.PutUint32(head[1:], uint32(len(payload)))
	if _, err := w.Write(head[:]); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

// ReadFrame reads one frame. The returned slice is freshly allocated, so the
// caller may keep it.
func ReadFrame(r io.Reader) (kind byte, payload []byte, err error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(head[1:])
	if n > MaxFrame {
		return 0, nil, fmt.Errorf("%w: %d bytes", ErrFrameTooBig, n)
	}
	if n == 0 {
		return head[0], nil, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return head[0], buf, nil
}

// Size reads a resize payload.
func Size(payload []byte) (cols, rows uint16, ok bool) {
	if len(payload) != 4 {
		return 0, 0, false
	}
	return binary.BigEndian.Uint16(payload), binary.BigEndian.Uint16(payload[2:]), true
}

// SizePayload builds a resize payload.
func SizePayload(cols, rows uint16) []byte {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint16(buf, cols)
	binary.BigEndian.PutUint16(buf[2:], rows)
	return buf
}
