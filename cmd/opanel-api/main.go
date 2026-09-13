// Command opanel-api serves the panel HTTP API.
//
// It runs as the unprivileged opanel user and holds no root capability of its
// own: anything that touches the host goes through opanel-agent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agentclient"
	"github.com/bnixvn/opanel-ent/internal/auth"
	"github.com/bnixvn/opanel-ent/internal/config"
	"github.com/bnixvn/opanel-ent/internal/db"
	"github.com/bnixvn/opanel-ent/internal/httpapi"
	"github.com/bnixvn/opanel-ent/internal/sites"
	"github.com/bnixvn/opanel-ent/internal/version"
	"github.com/bnixvn/opanel-ent/internal/webserver"
)

func main() {
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("opanel-api", version.String())
		return
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "opanel-api: configuration error:", err)
		os.Exit(2)
	}

	log := newLogger(cfg.LogLevel)
	slog.SetDefault(log)

	if err := run(cfg, log); err != nil {
		log.Error("opanel-api: fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfg *config.Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	database, err := db.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()

	schema, err := database.SchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	authSvc := auth.NewService(database, cfg.SessionTTL, cfg.SessionIdleTTL)
	ac := agentclient.New(cfg.AgentSocket, 0)

	// The agent is not required to start. A panel that refuses to boot when
	// the agent is down cannot show the operator why the agent is down.
	if err := ac.Ping(ctx); err != nil {
		log.Warn("opanel-api: agent not reachable at startup",
			"socket", cfg.AgentSocket, "err", err)
	}

	siteSvc := sites.New(database, ac, webserver.DefaultServerConfig(), log)
	api := httpapi.New(cfg, database, authSvc, ac, siteSvc, log)
	api.StartBackgroundTasks(ctx)

	srv := api.HTTPServer()
	errc := make(chan error, 1)
	go func() {
		log.Info("opanel-api: listening",
			"addr", cfg.ListenAddr,
			"version", version.String(),
			"db", database.Path(),
			"schema", schema,
			"env", cfg.Env,
			"webserver", cfg.WebserverBackend,
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}

	log.Info("opanel-api: shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}
