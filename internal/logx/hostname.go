package logx

import (
	"os"
	"strings"
)

// HostnameEnv names the environment variable that sets the host name of the
// exported logs. In a container the operating system's host name is the
// container id, which makes host-based queries hard; MINION_LOG_HOSTNAME
// gives a stable name (for example minion-hub).
const HostnameEnv = "MINION_LOG_HOSTNAME"

// LogHostname is the host name of exported logs (syslog HOSTNAME, OTEL
// host.name): MINION_LOG_HOSTNAME when it is set and printable, otherwise the
// operating system's host name.
func LogHostname() string {
	if v := strings.TrimSpace(os.Getenv(HostnameEnv)); v != "" && len(v) <= 255 && !strings.ContainsFunc(v, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return v
	}
	h, _ := os.Hostname()
	return h
}
