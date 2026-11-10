// Package winprobe tests scripts/windows-service-restart-probe.sh, the probe
// of the Windows job worker-service in .github/workflows/windows.yml that kills
// the service process and waits for the service control manager to run a new
// instance. The script runs here on Linux against fake sc.exe, taskkill and
// minion commands, so its decisions (large output, reused process ids,
// instances that change while the status is read, failure diagnostics) are
// checked without a Windows runner. It contains only tests; they need bash and
// skip without it.
package winprobe
