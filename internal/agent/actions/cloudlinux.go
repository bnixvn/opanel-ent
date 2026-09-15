package actions

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/bnixvn/opanel-ent/internal/agent"
	"github.com/bnixvn/opanel-ent/internal/cloudlinux"
	"github.com/bnixvn/opanel-ent/internal/platform/pkgmgr"
	"github.com/bnixvn/opanel-ent/internal/platform/run"
)

// The CloudLinux tools this panel reads. Each is optional: the subsystem can
// be present with some of its parts not installed, and a page that reports
// "not available" is more use than one that reports an error.
const (
	clDetect = "/usr/bin/cldetect"
	clVectl  = "/usr/sbin/lvectl"
	clVeinfo = "/usr/sbin/lveinfo"
)

// CLStatus is what the panel shows about CloudLinux.
type CLStatus struct {
	// Installed is whether the subsystem is here at all. Everything else is
	// meaningless when it is false.
	Installed bool   `json:"installed"`
	OS        string `json:"os,omitempty"`
	Edition   string `json:"edition,omitempty"`
	// Licence is "ok" when cldetect can verify it, or what it said instead.
	Licence string `json:"licence,omitempty"`
	// LVE is whether the kernel module that enforces the limits is loaded.
	// Without it the limits are recorded and not applied, which is the one
	// state that looks fine and is not.
	LVE bool `json:"lve"`
	// Integration is whether CloudLinux can read this panel.
	Integration bool `json:"integration"`
	// Manager is whether CloudLinux's own interface is installed, and
	// ManagerRunning whether the service that serves it is up. Both, because
	// installed-and-stopped is a real state and looks like neither.
	Manager        bool `json:"manager"`
	ManagerRunning bool `json:"manager_running"`
	// Selector is what PHP Selector currently offers. Part of the status
	// rather than a page of its own: "which PHP versions can a customer
	// choose?" is a fact about this host, and the answer is usually "none
	// yet", which is a thing the page has to be able to say.
	Selector cloudlinux.SelectorStatus `json:"selector"`
	// Tools reports which parts are installed, so the page can say why
	// something is missing rather than leaving a blank.
	Tools map[string]bool `json:"tools"`
}

// CLLimits is one row of lvectl's table. Values are kept as the strings the
// tool prints: "1024M" and "0K" carry their unit, and 0 means unlimited,
// which a number alone would not say.
type CLLimits struct {
	ID    string `json:"id"`
	Speed string `json:"speed"`
	CPU   string `json:"cpu,omitempty"`
	PMem  string `json:"pmem"`
	VMem  string `json:"vmem"`
	EP    string `json:"ep"`
	NProc string `json:"nproc"`
	IO    string `json:"io"`
	IOPS  string `json:"iops"`
}

// CLUsage is one account's resource use over a period.
//
// Averages and peaks come from lveinfo; the faults are the number that
// matters. A fault is a request that hit a limit -- the customer waiting, or
// the page that did not load -- so an account with faults is one whose plan
// is too small or whose code is too slow, and either way somebody should
// look at it.
type CLUsage struct {
	UID      int64  `json:"uid"`
	Username string `json:"username,omitempty"`

	CPUAvg  float64 `json:"cpu_avg"`
	CPUMax  float64 `json:"cpu_max"`
	CPULim  float64 `json:"cpu_limit"`
	MemAvg  int64   `json:"mem_avg_bytes"`
	MemMax  int64   `json:"mem_max_bytes"`
	MemLim  int64   `json:"mem_limit_bytes"`
	EPMax   int64   `json:"ep_max"`
	EPLim   int64   `json:"ep_limit"`
	IOMax   int64   `json:"io_max"`
	ProcMax int64   `json:"proc_max"`

	FaultsEP   int64 `json:"faults_ep"`
	FaultsMem  int64 `json:"faults_mem"`
	FaultsProc int64 `json:"faults_proc"`
}

// CLUsageRequest asks for a period. lveinfo accepts 5m, 4h, 2d, today and
// yesterday; anything else is refused here rather than passed through.
type CLUsageRequest struct {
	Period string `json:"period"`
}

var periodPattern = regexp.MustCompile(`^([0-9]{1,3}[mhd]|today|yesterday)$`)

// Validate checks the period's shape.
func (r *CLUsageRequest) Validate() error {
	if r.Period == "" {
		r.Period = "1d"
		return nil
	}
	if !periodPattern.MatchString(r.Period) {
		return fmt.Errorf("period %q is not one lveinfo understands", r.Period)
	}
	return nil
}

func registerCloudLinux(r *agent.Registry) {
	agent.Register(r, "cl.status", 1, func(ctx context.Context, _ struct{}) (CLStatus, error) {
		return clStatus(ctx), nil
	})

	agent.Register(r, "cl.limits", 1, func(ctx context.Context, _ struct{}) (struct {
		Limits []CLLimits `json:"limits"`
	}, error) {
		limits, err := clLimits(ctx)
		return struct {
			Limits []CLLimits `json:"limits"`
		}{limits}, err
	})

	agent.Register(r, "cl.usage", 1, func(ctx context.Context, in CLUsageRequest) (struct {
		Usage []CLUsage `json:"usage"`
	}, error) {
		usage, err := clUsage(ctx, in.Period)
		return struct {
			Usage []CLUsage `json:"usage"`
		}{usage}, err
	})

	// Slow: the vendor's installer copies its whole interface and can pull
	// packages while doing it.
	agent.RegisterSlow(r, "cl.manager_install", 1, 15*time.Minute,
		func(ctx context.Context, _ struct{}) (CLStatus, error) {
			if !fileExists(clDetect) {
				return CLStatus{}, &agent.DeniedError{Reason: "CloudLinux is not installed on this server"}
			}
			if err := cloudlinux.InstallManager(); err != nil {
				return CLStatus{}, err
			}
			// The webserver serves it, so it is not running yet: the vhost
			// appears on the next apply, which the API does as soon as this
			// returns. Reporting the status from here would say "installed,
			// not running" for a second and make the page offer to install it
			// again.
			return clStatus(ctx), nil
		})

	// By a wide margin the slowest thing the panel does: CageFS's skeleton is
	// a few gigabytes of the system copied into a template, and alt-php is
	// two more of interpreters, and then the skeleton is refreshed around
	// them.
	agent.RegisterSlow(r, "cl.selector_setup", 1, 60*time.Minute,
		func(ctx context.Context, _ struct{}) (cloudlinux.SelectorStatus, error) {
			if !fileExists(clDetect) {
				return cloudlinux.SelectorStatus{}, &agent.DeniedError{
					Reason: "CloudLinux is not installed on this server",
				}
			}
			install := func(ctx context.Context, name string) error {
				return pkgmgr.Install(ctx, name)
			}
			// CageFS first, because the selector is per-account isolation and
			// cannot exist without it. cldeploy does not install it, so on a
			// freshly converted host this is the step that puts it there.
			if err := cloudlinux.InstallCageFS(ctx, install); err != nil {
				return cloudlinux.SelectorStatus{}, err
			}
			err := cloudlinux.SetupSelector(ctx, func(ctx context.Context, group string) error {
				return pkgmgr.InstallGroup(ctx, group)
			})
			if err != nil {
				return cloudlinux.SelectorStatus{}, err
			}
			return cloudlinux.SelectorState(ctx), nil
		})

	agent.Register(r, "cl.install_integration", 1, func(ctx context.Context, _ struct{}) (CLStatus, error) {
		if !fileExists(clDetect) {
			return CLStatus{}, &agent.DeniedError{Reason: "CloudLinux is not installed on this server"}
		}
		if err := cloudlinux.Install(); err != nil {
			return CLStatus{}, err
		}
		return clStatus(ctx), nil
	})
}

func clStatus(ctx context.Context) CLStatus {
	out := CLStatus{
		Tools: map[string]bool{
			"cldetect":    fileExists(clDetect),
			"lvectl":      fileExists(clVectl),
			"lveinfo":     fileExists(clVeinfo),
			"cagefsctl":   fileExists("/usr/sbin/cagefsctl"),
			"selectorctl": fileExists("/usr/bin/selectorctl"),
		},
		Integration: cloudlinux.Installed(),
		Manager:     cloudlinux.ManagerInstalled(),
	}
	out.ManagerRunning = out.Manager && cloudlinux.ManagerRunning()
	if !out.Tools["cldetect"] {
		return out
	}
	out.Selector = cloudlinux.SelectorState(ctx)
	out.Installed = true
	out.OS = firstLineOf(ctx, clDetect, "--detect-os")
	out.Edition = firstLineOf(ctx, clDetect, "--detect-edition")
	out.Licence = firstLineOf(ctx, clDetect, "--check-license")

	// The module, not the tool. lvectl is installed by a package; the limits
	// are only real when the kernel is enforcing them, and "recorded but not
	// enforced" is the one state on this page that looks fine and is not.
	out.LVE = lveModuleLoaded()
	return out
}

func firstLineOf(ctx context.Context, argv ...string) string {
	res, err := run.Cmd(ctx, argv, run.Timeout(20*time.Second))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(res.Output(), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return ""
}

func clLimits(ctx context.Context) ([]CLLimits, error) {
	if !fileExists(clVectl) {
		return nil, nil
	}
	res, err := run.Cmd(ctx, []string{clVectl, "list", "--json"}, run.Timeout(30*time.Second))
	if err != nil {
		return nil, fmt.Errorf("read LVE limits: %w", err)
	}
	var doc struct {
		Data []map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &doc); err != nil {
		return nil, fmt.Errorf("read LVE limits: %w", err)
	}
	out := make([]CLLimits, 0, len(doc.Data))
	for _, row := range doc.Data {
		out = append(out, CLLimits{
			ID: row["ID"], Speed: row["SPEED"], CPU: row["CPU"], PMem: row["PMEM"],
			VMem: row["VMEM"], EP: row["EP"], NProc: row["NPROC"],
			IO: row["IO"], IOPS: row["IOPS"],
		})
	}
	return out, nil
}

func clUsage(ctx context.Context, period string) ([]CLUsage, error) {
	if !fileExists(clVeinfo) {
		return nil, nil
	}
	res, err := run.Cmd(ctx, []string{clVeinfo, "--json", "--period", period},
		run.Timeout(60*time.Second))
	if err != nil {
		return nil, fmt.Errorf("read LVE usage: %w", err)
	}
	var doc struct {
		Data []struct {
			ID     int64   `json:"ID"`
			ACPU   float64 `json:"aCPU"`
			MCPU   float64 `json:"mCPU"`
			LCPU   float64 `json:"lCPU"`
			APMem  int64   `json:"aPMem"`
			MPMem  int64   `json:"mPMem"`
			LPMem  int64   `json:"lPMem"`
			MEP    int64   `json:"mEP"`
			LEP    int64   `json:"lEP"`
			MIO    int64   `json:"mIO"`
			MNproc int64   `json:"mNproc"`
			EPf    int64   `json:"EPf"`
			PMemF  int64   `json:"PMemF"`
			NprocF int64   `json:"NprocF"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &doc); err != nil {
		return nil, fmt.Errorf("read LVE usage: %w", err)
	}
	out := make([]CLUsage, 0, len(doc.Data))
	for _, r := range doc.Data {
		out = append(out, CLUsage{
			UID:    r.ID,
			CPUAvg: r.ACPU, CPUMax: r.MCPU, CPULim: r.LCPU,
			MemAvg: r.APMem, MemMax: r.MPMem, MemLim: r.LPMem,
			EPMax: r.MEP, EPLim: r.LEP, IOMax: r.MIO, ProcMax: r.MNproc,
			FaultsEP: r.EPf, FaultsMem: r.PMemF, FaultsProc: r.NprocF,
		})
	}
	return out, nil
}

// lveModuleNames are what the kernel calls CloudLinux's LVE module.
//
// Two of them, because the name changed: CloudLinux 7 and 8 load "lve" and
// CloudLinux 10 loads "kmodlve". Measured on a converted host -- lsmod says
// kmodlve, and dmesg says "lve driver register status 0" under that name.
var lveModuleNames = []string{"lve", "kmodlve"}

// lveModuleLoaded reports whether the kernel is enforcing LVE limits.
//
// Field-by-field rather than a substring search of the whole file. The
// original here asked whether /proc/modules contained "lve ", which was true
// on this host only because "kmodlve " ends with it -- the right answer by
// accident, and one that would equally have been given by a module called
// "solve" or "valve". Anchoring to the start of a line instead, which is the
// obvious correction, would have been wrong the other way: it reports no LVE
// on every CloudLinux 10 host.
func lveModuleLoaded() bool { return lveLoadedIn("/proc/modules") }

// lveLoadedIn is lveModuleLoaded with the path as an argument, so the parsing
// can be tested against the contents of a real host's /proc/modules.
func lveLoadedIn(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		name, _, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		for _, want := range lveModuleNames {
			if name == want {
				return true
			}
		}
	}
	return false
}
