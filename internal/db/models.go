package db

import "time"

// Role is stored as a plain string here so this package stays free of policy
// logic; internal/auth owns the meaning of each value.
type User struct {
	ID           int64
	Username     string
	Email        string
	PasswordHash string
	Role         string
	// ParentID is the reseller who owns this account, nil when the account
	// belongs to the server itself.
	ParentID    *int64
	LinuxUID    *int64
	TOTPSecret  string
	TOTPEnabled bool
	Suspended   bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Session struct {
	ID         string // SHA-256 of the cookie value
	UserID     int64
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
	IP         string
	UserAgent  string
	// ImpersonatorID is the member of staff driving this session, or 0 for
	// an ordinary sign-in. The session still belongs to UserID: that is what
	// makes every permission check see the customer rather than the
	// operator.
	ImpersonatorID int64
}

type APIToken struct {
	ID         int64
	Name       string
	UserID     int64
	Prefix     string
	TokenHash  string
	Scopes     []string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// AuditEntry is one row of the append-only audit trail.
type AuditEntry struct {
	ID        int64
	At        time.Time
	ActorType string // "user" | "token" | "system" | "agent"
	ActorID   *int64
	ActorName string
	Action    string
	Target    string
	OK        bool
	Detail    string
	IP        string
}

// timestamps are stored as RFC3339Nano in UTC so ordering is lexicographic
// and comparisons in SQL work without date functions.
func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

func fmtTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return fmtTime(*t)
}
