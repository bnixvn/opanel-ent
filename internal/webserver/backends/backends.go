// Package backends builds a webserver backend by name and records which one
// the host is running.
//
// It exists so that nothing else has to know which backends there are. The
// agent asks for the active one on every action rather than choosing at
// startup, which is what lets a switch take effect without restarting the
// agent -- and what makes the switch itself a small, ordinary operation
// rather than a special case.
package backends

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bnixvn/opanel-ent/internal/webserver"
	"github.com/bnixvn/opanel-ent/internal/webserver/apache"
	"github.com/bnixvn/opanel-ent/internal/webserver/ols"
)

// StateFile records the active backend.
//
// A file rather than a row in the panel database: the agent is the side that
// has to know, and it deliberately has no database access. It is written by
// the switch action, which is root, and read on every render.
const StateFile = "/var/lib/opanel/webserver/active"

// Default is what a host runs when nothing has said otherwise.
const Default = webserver.BackendApache

// New builds the named backend.
func New(name string) (webserver.Backend, error) {
	switch name {
	case webserver.BackendApache:
		return apache.New()
	case webserver.BackendLSWS:
		return apache.NewLiteSpeed()
	case webserver.BackendOLS:
		return ols.New()
	default:
		return nil, fmt.Errorf("webserver backend %q is not one of %v", name, webserver.Backends)
	}
}

// Active returns the backend the host is set to run.
//
// A missing or unreadable state file is not an error -- the agent has to be
// able to render a configuration on a host where somebody deleted it -- but
// it is not simply the default either. A host installed before this file
// existed is running OpenLiteSpeed, and answering "apache" there would make
// the panel write Apache configuration for a host serving with something
// else. So the fallback looks at what is actually on disk.
func Active() string {
	data, err := os.ReadFile(StateFile)
	if err == nil {
		name := strings.TrimSpace(string(data))
		if slices.Contains(webserver.Backends, name) {
			return name
		}
	}
	return infer()
}

// infer guesses the backend of a host that never recorded one.
//
// Only reached on an upgrade from a build that predates the state file, where
// the answer is always OpenLiteSpeed: it was the only backend there was.
func infer() string {
	if b, err := ols.New(); err == nil && b.Installed() {
		return webserver.BackendOLS
	}
	return Default
}

// SetActive records the backend the host is now running.
func SetActive(name string) error {
	if !slices.Contains(webserver.Backends, name) {
		return fmt.Errorf("webserver backend %q is not one of %v", name, webserver.Backends)
	}
	if err := os.MkdirAll(filepath.Dir(StateFile), 0o750); err != nil {
		return err
	}
	return os.WriteFile(StateFile, []byte(name+"\n"), 0o640)
}

// ActiveBackend builds the backend the host is set to run.
func ActiveBackend() (webserver.Backend, error) {
	return New(Active())
}

// Info describes one backend for the panel to show.
type Info struct {
	Name string `json:"name"`
	// Unit is the systemd unit that runs it.
	Unit string `json:"unit"`
	// Installed reports whether the software is on the host. A backend that
	// is not installed can still be selected in the interface -- that is what
	// the install action is for -- but not switched to.
	Installed bool `json:"installed"`
	Active    bool `json:"active"`
	// Label is what a person should see.
	Label string `json:"label"`
	// Note explains the trade-off in one line.
	Note string `json:"note"`
}

var labels = map[string][2]string{
	webserver.BackendApache: {
		"Apache",
		"The default. PHP-FPM per site, .htaccess, and the configuration LiteSpeed Enterprise reads.",
	},
	webserver.BackendLSWS: {
		"LiteSpeed Enterprise",
		"Reads the same configuration as Apache and serves it faster. Needs a licence. " +
			"Cannot yet run this panel's PHP pools, so hosts with PHP sites cannot switch to it.",
	},
	webserver.BackendOLS: {
		"OpenLiteSpeed",
		"Free, fast, and a configuration format of its own. Switching to it re-renders every site.",
	},
}

// List describes every backend, in the order to offer them.
func List() []Info {
	active := Active()
	out := make([]Info, 0, len(webserver.Backends))
	for _, name := range webserver.Backends {
		info := Info{Name: name, Active: name == active}
		if l, ok := labels[name]; ok {
			info.Label, info.Note = l[0], l[1]
		}
		if b, err := New(name); err == nil {
			info.Unit = b.ServiceUnit()
			info.Installed = b.Installed()
		}
		out = append(out, info)
	}
	return out
}

// ErrNotInstalled is returned when a switch names a backend that is not on
// the host.
var ErrNotInstalled = errors.New("that webserver is not installed on this host")
