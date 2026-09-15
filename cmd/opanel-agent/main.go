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
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/agent/actions"
	"github.com/bnixvn/opanel-ent/internal/phpmgr"
	"github.com/bnixvn/opanel-ent/internal/platform/linuxuser"
	"github.com/bnixvn/opanel-ent/internal/platform/svc"
	"github.com/bnixvn/opanel-ent/internal/version"
	"github.com/bnixvn/opanel-ent/internal/webserver"
	"github.com/bnixvn/opanel-ent/internal/webserver/backends"
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

	// Resolved per call rather than chosen here. The panel can switch
	// webserver while this process is running, and a backend captured at
	// startup would go on writing the old server's configuration until
	// somebody restarted the agent.
	// What starts at boot has to agree with what the panel says is running.
	// Nothing kept those two in step: a switch sets both, but anything that
	// touched one of them alone -- a restore, an operator with systemctl, a
	// switch interrupted midway -- left the host set to start one server
	// while the panel rendered configuration for the other. The next reboot
	// resolves that in systemd's favour, silently, which is the worst moment
	// to find out.
	reconcileBootState(context.Background(), log)

	registry := agent.NewRegistry()
	actions.RegisterAll(registry, actions.Deps{
		Backend: backends.ActiveBackend,
		PHP:     func() phpmgr.Provider { return phpmgr.ForBackend(backends.Active()) },
	})

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

	// Homes provisioned by older builds belong to their occupant, and sshd
	// will not chroot into a directory its occupant owns -- so SFTP fails
	// with an error only the server's log ever sees. Put them right on the
	// way up rather than wait for somebody to report that uploads do not
	// work.
	if fixed, err := linuxuser.RepairHomes(context.Background()); err != nil {
		log.Warn("opanel-agent: could not check account homes", "err", err)
	} else if len(fixed) > 0 {
		log.Info("opanel-agent: repaired home ownership", "accounts", fixed)
	}

	// Terminals get their own listener beside the action socket, because a
	// shell is a stream and the action protocol is not.
	termSrv := agent.NewTerminalServer(agent.TerminalOptions{
		Logger:      log,
		SocketGID:   gid,
		AllowedUIDs: allowed,
	})
	termPath := filepath.Join(filepath.Dir(socketPath), "terminal.sock")
	if err := termSrv.Listen(termPath); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := termSrv.Serve(ctx); err != nil {
			log.Error("opanel-agent: terminal listener stopped", "err", err)
		}
	}()

	log.Info("opanel-agent: listening",
		"socket", socketPath,
		"version", version.String(),
		"allowed_uids", allowed,
		"actions", len(registry.Actions()),
		"terminal_socket", termPath,
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

// reconcileBootState makes systemd agree with the recorded backend.
//
// At agent startup, because that is the one moment the panel is certainly
// running and certainly root, and because a host that has just booted wrong
// is a host somebody is already looking at.
//
// Measured on a real server: both web servers running at once, and the one
// enabled at boot was not the one the panel was rendering configuration for.
// Nothing was going to notice until a reboot.
func reconcileBootState(ctx context.Context, log *slog.Logger) {
	active := backends.Active()
	for _, name := range webserver.Backends {
		b, err := backends.New(name)
		if err != nil || !b.Installed() {
			continue
		}
		unit := b.ServiceUnit()
		if name == active {
			if err := svc.Enable(ctx, unit, false); err != nil {
				log.Warn("agent: could not enable the active webserver", "unit", unit, "err", err)
			}
			continue
		}
		st, err := svc.Get(ctx, unit)
		if err != nil || st.Enabled == "not-found" {
			continue
		}
		// Two servers cannot both hold port 80. Whichever this is, it is
		// either serving nothing or serving instead of the one the panel
		// thinks is in charge.
		if st.Running() {
			log.Warn("agent: stopping a web server that is not the active one", "unit", unit)
			if err := svc.Stop(ctx, unit); err != nil {
				log.Warn("agent: could not stop it", "unit", unit, "err", err)
			}
		}
		if err := svc.Disable(ctx, unit, false); err != nil {
			log.Warn("agent: could not disable it", "unit", unit, "err", err)
		}
	}
}
