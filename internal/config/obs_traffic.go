package config

import (
	"fmt"
	"slices"
	"strings"
)

// MaxTrafficWorkers bounds worker_traffic_log_workers.
const MaxTrafficWorkers = 256

// TrafficConfig is the resolved Worker traffic log.
type TrafficConfig struct {
	Enabled bool
	// To lists the destinations: local, syslog, otel.
	To []string
	// Workers narrows the Hub's log to these Worker ids or names; empty: all.
	Workers []string
}

// ResolveTraffic resolves the traffic log settings of res's role. A
// destination whose sink is off in sinks is refused, naming both keys.
func ResolveTraffic(res *Resolver, sinks Sinks) (TrafficConfig, error) {
	var c TrafficConfig
	lvl, err := res.Resolve(KeyTrafficLog)
	if err != nil {
		return c, err
	}
	if lvl.Value != TrafficMetadata {
		return c, nil
	}
	c.Enabled = true
	to, err := res.Resolve(KeyTrafficLogTo)
	if err != nil {
		return c, err
	}
	for _, d := range strings.Split(to.Value, ",") {
		d = strings.TrimSpace(d)
		if d == "" || slices.Contains(c.To, d) {
			continue
		}
		switch d {
		case TrafficToLocal:
		case TrafficToSyslog:
			if !sinks.Syslog.Enabled {
				return c, fmt.Errorf("setting %q names syslog, but %q is off", KeyTrafficLogTo, KeySyslog)
			}
		case TrafficToOTel:
			if !sinks.OTLP.Enabled {
				return c, fmt.Errorf("setting %q names otel, but %q is off", KeyTrafficLogTo, KeyOTel)
			}
		default:
			return c, fmt.Errorf("setting %q: expected a comma-separated list of local, syslog and otel", KeyTrafficLogTo)
		}
		c.To = append(c.To, d)
	}
	if len(c.To) == 0 {
		return c, fmt.Errorf("setting %q names no destination", KeyTrafficLogTo)
	}
	if res.Role != ScopeHub {
		return c, nil
	}
	ws, err := res.Resolve(KeyTrafficLogWorkers)
	if err != nil {
		return c, err
	}
	for _, w := range strings.Split(ws.Value, ",") {
		if w = strings.TrimSpace(w); w != "" && !slices.Contains(c.Workers, w) {
			c.Workers = append(c.Workers, w)
		}
	}
	if len(c.Workers) > MaxTrafficWorkers {
		return c, fmt.Errorf("setting %q lists more than %d Workers", KeyTrafficLogWorkers, MaxTrafficWorkers)
	}
	return c, nil
}

// ResolveMetricsLevel resolves otel_metrics (Hub): off, basic or detailed. A
// level other than off needs the OpenTelemetry sink on, whose endpoint,
// headers and trust the metrics use.
func ResolveMetricsLevel(res *Resolver, sinks Sinks) (string, error) {
	r, err := res.Resolve(KeyOTelMetrics)
	if err != nil {
		return "", err
	}
	if r.Value != "off" && !sinks.OTLP.Enabled {
		return "", fmt.Errorf("setting %q needs %q on: the metrics use its endpoint, headers and trust", KeyOTelMetrics, KeyOTel)
	}
	if r.Value != "off" && sinks.OTLP.MetricsURL == "" {
		return "", fmt.Errorf("setting %q: the OpenTelemetry endpoint has no metrics URL", KeyOTelMetrics)
	}
	return r.Value, nil
}
