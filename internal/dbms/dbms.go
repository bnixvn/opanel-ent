// Package dbms manages MariaDB databases and users for hosted sites.
//
// It connects as root over the unix socket, which MariaDB authenticates with
// the unix_socket plugin — so there is no administrative password to store
// anywhere. Only the agent, which already runs as root, can do this.
//
// SQL identifiers cannot be parameterised: a database or user name has to be
// interpolated into the statement. That is the same class of hazard as
// building a shell command, and it is handled the same way — names are
// validated against a strict pattern up front and quoted on the way out,
// rather than filtered for dangerous characters.
package dbms

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// Socket is where MariaDB listens locally on EL10. Confirmed on the target
// host; Debian's /run/mysqld/mysqld.sock does not apply here.
const Socket = "/var/lib/mysql/mysql.sock"

// Limits imposed by MariaDB itself.
const (
	MaxDatabaseName = 64
	MaxUserName     = 32 // MariaDB allows 80, but 32 keeps older clients happy
)

// Charset defaults. utf8mb4 is the only correct choice: utf8 in MySQL is a
// three-byte subset that cannot store emoji or many CJK characters.
const (
	Charset   = "utf8mb4"
	Collation = "utf8mb4_unicode_ci"
)

// Host is the host part of every account the panel creates. Sites connect
// over the local socket or 127.0.0.1, never from elsewhere.
const Host = "localhost"

// passwordPattern is the alphabet a database password may use.
//
// MariaDB does not accept a placeholder in CREATE USER ... IDENTIFIED BY, so
// the password has to be written into the statement text. Rather than escape
// it, the panel restricts it to characters that need no escaping — which
// costs nothing, because the panel generates these passwords itself from
// base64url and never asks a human to choose one. If a caller-supplied
// password is ever allowed, this is the line that has to change, and it must
// change to a real escaping routine and not to a wider pattern.
var passwordPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{12,128}$`)

// ValidPassword reports whether p can be used safely.
func ValidPassword(p string) bool { return passwordPattern.MatchString(p) }

// quotePassword renders a validated password as a SQL string literal.
func quotePassword(p string) string { return "'" + p + "'" }

// namePattern is deliberately narrow. Anything outside it is rejected instead
// of escaped, because an identifier reaches the statement text directly.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ValidDatabaseName reports whether s may be used as a database name.
func ValidDatabaseName(s string) bool {
	return len(s) <= MaxDatabaseName && namePattern.MatchString(s)
}

// ValidUserName reports whether s may be used as a database account name.
func ValidUserName(s string) bool {
	return len(s) <= MaxUserName && namePattern.MatchString(s)
}

// Prefixed builds the owner-scoped name a panel object gets.
//
// The convention matters beyond tidiness: CloudLinux MySQL Governor maps
// database accounts to system users, and Stage B feeds it a mapping built
// from exactly this prefix.
func Prefixed(owner, suffix string) string { return owner + "_" + suffix }

// Errors callers branch on.
var (
	ErrExists   = errors.New("dbms: already exists")
	ErrNotFound = errors.New("dbms: not found")
	ErrBadName  = errors.New("dbms: invalid name")
)

// Server talks to the local MariaDB instance.
type Server struct {
	db *sql.DB
}

// Open connects as root over the unix socket.
func Open(ctx context.Context) (*Server, error) {
	dsn := fmt.Sprintf("root@unix(%s)/?charset=%s&parseTime=true&timeout=10s", Socket, Charset)
	conn, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("dbms: open: %w", err)
	}
	conn.SetMaxOpenConns(4)
	conn.SetConnMaxLifetime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.PingContext(pingCtx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("dbms: connect to MariaDB on %s: %w", Socket, err)
	}
	return &Server{db: conn}, nil
}

// Close releases the connection pool.
func (s *Server) Close() error { return s.db.Close() }

// quoteIdent wraps a validated identifier in backticks.
//
// The doubling of backticks is belt and braces: namePattern already rejects
// them, but an identifier that reaches SQL unquoted is the kind of mistake
// that only shows up once, in production, in someone else's data.
func quoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// CreateDatabase creates a database. Existing databases are reported as
// ErrExists rather than silently reused: a name collision between two
// customers must never be resolved by handing over someone else's data.
func (s *Server) CreateDatabase(ctx context.Context, name string) error {
	if !ValidDatabaseName(name) {
		return fmt.Errorf("%w: database name %q", ErrBadName, name)
	}
	exists, err := s.DatabaseExists(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("%w: database %q", ErrExists, name)
	}
	stmt := fmt.Sprintf("CREATE DATABASE %s CHARACTER SET %s COLLATE %s",
		quoteIdent(name), Charset, Collation)
	if _, err := s.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("dbms: create database %q: %w", name, err)
	}
	return nil
}

// DropDatabase removes a database and everything in it.
func (s *Server) DropDatabase(ctx context.Context, name string) error {
	if !ValidDatabaseName(name) {
		return fmt.Errorf("%w: database name %q", ErrBadName, name)
	}
	if _, err := s.db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(name)); err != nil {
		return fmt.Errorf("dbms: drop database %q: %w", name, err)
	}
	return nil
}

// DatabaseExists reports whether a database is present.
func (s *Server) DatabaseExists(ctx context.Context, name string) (bool, error) {
	if !ValidDatabaseName(name) {
		return false, fmt.Errorf("%w: database name %q", ErrBadName, name)
	}
	var found string
	err := s.db.QueryRowContext(ctx,
		`SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?`, name).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// CreateUser creates an account limited to connections from this host.
func (s *Server) CreateUser(ctx context.Context, username, password string) error {
	if !ValidUserName(username) {
		return fmt.Errorf("%w: user name %q", ErrBadName, username)
	}
	if !ValidPassword(password) {
		return errors.New("dbms: password must be 12-128 characters of letters, digits, underscore or hyphen")
	}
	exists, err := s.UserExists(ctx, username)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("%w: user %q", ErrExists, username)
	}
	stmt := fmt.Sprintf("CREATE USER %s@%s IDENTIFIED BY %s",
		quoteIdent(username), quoteIdent(Host), quotePassword(password))
	if _, err := s.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("dbms: create user %q: %w", username, err)
	}
	return nil
}

// DropUser removes an account.
func (s *Server) DropUser(ctx context.Context, username string) error {
	if !ValidUserName(username) {
		return fmt.Errorf("%w: user name %q", ErrBadName, username)
	}
	stmt := fmt.Sprintf("DROP USER IF EXISTS %s@%s", quoteIdent(username), quoteIdent(Host))
	if _, err := s.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("dbms: drop user %q: %w", username, err)
	}
	return nil
}

// SetPassword changes an account's password.
func (s *Server) SetPassword(ctx context.Context, username, password string) error {
	if !ValidUserName(username) {
		return fmt.Errorf("%w: user name %q", ErrBadName, username)
	}
	if !ValidPassword(password) {
		return errors.New("dbms: password must be 12-128 characters of letters, digits, underscore or hyphen")
	}
	stmt := fmt.Sprintf("ALTER USER %s@%s IDENTIFIED BY %s",
		quoteIdent(username), quoteIdent(Host), quotePassword(password))
	if _, err := s.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("dbms: set password for %q: %w", username, err)
	}
	return nil
}

// UserExists reports whether an account is present.
func (s *Server) UserExists(ctx context.Context, username string) (bool, error) {
	if !ValidUserName(username) {
		return false, fmt.Errorf("%w: user name %q", ErrBadName, username)
	}
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mysql.user WHERE User = ? AND Host = ?`, username, Host).Scan(&n)
	return n > 0, err
}

// Grant gives an account full rights on one database and nothing else.
//
// Deliberately not GRANT ALL ON *.*: a customer's application only ever needs
// its own schema, and a compromised site should not be able to read another
// customer's data through a shared account.
func (s *Server) Grant(ctx context.Context, username, database string) error {
	if !ValidUserName(username) {
		return fmt.Errorf("%w: user name %q", ErrBadName, username)
	}
	if !ValidDatabaseName(database) {
		return fmt.Errorf("%w: database name %q", ErrBadName, database)
	}
	stmt := fmt.Sprintf("GRANT ALL PRIVILEGES ON %s.* TO %s@%s",
		quoteIdent(database), quoteIdent(username), quoteIdent(Host))
	if _, err := s.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("dbms: grant %q on %q: %w", username, database, err)
	}
	if _, err := s.db.ExecContext(ctx, "FLUSH PRIVILEGES"); err != nil {
		return fmt.Errorf("dbms: flush privileges: %w", err)
	}
	return nil
}

// Revoke removes an account's rights on a database.
func (s *Server) Revoke(ctx context.Context, username, database string) error {
	if !ValidUserName(username) {
		return fmt.Errorf("%w: user name %q", ErrBadName, username)
	}
	if !ValidDatabaseName(database) {
		return fmt.Errorf("%w: database name %q", ErrBadName, database)
	}
	stmt := fmt.Sprintf("REVOKE ALL PRIVILEGES ON %s.* FROM %s@%s",
		quoteIdent(database), quoteIdent(username), quoteIdent(Host))
	if _, err := s.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("dbms: revoke %q on %q: %w", username, database, err)
	}
	_, err := s.db.ExecContext(ctx, "FLUSH PRIVILEGES")
	return err
}

// DatabaseSize returns the on-disk size of a database in bytes.
//
// information_schema figures are approximate for InnoDB — they come from
// table statistics, not from the filesystem — but they are what every panel
// shows and they are cheap. Quota enforcement should not depend on them.
func (s *Server) DatabaseSize(ctx context.Context, name string) (int64, error) {
	if !ValidDatabaseName(name) {
		return 0, fmt.Errorf("%w: database name %q", ErrBadName, name)
	}
	var size sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT SUM(data_length + index_length) FROM information_schema.TABLES WHERE table_schema = ?`,
		name).Scan(&size)
	if err != nil {
		return 0, fmt.Errorf("dbms: size of %q: %w", name, err)
	}
	return size.Int64, nil
}

// Version reports the running server version.
func (s *Server) Version(ctx context.Context) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&v)
	return v, err
}

// ListDatabases returns the databases the panel could own, excluding the
// server's own system schemas.
func (s *Server) ListDatabases(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT SCHEMA_NAME FROM information_schema.SCHEMATA
		 WHERE SCHEMA_NAME NOT IN ('information_schema','mysql','performance_schema','sys')
		 ORDER BY SCHEMA_NAME`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
