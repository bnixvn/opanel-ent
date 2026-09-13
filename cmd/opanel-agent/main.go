// Command opanel-agent is the privileged half of OPanel.
//
// It runs as root, listens on a unix socket, and executes a fixed set of
// typed actions on behalf of the unprivileged opanel-api process. Callers are
// identified by SO_PEERCRED, which the kernel supplies and no client can
// forge.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/version"
)

func main() {
	var (
		socketPath  = flag.String("socket", agent.SocketPath, "unix socket path")
		apiUser     = flag.String("api-user", "opanel", "unprivileged user allowed to call the agent")
		socketGroup = flag.String("socket-group", "opanel", "group given access to the socket")
		extraUIDs   = flag.String("allow-uid", "", "comma-separated extra uids allowed to call")
		logLevel    = flag.String("log-level", "info", "debug|info|warn|error")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("opanel-agent", version.String())
		return
	}

	log := newLogger(*logLevel)
	slog.SetDefault(log)

	if err := run(log, *socketPath, *apiUser, *socketGroup, *extraUIDs); err != nil {
		log.Error("opanel-agent: fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, socketPath, apiUser, socketGroup, extraUIDs string) error {
	// Running unprivileged would fail later at the first real action, with a
	// confusing permission error instead of a clear one here.
	if os.Geteuid() != 0 {
		return fmt.Errorf("must run as root (effective uid is %d)", os.Geteuid())
	}

	allowed, err := resolveUIDs(apiUser, extraUIDs)
	if err != nil {
		return err
	}
	gid := -1
	if socketGroup != "" {
		g, err := user.LookupGroup(socketGroup)
		if err != nil {
			return fmt.Errorf("look up socket group %q: %w", socketGroup, err)
		}
		if gid, err = strconv.Atoi(g.Gid); err != nil {
			return fmt.Errorf("parse gid of %q: %w", socketGroup, err)
		}
	}

	registry := agent.NewRegistry()
	actions.RegisterAll(registry)

	srv, err := agent.NewServer(agent.ServerOptions{
		Registry:    registry,
		AllowedUIDs: allowed,
		SocketGID:   gid,
		Logger:      log,
		// The agent's own record of every privileged action. It is written
		// here, on the root side, so a compromised api process cannot omit
		// entries from it.
		Audit: func(_ context.Context, ev agent.AuditEvent) {
			log.LogAttrs(context.Background(), slog.LevelInfo, "privileged action",
				slog.String("action", ev.Action),
				slog.Bool("ok", ev.OK),
				slog.Uint64("peer_uid", uint64(ev.PeerUID)),
				slog.Int64("peer_pid", int64(ev.PeerPID)),
				slog.Int64("ms", ev.Duration.Milliseconds()),
				slog.String("err", ev.Err),
			)
		},
	})
	if err != nil {
		return err
	}
	if err := srv.Listen(socketPath); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("opanel-agent: listening",
		"socket", socketPath,
		"version", version.String(),
		"allowed_uids", allowed,
		"actions", len(registry.Actions()),
	)

	if err := srv.Serve(ctx); err != nil {
		return err
	}
	log.Info("opanel-agent: stopped")
	return nil
}

// resolveUIDs builds the allowlist: root, the api user, and any extras.
func resolveUIDs(apiUser, extra string) ([]uint32, error) {
	uids := []uint32{0} // root can always call, for opanelctl recovery

	if apiUser != "" {
		u, err := user.Lookup(apiUser)
		if err != nil {
			// A fresh host runs the agent before the api user exists. Warn
			// rather than refuse, so the installer can start the agent first.
			slog.Warn("opanel-agent: api user not found, only root may call",
				"user", apiUser, "err", err)
		} else {
			n, err := strconv.ParseUint(u.Uid, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("parse uid of %q: %w", apiUser, err)
			}
			uids = append(uids, uint32(n))
		}
	}

	for _, s := range strings.Split(extra, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("invalid uid %q in -allow-uid: %w", s, err)
		}
		uids = append(uids, uint32(n))
	}
	return uids, nil
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
	// Text output: systemd captures stderr into the journal, where key=value
	// pairs stay readable with journalctl and greppable without jq.
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}
