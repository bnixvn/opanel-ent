package httpapi

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/db"
	"github.com/bnixvn/opanel-ent/internal/hostimport"
	"github.com/bnixvn/opanel-ent/internal/sites"
	"github.com/bnixvn/opanel-ent/internal/webserver"
)

// MaxImportBytes caps an uploaded account archive. A hosting account with
// years of uploads is large, and the limit exists so one upload cannot fill
// the panel's own disk before anybody looks at it.
const MaxImportBytes = 20 << 30 // 20 GiB

// handleImportUpload takes a cPanel or DirectAdmin archive and says what is
// in it, without changing anything.
//
// Inspect and import are separate on purpose: an operator moving a customer
// needs to see the domains and databases, and what will be left behind,
// before anything is created.
func (s *Server) handleImportUpload(w http.ResponseWriter, r *http.Request) {
	if err := os.MkdirAll(actions.ImportStageDir, 0o750); err != nil {
		s.log.Error("httpapi: import staging", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	src, hdr, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "no file was uploaded")
		return
	}
	defer func() { _ = src.Close() }()

	tmp, err := os.CreateTemp(actions.ImportStageDir, "import-*.tar.gz")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	staged := tmp.Name()
	n, err := io.Copy(tmp, io.LimitReader(src, MaxImportBytes+1))
	_ = tmp.Close()
	if err != nil || n > MaxImportBytes {
		_ = os.Remove(staged)
		writeError(w, http.StatusBadRequest, "too_large",
			fmt.Sprintf("%s is larger than this panel will take in one upload", hdr.Filename))
		return
	}

	plan, err := agentclient.Call[*hostimport.Plan](r.Context(),
		s.agent.WithTimeout(actions.ImportBudget), "import.inspect", 1,
		actions.ImportInspectRequest{StagedPath: staged})
	if err != nil {
		_ = os.Remove(staged)
		writeError(w, http.StatusBadRequest, "unreadable", trimAgent(err.Error()))
		return
	}
	s.audit(r, "import.inspect", hdr.Filename, true, plan.Format)

	writeJSON(w, http.StatusOK, map[string]any{
		"staged": path.Base(staged),
		"plan":   plan,
		"size":   n,
	})
}

// handleImportRun creates the account's websites and databases and unpacks
// the archive into them.
//
// One request rather than a job, because the operator is watching: they have
// just been shown exactly what will happen and pressed the button. It can
// take a while, and the client is expected to wait.
func (s *Server) handleImportRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Staged string `json:"staged"`
		// Owner is the panel account the import lands in. It must exist
		// already: creating a hosting account is its own decision, with its
		// own package and its own quota, and folding it into an import would
		// hide that.
		Owner string `json:"owner"`
		// Domains and Databases are the names the operator chose to bring
		// over, from the plan they were shown.
		Domains   []string `json:"domains"`
		Databases []string `json:"databases"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	staged, ok := stagedImportPath(w, req.Staged)
	if !ok {
		return
	}

	owner, err := s.db.UserByUsername(r.Context(), req.Owner)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "no such account")
		return
	}
	if !auth.CanManage(userFrom(r.Context()), owner) {
		writeError(w, http.StatusForbidden, "forbidden",
			"that account is not one you can import into")
		return
	}
	if owner.LinuxUID == nil || *owner.LinuxUID == 0 {
		writeError(w, http.StatusBadRequest, "no_account",
			"that account has no home directory; pick a hosting account")
		return
	}

	plan, err := agentclient.Call[*hostimport.Plan](r.Context(),
		s.agent.WithTimeout(actions.ImportBudget), "import.inspect", 1,
		actions.ImportInspectRequest{StagedPath: staged})
	if err != nil {
		writeError(w, http.StatusBadRequest, "unreadable", trimAgent(err.Error()))
		return
	}

	report := importReport{Owner: owner.Username, Format: plan.Format}
	wanted := setOf(req.Domains)
	dbWanted := setOf(req.Databases)

	// Websites first: a site has to exist before its files have anywhere to
	// go, and its document root is what the files are unpacked into.
	var moves []actions.ImportMove
	for _, d := range plan.Domains {
		if len(wanted) > 0 && !wanted[d.Name] {
			continue
		}
		site, err := s.importSite(r, owner, d)
		if err != nil {
			report.Failed = append(report.Failed, fmt.Sprintf("%s: %s", d.Name, err))
			continue
		}
		if d.DocRootIn != "" {
			rel, err := s.relativeToHome(r, owner.ID, site.DocumentRoot)
			if err != nil {
				report.Failed = append(report.Failed, fmt.Sprintf("%s: %s", d.Name, err))
				continue
			}
			moves = append(moves, actions.ImportMove{From: d.DocRootIn, To: rel})
		}
		report.Sites = append(report.Sites, site.Domain)
	}

	if len(moves) > 0 {
		res, err := agentclient.Call[actions.ImportExtractResult](r.Context(),
			s.agent.WithTimeout(actions.ImportBudget), "import.extract", 1,
			actions.ImportExtractRequest{StagedPath: staged, Owner: owner.Username, Moves: moves})
		if err != nil {
			report.Failed = append(report.Failed, "files: "+trimAgent(err.Error()))
		} else {
			report.Files, report.Bytes = res.Files, res.Bytes
			if len(res.Skipped) > 0 {
				report.Notes = append(report.Notes, fmt.Sprintf(
					"%d entries were skipped: links and special files are not "+
						"brought across, because one can point outside the account.",
					len(res.Skipped)))
			}
		}
	}

	for _, d := range plan.Databases {
		if len(dbWanted) > 0 && !dbWanted[d.Name] {
			continue
		}
		name, err := s.importDatabase(r, owner, staged, d)
		if err != nil {
			report.Failed = append(report.Failed, fmt.Sprintf("%s: %s", d.Name, err))
			continue
		}
		report.Databases = append(report.Databases, name)
	}

	if err := s.sites.SyncWebserver(r.Context()); err != nil {
		report.Failed = append(report.Failed, "webserver: "+err.Error())
	}
	report.NotImported = plan.NotImported
	report.Notes = append(report.Notes, plan.Warnings...)

	s.audit(r, "import.run", owner.Username, len(report.Failed) == 0,
		fmt.Sprintf("%d sites, %d databases", len(report.Sites), len(report.Databases)))
	writeJSON(w, http.StatusOK, map[string]any{"report": report})
}

// handleImportDiscard removes a staged archive nobody went on to import.
func (s *Server) handleImportDiscard(w http.ResponseWriter, r *http.Request) {
	staged, ok := stagedImportPath(w, r.URL.Query().Get("staged"))
	if !ok {
		return
	}
	if _, err := agentclient.Call[struct{}](r.Context(), s.agent, "import.discard", 1,
		actions.ImportInspectRequest{StagedPath: staged}); err != nil {
		s.log.Warn("httpapi: discard import", "err", err)
	}
	_ = os.Remove(staged)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// importSite creates one website for an imported domain.
func (s *Server) importSite(r *http.Request, owner *db.User, d hostimport.Domain) (*db.Site, error) {
	if existing, err := s.db.SiteByDomain(r.Context(), d.Name); err == nil {
		return existing, fmt.Errorf("a website for %s already exists on this server", existing.Domain)
	}
	php := d.PHPVersion
	if php == "" {
		php = s.newestPHP(r)
	}
	appType := webserver.AppPHP
	if d.DocRootIn == "" {
		appType = webserver.AppStatic
		php = ""
	}
	return s.sites.Create(r.Context(), sites.CreateRequest{
		Domain: d.Name, OwnerID: owner.ID, AppType: appType,
		PHPVersion: php, RewriteMode: webserver.RewriteNone,
	})
}

// importDatabase creates a database and loads the archive's dump into it.
//
// Renamed to this panel's prefix: names carry the old account's prefix, and
// two customers moving from the same source server would otherwise collide.
// The application's configuration has to be updated either way, so a rename
// costs nothing extra and prevents that.
func (s *Server) importDatabase(r *http.Request, owner *db.User, staged string, d hostimport.Database) (string, error) {
	suffix := d.Name
	if i := strings.Index(suffix, "_"); i > 0 {
		suffix = suffix[i+1:]
	}
	created, err := s.databases.CreateDatabase(r.Context(), owner.ID, suffix)
	if err != nil {
		return "", err
	}
	if _, err := agentclient.Call[actions.ImportDBResult](r.Context(),
		s.agent.WithTimeout(actions.ImportBudget), "import.database", 1,
		actions.ImportDBRequest{
			StagedPath: staged, DumpIn: d.DumpIn, Database: created.Name,
		}); err != nil {
		return "", fmt.Errorf("created %s but could not load the dump: %s",
			created.Name, trimAgent(err.Error()))
	}
	return created.Name, nil
}

// importReport is what the operator is shown afterwards.
type importReport struct {
	Owner       string   `json:"owner"`
	Format      string   `json:"format"`
	Sites       []string `json:"sites"`
	Databases   []string `json:"databases"`
	Files       int      `json:"files"`
	Bytes       int64    `json:"bytes"`
	Failed      []string `json:"failed,omitempty"`
	Notes       []string `json:"notes,omitempty"`
	NotImported []string `json:"not_imported,omitempty"`
}

// stagedImportPath turns a name from the client back into a path, refusing
// anything that is not one of the panel's own staged uploads.
func stagedImportPath(w http.ResponseWriter, name string) (string, bool) {
	if name == "" || strings.ContainsAny(name, "/\\") || strings.Contains(name, "..") {
		writeError(w, http.StatusBadRequest, "bad_request", "no uploaded archive was named")
		return "", false
	}
	full := filepath.Join(actions.ImportStageDir, name)
	if _, err := os.Stat(full); err != nil {
		writeError(w, http.StatusNotFound, "not_found",
			"that upload is no longer here; upload the archive again")
		return "", false
	}
	return full, true
}

func setOf(names []string) map[string]bool {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// relativeToHome turns an absolute document root into the path the agent
// unpacks into, which is relative to the account's home.
func (s *Server) relativeToHome(r *http.Request, ownerID int64, docRoot string) (string, error) {
	home, err := s.db.UserLinuxHome(r.Context(), ownerID)
	if err != nil || home == "" {
		return "", fmt.Errorf("that account has no home directory")
	}
	rel := strings.TrimPrefix(strings.TrimPrefix(docRoot, home), "/")
	if rel == "" || rel == docRoot {
		return "", fmt.Errorf("%s is not inside %s", docRoot, home)
	}
	return rel, nil
}

// newestPHP is what a site gets when the archive did not say.
//
// The newest installed version rather than a fixed default: an archive with
// no recorded version is usually an old account, and the alternative is
// picking a version this server may not have.
func (s *Server) newestPHP(r *http.Request) string {
	res, err := agentclient.Call[actions.PHPListResult](r.Context(), s.agent, "php.list", 1, struct{}{})
	if err != nil {
		return ""
	}
	newest := ""
	for _, v := range res.Versions {
		if v.Installed {
			newest = v.Version
		}
	}
	return newest
}
