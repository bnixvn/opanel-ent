// Package httpapi serves the panel REST API.
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/backups"
	"github.com/bnixvn/opanel-ent/internal/config"
	"github.com/bnixvn/opanel-ent/internal/databases"
	"github.com/bnixvn/opanel-ent/internal/db"
	"github.com/bnixvn/opanel-ent/internal/filemanager"
	"github.com/bnixvn/opanel-ent/internal/malware"
	"github.com/bnixvn/opanel-ent/internal/panelusers"
	"github.com/bnixvn/opanel-ent/internal/plans"
	"github.com/bnixvn/opanel-ent/internal/security"
	"github.com/bnixvn/opanel-ent/internal/sites"
	"github.com/bnixvn/opanel-ent/internal/wordpress"
)

// Server holds the dependencies every handler needs.
type Server struct {
	cfg       *config.Config
	db        *db.DB
	auth      *auth.Service
	agent     *agentclient.Client
	sites     *sites.Service
	databases *databases.Service
	users     *panelusers.Service
	plans     *plans.Service
	files     *filemanager.Service
	backups   *backups.Service
	wordpress *wordpress.Service
	firewall  *security.Firewall
	malware   *malware.Service
	log       *slog.Logger

	loginLimiter *limiter
	handler      http.Handler
}

// New builds the API server and its route table.
func New(cfg *config.Config, database *db.DB, authSvc *auth.Service, ac *agentclient.Client, siteSvc *sites.Service, dbSvc *databases.Service, userSvc *panelusers.Service, planSvc *plans.Service, fileSvc *filemanager.Service,
	backupSvc *backups.Service, wpSvc *wordpress.Service, fwSvc *security.Firewall,
	malwareSvc *malware.Service, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		cfg:       cfg,
		db:        database,
		auth:      authSvc,
		agent:     ac,
		sites:     siteSvc,
		databases: dbSvc,
		users:     userSvc,
		plans:     planSvc,
		files:     fileSvc,
		backups:   backupSvc,
		wordpress: wpSvc,
		firewall:  fwSvc,
		malware:   malwareSvc,
		log:       log,
		// Five password attempts per minute per IP. Enough that a person who
		// mistypes is unaffected, low enough that online guessing is futile.
		loginLimiter: newLimiter(5, time.Minute),
	}
	s.handler = s.routes()

	// A job is a goroutine and nothing more, so a restart takes every
	// running one with it. Saying so beats a row that claims to be running
	// for ever and a page that waits on it.
	if n, err := database.FailRunningFileJobs(context.Background()); err != nil {
		log.Warn("httpapi: could not close out interrupted file jobs", "err", err)
	} else if n > 0 {
		log.Info("httpapi: closed out file jobs interrupted by a restart", "jobs", n)
	}
	return s
}

// ServeHTTP makes Server an http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	r.Use(s.recoverer, s.accessLog, s.securityHeaders)

	r.Route("/api", func(api chi.Router) {
		// Unauthenticated.
		api.Get("/health", s.handleHealth)
		api.Post("/auth/login", s.handleLogin)
		// The login page needs the brand before anyone has signed in.
		api.Get("/branding", s.handleBranding)
		// Called by the phpMyAdmin shim over the loopback address, which
		// has no panel session. The one-time ticket is the credential.
		api.Post("/internal/sso/redeem", s.handleSSORedeem)

		// Authenticated.
		api.Group(func(pr chi.Router) {
			pr.Use(s.authenticate)

			pr.Post("/auth/logout", s.handleLogout)
			pr.Get("/auth/me", s.handleMe)
			// Available to the customer account the operator is wearing --
			// it is the way back out, so it cannot need staff rights.
			pr.Post("/auth/impersonate/stop", s.handleStopImpersonating)

			// Sites. Ownership is enforced per request inside the handlers,
			// so an end user reaches the same routes and sees only their own.
			pr.Get("/sites", s.handleSiteList)
			pr.Post("/sites", s.handleSiteCreate)
			pr.Get("/sites/{id}", s.handleSiteGet)
			pr.Patch("/sites/{id}", s.handleSiteUpdate)
			pr.Delete("/sites/{id}", s.handleSiteDelete)
			// Moving a site between accounts moves its files, so it is a
			// staff action and has its own endpoint.
			pr.With(s.requireRole(auth.RoleReseller)).
				Post("/sites/{id}/owner", s.handleSiteOwner)
			pr.Get("/sites/{id}/php", s.handleSitePHPSettings)
			pr.Put("/sites/{id}/php", s.handleSitePHPSettingsSave)
			pr.Get("/wordpress", s.handleWordPressList)
			pr.Get("/sites/{id}/wordpress/info", s.handleWordPressInfo)
			pr.Post("/sites/{id}/wordpress/manage", s.handleWordPressManage)
			pr.Post("/sites/{id}/wordpress/signon", s.handleWordPressSignOn)
			pr.Get("/sites/{id}/wordpress", s.handleWordPressStatus)
			pr.Post("/sites/{id}/wordpress", s.handleWordPressInstall)
			pr.Get("/sites/{id}/certificate/reusable", s.handleCertReusable)
			pr.Post("/sites/{id}/certificate/wildcard", s.handleSiteCertificateWildcard)
			pr.Post("/sites/{id}/certificate/reuse", s.handleSiteCertificateReuse)
			pr.Post("/sites/{id}/certificate/manual", s.handleSiteCertificateManual)
			pr.Post("/sites/{id}/certificate", s.handleSiteCertificate)
			pr.Delete("/sites/{id}/certificate", s.handleSiteCertificateDelete)

			// Choosing a PHP version needs the list, so any authenticated
			// user may read it; changing what is installed does not.
			// Databases. Ownership is enforced per request, like sites.
			pr.Get("/databases", s.handleDatabaseList)
			pr.Post("/databases", s.handleDatabaseCreate)
			pr.Get("/databases/{id}/export", s.handleDatabaseExport)
			pr.Get("/phpmyadmin", s.handlePMAStatus)
			pr.Post("/phpmyadmin/signon", s.handlePMASignon)
			pr.Delete("/databases/{id}", s.handleDatabaseDelete)
			pr.Post("/databases/{id}/grants", s.handleGrant)
			pr.Delete("/databases/{id}/grants/{userID}", s.handleRevoke)
			pr.Post("/database-users", s.handleDBUserCreate)
			pr.Delete("/database-users/{id}", s.handleDBUserDelete)
			pr.Post("/database-users/{id}/password", s.handleDBUserPassword)

			// Own account. Not while wearing somebody else's: a password or
			// second factor changed from inside an impersonated session
			// would be recorded against the customer, and the account holder
			// has to be able to tell what they did from what was done to
			// them.
			pr.Group(func(own chi.Router) {
				own.Use(s.refuseWhileImpersonating)
				own.Post("/auth/password", s.handleChangeOwnPassword)
				own.Post("/auth/email", s.handleChangeOwnEmail)
				own.Get("/auth/passkeys", s.handlePasskeyStatus)
				own.Post("/auth/passkeys/register/start", s.handlePasskeyRegisterStart)
				own.Post("/auth/passkeys/register/finish", s.handlePasskeyRegisterFinish)
				own.Delete("/auth/passkeys", s.handlePasskeyDelete)
				own.Post("/auth/2fa/setup", s.handleTOTPSetup)
				own.Post("/auth/2fa/enable", s.handleTOTPEnable)
				own.Post("/auth/2fa/disable", s.handleTOTPDisable)
			})

			// Usage against the assigned package. Readable by the account
			// itself; staff may read anyone's.
			pr.Get("/usage", s.handleUsage)
			pr.Get("/users/{id}/usage", s.handleUsage)
			pr.Get("/plans", s.handlePlanList)
			// A reseller reads their own allowance here; an administrator
			// reads anyone's with ?user=<id>.
			pr.Get("/reseller", s.handleResellerSummary)
			pr.Get("/quota/status", s.handleQuotaStatus)
			pr.Get("/system/address", s.handleServerAddress)

			// Logs. Scoped to a website the caller owns, because a log holds
			// visitors' addresses and the paths they asked for.
			pr.Get("/logs", s.handleLogTail)
			pr.Get("/logs/download", s.handleLogDownload)
			pr.Get("/backup-destinations", s.handleDestinationList)
			pr.Post("/backup-destinations", s.handleDestinationCreate)
			pr.Patch("/backup-destinations/{id}", s.handleDestinationUpdate)
			pr.Post("/backup-destinations/{id}/test", s.handleDestinationTest)
			pr.Delete("/backup-destinations/{id}", s.handleDestinationDelete)
			// SFTP credentials. A user manages their own; staff may pass
			// ?user= to manage one of their customers'.
			// A shell as your own Linux account. Staff may pass ?user= for
			// one of their customers; the agent decides whether to grant it.
			pr.Get("/terminal/status", s.handleTerminalStatus)
			pr.Get("/terminal", s.handleTerminal)
			pr.Post("/account/file-access", s.handleFileAccessEnable)
			pr.Get("/sftp", s.handleSFTPList)
			pr.Post("/sftp", s.handleSFTPCreate)
			pr.Post("/sftp/primary/password", s.handleSFTPPrimaryPassword)
			pr.Post("/sftp/{id}/password", s.handleSFTPPassword)
			pr.Delete("/sftp/{id}", s.handleSFTPDelete)
			pr.Get("/cron", s.handleCronList)
			pr.Post("/cron", s.handleCronCreate)
			pr.Patch("/cron/{id}", s.handleCronUpdate)
			pr.Delete("/cron/{id}", s.handleCronDelete)
			pr.Get("/malware", s.handleMalwareStatus)
			pr.Post("/malware/scan", s.handleMalwareScan)
			pr.Get("/malware/scans/{id}", s.handleMalwareScanGet)
			pr.Post("/malware/findings/{id}", s.handleMalwareAct)
			pr.Put("/malware/schedule", s.handleMalwareSchedule)
			pr.Delete("/malware/schedule", s.handleMalwareSchedule)
			pr.Get("/notifications", s.handleNotificationGet)
			pr.Put("/notifications", s.handleNotificationSet)

			// Files. The service resolves whose home is in play, so an end
			// user reaches the same routes and can only ever see their own.
			pr.Get("/files", s.handleFileList)
			pr.Get("/files/limits", s.handleFileLimits)
			pr.Get("/files/content", s.handleFileRead)
			pr.Put("/files/content", s.handleFileWrite)
			pr.Post("/files/directory", s.handleFileMkdir)
			pr.Post("/files/rename", s.handleFileRename)
			pr.Post("/files/chmod", s.handleFileChmod)
			pr.Post("/files/chmod-recursive", s.handleFileChmodRecursive)
			pr.Post("/files/copy", s.handleFileCopy)
			pr.Post("/files/archive", s.handleFileArchive)
			pr.Post("/files/extract", s.handleFileExtract)
			pr.Post("/files/upload", s.handleFileUpload)
			// Packing, unpacking and installing an upload run in the
			// background; these are how a page finds out how they went.
			pr.Get("/file-jobs", s.handleFileJobList)
			pr.Get("/file-jobs/{id}", s.handleFileJobGet)
			pr.Get("/files/download", s.handleFileDownload)
			pr.Delete("/files", s.handleFileDelete)

			// Backups. Same ownership rule as files: the service decides
			// whose account is in play, so an end user sees only their own.
			pr.Get("/backups", s.handleBackupList)
			pr.Post("/backups", s.handleBackupCreate)
			pr.Post("/backups/upload", s.handleBackupUpload)
			pr.Get("/backups/schedule", s.handleScheduleGet)
			pr.Put("/backups/schedule", s.handleScheduleSave)
			pr.Delete("/backups/schedule", s.handleScheduleDelete)
			pr.Get("/backups/{id}", s.handleBackupGet)
			pr.Get("/backups/{id}/download", s.handleBackupDownload)
			pr.Post("/backups/{id}/restore", s.handleBackupRestore)
			pr.Delete("/backups/{id}", s.handleBackupDelete)

			pr.Get("/php/versions", s.handlePHPList)
			// Readable by anyone signed in: these are the limits their
			// websites run under. Writing is administrator-only below.
			pr.Get("/php/versions/{version}/settings", s.handlePHPSettings)
			// Websites that do not follow their version's settings. Listed
			// here because this is the page that decides them.
			pr.Get("/php/overrides", s.handlePHPOverrides)
			pr.Delete("/sites/{id}/php", s.handlePHPOverrideClear)
			pr.Get("/webserver/status", s.handleWebserverStatus)

			// Panel users. A reseller may manage end users; the handlers
			// refuse anything above that.
			pr.Group(func(rr chi.Router) {
				rr.Use(s.requireRole(auth.RoleReseller))
				// Importing an account creates websites and databases for
				// somebody else, so it is not a customer's button.
				// How busy the machine is. Staff only: on a shared server
				// the load is largely made of other tenants' traffic, and
				// a customer has no business reading it.
				rr.Get("/system/stats", s.handleSystemStats)
				rr.Post("/import/upload", s.handleImportUpload)
				rr.Post("/import/run", s.handleImportRun)
				rr.Delete("/import", s.handleImportDiscard)
				rr.Get("/users", s.handleUserList)
				rr.Post("/users", s.handleUserCreate)
				rr.Patch("/users/{id}", s.handleUserUpdate)
				rr.Delete("/users/{id}", s.handleUserDelete)
				rr.Post("/users/{id}/password", s.handleUserPassword)
				rr.Post("/users/{id}/impersonate", s.handleImpersonate)
				rr.Post("/users/{id}/sftp-password", s.handleUserSFTPPassword)
				rr.Post("/users/{id}/plan", s.handleUserPlanAssign)
				rr.Post("/plans", s.handlePlanCreate)
				rr.Patch("/plans/{id}", s.handlePlanUpdate)
				rr.Delete("/plans/{id}", s.handlePlanDelete)
			})

			pr.Group(func(ar chi.Router) {
				ar.Use(s.requireRole(auth.RoleAdmin))
				ar.Get("/system/info", s.handleSystemInfo)
				ar.Get("/system/services", s.handleServiceList)
				ar.Post("/system/services/{unit}/{action}", s.handleServiceAction)
				ar.Get("/system/audit", s.handleAuditList)
				ar.Post("/php/versions/{version}/install", s.handlePHPInstall)
				ar.Delete("/php/versions/{version}", s.handlePHPUninstall)
				ar.Put("/php/versions/{version}/settings", s.handlePHPSettingsSave)
				ar.Post("/webserver/sync", s.handleWebserverSync)
				ar.Get("/cloudlinux", s.handleCloudLinux)
				ar.Post("/cloudlinux/manager", s.handleCloudLinuxManagerInstall)
				ar.Post("/cloudlinux/integration", s.handleCloudLinuxIntegration)
				ar.Post("/cloudlinux/selector", s.handleCloudLinuxSelectorSetup)
				ar.Get("/webserver/backends", s.handleWebserverBackends)
				ar.Post("/webserver/switch", s.handleWebserverSwitch)
				ar.Post("/webserver/litespeed/install", s.handleLiteSpeedInstall)
				ar.Get("/firewall", s.handleFirewallList)
				ar.Post("/firewall/rules", s.handleFirewallCreate)
				ar.Delete("/firewall/rules/{id}", s.handleFirewallDelete)
				ar.Post("/firewall/confirm", s.handleFirewallConfirm)
				ar.Post("/firewall/sources", s.handleFirewallSourceCreate)
				ar.Post("/firewall/sources/{id}/refresh", s.handleFirewallSourceRefresh)
				ar.Delete("/firewall/sources/{id}", s.handleFirewallSourceDelete)

				ar.Get("/waf", s.handleWAFStatus)
				ar.Post("/waf/install", s.handleWAFInstall)
				ar.Put("/waf", s.handleWAFConfigure)
				ar.Get("/waf/rules", s.handleWAFRules)
				ar.Get("/waf/events", s.handleWAFEvents)
				ar.Post("/sites/{id}/waf", s.handleSiteWAF)

				ar.Post("/malware/install", s.handleMalwareInstall)
				ar.Delete("/malware/install", s.handleMalwareRemove)
				ar.Post("/malware/update", s.handleMalwareUpdate)
				ar.Post("/phpmyadmin/install", s.handlePMAInstall)
				ar.Get("/certificates", s.handleCertList)
				ar.Delete("/certificates", s.handleCertDelete)
				ar.Get("/settings", s.handleSettings)
				ar.Put("/settings/branding", s.handleBrandingSet)
				ar.Post("/settings/hostnames", s.handleHostnameAdd)
				ar.Delete("/settings/hostnames", s.handleHostnameDelete)
				ar.Put("/settings/ipv6", s.handleIPv6Set)
				ar.Put("/settings/passkeys", s.handlePasskeyEnable)
				ar.Put("/settings/terminal", s.handleTerminalSettingsSet)
				ar.Put("/users/{id}/reseller-limits", s.handleResellerLimitsSet)
				ar.Post("/users/{id}/parent", s.handleUserParentSet)
			})
		})
	})

	// CloudLinux Manager, proxied to the service that serves it. Mounted here
	// rather than under /api because the Manager builds its own links from
	// the base_uri the integration file gives it, and that has to be a path
	// the browser can ask for.
	//
	// Administrators only. It manages every account's limits, and the Manager
	// itself decides what to show from the identity this panel gives it --
	// which means the panel has to be the thing deciding who may ask.
	r.Route("/lvemanager", func(lm chi.Router) {
		lm.Use(s.authenticate)
		lm.Use(s.requireRole(auth.RoleAdmin))
		lm.Handle("/*", s.lveManagerProxy())
		lm.Handle("/", s.lveManagerProxy())
	})

	// phpMyAdmin, proxied to its loopback vhost. Behind the session check,
	// so an unauthenticated request never reaches PHP at all.
	r.Route("/phpmyadmin", func(pma chi.Router) {
		pma.Use(s.authenticate)
		pma.Handle("/*", s.pmaProxy())
	})

	// Everything outside /api is the user interface.
	r.Handle("/*", uiHandler())

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		// Only /api reaches here now; a wrong path under it is a client
		// error worth naming rather than a page to render.
		writeError(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	})
	return r
}

// HTTPServer wraps the API in an *http.Server with sane timeouts.
//
// ReadHeaderTimeout in particular is what stops a Slowloris client from
// holding connections open indefinitely.
func (s *Server) HTTPServer() *http.Server {
	return &http.Server{
		Addr:              s.cfg.ListenAddr,
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
}

// StartBackgroundTasks runs periodic housekeeping until ctx is cancelled.
func (s *Server) StartBackgroundTasks(ctx context.Context) {
	// A row left at "running" by a restart describes a backup that is not
	// coming back; say so rather than showing a spinner for ever.
	if n, err := s.db.MarkStaleBackupsFailed(ctx); err != nil {
		s.log.Warn("httpapi: cannot close out interrupted backups", "err", err)
	} else if n > 0 {
		s.log.Warn("httpapi: marked interrupted backups as failed", "count", n)
	}

	go s.purgeSessionsLoop(ctx)
	go s.renewCertificatesLoop(ctx)
	go s.backupScheduleLoop(ctx)
	go s.blocklistRefreshLoop(ctx)
	go s.sweepSSOAccounts(ctx)
	go s.malwareScheduleLoop(ctx)
}

// blocklistRefreshLoop re-fetches the firewall's subscribed blocklists.
//
// Hourly, with each source deciding from its own interval whether it is due.
// A feed that is unreachable is recorded against that source and does not
// stop the others.
func (s *Server) blocklistRefreshLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			done, errs := s.firewall.RefreshDue(ctx)
			for _, err := range errs {
				s.log.Warn("httpapi: blocklist refresh", "err", err)
			}
			if done > 0 {
				s.log.Info("httpapi: blocklists refreshed", "count", done)
			}
		}
	}
}

// backupScheduleLoop fires due schedules and prunes what retention no longer
// needs.
//
// Every ten minutes rather than on the hour: the sweep has to catch a server
// that was down at 3am, and Due() is written so a late tick still runs the
// backup instead of skipping the day.
func (s *Server) backupScheduleLoop(ctx context.Context) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			started, errs := s.backups.RunDue(ctx, now)
			for _, err := range errs {
				s.log.Error("httpapi: scheduled backup", "err", err)
			}
			if started > 0 {
				s.log.Info("httpapi: scheduled backups started", "count", started)
			}
			// Pruning waits a tick behind the run that produced the newest
			// archive, so retention never deletes the oldest copy while its
			// replacement is still being written.
			removed, perrs := s.backups.Prune(ctx)
			for _, err := range perrs {
				s.log.Warn("httpapi: backup retention", "err", err)
			}
			if removed > 0 {
				s.log.Info("httpapi: pruned backups", "count", removed)
			}
		}
	}
}

func (s *Server) purgeSessionsLoop(ctx context.Context) {
	t := time.NewTicker(15 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := s.auth.PurgeExpiredSessions(ctx)
			if err != nil {
				s.log.Warn("httpapi: purge sessions", "err", err)
			} else if n > 0 {
				s.log.Debug("httpapi: purged expired sessions", "count", n)
			}
		}
	}
}

// renewCertificatesLoop re-issues certificates before they lapse.
//
// Twice a day, with the first sweep a minute after start. Certificates are
// renewed 30 days ahead, so this has sixty chances to succeed before anything
// is visible to a visitor — which matters because the failure mode of a
// missed renewal is every browser refusing the site at once.
func (s *Server) renewCertificatesLoop(ctx context.Context) {
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		renewed, errs := s.sites.RenewDueCertificates(ctx, s.cfg.ACMEEmail)
		if renewed > 0 {
			s.log.Info("httpapi: renewed certificates", "count", renewed)
		}
		for _, err := range errs {
			s.log.Error("httpapi: certificate renewal", "err", err)
		}
		timer.Reset(12 * time.Hour)
	}
}
