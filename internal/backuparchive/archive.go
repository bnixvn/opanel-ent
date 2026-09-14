// Package backuparchive defines the on-disk shape of an account backup.
//
// Kept in its own package because both sides need it and neither should own
// it: the agent writes archives, the API reads manifests to show what is in
// one, and a future import tool will read archives this process never wrote.
package backuparchive

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Extension is what a backup file is called. Named here so the API, the
// agent and the UI cannot disagree about it.
//
// gzip rather than a denser codec: an operator restoring on a bad day can
// open one of these with tar on any machine in the world, including one that
// is not running this panel.
const Extension = ".tar.gz"

// ManifestName is the archive member holding the manifest. It is the first
// member, so reading it costs one small decompression rather than a walk
// through however many gigabytes follow.
const ManifestName = "manifest.json"

// FilesPrefix and DatabasesPrefix are where the two kinds of content live
// inside the archive.
const (
	FilesPrefix     = "files/"
	DatabasesPrefix = "databases/"
)

// FormatVersion is bumped when the layout changes in a way an older reader
// would get wrong. A reader refuses a version it does not know rather than
// restoring something half-understood over a live account.
const FormatVersion = 1

// Manifest describes what an archive holds.
type Manifest struct {
	Format int `json:"format"`
	// Panel is the version of the panel that wrote the archive, for support
	// rather than for logic.
	Panel     string    `json:"panel"`
	CreatedAt time.Time `json:"created_at"`

	// Owner is the Linux and panel account the archive belongs to. A restore
	// always goes back to an account of this name; the panel refuses to
	// unpack one customer's files into another's home.
	Owner string `json:"owner"`
	// Home is where the files came from, recorded for diagnosis. The restore
	// uses the account's current home, not this.
	Home string `json:"home"`

	Sites     []ManifestSite `json:"sites"`
	Databases []string       `json:"databases"`
}

// The manifest deliberately carries no file counts. They are only known once
// the walk is finished, and keeping them here would mean writing the manifest
// last -- which would make reading one cost a full decompression of the
// archive behind it. The counts live on the backup row instead.

// ManifestSite records enough to rebuild a site row, so an archive restored
// onto a fresh server does not lose which PHP version a site was running.
type ManifestSite struct {
	Domain       string   `json:"domain"`
	Aliases      []string `json:"aliases,omitempty"`
	AppType      string   `json:"app_type"`
	PHPVersion   string   `json:"php_version,omitempty"`
	DocumentRoot string   `json:"document_root"`
	RewriteMode  string   `json:"rewrite_mode,omitempty"`
}

// Validate reports whether a manifest is one this build can act on.
func (m *Manifest) Validate() error {
	if m.Format != FormatVersion {
		return fmt.Errorf("archive format %d, this panel understands %d", m.Format, FormatVersion)
	}
	if m.Owner == "" {
		return fmt.Errorf("archive names no owner")
	}
	return nil
}

// Encode writes a manifest as indented JSON, which is worth the few bytes:
// someone will read one out of a broken archive with less(1).
func (m *Manifest) Encode(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// DecodeManifest reads a manifest.
func DecodeManifest(r io.Reader) (*Manifest, error) {
	var m Manifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	return &m, nil
}
