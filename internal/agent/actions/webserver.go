package actions

import (
	"context"
	"fmt"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/phpfpm"
	"github.com/bnixvn/opanel-ent/internal/phpmgr"
	"github.com/bnixvn/opanel-ent/internal/platform/svc"
	"github.com/bnixvn/opanel-ent/internal/webserver"
	"github.com/bnixvn/opanel-ent/internal/webserver/backends"
)

// pmaPoolName is the pool phpMyAdmin runs in. It is shaped like a domain
// because that is what a pool name has to be, and it is not one any site can
// take: the panel refuses to create a site under .opanel.
const pmaPoolName = "phpmyadmin.opanel"

// WebserverListResult describes every backend the panel can run.
type WebserverListResult struct {
	Active   string           `json:"active"`
	Backends []backends.Info  `json:"backends"`
	PHP      string           `json:"php_provider"`
	Versions []phpmgr.Version `json:"php_versions,omitempty"`
}

// WebserverSwitchRequest changes which webserver serves the host.
//
// It carries the whole configuration, not just a name. Switching means
// rendering every site for the new server and starting it, and doing that in
// two actions would leave the host serving nothing in between -- which is
// exactly the window an operator switching a live server cannot afford.
type WebserverSwitchRequest struct {
	Backend string `json:"backend"`
	WebserverApplyRequest
}

// Validate checks the name and the configuration it carries.
func (r *WebserverSwitchRequest) Validate() error {
	if !slices.Contains(webserver.Backends, r.Backend) {
		return fmt.Errorf("webserver %q is not one of %v", r.Backend, webserver.Backends)
	}
	return r.WebserverApplyRequest.Validate()
}

// WebserverSwitchResult reports what happened.
type WebserverSwitchResult struct {
	Backend string `json:"backend"`
	Unit    string `json:"unit"`
	// Previous is the backend that was running, so the panel can say what it
	// switched away from rather than just what it switched to.
	Previous string `json:"previous"`
	PHP      string `json:"php_provider"`
}

func registerWebserver(r *agent.Registry, deps Deps) {
	agent.Register(r, "webserver.backends", 1, func(ctx context.Context, _ struct{}) (WebserverListResult, error) {
		out := WebserverListResult{Active: backends.Active(), Backends: backends.List()}
		provider := deps.PHP()
		out.PHP = provider.Name()
		if vs, err := provider.Available(ctx); err == nil {
			out.Versions = vs
		}
		return out, nil
	})

	// Slow on purpose: a switch renders every site, writes every pool, and
	// stops one daemon while starting another. On a host with a few hundred
	// sites that is minutes, and being told "timed out" halfway through a
	// webserver switch is the worst possible moment for it.
	agent.RegisterSlow(r, "webserver.switch", 1, SwitchBudget,
		func(ctx context.Context, in WebserverSwitchRequest) (WebserverSwitchResult, error) {
			return switchBackend(ctx, in)
		})
}

// SwitchBudget bounds a webserver switch.
const SwitchBudget = 15 * time.Minute

// switchBackend renders for the new server, starts it, and stops the old one.
//
// The order is what makes this safe to run on a live host: everything that
// can fail -- rendering, the config test, writing pools -- happens before the
// running server is touched. Only once the new configuration is known good
// does the old daemon stop, and if the new one then fails to come up, the old
// one is started again and the active backend is left where it was.
func switchBackend(ctx context.Context, in WebserverSwitchRequest) (WebserverSwitchResult, error) {
	previous := backends.Active()

	target, err := backends.New(in.Backend)
	if err != nil {
		return WebserverSwitchResult{}, &agent.PayloadError{Err: err}
	}
	if !target.Installed() {
		return WebserverSwitchResult{}, &agent.DeniedError{
			Reason: fmt.Sprintf("%s is not installed on this host", in.Backend),
		}
	}

	provider := phpmgr.ForBackend(in.Backend)
	sites, err := resolve(provider, in.Sites)
	if err != nil {
		return WebserverSwitchResult{}, &agent.PayloadError{Err: err}
	}
	if err := canServePHP(in.Backend, sites); err != nil {
		return WebserverSwitchResult{}, err
	}

	cfg := in.Config
	cfg.WAFRulesFile = wafRulesIfInstalled()
	cfg.ServerUser, cfg.ServerGroup = serverAccount(in.Backend, cfg)
	if st := pmaStatus(); st.Installed {
		cfg.PMARoot = st.Root
		cfg.PMAPort = PMAPort
		if fp, ok := provider.(phpmgr.FPMProvider); ok {
			cfg.PMAFPMSocket = fp.SocketPath(PMAPHPVersion, pmaPoolName)
		}
	}

	rendered, err := target.Render(cfg, sites)
	if err != nil {
		return WebserverSwitchResult{}, err
	}

	// Pools first, and only then the handover. A site's interpreter is the
	// same pool under Apache and under LiteSpeed Enterprise, so switching
	// between those two does not disturb a single PHP process.
	if fp, ok := provider.(phpmgr.FPMProvider); ok {
		pools := phpfpm.PoolsFor(fp, sites, cfg.ServerUser, hostMemoryMB())
		if cfg.PMARoot != "" && cfg.PMAFPMSocket != "" {
			pools = append(pools, pmaPool(cfg))
		}
		if err := phpfpm.Apply(ctx, fp, pools); err != nil {
			return WebserverSwitchResult{}, err
		}
	}

	// One server owns ports 80 and 443. The old one has to let go before the
	// new one can bind, so this is the only moment the host is not serving,
	// and it lasts as long as one systemctl stop.
	if previous != in.Backend {
		if old, err := backends.New(previous); err == nil {
			if err := stopAndDisable(ctx, old.ServiceUnit()); err != nil {
				return WebserverSwitchResult{}, err
			}
			// systemctl returns when the unit is stopped, which is not the
			// same as the port being free: a server with worker processes
			// can hold the listening socket for a moment after its main
			// process is gone. Starting the new one into that moment fails
			// with "address already in use" and reads like a configuration
			// problem, which it is not.
			if err := waitForPorts(ctx, portsOf(cfg), 30*time.Second); err != nil {
				_ = svc.Enable(ctx, old.ServiceUnit(), true)
				return WebserverSwitchResult{}, err
			}
		}
	}

	if err := target.Apply(ctx, rendered); err != nil {
		// Put the old one back. The new server never started, so its
		// configuration on disk is inert and can stay where it is.
		if previous != in.Backend {
			if old, oerr := backends.New(previous); oerr == nil {
				_ = svc.Enable(ctx, old.ServiceUnit(), true)
			}
		}
		return WebserverSwitchResult{}, fmt.Errorf("switch to %s failed and %s was restarted: %w",
			in.Backend, previous, err)
	}

	if err := svc.Enable(ctx, target.ServiceUnit(), false); err != nil {
		return WebserverSwitchResult{}, fmt.Errorf("enable %s: %w", target.ServiceUnit(), err)
	}
	if err := backends.SetActive(in.Backend); err != nil {
		return WebserverSwitchResult{}, err
	}
	return WebserverSwitchResult{
		Backend:  in.Backend,
		Unit:     target.ServiceUnit(),
		Previous: previous,
		PHP:      provider.Name(),
	}, nil
}

// canServePHP refuses a switch that would stop PHP being PHP.
//
// LiteSpeed Enterprise reads Apache's configuration -- document roots,
// hostnames, rewrites, all of it -- but it does not honour the handler that
// sends .php to a PHP-FPM pool over a unix socket. What it does instead is
// serve the file: a request for index.php comes back as a download of the
// source, including whatever credentials are at the top of it.
//
// That is worse than refusing, and it is not the kind of failure an operator
// notices from the panel, so the switch is blocked while any site runs PHP.
// Static sites are unaffected and switch normally.
func canServePHP(backend string, sites []webserver.Site) error {
	if backend != webserver.BackendLSWS {
		return nil
	}
	var php []string
	for _, s := range sites {
		if s.NeedsPHP() {
			php = append(php, s.Domain)
		}
	}
	if len(php) == 0 {
		return nil
	}
	return &agent.DeniedError{Reason: fmt.Sprintf(
		"LiteSpeed Enterprise reads this panel's Apache configuration but not its PHP-FPM "+
			"handler, so %d site(s) including %s would have their PHP source served instead "+
			"of run. Switching is blocked until the panel can give LiteSpeed an interpreter "+
			"of its own", len(php), php[0])}
}

// portsOf is what the new server has to be able to bind.
func portsOf(cfg webserver.ServerConfig) []int {
	ports := []int{cfg.HTTPPort, cfg.HTTPSPort}
	out := ports[:0]
	for _, p := range ports {
		if p > 0 {
			out = append(out, p)
		}
	}
	return out
}

// waitForPorts blocks until nothing is listening on the given ports.
//
// Binding and immediately closing is the only reliable test: reading
// /proc/net/tcp says who has a socket open but not whether the kernel will
// let the next process have it, which is the actual question.
func waitForPorts(ctx context.Context, ports []int, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for _, port := range ports {
		for {
			ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
			if err == nil {
				_ = ln.Close()
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("port %d was still held %s after the previous server stopped; "+
					"nothing was switched", port, budget)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	return nil
}

func stopAndDisable(ctx context.Context, unit string) error {
	if st, err := svc.Get(ctx, unit); err == nil && st.Enabled == "not-found" {
		return nil
	}
	if err := svc.Stop(ctx, unit); err != nil {
		return fmt.Errorf("stop %s: %w", unit, err)
	}
	// Disabled as well as stopped: a unit left enabled comes back at the next
	// reboot and fights the new server for port 80, and the symptom of that
	// is a host that works until it is restarted.
	if err := svc.Disable(ctx, unit, false); err != nil {
		return fmt.Errorf("disable %s: %w", unit, err)
	}
	return nil
}

// pmaPool is the interpreter phpMyAdmin runs in.
//
// It runs as the panel's own account rather than any customer's: phpMyAdmin
// is the panel's software, serving whoever the panel has already
// authenticated, and giving it a customer's identity would let one customer's
// site read its configuration.
func pmaPool(cfg webserver.ServerConfig) phpfpm.Pool {
	return phpfpm.Pool{
		Name:        pmaPoolName,
		Version:     PMAPHPVersion,
		User:        "opanel",
		Group:       "opanel",
		Socket:      cfg.PMAFPMSocket,
		MaxChildren: 5,
		LogDir:      "/var/log/opanel",
		SocketOwner: cfg.ServerUser,
	}
}

// serverAccount is the account the webserver's own workers run as.
//
// Decided here rather than sent by the panel, because it is a property of the
// software on the host: Apache's packages create and use "apache", and
// LiteSpeed Enterprise adopts the same account when it takes over Apache's
// configuration. The panel has no way to know either and no business
// choosing.
func serverAccount(backend string, cfg webserver.ServerConfig) (string, string) {
	switch backend {
	case webserver.BackendApache, webserver.BackendLSWS:
		return "apache", "apache"
	default:
		return cfg.ServerUser, cfg.ServerGroup
	}
}

// hostMemoryMB reads total memory, for sizing pools.
//
// Zero when it cannot be read, which the caller treats as "small": the
// failure mode of guessing low is a queue under load, and of guessing high is
// a host that swaps itself to death.
func hostMemoryMB() int {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0
		}
		return kb / 1024
	}
	return 0
}
