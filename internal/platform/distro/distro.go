// Package distro identifies the host operating system.
package distro

import (
	"bufio"
	"context"
	"os"
	"strings"
	"sync"

	"github.com/bnixvn/opanel-ent/internal/platform/run"
)

// Info describes the running system.
type Info struct {
	ID        string `json:"id"`         // "almalinux"
	VersionID string `json:"version_id"` // "10.1"
	Major     int    `json:"major"`      // 10
	Pretty    string `json:"pretty"`

	// CloudLinux is true when the CloudLinux subsystem is installed.
	//
	// This must be detected with cldetect, never from /etc/os-release: the
	// CloudLinux 10 conversion deliberately leaves the OS identification
	// files reporting AlmaLinux, so os-release cannot answer the question.
	CloudLinux bool   `json:"cloudlinux"`
	CLEdition  string `json:"cl_edition,omitempty"`
}

// Supported reports whether the panel will run here.
func (i Info) Supported() bool {
	switch i.ID {
	case "almalinux", "rocky", "rhel", "centos", "cloudlinux":
		return i.Major >= 10
	}
	return false
}

const cldetectPath = "/usr/bin/cldetect"

var (
	once   sync.Once
	cached Info
)

// Detect inspects the host. The result is cached: neither the distribution
// nor the presence of CloudLinux changes without a restart.
func Detect(ctx context.Context) Info {
	once.Do(func() { cached = detect(ctx) })
	return cached
}

func detect(ctx context.Context) Info {
	info := parseOSRelease("/etc/os-release")
	if _, err := os.Stat(cldetectPath); err == nil {
		info.CloudLinux = true
		if res, err := run.Cmd(ctx, []string{cldetectPath, "--detect-edition"}); err == nil {
			info.CLEdition = strings.TrimSpace(res.Output())
		}
	}
	return info
}

func parseOSRelease(path string) Info {
	var info Info
	f, err := os.Open(path)
	if err != nil {
		return info
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"`)
		switch strings.TrimSpace(key) {
		case "ID":
			info.ID = val
		case "VERSION_ID":
			info.VersionID = val
		case "PRETTY_NAME":
			info.Pretty = val
		}
	}
	// VERSION_ID is "10.1"; only the major version gates behaviour.
	if major, _, _ := strings.Cut(info.VersionID, "."); major != "" {
		for _, r := range major {
			if r < '0' || r > '9' {
				return info
			}
		}
		n := 0
		for _, r := range major {
			n = n*10 + int(r-'0')
		}
		info.Major = n
	}
	return info
}
