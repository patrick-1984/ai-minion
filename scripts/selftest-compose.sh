#!/usr/bin/env bash
# selftest-compose.sh: proves with a stand-in "docker" that check-compose.sh
# runs "compose config" on every Compose file with a dummy environment, passes
# when the command succeeds and fails when it does not. Needs no Docker.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
# The stand-in runs under "env -i": the log path and the failure switch are
# baked into it, and it checks the dummy environment it receives.
write_fake() {
  cat > "$tmp/fake-docker" <<FAKE
#!/usr/bin/env bash
echo "\$*" >> "$tmp/log"
case " \$* " in *" compose "*" config "*) ;; *) exit 64 ;; esac
[ "$1" = fail ] && exit 1
[ "\${WORKER_UID:-}" = 1000 ] && [ -n "\${DOCKER_HOST:-}" ] || exit 65
FAKE
  chmod +x "$tmp/fake-docker"
}
write_fake ok
: > "$tmp/log"
DOCKER="$tmp/fake-docker" "$here/check-compose.sh" > /dev/null
n="$(grep -c 'config --quiet' "$tmp/log")"
want="$(ls "$here"/../deploy/compose/*.yaml | wc -l)"
[ "$n" = "$want" ] || { echo "selftest-compose: $n config calls, want $want" >&2; exit 1; }
write_fake fail
if DOCKER="$tmp/fake-docker" "$here/check-compose.sh" > /dev/null 2>&1; then
  echo "selftest-compose: a failing 'compose config' did not fail the check" >&2
  exit 1
fi
echo "selftest-compose: ok"
