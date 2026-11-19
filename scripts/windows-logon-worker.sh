#!/usr/bin/env bash
# windows-logon-worker.sh: the Windows Worker as the user (security fix S1),
# run by the job worker-logon of .github/workflows/windows.yml on a Windows
# runner. It checks, in one shell so that the supervisor started by
# "worker up" stays alive throughout:
#   1. migration: a LocalSystem service MinionWorker of an older release (made
#      with sc.exe) is reported by "worker status", ends cleanly when started
#      ("worker service-run" no longer runs the Worker as SYSTEM) and is
#      removed by "worker up" (the runner's account is an administrator);
#   2. install: the Run entry HKCU\...\Run\MinionWorker starts
#      "minion.exe worker launch"; the Worker is live over the user's pipe, in
#      run mode "Windows logon autostart", as the runner's user, not SYSTEM;
#   3. restart: a killed Worker is started again by its supervisor;
#   4. stop: "worker down" stops the Worker and its supervisor; "worker up"
#      starts them again.
# Usage: scripts/windows-logon-worker.sh <path of the user's minion.exe>
set -euo pipefail

minion=$1
minion_win=$(cygpath -w "$minion")
log() { printf '== %s\n' "$*"; }
fail() {
	printf 'FAIL: %s\n' "$*"
	"$minion" worker status --json || true
	sc.exe query MinionWorker || true
	reg.exe query 'HKCU\Software\Microsoft\Windows\CurrentVersion\Run' || true
	cat "$MINION_STATE_DIR/worker-launch.log" 2>/dev/null | tail -50 || true
	powershell -NoProfile -Command "Get-CimInstance Win32_Process -Filter \"Name='minion.exe'\" | Select-Object ProcessId,ParentProcessId,CommandLine | Format-Table -AutoSize | Out-String -Width 300" || true
	exit 1
}

status_json() { "$minion" worker status --json > status.json 2> status.err || true; }
live() { status_json; grep -q '"live": true' status.json; }
wait_live() { # want: true|false
	for _ in $(seq 1 60); do
		if live; then [ "$1" = true ] && return 0; else [ "$1" = false ] && return 0; fi
		sleep 1
	done
	return 1
}
# worker_pid prints the process id of "minion.exe worker run" (the Worker the
# supervisor runs), empty when there is none.
worker_pid() {
	powershell -NoProfile -Command "(Get-CimInstance Win32_Process -Filter \"Name='minion.exe'\" | Where-Object { \$_.CommandLine -match ' worker run' } | Select-Object -First 1).ProcessId" | tr -d '\r'
}
count_launchers() {
	powershell -NoProfile -Command "@(Get-CimInstance Win32_Process -Filter \"Name='minion.exe'\" | Where-Object { \$_.CommandLine -match ' worker launch' }).Count" | tr -d '\r'
}

log "1. a LocalSystem service of an older release"
sc.exe create MinionWorker binPath= "\"$minion_win\" worker service-run" start= demand
status_json
grep -q 'LocalSystem' status.json || fail "worker status does not warn about the LocalSystem service"
# Started, it ends cleanly instead of running the Worker as SYSTEM.
sc.exe start MinionWorker || true
stopped=0
for _ in $(seq 1 30); do
	sc.exe query MinionWorker > q.txt 2>&1 || true
	if grep -q STOPPED q.txt; then stopped=1; break; fi
	sleep 1
done
[ "$stopped" = 1 ] || fail "the old service did not end by itself"
live && fail "the old service runs a Worker"

log "2. worker up removes it and installs the Worker as the user"
"$minion" worker up --share-existing none 2>&1 | tee up.log
grep -q 'Removed the service MinionWorker' up.log || fail "the old service was not removed"
grep -q 'runs as you' up.log || fail "worker up did not install the logon Worker"
if sc.exe query MinionWorker > q.txt 2>&1; then fail "the service MinionWorker still exists"; fi
reg.exe query 'HKCU\Software\Microsoft\Windows\CurrentVersion\Run' /v MinionWorker > run.txt || fail "no Run entry"
grep -q 'worker launch' run.txt || fail "the Run entry does not start the supervisor"
wait_live true || fail "the Worker is not live after worker up"
grep -q '"run_mode": "Windows logon autostart"' status.json || fail "wrong run mode"
grep -q '"problems"' status.json && fail "worker status reports problems"
pid=$(worker_pid)
[ -n "$pid" ] || fail "no worker run process"
owner=$(powershell -NoProfile -Command "(Invoke-CimMethod -InputObject (Get-CimInstance Win32_Process -Filter \"ProcessId=$pid\") -MethodName GetOwner).User" | tr -d '\r')
[ "$owner" = "$USERNAME" ] || fail "the Worker runs as '$owner', not as $USERNAME"
[ "$(count_launchers)" = 1 ] || fail "not exactly one supervisor"

log "3. a killed Worker is started again by its supervisor"
taskkill /F /PID "$pid" > /dev/null
restarted=0
for _ in $(seq 1 60); do
	sleep 1
	new=$(worker_pid)
	if [ -n "$new" ] && [ "$new" != "$pid" ] && live; then restarted=1; break; fi
done
[ "$restarted" = 1 ] || fail "the supervisor did not start the Worker again"

log "4. worker down stops it; worker up starts it again"
"$minion" worker down 2>&1 | tee down.log
grep -q 'next logon' down.log || fail "worker down did not mention the logon start"
wait_live false || fail "the Worker still answers after worker down"
for _ in $(seq 1 20); do [ "$(count_launchers)" = 0 ] && break; sleep 1; done
[ "$(count_launchers)" = 0 ] || fail "the supervisor still runs after worker down"
"$minion" worker up --share-existing none 2>&1 | tee up2.log
wait_live true || fail "the Worker is not live after the second worker up"
"$minion" worker down
wait_live false || fail "the Worker still answers after the last worker down"
log "ok"
