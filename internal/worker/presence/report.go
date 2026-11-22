package presence

import (
	"encoding/json"
	"time"
)

// Found is one discovered Worker instance as `minion worker discover --json`
// prints it. Everything except Usage comes from the verified record; an
// unavailable result carries only Source and Error.
type Found struct {
	Instance      string          `json:"instance,omitempty"`
	Status        Status          `json:"status"`
	Source        Source          `json:"source"`
	DaemonVersion string          `json:"daemon_version,omitempty"`
	OSScope       string          `json:"os_scope,omitempty"`
	StartedAt     string          `json:"started_at,omitempty"`
	UpdatedAt     string          `json:"updated_at,omitempty"`
	StartedAgeS   *int64          `json:"started_age_s,omitempty"`
	UpdatedAgeS   *int64          `json:"updated_age_s,omitempty"`
	StaleAfterS   int             `json:"stale_after_s,omitempty"`
	PID           int             `json:"pid,omitempty"`
	HubOrigin     string          `json:"hub_origin,omitempty"`
	HubLink       string          `json:"hub_link,omitempty"`
	Error         string          `json:"error,omitempty"`
	Usage         json.RawMessage `json:"usage,omitempty"`
}

const foundTime = "2006-01-02T15:04:05.000Z"

// Discover scans every directory the reader knows and describes each result.
// With withUsage the stored usage-meters document of a readable record is
// attached (read through the same verified handle checks).
func (r Reader) Discover(withUsage bool) []Found {
	results := r.Scan()
	out := make([]Found, 0, len(results))
	for _, res := range results {
		out = append(out, r.describe(res, withUsage))
	}
	return out
}

func (r Reader) describe(res Result, withUsage bool) Found {
	f := Found{Instance: res.Candidate.Instance, Status: res.Status, Source: res.Candidate.Source}
	if res.Record == nil {
		if res.Err != nil {
			f.Error = res.Err.Error()
		}
		return f
	}
	rec, now := res.Record, r.now()
	f.DaemonVersion, f.OSScope, f.StaleAfterS, f.PID, f.HubOrigin = rec.DaemonVersion, rec.OSScope, rec.StaleAfterS, rec.PID, rec.HubOrigin
	f.HubLink = rec.HubLink
	f.StartedAt = time.UnixMilli(rec.StartedAt).UTC().Format(foundTime)
	f.UpdatedAt = time.UnixMilli(rec.UpdatedAt).UTC().Format(foundTime)
	up := int64(res.Age / time.Second)
	f.UpdatedAgeS = &up
	// A stopped marker or a start in the future has no meaningful uptime.
	if started := now.Sub(time.UnixMilli(rec.StartedAt)); started >= 0 {
		s := int64(started / time.Second)
		f.StartedAgeS = &s
	}
	if withUsage && res.Status != StatusUnavailable {
		if raw, err := r.ReadUsage(res.Candidate); err == nil {
			f.Usage = json.RawMessage(raw)
		}
	}
	return f
}
