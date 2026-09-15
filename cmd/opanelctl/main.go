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
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/bnixvn/opanel-ent/internal/acme"
	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/cloudlinux"
	"github.com/bnixvn/opanel-ent/internal/config"
	"github.com/bnixvn/opanel-ent/internal/db"
	"github.com/bnixvn/opanel-ent/internal/installer"
	"github.com/bnixvn/opanel-ent/internal/phpmgr"
	"github.com/bnixvn/opanel-ent/internal/platform/linuxuser"
	"github.com/bnixvn/opanel-ent/internal/sites"
	"github.com/bnixvn/opanel-ent/internal/version"
	"github.com/bnixvn/opanel-ent/internal/webserver"

	"github.com/bnixvn/opanel-ent/internal/webserver/backends"
)

const usage = `opanelctl -- OPanel operator CLI

Usage:
  opanelctl install [flags]             install the panel on this host
                       [--webserver apache|lsws]
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
  opanelctl cloudlinux setup            bring a converted host the rest of
                                        the way: integration, alt-php, PHP
                                        Selector, Manager, and move sites
                       [--skip-selector] [--skip-manager] [--stay-on-remi]
  opanelctl cloudlinux install          let CloudLinux read this panel
  opanelctl cloudlinux cpapi <script>   answer one CloudLinux query
  opanelctl passkeys off                stop asking for passkeys, server-wide
  opanelctl passkeys forget <name>      remove one account's passkeys
  opanelctl cert issue <domain> [email] obtain a Let's Encrypt certificate
                       [--staging] [--panel]

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
	case "install":
		return cmdInstall(ctx, args[1:])
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
	case "cert":
		return cmdCert(ctx, args[1:])
	case "passkeys":
		return cmdPasskeys(ctx, args[1:])
	case "cloudlinux":
		return cmdCloudLinux(ctx, args[1:])
	default:
		return fmt.Errorf("unknown command %q (try: opanelctl -h)", args[0])
	}
}

func cmdInstall(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	port := fs.Int("port", 2222, "port for the panel API")
	admin := fs.String("admin", "admin", "name of the first administrator")
	php := fs.String("php", "8.4,8.3", "comma-separated PHP versions to install")
	binDir := fs.String("bin-dir", "", "directory holding the opanel binaries (default: alongside opanelctl)")
	skipFW := fs.Bool("skip-firewall", false, "leave nftables alone")
	ws := fs.String("webserver", backends.Default, "webserver to install: apache, lsws")
	if err := fs.Parse(args); err != nil {
		return err
	}

	opts := &installer.Options{
		Backend:      *ws,
		PanelPort:    *port,
		AdminUser:    *admin,
		BinDir:       *binDir,
		SkipFirewall: *skipFW,
		PHPVersions:  splitList(*php),
	}
	if _, err := installer.Run(ctx, opts); err != nil {
		return err
	}

	// The first administrator is created after the services are up, so the
	// credentials are the last thing printed and cannot scroll away behind
	// package output.
	cfg, database, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()

	n, err := database.CountUsers(ctx)
	if err != nil {
		return err
	}
	fmt.Println()
	if n == 0 {
		if err := createUser(ctx, opts.AdminUser, auth.RoleAdmin); err != nil {
			return err
		}
	} else {
		fmt.Printf("%d panel user(s) already exist; no administrator was created.\n", n)
	}

	// An upgrade refreshes the rendered configuration, and this is the only
	// place that can: the installer step deliberately leaves an existing
	// estate alone, because it renders from no sites and Apply replaces
	// everything it is given. Here the database is open, so the render has
	// every site in it -- which is also how a template change in a new
	// release reaches vhosts that already exist.
	if n, err := database.CountSites(ctx); err == nil && n > 0 {
		ac := agentclient.New(cfg.AgentSocket, actions.SwitchBudget)
		svc := sites.New(database, ac, webserver.DefaultServerConfig(), nil, nil)
		if err := svc.SyncWebserver(ctx); err != nil {
			return fmt.Errorf("re-render the %d existing site(s): %w", n, err)
		}
		fmt.Printf("Re-rendered %d existing site(s).\n", n)
	}

	// A host that already has a certificate is usually one being upgraded,
	// and telling that operator to go and fix a self-signed certificate they
	// replaced months ago sends them looking for a problem that is not there.
	host, tls := panelAddress(ctx, cfg), "self-signed; see the README to replace it"
	if names, err := database.ListPanelHostnames(ctx); err == nil && len(names) > 0 {
		if names[0].CertFile != "" {
			host, tls = names[0].Hostname, names[0].CertFile
		}
	}

	fmt.Println()
	fmt.Printf("Panel:  https://%s:%d\n", host, opts.PanelPort)
	fmt.Printf("Config: %s/opanel.env\n", installer.ConfigDir)
	fmt.Printf("Data:   %s\n", cfg.DataDir)
	fmt.Printf("TLS:    %s\n", tls)
	return nil
}

// panelAddress is the address to print as the one to open.
//
// The agent is asked rather than the database, because a fresh install has no
// hostname recorded and printing the literal "<server-ip>" leaves the person
// who most needs the answer -- somebody who has just installed this on a
// server they may know only by their provider's console -- to go and find it.
// A host with no global address falls back to the placeholder, which is at
// least honest.
func panelAddress(ctx context.Context, cfg *config.Config) string {
	ac := agentclient.New(cfg.AgentSocket, 10*time.Second)
	info, err := agentclient.Call[actions.NetworkInfo](ctx, ac, "net.info", 1, struct{}{})
	if err != nil {
		return "<server-ip>"
	}
	switch {
	case info.PrimaryIPv4 != "":
		return info.PrimaryIPv4
	case info.PrimaryIPv6 != "":
		return "[" + info.PrimaryIPv6 + "]"
	default:
		return "<server-ip>"
	}
}

// splitFlags separates "-x"/"--x" arguments from positional ones, so a flag
// may appear anywhere in the command line rather than only before the first
// positional. A bare "--" ends flag parsing, as usual.
//
// Boolean flags only: a flag that took its value as the next argument would
// have that value read as a positional here. Every flag this CLI has is a
// bool, and one that is not should be spelled --name=value.
func splitFlags(args []string) (flags, positional []string) {
	for i, a := range args {
		switch {
		case a == "--":
			return flags, append(positional, args[i+1:]...)
		case len(a) > 1 && a[0] == '-':
			flags = append(flags, a)
		default:
			positional = append(positional, a)
		}
	}
	return flags, positional
}

func cmdCert(ctx context.Context, args []string) error {
	if len(args) < 1 || args[0] != "issue" {
		return errors.New("cert: want 'issue <domain> <email> [--staging] [--panel]'")
	}
	fs := flag.NewFlagSet("cert issue", flag.ContinueOnError)
	staging := fs.Bool("staging", false, "use the Let's Encrypt staging CA")
	panel := fs.Bool("panel", false, "point the panel's own TLS at this certificate")
	// Split the flags out by hand. flag.Parse stops at the first positional,
	// so the spelling this command documents -- domain first, flags after --
	// left "--panel" sitting in the argument list where the email goes. It
	// was then sent to Let's Encrypt as a contact address, and the only
	// report of the mistake was a 400 from the CA.
	flags, rest := splitFlags(args[1:])
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if len(rest) < 1 {
		return errors.New("cert issue: want <domain> [email]")
	}
	domain := rest[0]
	var email string
	if len(rest) > 1 {
		email = rest[1]
		if !strings.Contains(email, "@") {
			return fmt.Errorf("cert issue: %q is not an email address", email)
		}
	} else {
		fmt.Println("No contact email given. Let's Encrypt will not be able to warn")
		fmt.Println("you before this certificate expires; pass one to enable that.")
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ac := agentclient.New(cfg.AgentSocket, 5*time.Minute)

	fmt.Printf("Requesting a certificate for %s ...\n", domain)
	cert, err := agentclient.Call[acme.Certificate](ctx, ac, "cert.issue", 1,
		actions.CertIssueRequest{Domains: rest[:1], Email: email, Staging: *staging})
	if err != nil {
		return err
	}
	fmt.Printf("Issued, valid until %s\n", cert.NotAfter.Format(time.RFC1123))
	fmt.Printf("  certificate %s\n", cert.CertFile)
	fmt.Printf("  private key %s\n", cert.KeyFile)

	if !*panel {
		return nil
	}
	// Written as a drop-in rather than edited into the existing file, so
	// re-running this never duplicates or mangles an operator's own settings.
	envPath := filepath.Join(installer.ConfigDir, "opanel.env")
	line := fmt.Sprintf(
		"\n# Set by 'opanelctl cert issue --panel' on %s\n"+
			"OPANEL_TLS_CERT=%s\nOPANEL_TLS_KEY=%s\nOPANEL_PANEL_HOST=%s\n",
		time.Now().Format(time.RFC3339), cert.CertFile, cert.KeyFile, domain)
	f, err := os.OpenFile(envPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("update %s: %w", envPath, err)
	}
	if _, err := f.WriteString(line); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Also record it as a hostname the panel answers on. Without this the
	// panel would serve the certificate but refuse the name, and anything
	// that reads the hostname list -- passkeys, most visibly -- would not
	// know the panel has a name at all.
	if store, err := db.Open(ctx, cfg.DBPath); err == nil {
		defer func() { _ = store.Close() }()
		if err := store.AddPanelHostname(ctx, &db.PanelHostname{
			Hostname: domain, CertFile: cert.CertFile, KeyFile: cert.KeyFile,
			IsPrimary: true,
		}); err != nil {
			fmt.Printf("Warning: could not record %s as a panel hostname: %v\n", domain, err)
		} else if err := store.SetPrimaryHostname(ctx, domain); err != nil {
			fmt.Printf("Warning: could not make %s the primary hostname: %v\n", domain, err)
		} else {
			fmt.Printf("Recorded %s as the panel's primary hostname.\n", domain)
		}
	}

	fmt.Printf("Recorded in %s. Restart the panel to use it:\n", envPath)
	fmt.Println("  systemctl restart opanel-api")
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
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

// cmdPasskeys is the way back in when a passkey cannot be produced.
//
// A passkey is a second factor, so an account that registered one and then
// lost the device has no way to sign in -- and if that account is the only
// administrator, nobody does. Every other lever in this panel is behind a
// login, which is exactly the thing that is not working, so this one is on
// the command line where a person with the server has it and a person with
// the password does not.
func cmdPasskeys(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("passkeys: want 'off' or 'forget <username>'")
	}
	_, database, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()

	switch args[0] {
	case "off":
		// The setting rather than the credentials: nothing is destroyed, and
		// an administrator who finds their key again turns it back on from
		// Settings.
		if err := database.SetSetting(ctx, "passkey.enabled", ""); err != nil {
			return err
		}
		fmt.Println("Passkeys will not be asked for. Registered keys are kept;")
		fmt.Println("turn them back on under Settings once you can sign in.")
		return nil

	case "forget":
		if len(args) < 2 {
			return errors.New("passkeys: want 'forget <username>'")
		}
		user, err := database.UserByUsername(ctx, args[1])
		if err != nil {
			return fmt.Errorf("passkeys: no such user %q", args[1])
		}
		keys, err := database.PasskeysFor(ctx, user.ID)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if err := database.DeletePasskey(ctx, user.ID, k.ID); err != nil {
				return err
			}
		}
		fmt.Printf("Removed %d passkey(s) from %s.\n", len(keys), user.Username)
		return nil

	default:
		return fmt.Errorf("passkeys: unknown subcommand %q", args[0])
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

// cmdCloudLinux drives the integration CloudLinux expects from a panel it
// does not otherwise know.
//
// "install" writes /opt/cpvendor/etc/integration.ini and the small programs
// it names; "cpapi" is what those programs run. Both live here rather than in
// the API because CloudLinux calls them as programs, and because they have to
// keep working when the panel is the thing that is broken.
func cmdCloudLinux(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("cloudlinux: want 'setup', 'install' or 'cpapi <script>'")
	}
	switch args[0] {
	case "install":
		if err := cloudlinux.Install(); err != nil {
			return err
		}
		fmt.Printf("Wrote %s and %d scripts in %s.\n",
			cloudlinux.ConfigPath, len(cloudlinux.Scripts), cloudlinux.ScriptDir)
		fmt.Println("CloudLinux's components can now read this panel's accounts,")
		fmt.Println("domains and packages.")
		return nil

	case "cpapi":
		if len(args) < 2 {
			return fmt.Errorf("cloudlinux cpapi: want one of %v", cloudlinux.Scripts)
		}
		cfg, database, err := openDB(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = database.Close() }()

		api := &cloudlinux.API{
			Store:    database,
			PHP:      phpmgr.ForBackend(backends.Active()),
			PanelURL: panelURL(cfg),
		}
		return api.Run(ctx, os.Stdout, args[1], args[2:])

	case "setup":
		return cloudLinuxSetup(ctx, args[1:])

	default:
		return fmt.Errorf("cloudlinux: unknown command %q", args[0])
	}
}

// cloudLinuxSetup brings a converted host the rest of the way, in one
// command.
//
// This is the step between "CloudLinux is installed" and "the panel is
// running sites on it", and it is a command rather than a page because it is
// the only part of the sequence an operator runs once and never again. Each
// piece is idempotent, so a run that fails partway is resumed by running it
// again rather than unpicked.
//
// The order is not arbitrary. The integration has to exist before the Manager
// is installed, because the vendor's installer reads base_path out of it. PHP
// Selector needs CageFS, which the wizard or cagefsctl has already set up.
// And the migration to alt-php goes last, because it is the only step that
// touches running sites.
func cloudLinuxSetup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("cloudlinux setup", flag.ContinueOnError)
	skipSelector := fs.Bool("skip-selector", false, "do not install alt-php or set up PHP Selector")
	skipManager := fs.Bool("skip-manager", false, "do not install CloudLinux Manager")
	stay := fs.Bool("stay-on-remi", false, "leave sites on Remi's PHP instead of moving them to alt-php")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !cloudlinux.Detected() {
		return errors.New("cloudlinux setup: this host is not running CloudLinux.\n" +
			"Convert it first with cldeploy, reboot, then run this again.")
	}

	cfg, database, err := openDB(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()
	ac := agentclient.New(cfg.AgentSocket, actions.PHPProviderBudget+time.Minute)

	fmt.Println("==> Integration")
	if _, err := agentclient.Call[actions.CLStatus](ctx, ac, "cl.install_integration", 1, struct{}{}); err != nil {
		return fmt.Errorf("install the integration: %w", err)
	}
	fmt.Println("    CloudLinux's tools can read this panel's accounts, domains and packages.")

	if !*skipSelector {
		fmt.Println("==> alt-php and PHP Selector (this installs about 2 GB and takes a few minutes)")
		st, err := agentclient.Call[cloudlinux.SelectorStatus](ctx, ac, "cl.selector_setup", 1, struct{}{})
		if err != nil {
			return fmt.Errorf("set up PHP Selector: %w", err)
		}
		fmt.Printf("    %d interpreter(s) available: %s\n", len(st.Versions), strings.Join(st.Versions, " "))
	}

	if !*skipManager {
		fmt.Println("==> CloudLinux Manager")
		if _, err := agentclient.Call[actions.CLStatus](ctx, ac, "cl.manager_install", 1, struct{}{}); err != nil {
			return fmt.Errorf("install CloudLinux Manager: %w", err)
		}
		fmt.Println("    Served inside the panel at /lvemanager, behind the panel's own session.")
	}

	// nil limits and nil logger: this is the operator at a shell rather than
	// a reseller spending an allowance, and the service treats both as
	// optional.
	svc := sites.New(database, ac, webserver.DefaultServerConfig(), nil, nil)

	// Every site moves onto CloudLinux's PHP. Last, and separately reported,
	// because it is the step that touches what visitors are being served.
	if !*stay {
		fmt.Println("==> Moving sites to alt-php")
		res, err := svc.SetPHPProvider(ctx, phpmgr.ProviderAltPHP)
		if err != nil {
			return fmt.Errorf("move sites to alt-php: %w", err)
		}
		fmt.Printf("    %d site(s) moved from %s to %s.\n", res.Migrated, res.Previous, res.Provider)
	}

	// The webserver is re-rendered whatever happened above: the Manager's
	// vhost and the LiteSpeed handlers only appear on a render that runs
	// after the things they describe exist.
	if err := svc.SyncWebserver(ctx); err != nil {
		return fmt.Errorf("apply the webserver configuration: %w", err)
	}

	fmt.Println()
	fmt.Println("Done. Sites run on CloudLinux's PHP, and LiteSpeed Enterprise can now be")
	fmt.Println("installed: every PHP vhost already carries a handler it can use.")
	return nil
}

// panelURL is where CloudLinux should send somebody who wants to sign in.
func panelURL(cfg *config.Config) string {
	host := cfg.PanelHost
	if host == "" {
		return ""
	}
	_, port, ok := strings.Cut(cfg.ListenAddr, ":")
	if !ok {
		port = "2222"
	}
	return "https://" + host + ":" + port + "/"
}
