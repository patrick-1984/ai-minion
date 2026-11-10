package service

import "testing"

func envOf(goos string, euid int, existing []string, env map[string]string) Env {
	set := map[string]bool{}
	for _, p := range existing {
		set[p] = true
	}
	return Env{
		GOOS:   goos,
		EUID:   euid,
		Getenv: func(k string) string { return env[k] },
		Exists: func(p string) bool { return set[p] },
	}
}

func TestDetectMatrix(t *testing.T) {
	systemd := []string{"/run/systemd/system"}
	cases := []struct {
		name    string
		env     Env
		manager Manager
		scope   Scope
	}{
		{"linux systemd as root (sudo)", envOf("linux", 0, systemd, nil), ManagerSystemd, ScopeSystem},
		{"linux systemd without sudo", envOf("linux", 1000, systemd, nil), ManagerSystemd, ScopeUser},
		{"linux without systemd", envOf("linux", 1000, nil, nil), ManagerNone, ""},
		{"docker container", envOf("linux", 0, []string{"/.dockerenv"}, nil), ManagerNone, ""},
		{"podman container", envOf("linux", 0, []string{"/run/.containerenv"}, nil), ManagerNone, ""},
		{"container variable", envOf("linux", 0, nil, map[string]string{"container": "lxc"}), ManagerNone, ""},
		{"kubernetes", envOf("linux", 0, nil, map[string]string{"KUBERNETES_SERVICE_HOST": "10.0.0.1"}), ManagerNone, ""},
		{"systemd inside a container wins", envOf("linux", 0, append([]string{"/.dockerenv"}, systemd...), nil), ManagerSystemd, ScopeSystem},
		{"macOS", envOf("darwin", 501, nil, nil), ManagerLaunchd, ScopeUser},
		{"macOS as root", envOf("darwin", 0, nil, nil), ManagerLaunchd, ScopeUser},
		{"windows", envOf("windows", 0, nil, nil), ManagerWindows, ScopeSystem},
		{"freebsd", envOf("freebsd", 0, nil, nil), ManagerNone, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Detect(c.env)
			if got.Manager != c.manager || got.Scope != c.scope {
				t.Fatalf("Detect = %+v, want manager %s scope %q", got, c.manager, c.scope)
			}
			if got.Reason == "" {
				t.Error("no reason")
			}
		})
	}
}

func TestDetectReasonExplainsTheContainer(t *testing.T) {
	got := Detect(envOf("linux", 0, []string{"/.dockerenv"}, nil))
	if got.Manager != ManagerNone || got.Reason == "" {
		t.Fatalf("%+v", got)
	}
	if want := "/.dockerenv"; !contains(got.Reason, want) {
		t.Errorf("reason %q does not mention %s", got.Reason, want)
	}
}

func TestSystemEnvIsFilled(t *testing.T) {
	e := SystemEnv()
	if e.GOOS == "" || e.Getenv == nil || e.Exists == nil {
		t.Fatalf("%+v", e)
	}
	if !e.Exists(t.TempDir()) || e.Exists("/definitely/not/here") {
		t.Error("Exists is wrong")
	}
}
