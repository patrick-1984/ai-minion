package truenas

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/version"
)

// Config is the root-owned configuration of the executor.
type Config struct {
	// App is the TrueNAS app name; Service the compose service of the Hub.
	App     string `json:"app"`
	Service string `json:"service"`
	// Dataset is the ZFS dataset (pool/path, not a mount path) that holds
	// exactly the Hub data. It is snapshotted and rolled back as a whole.
	Dataset string `json:"dataset"`
	// ReadinessURL is the Hub's /readyz URL as seen from the host.
	ReadinessURL string `json:"readiness_url"`
	// Window is the UTC window "HH-HH" in which unattended attempts start.
	Window string `json:"window"`
	// Channel is "latest", "X" or "X.Y".
	Channel string `json:"channel"`
	// Enabled allows unattended attempts inside the window. Reconciling an
	// unfinished attempt and one-shot requests do not depend on it.
	Enabled bool `json:"enabled"`
	// AllowHubPolicy lets the policy in the request file (written by the Hub)
	// replace Enabled, Channel and Window. Off by default.
	AllowHubPolicy bool `json:"allow_hub_policy"`
	// StateDir holds the journal and the Sigstore cache (root-owned).
	StateDir string `json:"state_dir"`
	// RequestFile is read (bounded, no links) and may be written by the Hub.
	RequestFile string `json:"request_file"`
	// StatusFile is written atomically; its directory is read by the Hub.
	StatusFile string `json:"status_file"`
	// MinFreeBytes is the free space the dataset must have before an attempt.
	MinFreeBytes uint64 `json:"min_free_bytes"`
}

// DefaultStateDir is where the journal lives when the config names none.
const DefaultStateDir = "/var/lib/minion-hub-updater"

var (
	appRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	serviceRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	datasetRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*(/[A-Za-z0-9][A-Za-z0-9._:-]*)+$`)
)

// maxConfigSize bounds the config file.
const maxConfigSize = 64 << 10

// LoadConfig reads the config file. The file must be a regular file owned by
// ownerUID (0 in production; a negative value skips the owner check in
// tests) and not writable by group or others.
func LoadConfig(path string, ownerUID int) (Config, error) {
	f, err := openNoFollow(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return Config{}, errors.New("config: not a regular file")
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return Config{}, errors.New("config: must not be writable by group or others")
	}
	if err := checkOwner(fi, ownerUID); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfigSize+1))
	if err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	if len(data) > maxConfigSize {
		return Config{}, errors.New("config: too large")
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return c, nil
}

// WithDefaults fills the optional fields.
func (c Config) WithDefaults() Config {
	if c.Service == "" {
		c.Service = "minion-hub"
	}
	if c.Channel == "" {
		c.Channel = "latest"
	}
	if c.Window == "" {
		c.Window = "03-05"
	}
	if c.StateDir == "" {
		c.StateDir = DefaultStateDir
	}
	if c.MinFreeBytes == 0 {
		c.MinFreeBytes = 1 << 30
	}
	return c
}

// Validate checks every field that reaches a command line or a path.
func (c Config) Validate() error {
	if !appRE.MatchString(c.App) {
		return errors.New("config: app must be a lowercase TrueNAS app name")
	}
	if !serviceRE.MatchString(c.Service) {
		return errors.New("config: invalid service name")
	}
	if !datasetRE.MatchString(c.Dataset) || strings.Contains(c.Dataset, "..") {
		return errors.New("config: dataset must be a ZFS dataset name like pool/apps/hub-data (not a mount path)")
	}
	if !strings.HasPrefix(c.ReadinessURL, "http://") && !strings.HasPrefix(c.ReadinessURL, "https://") {
		return errors.New("config: readiness_url must be an http(s) URL")
	}
	if _, err := ParseWindow(c.Window); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if _, err := ParseChannel(c.Channel); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if !strings.HasPrefix(c.StateDir, "/") {
		return errors.New("config: state_dir must be an absolute path")
	}
	for _, p := range []string{c.RequestFile, c.StatusFile} {
		if p != "" && !strings.HasPrefix(p, "/") {
			return errors.New("config: request_file and status_file must be absolute paths")
		}
	}
	return nil
}

// ParseChannel accepts "latest", "X" and "X.Y" (never an exact version).
func ParseChannel(s string) (version.Selector, error) {
	sel, err := version.ParseSelector(s)
	if err != nil {
		return version.Selector{}, err
	}
	if !sel.IsChannel() {
		return version.Selector{}, fmt.Errorf("channel %q must be latest, X or X.Y", s)
	}
	return sel, nil
}

// Window is a UTC hour window [Start, End).
type Window struct{ Start, End int }

// ParseWindow parses "HH-HH" (UTC hours, end 24 allowed; 22-02 wraps midnight).
func ParseWindow(s string) (Window, error) {
	var a, b int
	if len(s) != 5 || s[2] != '-' {
		return Window{}, fmt.Errorf("window %q is not HH-HH", s)
	}
	if _, err := fmt.Sscanf(s, "%02d-%02d", &a, &b); err != nil {
		return Window{}, fmt.Errorf("window %q is not HH-HH", s)
	}
	if a < 0 || a > 23 || b < 0 || b > 24 || a == b {
		return Window{}, fmt.Errorf("window %q: hours must be 00-23 (end up to 24) and differ", s)
	}
	return Window{Start: a, End: b}, nil
}

// Contains reports whether t (any zone) lies in the window.
func (w Window) Contains(t time.Time) bool {
	h := t.UTC().Hour()
	if w.Start < w.End {
		return h >= w.Start && h < w.End
	}
	return h >= w.Start || h < w.End
}

func (w Window) String() string { return fmt.Sprintf("%02d-%02d", w.Start, w.End) }
