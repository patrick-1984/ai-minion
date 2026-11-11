package smoke

import (
	"strings"
	"testing"
)

// dockerSystemd emulates the docker commands of deb.sh and rpm.sh. The
// scenario is chosen through STUB_* variables.
const dockerSystemd = `printf 'docker %s\n' "$*" >>"$STUB_LOG"
case "$1" in
  build) cat >"$STUB_LOG.dockerfile"; exit 0 ;;
  run) echo cid123; exit 0 ;;
  rm | rmi) exit 0 ;;
  exec)
    shift
    while :; do case "$1" in -u | -w | -e) shift 2 ;; *) break ;; esac; done
    shift
    cmd="$*"
    case "$cmd" in
      "systemctl is-system-running") echo running ;;
      "minion version" | "mn version") printf 'version: %s\ncommit: abc\nbuilt: now\ngo: go1.27.4\n' "${STUB_VERSION:-v0.1.0}" ;;
      "systemctl is-active --quiet "*) [ -z "${STUB_WORKER_DOWN:-}" ] || exit 3 ;;
      *"worker status"*)
        case "$cmd" in
          *grep*) exit 0 ;;
        esac
        printf 'Version:             %s\nName:                smoke\nMode:                Hub\nHub URL:             %s\nConnection:          idle (not enrolled)\n' \
          "${STUB_VERSION:-v0.1.0}" "${STUB_HUB_URL:-http://127.0.0.1:8443}" ;;
      *"command -v tmux"*) [ -z "${STUB_NO_TMUX:-}" ] || exit 1 ;;
      *os-release*) [ -z "${STUB_WRONG_OS:-}" ] || exit 1 ;;
    esac
    exit 0 ;;
esac
exit 0
`

func systemdEnv(t *testing.T, extra ...string) (env []string, logPath string) {
	t.Helper()
	env, logPath = stubBin(t, map[string]string{"docker": dockerSystemd, "sleep": "exit 0"})
	env = append(env, "SMOKE_SETTLE_SECONDS=0")
	return append(env, extra...), logPath
}

func assetsWith(t *testing.T, names ...string) string {
	t.Helper()
	files := map[string]string{}
	for _, n := range names {
		files[n] = "bytes of " + n
	}
	return fixtureAssets(t, files)
}

// AC: install the deb on a clean Debian 13 container with systemd (amd64 and
// arm64 via QEMU), run minion version, start minion-worker@<user> against a
// test Hub and check minion worker status.
func TestDebSmoke(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			env, logPath := systemdEnv(t)
			assets := assetsWith(t, "minion_0.1.0_"+arch+".deb", "minion_0.1.0_other.deb")
			r := script(t, env, "", "deb.sh", assets, testTag, arch)
			if r.code != 0 {
				t.Fatalf("deb.sh: exit %d %s", r.code, r.all())
			}
			calls := readLog(t, logPath)
			dockerfile := readLog(t, logPath+".dockerfile")
			for _, want := range []string{
				"docker build --platform linux/" + arch,
				"docker run -d --platform linux/" + arch + " --privileged",
				assets + ":/assets:ro",
				"apt-get install -y /assets/minion_0.1.0_" + arch + ".deb",
				"minion version",
				"systemd-run --unit=smoke-hub",
				"MINION_ADMIN_PASSWORD_FILE=/root/admin.pw",
				"systemctl enable --now minion-worker@smokeuser",
				"exec -u smokeuser -w /home/smokeuser -e HOME=/home/smokeuser cid123 minion worker status --server http://127.0.0.1:8443",
				"docker rm -f cid123",
			} {
				if !strings.Contains(calls, want) {
					t.Errorf("missing docker call %q; calls:\n%s", want, calls)
				}
			}
			if !strings.Contains(dockerfile, "FROM debian:13") || !strings.Contains(dockerfile, "systemd") || !strings.Contains(dockerfile, `CMD ["/sbin/init"]`) {
				t.Errorf("the container is not Debian 13 with systemd as PID 1:\n%s", dockerfile)
			}
			if strings.Contains(calls, "MINION_ADMIN_PASSWORD=") {
				t.Error("the admin password must not be passed in argv")
			}
		})
	}
}

func TestDebSmokeFailures(t *testing.T) {
	cases := []struct {
		name  string
		env   []string
		args  []string
		files []string
		want  string
	}{
		{"wrong version", []string{"STUB_VERSION=v9.9.9"}, nil, nil, "expected 'version: v0.1.0'"},
		{"worker not active", []string{"STUB_WORKER_DOWN=1"}, nil, nil, "timed out"},
		{"no tmux dependency", []string{"STUB_NO_TMUX=1"}, nil, nil, "tmux was not installed"},
		{"not debian 13", []string{"STUB_WRONG_OS=1"}, nil, nil, "not Debian 13"},
		{"status without the hub url", []string{"STUB_HUB_URL=none"}, nil, nil, "does not show the test Hub URL"},
		{"two debs", nil, nil, []string{"minion_0.1.0_amd64.deb", "minion_0.1.1_amd64.deb"}, "exactly one"},
		{"no deb", nil, nil, []string{"readme.txt"}, "exactly one"},
		{"unsupported arch", nil, []string{"armhf"}, nil, "unsupported deb architecture"},
		{"bad tag", nil, []string{"amd64", "tagx"}, nil, "not a release tag"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env, logPath := systemdEnv(t, c.env...)
			files := c.files
			if files == nil {
				files = []string{"minion_0.1.0_amd64.deb"}
			}
			assets := assetsWith(t, files...)
			args := []string{assets, testTag}
			if c.name == "bad tag" {
				args = []string{assets, "main"}
			} else if len(c.args) > 0 {
				args = append(args, c.args...)
			}
			r := script(t, env, "", "deb.sh", args...)
			if r.code == 0 || !strings.Contains(r.all(), c.want) {
				t.Fatalf("expected failure containing %q, got exit %d: %s", c.want, r.code, r.all())
			}
			if calls := readLog(t, logPath); strings.Contains(calls, "docker run -d") && !strings.Contains(calls, "docker rm -f cid123") {
				t.Errorf("the container was not cleaned up after a failure:\n%s", calls)
			}
		})
	}
}

// AC: the rpm on a clean Fedora container.
func TestRpmSmoke(t *testing.T) {
	env, logPath := systemdEnv(t)
	assets := assetsWith(t, "minion-0.1.0-1.x86_64.rpm", "minion-0.1.0-1.aarch64.rpm")
	r := script(t, env, "", "rpm.sh", assets, testTag)
	if r.code != 0 {
		t.Fatalf("rpm.sh: exit %d %s", r.code, r.all())
	}
	calls := readLog(t, logPath)
	dockerfile := readLog(t, logPath+".dockerfile")
	for _, want := range []string{
		"docker build --platform linux/amd64",
		"dnf install -y /assets/minion-0.1.0-1.x86_64.rpm",
		"systemctl enable --now minion-worker@smokeuser",
		"exec -u smokeuser -w /home/smokeuser -e HOME=/home/smokeuser cid123 minion worker status",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing docker call %q; calls:\n%s", want, calls)
		}
	}
	if !strings.Contains(dockerfile, "FROM fedora:") || !strings.Contains(dockerfile, "systemd") {
		t.Errorf("the container is not Fedora with systemd:\n%s", dockerfile)
	}
	env, _ = systemdEnv(t, "STUB_VERSION=v0.0.1")
	if r := script(t, env, "", "rpm.sh", assets, testTag); r.code == 0 || !strings.Contains(r.all(), "expected 'version: v0.1.0'") {
		t.Errorf("a wrong version passed: exit %d %s", r.code, r.all())
	}
}

func TestRpmSmokeFailures(t *testing.T) {
	cases := []struct {
		name  string
		env   []string
		files []string
		tag   string
		want  string
	}{
		{"worker not active", []string{"STUB_WORKER_DOWN=1"}, nil, testTag, "timed out"},
		{"no tmux dependency", []string{"STUB_NO_TMUX=1"}, nil, testTag, "tmux was not installed"},
		{"not fedora", []string{"STUB_WRONG_OS=1"}, nil, testTag, "not Fedora"},
		{"status without the hub url", []string{"STUB_HUB_URL=none"}, nil, testTag, "does not show the test Hub URL"},
		{"status of another version", []string{"STUB_VERSION=v9.9.9"}, nil, testTag, "expected 'version: v0.1.0'"},
		{"two rpms", nil, []string{"minion-0.1.0-1.x86_64.rpm", "minion-0.1.1-1.x86_64.rpm"}, testTag, "exactly one"},
		{"only an aarch64 rpm", nil, []string{"minion-0.1.0-1.aarch64.rpm"}, testTag, "exactly one"},
		{"bad tag", nil, nil, "main", "not a release tag"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env, logPath := systemdEnv(t, c.env...)
			files := c.files
			if files == nil {
				files = []string{"minion-0.1.0-1.x86_64.rpm"}
			}
			r := script(t, env, "", "rpm.sh", assetsWith(t, files...), c.tag)
			if r.code == 0 || !strings.Contains(r.all(), c.want) {
				t.Fatalf("expected failure containing %q, got exit %d: %s", c.want, r.code, r.all())
			}
			if calls := readLog(t, logPath); strings.Contains(calls, "docker run -d") && !strings.Contains(calls, "docker rm -f cid123") {
				t.Errorf("the container was not cleaned up after a failure:\n%s", calls)
			}
		})
	}
}
