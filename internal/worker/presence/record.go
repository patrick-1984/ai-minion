package presence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// Schema is the value of the schema field of a presence record.
	Schema = "minion.presence/1"
	// UsageSchema is the schema of the usage-meters document stored beside it.
	UsageSchema = "minion.usage-meters/1"
	// IndexSchema is the schema of a discovery entry of a relocated profile.
	IndexSchema = "minion.presence-index/1"

	// DefaultInterval is the heartbeat period.
	DefaultInterval = 30 * time.Second
	// DefaultStaleAfterS is stale_after_s of a record.
	DefaultStaleAfterS = 90
	// MaxRecordSize bounds the read of a presence record.
	MaxRecordSize = 4 << 10
	// MaxUsageSize bounds a usage-meters document.
	MaxUsageSize = 32 << 10

	// StateRunning and StateStopped are the values of Record.State.
	StateRunning = "running"
	StateStopped = "stopped"
	// ScopeUser and ScopeRoot are the values of Record.OSScope.
	ScopeUser = "user"
	ScopeRoot = "root"
)

// Record is one minion.presence/1 document. It is public by construction: it
// holds no owner, Worker name, key, path, session data or error.
type Record struct {
	Schema        string `json:"schema"`
	Instance      string `json:"instance"`
	DaemonVersion string `json:"daemon_version"`
	StartedAt     int64  `json:"started_at"`
	UpdatedAt     int64  `json:"updated_at"`
	StaleAfterS   int    `json:"stale_after_s"`
	State         string `json:"state"`
	OSScope       string `json:"os_scope"`
	PID           int    `json:"pid,omitempty"`
	HubOrigin     string `json:"hub_origin,omitempty"`
}

var (
	instanceRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	originRE   = regexp.MustCompile(`^https?://(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?)(:[0-9]{1,5})?$`)
)

// ValidInstance reports whether id may name a record file: letters, digits,
// underscore and hyphen only (no dot, so <id>.json and <id>.usage.json cannot
// collide, and no path separator).
func ValidInstance(id string) bool { return instanceRE.MatchString(id) }

// Validate checks r against the rules of the schema.
func (r Record) Validate() error {
	switch {
	case r.Schema != Schema:
		return fmt.Errorf("schema is %q, want %q", r.Schema, Schema)
	case !ValidInstance(r.Instance):
		return errors.New("invalid instance")
	case r.DaemonVersion == "" || len(r.DaemonVersion) > 64:
		return errors.New("invalid daemon_version")
	case r.StartedAt < 0 || r.UpdatedAt < 0:
		return errors.New("negative time")
	case r.StaleAfterS < 1 || r.StaleAfterS > 3600:
		return errors.New("invalid stale_after_s")
	case r.State != StateRunning && r.State != StateStopped:
		return errors.New("invalid state")
	case r.OSScope != ScopeUser && r.OSScope != ScopeRoot:
		return errors.New("invalid os_scope")
	case r.PID < 0:
		return errors.New("invalid pid")
	case r.HubOrigin != "" && (len(r.HubOrigin) > 300 || !originRE.MatchString(r.HubOrigin)):
		return errors.New("invalid hub_origin")
	}
	return nil
}

// DecodeRecord parses and validates one record. Unknown fields are refused,
// like the schema does (additionalProperties false).
func DecodeRecord(b []byte) (Record, error) {
	var r Record
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Record{}, fmt.Errorf("not a presence record: %w", err)
	}
	if dec.More() {
		return Record{}, errors.New("not a presence record: trailing data")
	}
	if err := r.Validate(); err != nil {
		return Record{}, fmt.Errorf("not a valid presence record: %w", err)
	}
	return r, nil
}

// SanitizeHubOrigin reduces a configured Hub URL to scheme://host[:port]: user
// info, path, query and fragment are dropped (a proxy token may live there),
// ws and wss become http and https. An unusable value yields "" (the field is
// then left out).
func SanitizeHubOrigin(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "ws":
		scheme = "http"
	case "wss":
		scheme = "https"
	case "http", "https":
	default:
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	out := scheme + "://" + host
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return ""
		}
		out += ":" + p
	}
	if len(out) > 300 || !originRE.MatchString(out) {
		return ""
	}
	return out
}

func ms(t time.Time) int64 { return t.UnixMilli() }

// checkUsage verifies the minimal envelope of a usage-meters document: a JSON
// object of at most [MaxUsageSize] bytes whose schema is minion.usage-meters/1.
// The full schema check belongs to the code that verified the document.
func checkUsage(raw []byte) error {
	if len(raw) == 0 || len(raw) > MaxUsageSize {
		return fmt.Errorf("a usage document must be 1 to %d bytes", MaxUsageSize)
	}
	var head struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return fmt.Errorf("the usage document is not a JSON object: %w", err)
	}
	if head.Schema != UsageSchema {
		return fmt.Errorf("the usage document schema is %q, want %q", head.Schema, UsageSchema)
	}
	return nil
}
