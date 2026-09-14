// Package agentclient talks to opanel-agent over its unix socket.
package agentclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
)

// Client dials the agent socket. It holds no connection between calls: each
// request opens its own, which keeps the protocol free of multiplexing state
// and means a wedged action cannot block unrelated ones.
type Client struct {
	socket  string
	timeout time.Duration
}

// New returns a client for the socket at path. A zero timeout means 2m30s,
// slightly above the agent's own per-action budget so the server's timeout
// reply wins the race and the caller sees a real error code.
func New(path string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 150 * time.Second
	}
	return &Client{socket: path, timeout: timeout}
}

// Socket returns the configured socket path.
func (c *Client) Socket() string { return c.socket }

// WithTimeout returns a client for the same socket with a different deadline.
// Used for the few actions -- backup, restore -- that are expected to run for
// minutes, without loosening the deadline that protects every other call.
func (c *Client) WithTimeout(d time.Duration) *Client {
	return &Client{socket: c.socket, timeout: d}
}

// Error is a failure reported by the agent, carrying its machine-readable code.
type Error struct {
	Action string
	Code   string
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("agent action %s failed (%s): %s", e.Action, e.Code, e.Msg)
}

// IsDenied reports whether err is an agent refusal by policy.
func IsDenied(err error) bool {
	var ae *Error
	return errors.As(err, &ae) && ae.Code == agent.CodeDenied
}

// Ping checks that the agent is reachable and answering.
func (c *Client) Ping(ctx context.Context) error {
	_, err := Call[PingResult](ctx, c, "ping", 1, struct{}{})
	return err
}

// PingResult is the reply from the built-in ping action.
type PingResult struct {
	Agent   string `json:"agent"`
	Version string `json:"version"`
	PID     int    `json:"pid"`
}

// Call performs one typed request. Out is decoded from the response payload.
func Call[Out any, In any](ctx context.Context, c *Client, action string, version int, in In) (Out, error) {
	var zero Out

	payload, err := json.Marshal(in)
	if err != nil {
		return zero, fmt.Errorf("agentclient: encode payload for %s: %w", action, err)
	}
	body, err := json.Marshal(agent.Request{Action: action, Version: version, Payload: payload})
	if err != nil {
		return zero, fmt.Errorf("agentclient: encode request for %s: %w", action, err)
	}
	if len(body) > agent.MaxMessageBytes {
		return zero, fmt.Errorf("agentclient: request for %s exceeds %d bytes", action, agent.MaxMessageBytes)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return zero, fmt.Errorf("agentclient: dial %s: %w", c.socket, err)
	}
	defer func() { _ = conn.Close() }()

	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if _, err := conn.Write(append(body, '\n')); err != nil {
		return zero, fmt.Errorf("agentclient: write %s: %w", action, err)
	}

	br := bufio.NewReader(io.LimitReader(conn, agent.MaxMessageBytes+1))
	line, err := br.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return zero, fmt.Errorf("agentclient: read reply for %s: %w", action, err)
	}
	if len(line) == 0 {
		return zero, fmt.Errorf("agentclient: agent closed connection without replying to %s", action)
	}

	var resp agent.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return zero, fmt.Errorf("agentclient: decode reply for %s: %w", action, err)
	}
	if !resp.OK {
		return zero, &Error{Action: action, Code: resp.Code, Msg: resp.Error}
	}
	if len(resp.Payload) == 0 {
		return zero, nil
	}
	if err := json.Unmarshal(resp.Payload, &zero); err != nil {
		return zero, fmt.Errorf("agentclient: decode result of %s: %w", action, err)
	}
	return zero, nil
}
