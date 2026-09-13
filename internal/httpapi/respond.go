package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// errorBody is the single error shape every endpoint returns, so clients can
// branch on code without parsing prose.
type errorBody struct {
	Error   string `json:"error"`
	Code    string `json:"code,omitempty"`
	Details any    `json:"details,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// The status line is already sent, so this can only be logged.
		slog.Error("httpapi: encode response", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: msg, Code: code})
}

// decodeJSON reads a request body into v, rejecting unknown fields so a
// misspelled key is reported instead of silently ignored.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	const maxBody = 1 << 20 // 1 MiB
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body: "+err.Error())
		return false
	}
	return true
}
