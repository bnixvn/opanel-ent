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
	"github.com/bnixvn/opanel-ent/internal/config"
	"github.com/bnixvn/opanel-ent/internal/databases"
	"github.com/bnixvn/opanel-ent/internal/db"
	"github.com/bnixvn/opanel-ent/internal/panelusers"
	"github.com/bnixvn/opanel-ent/internal/plans"
	"github.com/bnixvn/opanel-ent/internal/sites"
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
	log       *slog.Logger

	loginLimiter *limiter
	handler      http.Handler
}

// New builds the API server and its route table.
func New(cfg *config.Config, database *db.DB, authSvc *auth.Service, ac *agentclient.Client, siteSvc *sites.Service, dbSvc *databases.Service, userSvc *panelusers.Service, planSvc *plans.Service,
	log *slog.Logger) *Server {
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
		log:       log,
		// Five password attempts per minute per IP. Enough that a person who
		// mistypes is unaffected, low enough that online guessing is futile.
		loginLimiter: newLimiter(5, time.Minute),
	}
	s.handler = s.routes()
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

		// Authenticated.
		api.Group(func(pr chi.Router) {
			pr.Use(s.authenticate)

			pr.Post("/auth/logout", s.handleLogout)
			pr.Get("/auth/me", s.handleMe)

			// Sites. Ownership is enforced per request inside the handlers,
			// so an end user reaches the same routes and sees only their own.
			pr.Get("/sites", s.handleSiteList)
			pr.Post("/sites", s.handleSiteCreate)
			pr.Get("/sites/{id}", s.handleSiteGet)
			pr.Patch("/sites/{id}", s.handleSiteUpdate)
			pr.Delete("/sites/{id}", s.handleSiteDelete)

			// Choosing a PHP version needs the list, so any authenticated
			// user may read it; changing what is installed does not.
			// Databases. Ownership is enforced per request, like sites.
			pr.Get("/databases", s.handleDatabaseList)
			pr.Post("/databases", s.handleDatabaseCreate)
			pr.Delete("/databases/{id}", s.handleDatabaseDelete)
			pr.Post("/databases/{id}/grants", s.handleGrant)
			pr.Delete("/databases/{id}/grants/{userID}", s.handleRevoke)
			pr.Post("/database-users", s.handleDBUserCreate)
			pr.Delete("/database-users/{id}", s.handleDBUserDelete)
			pr.Post("/database-users/{id}/password", s.handleDBUserPassword)

			// Own account.
			pr.Post("/auth/password", s.handleChangeOwnPassword)
			pr.Post("/auth/2fa/setup", s.handleTOTPSetup)
			pr.Post("/auth/2fa/enable", s.handleTOTPEnable)
			pr.Post("/auth/2fa/disable", s.handleTOTPDisable)

			// Usage against the assigned package. Readable by the account
			// itself; staff may read anyone's.
			pr.Get("/usage", s.handleUsage)
			pr.Get("/users/{id}/usage", s.handleUsage)
			pr.Get("/plans", s.handlePlanList)

			pr.Get("/php/versions", s.handlePHPList)
			pr.Get("/webserver/status", s.handleWebserverStatus)

			// Panel users. A reseller may manage end users; the handlers
			// refuse anything above that.
			pr.Group(func(rr chi.Router) {
				rr.Use(s.requireRole(auth.RoleReseller))
				rr.Get("/users", s.handleUserList)
				rr.Post("/users", s.handleUserCreate)
				rr.Patch("/users/{id}", s.handleUserUpdate)
				rr.Delete("/users/{id}", s.handleUserDelete)
				rr.Post("/users/{id}/password", s.handleUserPassword)
				rr.Post("/users/{id}/sftp-password", s.handleUserSFTPPassword)
				rr.Post("/users/{id}/plan", s.handleUserPlanAssign)
			})

			pr.Group(func(ar chi.Router) {
				ar.Use(s.requireRole(auth.RoleAdmin))
				ar.Get("/system/info", s.handleSystemInfo)
				ar.Get("/system/services", s.handleServiceList)
				ar.Post("/system/services/{unit}/{action}", s.handleServiceAction)
				ar.Get("/system/audit", s.handleAuditList)
				ar.Post("/php/versions/{version}/install", s.handlePHPInstall)
				ar.Delete("/php/versions/{version}", s.handlePHPUninstall)
				ar.Post("/webserver/sync", s.handleWebserverSync)
				ar.Post("/plans", s.handlePlanCreate)
				ar.Patch("/plans/{id}", s.handlePlanUpdate)
				ar.Delete("/plans/{id}", s.handlePlanDelete)
			})
		})
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
	go func() {
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
	}()
}
