package cloudlinux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Where CloudLinux looks for the integration, and where the panel puts the
// programs it names. Both paths are the vendor's convention rather than ours:
// /opt/cpvendor is the directory their components read on a panel they do not
// otherwise know.
const (
	ConfigDir    = "/opt/cpvendor/etc"
	ConfigPath   = ConfigDir + "/integration.ini"
	ScriptDir    = "/opt/cpvendor/bin"
	scriptRunner = "/usr/local/bin/opanelctl"
)

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// Detected reports whether this host is running CloudLinux at all.
//
// cldetect, not /etc/os-release. A converted host keeps saying AlmaLinux
// there -- CloudLinux 10 runs as a subsystem rather than as its own
// distribution -- so the only honest answer comes from their own tool being
// present.
func Detected() bool { return fileExists("/usr/bin/cldetect") }

// Installed reports whether the integration file is in place.
func Installed() bool {
	st, err := os.Stat(ConfigPath)
	return err == nil && !st.IsDir()
}

// Install writes the integration file and the wrappers it names.
//
// One wrapper per script rather than pointing the ini straight at opanelctl
// with an argument. The ini does allow an argument, but CloudLinux's
// components also have to be able to call some of these from inside CageFS,
// where the path they invoke is the thing that has to exist and be allowed. A
// small file per script is the shape that survives that; it costs eight lines
// of shell.
func Install() error {
	if err := os.MkdirAll(ConfigDir, 0o755); err != nil {
		return fmt.Errorf("cloudlinux: create %s: %w", ConfigDir, err)
	}
	if err := os.MkdirAll(ScriptDir, 0o755); err != nil {
		return fmt.Errorf("cloudlinux: create %s: %w", ScriptDir, err)
	}

	for _, name := range Scripts {
		body := fmt.Sprintf("#!/bin/sh\n"+
			"# Written by OPanel. Answers CloudLinux's %s query.\n"+
			"exec %s cloudlinux cpapi %s \"$@\"\n", name, scriptRunner, name)
		path := filepath.Join(ScriptDir, name)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			return fmt.Errorf("cloudlinux: write %s: %w", path, err)
		}
	}

	var b strings.Builder
	b.WriteString("; Written by OPanel. Do not edit: this file is rewritten\n")
	b.WriteString("; whenever the integration is installed.\n")
	b.WriteString(";\n")
	b.WriteString("; CloudLinux only ever reads this, which is why it is world\n")
	b.WriteString("; readable: its components run as the customer as well as as root.\n")
	b.WriteString("\n[integration_scripts]\n")
	for _, name := range Scripts {
		fmt.Fprintf(&b, "%s = %s\n", name, filepath.Join(ScriptDir, name))
	}
	// The manager's own section, when its files are here. CloudLinux reads
	// both from one file, and the installer that copies the manager is the
	// thing that reads base_path out of it -- so the section has to exist
	// before that runs, not after.
	if ManagerInstalled() || fileExists(pluginInstall) {
		b.WriteString(managerSection())
	}
	if err := os.WriteFile(ConfigPath, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("cloudlinux: write %s: %w", ConfigPath, err)
	}
	return nil
}
