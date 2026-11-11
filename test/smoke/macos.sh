#!/usr/bin/env bash
# macos.sh: run the exact release binary on a clean macOS runner (Story 9.7,
# AD-24, R1).
#
# Usage: test/smoke/macos.sh ASSETS_DIR TAG
#
# Unpacks the darwin archive for this machine, runs "minion version", then
# runs a Worker in the foreground ("worker run") on a PRIVATE tmux socket with
# a throw-away state directory, with one tmux session on that socket, and
# checks "minion worker status" sees the running Worker and the session. The
# default tmux server is never touched: tmux runs only with -S <private
# socket>, with TMUX and TMUX_PANE unset, and only that socket is ended.
#
# SMOKE_TMUX_SOCKET names an existing private tmux socket that already has one
# session (the Go test creates it through internal/testutil/tmuxtest). The
# script then neither starts nor ends a tmux server; it only points the Worker
# at that socket. Without it (the CI macOS runner) the script starts its own
# server on a private socket and ends it, and only it, on exit.
#
# SMOKE_OS and SMOKE_ARCH override the detected platform (used by the unit
# tests of this script, which run it against a Linux binary).
# SMOKE_WORKER_WAIT_SECONDS (default 30) bounds the wait for the Worker.
#
# On any failure the script prints a report (Worker still running or its exit
# status, the end of its log with secrets redacted, the tmux state) before it
# removes the work directory, so that a failed run on a CI runner explains
# itself.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=test/smoke/common.sh
. "$here/common.sh"

[ $# -eq 2 ] || smoke_die "usage: macos.sh ASSETS_DIR TAG"
assets=$(cd "$1" && pwd)
tag=$2
smoke_require_tag "$tag"

os=${SMOKE_OS:-$(uname -s | tr '[:upper:]' '[:lower:]')}
case "${SMOKE_ARCH:-$(uname -m)}" in
arm64 | aarch64) arch=arm64 ;;
x86_64 | amd64) arch=amd64 ;;
*) smoke_die "unsupported machine $(uname -m)" ;;
esac
archive=$(smoke_archive_name "$tag" "$os" "$arch")
[ -f "$assets/$archive" ] || smoke_die "missing $archive in $assets"
smoke_listed_in_checksums "$assets/checksums.txt" "$assets/$archive" ||
	smoke_die "$archive is not in the signed checksums.txt"
command -v tmux >/dev/null 2>&1 || smoke_die "tmux is not installed"

# A short path: unix socket paths are limited to about 100 bytes on macOS.
work=$(mktemp -d "${TMPDIR:-/tmp}/mn.XXXXXX")
worker_pid=""
minion=""
unset TMUX TMUX_PANE
sock="$work/t.sock"
own_server=0
if [ -n "${SMOKE_TMUX_SOCKET:-}" ]; then
	sock=$SMOKE_TMUX_SOCKET
	[ -S "$sock" ] || smoke_die "SMOKE_TMUX_SOCKET $sock is not a socket"
else
	own_server=1
fi
# failure_report: what is left to look at when the script ends with an error,
# printed BEFORE cleanup removes the work directory: whether the Worker still
# runs (its exit status when it does not), the end of its log and the answer of
# "worker status", redacted (smoke_redact) and bounded in size and in time
# (SMOKE_DIAG_TIMEOUT_SECONDS, default 5, for each subprocess). Everything goes to
# stderr and nothing in it can fail the script.
reported=0
worker_exit=""
# Every diagnostic subprocess is bounded (smoke_timeout): a stopped tmux server
# of the caller must not keep the script from cleaning up.
diag_seconds=${SMOKE_DIAG_TIMEOUT_SECONDS:-5}
# excerpt LINES WIDTH: stdin, redacted FIRST and truncated after (a cut inside a
# quoted secret would remove the quote the redaction needs), at most LINES lines
# of WIDTH characters.
excerpt() { smoke_redact | head -n "$1" | cut -c1-"$2"; }
# diag COMMAND...: a bounded diagnostic subprocess; its output (and a note when
# it was stopped) is on stdout; it never fails.
diag() {
	local rc=0
	smoke_timeout "$diag_seconds" "$@" || rc=$?
	case "$rc" in
	0) ;;
	124) printf '(stopped after %ss)\n' "$diag_seconds" ;;
	*) printf '(exit status %s)\n' "$rc" ;;
	esac
	return 0
}
failure_report() {
	local rc st
	[ "$reported" -eq 0 ] || return 0
	reported=1
	{
		smoke_log "--- failure report (the work directory is removed next) ---"
		if [ -n "$worker_pid" ]; then
			if kill -0 "$worker_pid" 2>/dev/null; then
				smoke_log "the Worker (pid $worker_pid) is still running"
			else
				if [ -z "$worker_exit" ]; then
					if wait "$worker_pid" 2>/dev/null; then worker_exit=0; else worker_exit=$?; fi
				fi
				smoke_log "the Worker (pid $worker_pid) exited with status $worker_exit"
			fi
		else
			smoke_log "the Worker was not started, or was already stopped"
		fi
		if [ -e "$sock" ]; then
			smoke_log "$(diag tmux -S "$sock" -V | excerpt 1 200); private server: $(diag tmux -S "$sock" list-sessions | excerpt 3 200 | tr '\n' '|')"
		else
			smoke_log "the private tmux socket $sock does not exist"
		fi
		if [ -s "$work/worker.log" ]; then
			smoke_log "last lines of the Worker log (redacted):"
			tail -n 40 "$work/worker.log" | excerpt 40 400 | sed 's/^/smoke:   | /'
		else
			smoke_log "the Worker log is empty or missing"
		fi
		if [ -x "$minion" ] && [ -d "$work/state" ]; then
			smoke_log "minion worker status (redacted):"
			# Into a variable first: a pipe into head would end it with SIGPIPE.
			st=$(diag "$minion" worker status --tmux-socket "$sock")
			printf '%s\n' "$st" | excerpt 30 400 | sed 's/^/smoke:   | /'
		fi
	} >&2 2>&1 || true
}
cleanup() {
	local rc=$?
	trap - EXIT
	if [ "$rc" -ne 0 ]; then
		failure_report
	fi
	[ -z "$worker_pid" ] || kill "$worker_pid" 2>/dev/null || true
	# Only a server this script started itself, only the private one, always
	# with -S. A socket handed in through SMOKE_TMUX_SOCKET belongs to the caller.
	if [ "$own_server" -eq 1 ] && [ -S "$sock" ]; then
		tmux -S "$sock" kill-server 2>/dev/null || true
	fi
	rm -rf "$work"
	exit "$rc"
}
trap cleanup EXIT

mkdir -p "$work/bin"
tar -xzf "$assets/$archive" -C "$work/bin" minion
chmod 755 "$work/bin/minion"
minion="$work/bin/minion"

out=$("$minion" version)
smoke_expect_version "$out" "$tag"
smoke_log "$archive: $(printf '%s\n' "$out" | head -n 1)"

export MINION_STATE_DIR="$work/state"
export XDG_RUNTIME_DIR="$work/run"
mkdir -p "$work/run" "$work/state"
chmod 700 "$work/run" "$work/state"
if [ "$own_server" -eq 1 ]; then
	tmux -S "$sock" -f /dev/null new-session -d -s smoke
fi
[ -S "$sock" ] || smoke_die "the private tmux socket was not created"

"$minion" worker run --tmux-socket "$sock" >"$work/worker.log" 2>&1 &
worker_pid=$!

# wait_for_worker: polls "worker status" once a second until it sees the
# running Worker. It runs in the script itself, not through smoke_wait_for
# (which sends everything to /dev/null): when the Worker dies, the reason is
# printed, together with the exit status of the Worker.
wait_for_worker() {
	local i=0 st rc limit=${SMOKE_WORKER_WAIT_SECONDS:-30}
	while :; do
		if ! kill -0 "$worker_pid" 2>/dev/null; then
			if wait "$worker_pid" 2>/dev/null; then
				worker_exit=0
			else
				worker_exit=$?
			fi
			smoke_die "the Worker exited with status $worker_exit before 'minion worker status' reached it: $(tail -n 5 "$work/worker.log" 2>&1 | excerpt 5 400 | tr '\n' '|')"
		fi
		# A here-string, not a pipe: with pipefail, a grep -q that quits early
		# would turn the exit status of "worker status" into SIGPIPE (141).
		rc=0
		st=$("$minion" worker status --tmux-socket "$sock" 2>&1) || rc=$?
		if [ "$rc" -eq 0 ] && grep -Eq '^Connection: +idle' <<<"$st"; then
			return 0
		fi
		i=$((i + 1))
		[ "$i" -le "$limit" ] || smoke_die "timed out after ${limit}s waiting for minion worker status to reach the running Worker; last status (exit $rc): $(printf '%s\n' "$st" | excerpt 6 200 | tr '\n' '|')"
		sleep 1
	done
}
wait_for_worker
status=$("$minion" worker status --tmux-socket "$sock") || smoke_die "minion worker status failed (exit $?) after it had reached the Worker"
grep -Eq "^Version: +$tag\$" <<<"$status" || smoke_die "worker status shows another version: $(printf '%s\n' "$status" | excerpt 30 400)"
grep -Eq '^Sessions: +1$' <<<"$status" || smoke_die "worker status does not count the private tmux session: $(printf '%s\n' "$status" | excerpt 30 400)"
# The status prints the cleaned socket path: on macOS TMPDIR ends with "/" (so the
# raw path may hold "//") and /var is a symlink to /private/var; accept the
# cleaned logical or physical spelling of the same file.
sock_dir=$(dirname "$sock")
sock_logical="$(cd "$sock_dir" && pwd -L)/$(basename "$sock")"
sock_physical="$(cd "$sock_dir" && pwd -P)/$(basename "$sock")"
{ grep -Fq "$sock_logical" <<<"$status" || grep -Fq "$sock_physical" <<<"$status" || grep -Fq "$sock" <<<"$status"; } || smoke_die "worker status does not show the private tmux socket: $(printf '%s\n' "$status" | excerpt 30 400)"

kill -0 "$worker_pid" 2>/dev/null || smoke_die "the Worker was gone before the smoke test ended"
kill "$worker_pid"
wait "$worker_pid" 2>/dev/null || true
worker_pid=""
smoke_log "macos smoke test passed ($archive)"
