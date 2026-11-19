package compliance

import (
	"fmt"
	"strconv"
	"strings"
)

// Levels of compliance_monitoring.
const (
	LevelOff     = "off"
	LevelActions = "actions"
	LevelOutput  = "output"
)

// Policy is the monitoring policy in force. Version increases by one with
// every effective change (level, input, destination, traffic payload).
type Policy struct {
	Version int64
	// Level is off, actions or output.
	Level string
	// Input says that the content accepted for sending is recorded too.
	Input bool
	// TrafficPayload says that the Worker traffic log carries frame bodies.
	TrafficPayload bool
	// Destination is scheme://host:port of the collector; "" when off.
	Destination string
}

// Active reports whether anything that holds session content or actions is
// recorded or logged: the compliance records or the traffic payload.
func (p Policy) Active() bool { return p.Records() || p.TrafficPayload }

// Records reports whether the compliance records are written.
func (p Policy) Records() bool { return p.Level == LevelActions || p.Level == LevelOutput }

// Captures reports whether the policy records the output content.
func (p Policy) captureOutput() bool { return p.Level == LevelOutput }

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func traffic(b bool) string {
	if b {
		return "payload"
	}
	return "metadata"
}

// Header is the value of the Minion-Monitoring response header.
func (p Policy) Header() string {
	lvl := p.Level
	if lvl == "" {
		lvl = LevelOff
	}
	s := fmt.Sprintf("v=%d; level=%s; input=%s", p.Version, lvl, onOff(p.Input))
	if p.TrafficPayload {
		s += "; traffic=payload"
	}
	return s
}

// Notice is the text users see while monitoring is on; "" when it is not.
func (p Policy) Notice() string {
	if !p.Active() {
		return ""
	}
	var parts []string
	if p.Records() {
		parts = append(parts, p.Level+", input "+onOff(p.Input))
	}
	if p.TrafficPayload {
		parts = append(parts, "Worker traffic with frame contents")
	}
	return "Sessions are monitored: " + strings.Join(parts, "; ") + " (policy v" + strconv.FormatInt(p.Version, 10) + ")"
}

// AuditTarget is the target of the activation rows of the audit log:
// version, level, input, traffic and the destination label, at most 256
// characters, no credential and no content.
func (p Policy) AuditTarget() string {
	lvl := p.Level
	if lvl == "" {
		lvl = LevelOff
	}
	s := fmt.Sprintf("v=%d level=%s input=%s traffic=%s dest=%s", p.Version, lvl, onOff(p.Input), traffic(p.TrafficPayload), p.Destination)
	if len(s) > 256 {
		s = s[:256]
	}
	return s
}

// Same reports whether q has the effective fields of p (the version
// excluded).
func (p Policy) Same(q Policy) bool {
	return p.Level == q.Level && p.Input == q.Input && p.TrafficPayload == q.TrafficPayload && p.Destination == q.Destination
}

// StricterThan reports whether p records a class of content that seen does
// not: a higher level, input where seen had none, traffic payload where seen
// had none. A looser policy is never stricter.
func (p Policy) StricterThan(seen Policy) bool {
	rank := func(l string) int {
		switch l {
		case LevelActions:
			return 1
		case LevelOutput:
			return 2
		}
		return 0
	}
	return rank(p.Level) > rank(seen.Level) || (p.Input && !seen.Input) || (p.TrafficPayload && !seen.TrafficPayload)
}
