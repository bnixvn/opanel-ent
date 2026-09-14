package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Database is a MariaDB database the panel created.
type Database struct {
	ID            int64
	Name          string
	OwnerID       int64
	CreatedAt     time.Time
	OwnerUsername string
	OwnerParentID int64
	// Users is filled by listings that resolve grants.
	Users []string
	// SizeBytes is filled by listings that ask the server; it is not stored.
	SizeBytes int64
}

// DBUser is a MariaDB account the panel created.
type DBUser struct {
	ID            int64
	Username      string
	OwnerID       int64
	CreatedAt     time.Time
	OwnerUsername string
	OwnerParentID int64
	// Databases is filled by listings that resolve grants.
	Databases []string
}

// CreateDatabaseRecord stores a database the panel created on the server.
func (d *DB) CreateDatabaseRecord(ctx context.Context, name string, ownerID int64) (*Database, error) {
	now := time.Now().UTC()
	res, err := d.ExecContext(ctx,
		`INSERT INTO db_databases (name, owner_id, created_at) VALUES (?,?,?)`,
		name, ownerID, fmtTime(now))
	if err != nil {
		return nil, fmt.Errorf("db: record database %q: %w", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return d.DatabaseByID(ctx, id)
}

const dbCols = `d.id, d.name, d.owner_id, d.created_at, u.username, COALESCE(u.parent_id, 0)`

func scanDatabase(row interface{ Scan(...any) error }) (*Database, error) {
	var x Database
	var created string
	err := row.Scan(&x.ID, &x.Name, &x.OwnerID, &created, &x.OwnerUsername, &x.OwnerParentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if x.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	return &x, nil
}

// DatabaseByID looks a database up by primary key.
func (d *DB) DatabaseByID(ctx context.Context, id int64) (*Database, error) {
	return scanDatabase(d.QueryRowContext(ctx,
		`SELECT `+dbCols+` FROM db_databases d JOIN users u ON u.id = d.owner_id WHERE d.id = ?`, id))
}

// DatabaseByName looks a database up by its full name.
func (d *DB) DatabaseByName(ctx context.Context, name string) (*Database, error) {
	return scanDatabase(d.QueryRowContext(ctx,
		`SELECT `+dbCols+` FROM db_databases d JOIN users u ON u.id = d.owner_id WHERE d.name = ?`, name))
}

// ListDatabases returns databases ordered by name. ownerID 0 means all.
func (d *DB) ListDatabases(ctx context.Context, scope Scope) ([]*Database, error) {
	where, args := scope.Where("d.owner_id", "u.parent_id")
	q := `SELECT ` + dbCols + ` FROM db_databases d JOIN users u ON u.id = d.owner_id` +
		` WHERE ` + where + ` ORDER BY d.name`

	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]*Database, 0, 8)
	byID := make(map[int64]*Database)
	for rows.Next() {
		x, err := scanDatabase(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
		byID[x.ID] = x
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, d.attachGrantedUsers(ctx, byID)
}

// attachGrantedUsers fills each database's Users list in one query rather
// than one per database.
func (d *DB) attachGrantedUsers(ctx context.Context, byID map[int64]*Database) error {
	if len(byID) == 0 {
		return nil
	}
	rows, err := d.QueryContext(ctx,
		`SELECT g.database_id, u.username FROM db_grants g
		 JOIN db_users u ON u.id = g.db_user_id ORDER BY u.username`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var username string
		if err := rows.Scan(&id, &username); err != nil {
			return err
		}
		if x, ok := byID[id]; ok {
			x.Users = append(x.Users, username)
		}
	}
	return rows.Err()
}

// DeleteDatabaseRecord removes the panel's record of a database.
func (d *DB) DeleteDatabaseRecord(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM db_databases WHERE id = ?`, id)
	return err
}

// CreateDBUserRecord stores an account the panel created on the server.
func (d *DB) CreateDBUserRecord(ctx context.Context, username string, ownerID int64) (*DBUser, error) {
	now := time.Now().UTC()
	res, err := d.ExecContext(ctx,
		`INSERT INTO db_users (username, owner_id, created_at) VALUES (?,?,?)`,
		username, ownerID, fmtTime(now))
	if err != nil {
		return nil, fmt.Errorf("db: record database user %q: %w", username, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return d.DBUserByID(ctx, id)
}

const dbUserCols = `x.id, x.username, x.owner_id, x.created_at, u.username, COALESCE(u.parent_id, 0)`

func scanDBUser(row interface{ Scan(...any) error }) (*DBUser, error) {
	var x DBUser
	var created string
	err := row.Scan(&x.ID, &x.Username, &x.OwnerID, &created, &x.OwnerUsername, &x.OwnerParentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if x.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	return &x, nil
}

// DBUserByID looks an account up by primary key.
func (d *DB) DBUserByID(ctx context.Context, id int64) (*DBUser, error) {
	return scanDBUser(d.QueryRowContext(ctx,
		`SELECT `+dbUserCols+` FROM db_users x JOIN users u ON u.id = x.owner_id WHERE x.id = ?`, id))
}

// DBUserByName looks an account up by its full name.
func (d *DB) DBUserByName(ctx context.Context, username string) (*DBUser, error) {
	return scanDBUser(d.QueryRowContext(ctx,
		`SELECT `+dbUserCols+` FROM db_users x JOIN users u ON u.id = x.owner_id WHERE x.username = ?`, username))
}

// ListDBUsers returns accounts ordered by name, restricted to the scope.
func (d *DB) ListDBUsers(ctx context.Context, scope Scope) ([]*DBUser, error) {
	where, args := scope.Where("x.owner_id", "u.parent_id")
	q := `SELECT ` + dbUserCols + ` FROM db_users x JOIN users u ON u.id = x.owner_id` +
		` WHERE ` + where + ` ORDER BY x.username`

	rows, err := d.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make([]*DBUser, 0, 8)
	byID := make(map[int64]*DBUser)
	for rows.Next() {
		x, err := scanDBUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
		byID[x.ID] = x
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	grants, err := d.QueryContext(ctx,
		`SELECT g.db_user_id, dd.name FROM db_grants g
		 JOIN db_databases dd ON dd.id = g.database_id ORDER BY dd.name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = grants.Close() }()
	for grants.Next() {
		var id int64
		var name string
		if err := grants.Scan(&id, &name); err != nil {
			return nil, err
		}
		if x, ok := byID[id]; ok {
			x.Databases = append(x.Databases, name)
		}
	}
	return out, grants.Err()
}

// DeleteDBUserRecord removes the panel's record of an account.
func (d *DB) DeleteDBUserRecord(ctx context.Context, id int64) error {
	_, err := d.ExecContext(ctx, `DELETE FROM db_users WHERE id = ?`, id)
	return err
}

// AddGrant records that an account may reach a database.
func (d *DB) AddGrant(ctx context.Context, databaseID, dbUserID int64) error {
	_, err := d.ExecContext(ctx,
		`INSERT OR IGNORE INTO db_grants (database_id, db_user_id, created_at) VALUES (?,?,?)`,
		databaseID, dbUserID, fmtTime(time.Now()))
	return err
}

// RemoveGrant forgets that an account may reach a database.
func (d *DB) RemoveGrant(ctx context.Context, databaseID, dbUserID int64) error {
	_, err := d.ExecContext(ctx,
		`DELETE FROM db_grants WHERE database_id = ? AND db_user_id = ?`, databaseID, dbUserID)
	return err
}

// GrantedDatabaseNames lists the databases an account may reach.
func (d *DB) GrantedDatabaseNames(ctx context.Context, dbUserID int64) ([]string, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT dd.name FROM db_grants g JOIN db_databases dd ON dd.id = g.database_id
		 WHERE g.db_user_id = ? ORDER BY dd.name`, dbUserID)
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

// CountDatabasesByOwner reports how many databases a user owns, for plan limits.
func (d *DB) CountDatabasesByOwner(ctx context.Context, ownerID int64) (int, error) {
	var n int
	err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM db_databases WHERE owner_id = ?`, ownerID).Scan(&n)
	return n, err
}
