package actions

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/phpfpm"
	"github.com/bnixvn/opanel-ent/internal/phpmgr"
	"github.com/bnixvn/opanel-ent/internal/webserver"
)

// PHPProviderRequest moves every site to a different source of PHP.
//
// It carries the whole estate for the same reason the webserver switch does:
// changing provider changes the socket every vhost proxies to, so the vhosts
// have to be rewritten in the same operation. Splitting it would leave every
// PHP site pointing at a socket nothing is listening on.
type PHPProviderRequest struct {
	Provider string `json:"provider"`
	WebserverApplyRequest
}

// Validate checks the name and the configuration it carries.
func (r *PHPProviderRequest) Validate() error {
	if !slices.Contains(phpmgr.Providers, r.Provider) {
		return fmt.Errorf("php provider %q is not one of %v", r.Provider, phpmgr.Providers)
	}
	return r.WebserverApplyRequest.Validate()
}

// PHPProviderResult reports what happened.
type PHPProviderResult struct {
	Provider string `json:"provider"`
	Previous string `json:"previous"`
	// Migrated is how many sites now run on the new provider.
	Migrated int `json:"migrated"`
}

// PHPProviderBudget bounds a migration. Longer than a webserver switch: this
// one may have to install a version before it can move anything.
const PHPProviderBudget = 30 * time.Minute

func registerPHPProvider(r *agent.Registry, deps Deps) {
	agent.Register(r, "php.provider", 1, func(ctx context.Context, _ struct{}) (PHPProviderStatus, error) {
		return phpProviderStatus(ctx), nil
	})

	agent.RegisterSlow(r, "php.set_provider", 1, PHPProviderBudget,
		func(ctx context.Context, in PHPProviderRequest) (PHPProviderResult, error) {
			return setPHPProvider(ctx, in, deps)
		})
}

// PHPProviderStatus is what the panel shows about where PHP comes from.
type PHPProviderStatus struct {
	Active    string   `json:"active"`
	Providers []string `json:"providers"`
	// AltPHPAvailable is whether CloudLinux's PHP is on this host at all.
	// Without it the migration has nothing to migrate to, and the panel
	// should say so rather than offering a button that fails.
	AltPHPAvailable bool `json:"altphp_available"`
}

func phpProviderStatus(context.Context) PHPProviderStatus {
	return PHPProviderStatus{
		Active:          phpmgr.Active(),
		Providers:       phpmgr.Providers,
		AltPHPAvailable: phpmgr.AltPHPInstalled(),
	}
}

// setPHPProvider moves every site onto a different PHP and leaves the old one
// stopped.
//
// The order is what keeps the host serving. Nothing is torn down until the
// new interpreters are running and the webserver has accepted a configuration
// pointing at them; only then are the old pools removed. A failure before
// that point leaves the host exactly as it was.
func setPHPProvider(ctx context.Context, in PHPProviderRequest, deps Deps) (PHPProviderResult, error) {
	previous := phpmgr.Active()

	target, err := phpmgr.New(in.Provider)
	if err != nil {
		return PHPProviderResult{}, &agent.PayloadError{Err: err}
	}
	fpTarget, ok := target.(phpmgr.FPMProvider)
	if !ok {
		return PHPProviderResult{}, &agent.DeniedError{
			Reason: fmt.Sprintf("provider %q does not run PHP as pools, which is the only shape this panel serves", in.Provider),
		}
	}

	// Every version in use has to exist in the new provider before anything
	// moves. Checked against the filesystem, not against the supported list:
	// alt-php publishes 5.3 to 8.5, but what matters is what is installed
	// here, and a site pointed at a version with no interpreter would render
	// a vhost proxying to a socket nothing creates.
	installed, err := target.Available(ctx)
	if err != nil {
		return PHPProviderResult{}, err
	}
	have := make(map[string]bool, len(installed))
	for _, v := range installed {
		have[v.Version] = v.Installed
	}
	var missing []string
	for _, sp := range in.Sites {
		if sp.PHPVersion == "" || !siteNeedsPHP(sp.AppType) {
			continue
		}
		if !have[sp.PHPVersion] {
			missing = append(missing, sp.Domain+" needs PHP "+sp.PHPVersion)
		}
	}
	if len(missing) > 0 {
		return PHPProviderResult{}, &agent.DeniedError{Reason: fmt.Sprintf(
			"%s does not have %d of the PHP versions this host's sites run, including %s. "+
				"Install them first; nothing has been changed", in.Provider, len(missing), missing[0])}
	}

	// Render against the new provider. This is where the socket paths change.
	sites, err := resolve(target, in.Sites)
	if err != nil {
		return PHPProviderResult{}, &agent.PayloadError{Err: err}
	}
	ws, err := deps.Backend()
	if err != nil {
		return PHPProviderResult{}, err
	}
	if err := canServePHP(ws.Name(), sites); err != nil {
		return PHPProviderResult{}, err
	}

	cfg := in.Config
	cfg.WAFRulesFile = wafRulesIfInstalled()
	cfg.ServerUser, cfg.ServerGroup = serverAccount(ws.Name(), cfg)
	// The panel's own PHP tools move with everything else. One provider on
	// the host, not two: a phpMyAdmin left on Remi would keep a second set of
	// interpreters alive for one application, and the whole point of the
	// migration is that what LiteSpeed can be handed and what Apache runs are
	// the same build.
	panelAppConfig(&cfg, target)

	rendered, err := ws.Render(cfg, sites)
	if err != nil {
		return PHPProviderResult{}, err
	}

	// New pools first, and only then the vhosts that point at them: a vhost
	// proxying to a socket that does not exist yet answers 503 for as long as
	// the gap lasts.
	pools := phpfpm.PoolsFor(fpTarget, sites, cfg.ServerUser, hostMemoryMB())
	pools = append(pools, panelAppPools(cfg)...)
	if err := phpfpm.Apply(ctx, fpTarget, pools); err != nil {
		return PHPProviderResult{}, err
	}
	if err := ws.Apply(ctx, rendered); err != nil {
		return PHPProviderResult{}, fmt.Errorf("the webserver refused a configuration for %s, "+
			"so nothing was moved: %w", in.Provider, err)
	}

	// The host is now serving from the new interpreters. Record it before
	// tearing anything down, so an interruption here leaves a host whose
	// state file matches what is actually running.
	if err := phpmgr.SetActive(in.Provider); err != nil {
		return PHPProviderResult{}, err
	}

	// The old provider's pools, now that nothing points at them. An empty set
	// is what prunes them and stops the units -- empty, not "the panel's own
	// applications", because those moved too: cfg now holds the new
	// provider's socket paths, and writing them into the old provider's pool
	// directory would recreate under the old interpreter a pool listening
	// where the new one already listens.
	//
	// Reported rather than fatal. The host is serving from the new provider
	// by this point, and leftover pool files are untidy, not broken.
	if previous != in.Provider {
		if old, err := phpmgr.New(previous); err == nil {
			if fpOld, ok := old.(phpmgr.FPMProvider); ok {
				if err := phpfpm.Apply(ctx, fpOld, nil); err != nil {
					return PHPProviderResult{
						Provider: in.Provider, Previous: previous, Migrated: countPHP(sites),
					}, fmt.Errorf("sites are running on %s, but clearing the old %s pools failed: %w",
						in.Provider, previous, err)
				}
			}
		}
	}

	return PHPProviderResult{
		Provider: in.Provider,
		Previous: previous,
		Migrated: countPHP(sites),
	}, nil
}

// siteNeedsPHP answers the question webserver.Site.NeedsPHP answers, for a
// spec that has not been resolved into a Site yet.
func siteNeedsPHP(appType string) bool {
	return appType == webserver.AppPHP || appType == webserver.AppWordPress
}

func countPHP(sites []webserver.Site) int {
	n := 0
	for _, s := range sites {
		if s.NeedsPHP() {
			n++
		}
	}
	return n
}
