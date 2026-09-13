package actions

import (
	"context"
	"fmt"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/dbms"
)

// DatabaseRequest names a database.
type DatabaseRequest struct {
	Name string `json:"name"`
}

// Validate checks the name against MariaDB's rules and the panel's.
func (r *DatabaseRequest) Validate() error {
	if !dbms.ValidDatabaseName(r.Name) {
		return fmt.Errorf("database name %q is not acceptable: lowercase letters, "+
			"digits and underscore only, starting with a letter, at most %d characters",
			r.Name, dbms.MaxDatabaseName)
	}
	return nil
}

// DBUserRequest names a database account.
type DBUserRequest struct {
	Username string `json:"username"`
}

// Validate checks the account name.
func (r *DBUserRequest) Validate() error {
	if !dbms.ValidUserName(r.Username) {
		return fmt.Errorf("database user %q is not acceptable: lowercase letters, "+
			"digits and underscore only, starting with a letter, at most %d characters",
			r.Username, dbms.MaxUserName)
	}
	return nil
}

// DBUserPasswordRequest sets an account's password.
type DBUserPasswordRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Validate checks the name and password length.
func (r *DBUserPasswordRequest) Validate() error {
	if err := (&DBUserRequest{Username: r.Username}).Validate(); err != nil {
		return err
	}
	if len(r.Password) < 12 {
		return fmt.Errorf("database password must be at least 12 characters")
	}
	return nil
}

// GrantRequest links an account to a database.
type GrantRequest struct {
	Username string `json:"username"`
	Database string `json:"database"`
}

// Validate checks both names.
func (r *GrantRequest) Validate() error {
	if err := (&DBUserRequest{Username: r.Username}).Validate(); err != nil {
		return err
	}
	return (&DatabaseRequest{Name: r.Database}).Validate()
}

// DatabaseSizeResult reports a database's approximate size.
type DatabaseSizeResult struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
}

// DatabaseListResult lists what actually exists on the server, which is what
// a reconcile compares the panel's records against.
type DatabaseListResult struct {
	Version   string   `json:"version"`
	Databases []string `json:"databases"`
}

// withServer opens a connection, runs fn, and closes it.
//
// A connection per action rather than a pooled one: database work is
// infrequent, and holding a root connection open for the life of the agent
// buys nothing and keeps a privileged handle around for no reason.
func withServer[T any](ctx context.Context, fn func(*dbms.Server) (T, error)) (T, error) {
	var zero T
	srv, err := dbms.Open(ctx)
	if err != nil {
		return zero, err
	}
	defer func() { _ = srv.Close() }()
	return fn(srv)
}

func registerDatabases(r *agent.Registry) {
	agent.Register(r, "db.create", 1, func(ctx context.Context, in DatabaseRequest) (struct{}, error) {
		return withServer(ctx, func(s *dbms.Server) (struct{}, error) {
			return struct{}{}, s.CreateDatabase(ctx, in.Name)
		})
	})

	agent.Register(r, "db.drop", 1, func(ctx context.Context, in DatabaseRequest) (struct{}, error) {
		return withServer(ctx, func(s *dbms.Server) (struct{}, error) {
			return struct{}{}, s.DropDatabase(ctx, in.Name)
		})
	})

	agent.Register(r, "db.size", 1, func(ctx context.Context, in DatabaseRequest) (DatabaseSizeResult, error) {
		return withServer(ctx, func(s *dbms.Server) (DatabaseSizeResult, error) {
			n, err := s.DatabaseSize(ctx, in.Name)
			return DatabaseSizeResult{Name: in.Name, SizeBytes: n}, err
		})
	})

	agent.Register(r, "db.list", 1, func(ctx context.Context, _ struct{}) (DatabaseListResult, error) {
		return withServer(ctx, func(s *dbms.Server) (DatabaseListResult, error) {
			v, err := s.Version(ctx)
			if err != nil {
				return DatabaseListResult{}, err
			}
			names, err := s.ListDatabases(ctx)
			return DatabaseListResult{Version: v, Databases: names}, err
		})
	})

	agent.Register(r, "dbuser.create", 1, func(ctx context.Context, in DBUserPasswordRequest) (struct{}, error) {
		return withServer(ctx, func(s *dbms.Server) (struct{}, error) {
			return struct{}{}, s.CreateUser(ctx, in.Username, in.Password)
		})
	})

	agent.Register(r, "dbuser.drop", 1, func(ctx context.Context, in DBUserRequest) (struct{}, error) {
		return withServer(ctx, func(s *dbms.Server) (struct{}, error) {
			return struct{}{}, s.DropUser(ctx, in.Username)
		})
	})

	agent.Register(r, "dbuser.password", 1, func(ctx context.Context, in DBUserPasswordRequest) (struct{}, error) {
		return withServer(ctx, func(s *dbms.Server) (struct{}, error) {
			return struct{}{}, s.SetPassword(ctx, in.Username, in.Password)
		})
	})

	agent.Register(r, "db.grant", 1, func(ctx context.Context, in GrantRequest) (struct{}, error) {
		return withServer(ctx, func(s *dbms.Server) (struct{}, error) {
			return struct{}{}, s.Grant(ctx, in.Username, in.Database)
		})
	})

	agent.Register(r, "db.revoke", 1, func(ctx context.Context, in GrantRequest) (struct{}, error) {
		return withServer(ctx, func(s *dbms.Server) (struct{}, error) {
			return struct{}{}, s.Revoke(ctx, in.Username, in.Database)
		})
	})
}
