package config

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
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
	if c.Workers, err = ParseTrafficWorkers(ws.Value); err != nil {
		return c, fmt.Errorf("setting %q: %w", KeyTrafficLogWorkers, err)
	}
	if len(c.Workers) > MaxTrafficWorkers {
		return c, fmt.Errorf("setting %q lists more than %d Workers", KeyTrafficLogWorkers, MaxTrafficWorkers)
	}
	return c, nil
}

// ParseTrafficWorkers reads the worker_traffic_log_workers value: Worker ids
// or names separated by commas; a name that contains a comma is written in
// double quotes ("pi,usb", a quote inside it doubled). Empty means no
// filter; a value that is not empty but names no Worker is an error (it
// must not become "every Worker").
func ParseTrafficWorkers(v string) ([]string, error) {
	if strings.TrimSpace(v) == "" {
		return nil, nil
	}
	r := csv.NewReader(strings.NewReader(v))
	r.TrimLeadingSpace = true
	r.FieldsPerRecord = -1
	fields, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("expected a comma-separated list of Worker ids or names (quote a name that contains a comma): %v", err)
	}
	if _, err := r.Read(); err != io.EOF {
		return nil, errors.New("expected one line: a comma-separated list of Worker ids or names")
	}
	var out []string
	for _, w := range fields {
		if w = strings.TrimSpace(w); w != "" && !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("it names no Worker (leave it empty for all Workers)")
	}
	return out, nil
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

// ResolveCompliance resolves compliance_monitoring (Hub): off, actions, output
// or full. Anything but off needs the OpenTelemetry sink on and its logs
// endpoint to be https or http to a loopback host: session content never
// leaves in clear text over a network. The level of the sink is not checked
// here: the status shows when it filters the events.
func ResolveCompliance(res *Resolver, sinks Sinks) (string, error) {
	r, err := res.Resolve(KeyCompliance)
	if err != nil {
		return "", err
	}
	if r.Value == ComplianceOff {
		return r.Value, nil
	}
	if !sinks.OTLP.Enabled {
		return "", fmt.Errorf("setting %q needs %q on: the session events use its endpoint, headers and trust", KeyCompliance, KeyOTel)
	}
	u, err := url.Parse(sinks.OTLP.LogsURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("setting %q: the OpenTelemetry endpoint is not valid", KeyCompliance)
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !LoopbackHost(u.Hostname())) {
		return "", fmt.Errorf("setting %q needs an https OpenTelemetry endpoint (or http to localhost)", KeyCompliance)
	}
	return r.Value, nil
}

// LoopbackHost reports whether h (a host name or address, any letter case)
// is localhost or a loopback address.
func LoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
