// Package cloudlinux implements the integration CloudLinux expects from a
// control panel it does not otherwise know.
//
// CloudLinux's own components -- the LVE manager, the PHP selector, CageFS,
// MySQL Governor -- need to know who the accounts are, which domains belong
// to whom, and how the hosting packages are grouped. For cPanel and Plesk
// they know already. For anything else the vendor publishes an interface: a
// set of small programs that answer those questions as JSON, named in
// /opt/cpvendor/etc/integration.ini.
//
// Implementing that is deliberately preferred over calling lvectl and
// cagefsctl directly. The vendor's own guidance is that a panel should
// describe itself and let their components decide what to do, because the
// low-level tools change and the interface does not. It is also the only way
// to get their manager UI, which is the thing an operator actually wants:
// resource graphs and a selector that already work, rather than a second
// implementation of both.
package cloudlinux

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/db"
	"github.com/bnixvn/opanel-ent/internal/phpmgr"
	"github.com/bnixvn/opanel-ent/internal/version"
)

// Every answer has this shape: the payload under "data", the outcome under
// "metadata". A failure is still exit status zero with a result other than
// "ok" -- the vendor's components read the metadata, not the exit code.
type envelope struct {
	Data     any      `json:"data"`
	Metadata metadata `json:"metadata"`
}

type metadata struct {
	Result  string `json:"result"`
	Message string `json:"message,omitempty"`
}

// Result values CloudLinux understands.
const (
	resultOK        = "ok"
	resultInternal  = "InternalAPIError"
	resultNotFound  = "NotFound"
	resultBadReq    = "BadRequest"
	resultForbidden = "PermissionDenied"
)

// PanelName is how this panel introduces itself to CloudLinux.
const PanelName = "OPanel"

// Store is the part of the database this package reads. An interface rather
// than the concrete type so the scripts can be tested against a handful of
// rows instead of a server.
type Store interface {
	ListUsers(ctx context.Context, scope db.Scope) ([]*db.User, error)
	ListSites(ctx context.Context, scope db.Scope) ([]*db.Site, error)
	ListPlans(ctx context.Context, scope db.Scope) ([]*db.Plan, error)
	UserPlanID(ctx context.Context, userID int64) (*int64, error)
	DBUsersByOwner(ctx context.Context, ownerID int64) ([]*db.DBUser, error)
}

// API answers the integration scripts.
type API struct {
	Store Store
	// PHP lists the interpreters on the host. CloudLinux asks so that its
	// selector can offer them.
	PHP phpmgr.Provider
	// PanelURL is where a customer signs in, with {domain} where the
	// customer's own domain goes. CloudLinux uses it to link back.
	PanelURL string
}

// Run answers one script by name and writes JSON to w.
//
// Always returns nil: a failure is reported inside the document, because that
// is what the caller reads. Returning an error here would mean a non-zero
// exit and a component that says nothing about why.
func (a *API) Run(ctx context.Context, w io.Writer, script string, args []string) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	data, result, message := a.dispatch(ctx, script, parseFlags(args))
	return enc.Encode(envelope{Data: data, Metadata: metadata{Result: result, Message: message}})
}

func (a *API) dispatch(ctx context.Context, script string, flags map[string]string) (any, string, string) {
	switch script {
	case "panel_info":
		return a.panelInfo(), resultOK, ""
	case "users":
		return a.users(ctx, flags)
	case "domains":
		return a.domains(ctx, flags)
	case "packages":
		return a.packages(ctx, flags)
	case "resellers":
		return a.staff(ctx, auth.RoleReseller)
	case "admins":
		return a.admins(ctx)
	case "db_info":
		return a.dbInfo(ctx)
	case "php":
		return a.phpVersions(ctx)
	default:
		return nil, resultBadReq, fmt.Sprintf("no integration script named %q", script)
	}
}

// Scripts lists every script this package answers, for the ini file and for
// anybody checking the implementation is complete.
var Scripts = []string{
	"panel_info", "users", "domains", "packages",
	"resellers", "admins", "db_info", "php",
}

// --- panel_info -----------------------------------------------------------

// The field names and their presence are checked by CloudLinux against its
// own JSON schemas, which ship in its venv and are the authoritative
// description -- stricter than the published documentation, which named this
// field user_login_url_template and did not mention that unknown fields are
// rejected outright.
type panelInfoData struct {
	Name              string          `json:"name"`
	Version           string          `json:"version"`
	UserLoginURL      *string         `json:"user_login_url"`
	SupportedFeatures map[string]bool `json:"supported_cl_features"`
}

// panelInfo declares what this panel can actually drive.
//
// This list is not documentation: CloudLinux reads it and hides every feature
// it does not find here. Declaring one the panel cannot drive produces a
// button that does nothing; declaring none -- which this did at first --
// produces a manager with its tabs switched off and no explanation, which is
// how "the selector is not supported in this environment" turns out to be the
// panel's own doing.
//
// So each is answered from the host rather than written down. A feature is
// supported when the thing that implements it is installed.
func (a *API) panelInfo() panelInfoData {
	// PHP Selector needs both: alt-php to choose between, and CageFS, because
	// the selector works by giving each account its own view of /usr/bin/php.
	phpSelector := fileExists(cageFSCtl) && hasAltPHP()

	return panelInfoData{
		Name:         PanelName,
		Version:      version.String(),
		UserLoginURL: optional(a.PanelURL),
		SupportedFeatures: map[string]bool{
			"php_selector":    phpSelector,
			"cagefs":          fileExists(cageFSCtl),
			"mod_lsapi":       fileExists(modLSAPI),
			"reseller_limits": true,
			// Not wired up yet, and saying so is the point of the list.
			"ruby_selector":   false,
			"python_selector": false,
			"nodejs_selector": false,
			"mysql_governor":  false,
			"xray":            false,
			"accelerate_wp":   false,
			"wizard":          false,
			"autotracing":     false,
		},
	}
}

// Where the things those features need live.
const (
	cageFSCtl = "/usr/sbin/cagefsctl"
	modLSAPI  = "/etc/httpd/conf.d/lsapi.conf"
	altPHPDir = "/opt/alt"
)

// hasAltPHP reports whether any alt-php version is installed.
func hasAltPHP() bool {
	entries, err := os.ReadDir(altPHPDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "php") {
			return true
		}
	}
	return false
}

// --- users ----------------------------------------------------------------

type userPackage struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
}

// Every field is sent every time, including the ones the schema does not
// require. CloudLinux's own code reads user.package without checking, and its
// model raises "package is not set, but used in code" for a field that was
// merely absent -- which surfaced as a traceback from lvectl when setting
// limits for an account that had no package. Absent and null are different
// things to it, so nothing here is omitted.
type userData struct {
	ID       int64        `json:"id"`
	Username string       `json:"username"`
	Owner    string       `json:"owner"`
	Domain   string       `json:"domain"`
	Package  *userPackage `json:"package"`
	Email    *string      `json:"email"`
	Locale   *string      `json:"locale_code"`
}

func (a *API) users(ctx context.Context, flags map[string]string) (any, string, string) {
	users, err := a.Store.ListUsers(ctx, db.ScopeAll())
	if err != nil {
		return nil, resultInternal, err.Error()
	}
	sites, err := a.Store.ListSites(ctx, db.ScopeAll())
	if err != nil {
		return nil, resultInternal, err.Error()
	}
	plans, err := a.Store.ListPlans(ctx, db.ScopeAll())
	if err != nil {
		return nil, resultInternal, err.Error()
	}

	byID := make(map[int64]*db.User, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}
	planByID := make(map[int64]*db.Plan, len(plans))
	for _, p := range plans {
		planByID[p.ID] = p
	}
	// The main domain is the oldest site an account has: the one they set up
	// first is the one they think of as theirs.
	main := make(map[int64]string)
	for _, s := range sites {
		if _, seen := main[s.OwnerID]; !seen {
			main[s.OwnerID] = s.Domain
		}
	}

	out := make([]userData, 0, len(users))
	for _, u := range users {
		// Only accounts with a Linux user. CloudLinux works in uids, and an
		// administrator who has never owned a site does not have one.
		if u.LinuxUID == nil || *u.LinuxUID <= 0 {
			continue
		}
		if name := flags["username"]; name != "" && name != u.Username {
			continue
		}
		row := userData{
			ID:       *u.LinuxUID,
			Username: u.Username,
			Owner:    ownerName(u, byID),
			Domain:   main[u.ID],
			Email:    optional(u.Email),
			Locale:   nil,
		}
		if id, err := a.Store.UserPlanID(ctx, u.ID); err == nil && id != nil {
			if p := planByID[*id]; p != nil {
				row.Package = &userPackage{Name: p.Name, Owner: planOwner(p, byID)}
			}
		}
		if owner := flags["owner"]; owner != "" && owner != row.Owner {
			continue
		}
		if pkg := flags["package-name"]; pkg != "" && (row.Package == nil || row.Package.Name != pkg) {
			continue
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out, resultOK, ""
}

// ownerName is the account above this one: the reseller who owns it, or the
// server's main administrator.
func ownerName(u *db.User, byID map[int64]*db.User) string {
	if u.ParentID != nil {
		if parent := byID[*u.ParentID]; parent != nil {
			return parent.Username
		}
	}
	return mainAdmin(byID)
}

func planOwner(p *db.Plan, byID map[int64]*db.User) string {
	if p.OwnerID != nil {
		if owner := byID[*p.OwnerID]; owner != nil {
			return owner.Username
		}
	}
	return mainAdmin(byID)
}

// mainAdmin is the lowest-numbered administrator, which on every install is
// the one the installer created.
func mainAdmin(byID map[int64]*db.User) string {
	best := ""
	var bestID int64
	for id, u := range byID {
		if u.Role != string(auth.RoleAdmin) {
			continue
		}
		if best == "" || id < bestID {
			best, bestID = u.Username, id
		}
	}
	return best
}

// --- domains --------------------------------------------------------------

type domainPHP struct {
	VersionID string  `json:"php_version_id"`
	Version   string  `json:"version"`
	IniPath   string  `json:"ini_path"`
	Handler   *string `json:"handler"`
	FPM       string  `json:"fpm,omitempty"`
}

type domainData struct {
	Owner        string     `json:"owner"`
	DocumentRoot string     `json:"document_root"`
	IsMain       bool       `json:"is_main"`
	PHP          *domainPHP `json:"php,omitempty"`
}

func (a *API) domains(ctx context.Context, flags map[string]string) (any, string, string) {
	sites, err := a.Store.ListSites(ctx, db.ScopeAll())
	if err != nil {
		return nil, resultInternal, err.Error()
	}
	_, withPHP := flags["with-php"]

	seenOwner := make(map[int64]bool)
	out := make(map[string]domainData, len(sites))
	for _, s := range sites {
		if owner := flags["owner"]; owner != "" && owner != s.OwnerUsername {
			continue
		}
		if name := flags["name"]; name != "" && name != s.Domain {
			continue
		}
		// Asked with --with-php, the schema requires a php block on every
		// domain it returns. A static site has none, so it is left out of
		// that answer entirely rather than given an interpreter it does not
		// use -- the question is being asked by the features that trace PHP,
		// and a site without any is not something they can act on.
		if withPHP && s.PHPVersion == "" {
			continue
		}
		row := domainData{
			Owner:        s.OwnerUsername,
			DocumentRoot: s.DocumentRoot,
			IsMain:       !seenOwner[s.OwnerID],
		}
		seenOwner[s.OwnerID] = true
		if withPHP {
			row.PHP = a.phpFor(s.PHPVersion)
		}
		out[s.Domain] = row

		// An alias is the same site under another name, and CloudLinux keys
		// this map by hostname -- so each one is listed, never as the main.
		for _, alias := range s.Aliases {
			out[alias] = domainData{Owner: s.OwnerUsername, DocumentRoot: s.DocumentRoot, IsMain: false, PHP: row.PHP}
		}
	}
	return out, resultOK, ""
}

func (a *API) phpFor(v string) *domainPHP {
	if a.PHP == nil {
		return nil
	}
	handler := "fpm"
	ini := a.PHP.IniDropIn(v)
	p := &domainPHP{
		VersionID: a.PHP.Name() + v,
		Version:   v,
		IniPath:   path.Dir(ini),
		Handler:   &handler,
	}
	if fp, ok := a.PHP.(phpmgr.FPMProvider); ok {
		p.FPM = fp.ServiceUnit(v)
	}
	return p
}

// --- packages -------------------------------------------------------------

type packageData struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
}

func (a *API) packages(ctx context.Context, flags map[string]string) (any, string, string) {
	plans, err := a.Store.ListPlans(ctx, db.ScopeAll())
	if err != nil {
		return nil, resultInternal, err.Error()
	}
	users, err := a.Store.ListUsers(ctx, db.ScopeAll())
	if err != nil {
		return nil, resultInternal, err.Error()
	}
	byID := make(map[int64]*db.User, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}

	out := make([]packageData, 0, len(plans))
	for _, p := range plans {
		owner := planOwner(p, byID)
		if want := flags["owner"]; want != "" && want != owner {
			continue
		}
		out = append(out, packageData{Name: p.Name, Owner: owner})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, resultOK, ""
}

// --- resellers and admins -------------------------------------------------

type staffData struct {
	Name   string  `json:"name"`
	Locale *string `json:"locale_code"`
	Email  *string `json:"email"`
	ID     *int64  `json:"id"`
}

type adminData struct {
	Name     string  `json:"name"`
	UnixUser *string `json:"unix_user"`
	Locale   *string `json:"locale_code"`
	Email    *string `json:"email"`
	IsMain   bool    `json:"is_main"`
}

func (a *API) staff(ctx context.Context, role auth.Role) (any, string, string) {
	users, err := a.Store.ListUsers(ctx, db.ScopeAll())
	if err != nil {
		return nil, resultInternal, err.Error()
	}
	out := make([]staffData, 0)
	for _, u := range users {
		if u.Role != string(role) {
			continue
		}
		id := u.ID
		out = append(out, staffData{Name: u.Username, Email: optional(u.Email), ID: &id})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, resultOK, ""
}

func (a *API) admins(ctx context.Context) (any, string, string) {
	users, err := a.Store.ListUsers(ctx, db.ScopeAll())
	if err != nil {
		return nil, resultInternal, err.Error()
	}
	byID := make(map[int64]*db.User, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}
	main := mainAdmin(byID)

	out := make([]adminData, 0)
	for _, u := range users {
		if u.Role != string(auth.RoleAdmin) {
			continue
		}
		row := adminData{Name: u.Username, Email: optional(u.Email), IsMain: u.Username == main}
		if u.LinuxUID != nil && *u.LinuxUID > 0 {
			name := u.Username
			row.UnixUser = &name
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, resultOK, ""
}

// --- db_info --------------------------------------------------------------

type dbAccess struct {
	Login    string `json:"login"`
	Password string `json:"password"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
}

type dbEngine struct {
	Access  dbAccess            `json:"access"`
	Mapping map[string][]string `json:"mapping"`
}

// dbInfo maps each account to the database users it owns.
//
// The credential is what MySQL Governor would connect with. This panel's
// MariaDB trusts the unix socket for root rather than storing a password, so
// there is nothing to hand over and the access block says so with an empty
// password rather than inventing one. Governor is declared unsupported in
// panel_info for the same reason: it is not wired up, and saying otherwise
// would put a button in front of an operator that cannot work.
func (a *API) dbInfo(ctx context.Context) (any, string, string) {
	users, err := a.Store.ListUsers(ctx, db.ScopeAll())
	if err != nil {
		return nil, resultInternal, err.Error()
	}
	mapping := make(map[string][]string)
	for _, u := range users {
		if u.LinuxUID == nil || *u.LinuxUID <= 0 {
			continue
		}
		dbUsers, err := a.Store.DBUsersByOwner(ctx, u.ID)
		if err != nil {
			return nil, resultInternal, err.Error()
		}
		names := make([]string, 0, len(dbUsers))
		for _, d := range dbUsers {
			names = append(names, d.Username)
		}
		sort.Strings(names)
		mapping[u.Username] = names
	}
	return map[string]dbEngine{
		"mysql": {
			Access:  dbAccess{Login: "root", Password: "", Host: "localhost", Port: 3306},
			Mapping: mapping,
		},
	}, resultOK, ""
}

// --- php ------------------------------------------------------------------

type phpData struct {
	Identifier string `json:"identifier"`
	Version    string `json:"version"`
	ModulesDir string `json:"modules_dir"`
	Dir        string `json:"dir"`
	Bin        string `json:"bin"`
	Ini        string `json:"ini"`
}

func (a *API) phpVersions(ctx context.Context) (any, string, string) {
	if a.PHP == nil {
		return []phpData{}, resultOK, ""
	}
	versions, err := a.PHP.Available(ctx)
	if err != nil {
		return nil, resultInternal, err.Error()
	}
	out := make([]phpData, 0, len(versions))
	for _, v := range versions {
		if !v.Installed {
			continue
		}
		bin := a.PHP.CLIBinary(v.Version)
		ini := a.PHP.IniDropIn(v.Version)
		out = append(out, phpData{
			Identifier: a.PHP.Name() + v.Version,
			Version:    v.Version,
			Dir:        path.Dir(path.Dir(bin)),
			Bin:        bin,
			Ini:        ini,
			ModulesDir: path.Dir(ini),
		})
	}
	return out, resultOK, ""
}

// --- helpers --------------------------------------------------------------

// parseFlags reads the --name=value and --flag arguments the vendor's
// components pass. Unknown ones are kept rather than refused: a newer
// component asking a question this build does not answer should get the
// unfiltered list, not an error.
func parseFlags(args []string) map[string]string {
	out := make(map[string]string, len(args))
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			continue
		}
		name, value, ok := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if !ok {
			value = ""
		}
		out[name] = value
	}
	return out
}

// optional renders an empty string as JSON null, which is what the schema
// asks for on every nullable field.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
