// Command opanelctl is the operator CLI.
//
// It talks to the agent and the database directly, so it keeps working when
// the API is down -- which is exactly when an operator needs it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/config"
	"github.com/bnixvn/opanel-ent/internal/db"
	"github.com/bnixvn/opanel-ent/internal/platform/linuxuser"
	"github.com/bnixvn/opanel-ent/internal/version"
)

const usage = `opanelctl -- OPanel operator CLI

Usage:
  opanelctl version                     print build version
  opanelctl doctor                      check database and agent health
  opanelctl db version                  print the applied schema version
  opanelctl db migrate                  apply pending migrations
  opanelctl agent ping                  call the agent's ping action
  opanelctl agent actions               list actions this agent serves
  opanelctl agent sysinfo               print detected host information
  opanelctl service list                list managed systemd units
  opanelctl user create-admin <name>    create an administrator
  opanelctl user create <name> <role>   create a user (admin|reseller|end_user)
  opanelctl user list                   list panel users

Environment: OPANEL_DB_PATH, OPANEL_AGENT_SOCKET, OPANEL_DATA_DIR
`

func main() {
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	if flag.NArg() == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if err := dispatch(ctx, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "opanelctl:", err)
		os.Exit(1)
	}
}

func dispatch(ctx context.Context, args []string) error {
	switch args[0] {
	case "version":
		fmt.Println("opanelctl", version.String())
		return nil
	case "doctor":
		return cmdDoctor(ctx)
	case "db":
		return cmdDB(ctx, args[1:])
	case "agent":
		return cmdAgent(ctx, args[1:])
	case "service":
		return cmdService(ctx, args[1:])
	case "user":
		return cmdUser(ctx, args[1:])
	default:
		return fmt.Errorf("unknown command %q (try: opanelctl -h)", args[0])
	}
}

func loadConfig() (*config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	return cfg, nil
}

func openDB(ctx context.Context) (*config.Config, *db.DB, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, err
	}
	database, err := db.Open(ctx, cfg.DBPath)
	if err != nil {
		return nil, nil, err
	}
	return cfg, database, nil
}

func cmdDoctor(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer func() { _ = tw.Flush() }()

	fmt.Fprintf(tw, "version\t%s\n", version.String())
	fmt.Fprintf(tw, "env\t%s\n", cfg.Env)

	// Report every check rather than stopping at the first failure: an
	// operator running doctor wants the whole picture in one pass.
	var problems int

	database, err := db.Open(ctx, cfg.DBPath)
	if err != nil {
		fmt.Fprintf(tw, "database\tFAIL\t%s: %v\n", cfg.DBPath, err)
		problems++
	} else {
		v, verr := database.SchemaVersion(ctx)
		if verr != nil {
			fmt.Fprintf(tw, "database\tFAIL\t%v\n", verr)
			problems++
		} else {
			fmt.Fprintf(tw, "database\tok\t%s (schema %d)\n", cfg.DBPath, v)
		}
		n, uerr := database.CountUsers(ctx)
		if uerr == nil {
			fmt.Fprintf(tw, "users\t%d\n", n)
			if n == 0 {
				fmt.Fprintf(tw, "\tWARN\tno users yet: run 'opanelctl user create-admin <name>'\n")
			}
		}
		_ = database.Close()
	}

	ac := agentclient.New(cfg.AgentSocket, 15*time.Second)
	if res, err := agentclient.Call[actions.PingResult](ctx, ac, "ping", 1, struct{}{}); err != nil {
		fmt.Fprintf(tw, "agent\tFAIL\t%s: %v\n", cfg.AgentSocket, err)
		problems++
	} else {
		fmt.Fprintf(tw, "agent\tok\t%s (pid %d, %s)\n", cfg.AgentSocket, res.PID, res.Version)
	}

	if info, err := agentclient.Call[actions.SysInfoResult](ctx, ac, "sysinfo", 1, struct{}{}); err == nil {
		d := info.Distro
		fmt.Fprintf(tw, "host\t%s\n", d.Pretty)
		fmt.Fprintf(tw, "cloudlinux\t%v %s\n", d.CloudLinux, d.CLEdition)
		if !d.Supported() {
			fmt.Fprintf(tw, "\tWARN\tunsupported platform: OPanel targets EL10 and CloudLinux 10\n")
		}
	}

	if problems > 0 {
		return fmt.Errorf("%d check(s) failed", problems)
	}
	return nil
}

func cmdDB(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("db: want 'version' or 'migrate'")
	}
	_, database, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()

	switch args[0] {
	case "version":
		v, err := database.SchemaVersion(ctx)
		if err != nil {
			return err
		}
		fmt.Println(v)
		return nil
	case "migrate":
		// Open already migrates; this makes it explicit and reports the result.
		if err := database.Migrate(ctx); err != nil {
			return err
		}
		v, err := database.SchemaVersion(ctx)
		if err != nil {
			return err
		}
		fmt.Println("schema at version", v)
		return nil
	default:
		return fmt.Errorf("db: unknown subcommand %q", args[0])
	}
}

func cmdAgent(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("agent: want 'ping', 'actions' or 'sysinfo'")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ac := agentclient.New(cfg.AgentSocket, 30*time.Second)

	switch args[0] {
	case "ping":
		res, err := agentclient.Call[actions.PingResult](ctx, ac, "ping", 1, struct{}{})
		if err != nil {
			return err
		}
		fmt.Printf("%s %s (pid %d)\n", res.Agent, res.Version, res.PID)
		return nil

	case "actions":
		// Asking the agent avoids reporting what this binary happens to know
		// when the two are different builds.
		res, err := agentclient.Call[map[string]int](ctx, ac, "agent.actions", 1, struct{}{})
		if err != nil {
			return err
		}
		names := make([]string, 0, len(res))
		for n := range res {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			fmt.Printf("%-28s v%d\n", n, res[n])
		}
		return nil

	case "sysinfo":
		res, err := agentclient.Call[actions.SysInfoResult](ctx, ac, "sysinfo", 1, struct{}{})
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)

	default:
		return fmt.Errorf("agent: unknown subcommand %q", args[0])
	}
}

func cmdService(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "list" {
		return errors.New("service: want 'list'")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ac := agentclient.New(cfg.AgentSocket, 60*time.Second)
	res, err := agentclient.Call[actions.UnitListResult](ctx, ac, "systemd.list", 1, struct{}{})
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "UNIT\tACTIVE\tENABLED")
	for _, u := range res.Units {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", u.Unit, u.Active, u.Enabled)
	}
	return tw.Flush()
}

func cmdUser(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("user: want 'create-admin', 'create' or 'list'")
	}
	switch args[0] {
	case "create-admin":
		if len(args) < 2 {
			return errors.New("user: want 'create-admin <username>'")
		}
		return createUser(ctx, args[1], auth.RoleAdmin)
	case "create":
		if len(args) < 3 {
			return errors.New("user: want 'create <username> <admin|reseller|end_user>'")
		}
		role := auth.Role(args[2])
		if !role.Valid() {
			return fmt.Errorf("user: %q is not a valid role", args[2])
		}
		return createUser(ctx, args[1], role)
	case "list":
		return listUsers(ctx)
	default:
		return fmt.Errorf("user: unknown subcommand %q", args[0])
	}
}

func listUsers(ctx context.Context) error {
	_, database, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()

	rows, err := database.QueryContext(ctx,
		`SELECT id, username, role, linux_uid, suspended FROM users ORDER BY id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tUSERNAME\tROLE\tLINUX UID\tSUSPENDED")
	for rows.Next() {
		var id int64
		var username, role string
		var uid *int64
		var suspended bool
		if err := rows.Scan(&id, &username, &role, &uid, &suspended); err != nil {
			return err
		}
		linux := "-"
		if uid != nil {
			linux = fmt.Sprint(*uid)
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%v\n", id, username, role, linux, suspended)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return tw.Flush()
}

func createUser(ctx context.Context, username string, role auth.Role) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return errors.New("user: username must not be empty")
	}
	// An end user gets a Linux account named after them the first time they
	// own a site, so the name has to be acceptable to useradd from the start.
	if role == auth.RoleEndUser && !linuxuser.ValidName(username) {
		return fmt.Errorf("user: %q cannot be a Linux account name: 3-32 characters, "+
			"starting with a letter, using lowercase letters, digits, _ and - only, "+
			"and not a reserved system name", username)
	}

	_, database, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()

	if _, err := database.UserByUsername(ctx, username); err == nil {
		return fmt.Errorf("user %q already exists", username)
	} else if !errors.Is(err, db.ErrNotFound) {
		return err
	}

	// Generated rather than prompted: this runs from the installer, where
	// there is no terminal to read a password from, and a generated secret
	// is stronger than one chosen under time pressure.
	password, err := auth.NewRecoveryCodes(1)
	if err != nil {
		return err
	}
	hash, err := auth.HashPassword(password[0])
	if err != nil {
		return err
	}
	u, err := database.CreateUser(ctx, &db.User{
		Username:     username,
		PasswordHash: hash,
		Role:         string(role),
	})
	if err != nil {
		return err
	}

	fmt.Printf("created %s %q (id %d)\n", role, u.Username, u.ID)
	fmt.Printf("password: %s\n", password[0])
	fmt.Println("Store it now: it is not recoverable and will not be shown again.")
	return nil
}
