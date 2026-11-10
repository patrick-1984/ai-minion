#!/usr/bin/env bash
# windows-service-restart-probe.sh: the "service restarts after its process is
# killed" probe of the Windows job worker-service (.github/workflows/windows.yml).
#
# It reads the instance of the service (process id and process creation time),
# kills that process the hard way and then waits until the service control
# manager (SCM) runs a NEW instance (another process id, or the same id with
# another creation time, because Windows reuses process ids) that the CLI
# reaches over the named pipe and that reports itself live.
#
# Why it is a script and not a few lines of workflow shell:
#   - Every command writes to a file and its exit status is checked. Nothing
#     is piped into "grep -q": grep stops at the first match, the producer
#     (sc.exe, minion.exe) then writes to a closed pipe and fails, and under
#     "pipefail" the whole pipeline is false even though the text matched.
#   - A new instance is told apart by process id AND creation time. The status
#     answer is accepted only if the SCM reports the same running instance
#     before and after the CLI asked, so the answer is from that instance
#     (the status report has no process id of its own).
#   - A failure prints the history of the polls and the last output, exit
#     status and error text of every command, and the SCM and event log state.
#
# Usage: scripts/windows-service-restart-probe.sh
# Environment (all optional; the PROBE_* commands are replaced by the tests,
# which run this script on Linux against fake commands):
#   PROBE_SERVICE      service name                      (MinionWorker)
#   PROBE_SC           the sc.exe command                (sc.exe)
#   PROBE_TASKKILL     the taskkill command              (taskkill)
#   PROBE_MINION       the minion command                (./minion.exe)
#   PROBE_START_TIME   command: process id -> creation time (PowerShell CIM)
#   PROBE_DIAG         command run once after a failure  (SCM and event log)
#   PROBE_ATTEMPTS     polls after the kill              (90)
#   PROBE_INTERVAL     seconds between polls             (1)
#   PROBE_DIR          directory for the captured output (a new temp dir)
set -u

service="${PROBE_SERVICE:-MinionWorker}"
sc="${PROBE_SC:-sc.exe}"
taskkill="${PROBE_TASKKILL:-taskkill}"
minion="${PROBE_MINION:-./minion.exe}"
attempts="${PROBE_ATTEMPTS:-90}"
interval="${PROBE_INTERVAL:-1}"
dir="${PROBE_DIR:-$(mktemp -d)}"
mkdir -p "$dir" || exit 2
history="$dir/history.txt"
: > "$history"

say() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*"; }

# capture NAME COMMAND [ARG...] runs the command with stdout in $dir/NAME.out and
# stderr in $dir/NAME.err (each run replaces the files of the run before, so
# they hold the last run) and sets cap_rc to its real exit status.
cap_rc=0
capture() {
  local name="$1"
  shift
  "$@" > "$dir/$name.out" 2> "$dir/$name.err"
  cap_rc=$?
  printf '%s' "$cap_rc" > "$dir/$name.rc"
  return 0
}

# shellcheck disable=SC2329 # called through $start_time_cmd
windows_start_time() {
  pwsh -NoLogo -NoProfile -NonInteractive -Command \
    "(Get-CimInstance Win32_Process -Filter 'ProcessId=$1').CreationDate.ToUniversalTime().ToString('o')"
}
start_time_cmd="${PROBE_START_TIME:-windows_start_time}"

# shellcheck disable=SC2329 # called through $diag_cmd
windows_diag() {
  local ps
  # shellcheck disable=SC2016 # PowerShell variables, not shell ones
  ps='$ErrorActionPreference = "SilentlyContinue"
foreach ($f in @(@{LogName="System"; ProviderName="Service Control Manager"}, @{LogName="Application"; ProviderName="'"$service"'"})) {
  Get-WinEvent -FilterHashtable $f -MaxEvents 30 | Format-List TimeCreated, ProviderName, Id, Message
}'
  echo "--- sc qfailure"
  "$sc" qfailure "$service"
  echo "--- sc queryex"
  "$sc" queryex "$service"
  echo "--- event log (System: Service Control Manager, Application: $service)"
  pwsh -NoLogo -NoProfile -NonInteractive -Command "$ps"
  echo "--- state directory"
  ls -la "${MINION_STATE_DIR:-.}"
}
diag_cmd="${PROBE_DIAG:-windows_diag}"

show() { # show NAME: the last captured output of a command
  local f
  for f in rc out err; do
    [ -f "$dir/$1.$f" ] || continue
    printf -- '--- %s.%s\n' "$1" "$f"
    cat "$dir/$1.$f"
    [ "$f" = rc ] && echo
  done
}

fail() {
  echo "::error::$*"
  echo "the service was not restarted after its process was killed: $*"
  echo "=== polls (time, SCM state, process id, creation time, status exit, live)"
  cat "$history"
  echo "=== last output of every command (captured in $dir)"
  local n
  for n in queryex start status kill; do show "$n"; done
  echo "=== diagnostics"
  "$diag_cmd" 2>&1 || echo "(diagnostics exit status $?)"
  exit 1
}

# observe queries the SCM. It sets obs_state (the number: 4 is RUNNING), obs_pid
# and obs_start (the creation time of that process, empty if unknown) and
# returns 1 if the SCM could not be queried.
obs_state="" obs_pid="" obs_start=""
observe() {
  obs_state="" obs_pid="" obs_start=""
  capture queryex "$sc" queryex "$service"
  [ "$cap_rc" -eq 0 ] || return 1
  obs_state="$(awk '$1 == "STATE" { gsub(/\r/, ""); print $3 }' "$dir/queryex.out")"
  obs_pid="$(awk '$1 == "PID" { gsub(/\r/, ""); print $3 }' "$dir/queryex.out")"
  case "$obs_pid" in '' | *[!0-9]*) obs_pid="" ;; esac
  if [ -n "$obs_pid" ] && [ "$obs_pid" != 0 ]; then
    capture start "$start_time_cmd" "$obs_pid"
    [ "$cap_rc" -eq 0 ] && obs_start="$(tr -d '\r\n ' < "$dir/start.out")"
  fi
  return 0
}

running() { [ "$obs_state" = 4 ] && [ -n "$obs_pid" ] && [ "$obs_pid" != 0 ]; }

# 1. The instance that is running now.
first_pid="" first_start=""
for _ in 1 2 3 4 5; do
  if observe && running && [ -n "$obs_start" ]; then
    first_pid="$obs_pid" first_start="$obs_start"
    break
  fi
  sleep "$interval"
done
[ -n "$first_pid" ] || fail "cannot read a running instance of $service before the kill (SCM state '${obs_state}', process id '${obs_pid}', creation time '${obs_start}')"
say "instance before the kill: process $first_pid, created $first_start"

# 2. Kill it. A kill that did not work is not a restart failure.
capture kill "$taskkill" /F /PID "$first_pid"
[ "$cap_rc" -eq 0 ] || fail "taskkill of process $first_pid exited with $cap_rc"
say "killed process $first_pid"

# is_new: the observed instance is not the one that was killed.
is_new() {
  [ "$obs_pid" != "$first_pid" ] && return 0
  # The same process id: a new process only if its creation time differs.
  [ -n "$obs_start" ] && [ "$obs_start" != "$first_start" ]
}

# 3. Wait for a new, running, reachable instance.
n=0
while [ "$n" -lt "$attempts" ]; do
  n=$((n + 1))
  status_rc="-" live=no
  if ! observe; then
    printf '%s poll %s: cannot query the SCM (exit %s)\n' "$(date +%H:%M:%S)" "$n" "$cap_rc" >> "$history"
  elif ! running; then
    printf '%s poll %s: SCM state %s, process %s (not running)\n' "$(date +%H:%M:%S)" "$n" "${obs_state:-?}" "${obs_pid:-?}" >> "$history"
  elif ! is_new; then
    printf '%s poll %s: still the killed instance, process %s created %s\n' "$(date +%H:%M:%S)" "$n" "$obs_pid" "${obs_start:-?}" >> "$history"
  else
    seen_pid="$obs_pid" seen_start="$obs_start"
    capture status "$minion" worker status --json
    status_rc="$cap_rc"
    if [ "$status_rc" -eq 0 ] && grep -q '"live": true' "$dir/status.out" && ! grep -q '"problems"' "$dir/status.out"; then
      live=yes
    fi
    printf '%s poll %s: new instance, process %s created %s, status exit %s, live %s\n' \
      "$(date +%H:%M:%S)" "$n" "$seen_pid" "${seen_start:-?}" "$status_rc" "$live" >> "$history"
    if [ "$live" = yes ]; then
      # The answer is from this instance only if the SCM still reports it.
      if observe && running && [ "$obs_pid" = "$seen_pid" ] && [ "$obs_start" = "$seen_start" ]; then
        say "restarted: process $first_pid (created $first_start) -> process $seen_pid (created ${seen_start:-unknown}); status live after $n poll(s)"
        cat "$dir/status.out"
        exit 0
      fi
      printf '%s poll %s: the instance changed while the status was read (now process %s state %s)\n' \
        "$(date +%H:%M:%S)" "$n" "${obs_pid:-?}" "${obs_state:-?}" >> "$history"
    fi
  fi
  sleep "$interval"
done

fail "no new running instance that answers over the named pipe within $attempts polls"
