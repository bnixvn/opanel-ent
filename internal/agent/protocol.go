// Package agent implements the privileged side of OPanel.
//
// opanel-api runs unprivileged and can do nothing to the host on its own. It
// asks opanel-agent, which runs as root behind a unix socket, to carry out
// each privileged step. Every action is a typed Go function with a validated
// input struct, and every external command is executed with an argv slice --
// never a shell string. That is the whole point of the split: it removes
// shell quoting, word splitting and injection from the threat model instead
// of trying to filter them out with regular expressions.
package agent

import "encoding/json"

// SocketPath is the default unix socket. Mode 0660, owned root:opanel.
const SocketPath = "/run/opanel/agent.sock"

// MaxMessageBytes caps a single request or response. Actions move config
// text, not files; anything larger is a bug or an attack.
const MaxMessageBytes = 4 << 20 // 4 MiB

// Request is one call. The wire format is a single JSON object followed by a
// newline; the connection carries exactly one request and one response.
type Request struct {
	// ID is echoed back so a caller can correlate logs. It has no protocol
	// meaning: one connection carries one exchange.
	ID string `json:"id,omitempty"`
	// Action names the handler, e.g. "ping" or "systemd.restart".
	Action string `json:"action"`
	// Version pins the payload shape. An action bumps it when its input
	// struct changes incompatibly, so a stale api binary fails loudly
	// instead of being misread by a newer agent.
	Version int `json:"version"`
	// Payload is the action-specific input, decoded by the handler.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Response is the single reply to a Request.
type Response struct {
	ID      string          `json:"id,omitempty"`
	OK      bool            `json:"ok"`
	Error   string          `json:"error,omitempty"`
	Code    string          `json:"code,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Error codes returned in Response.Code so callers can branch without
// string-matching the message.
const (
	CodeUnknownAction   = "unknown_action"
	CodeVersionMismatch = "version_mismatch"
	CodeBadPayload      = "bad_payload"
	CodeDenied          = "denied"
	CodeInternal        = "internal"
	CodeTimeout         = "timeout"
)
