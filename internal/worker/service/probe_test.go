package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// systemctlAnswers answers "systemctl [--user] is-active|is-enabled <unit>"
// from a table; anything else is "inactive"/"disabled" with an exit error,
// as systemctl reports it.
func systemctlAnswers(states map[string]string) func(string) (string, error) {
	return func(line string) (string, error) {
		if s, ok := states[line]; ok {
			if s == "active" || strings.HasPrefix(s, "enabled") {
				return s + "\n", nil
			}
			return s + "\n", errors.New("exit status 3")
		}
		if strings.Contains(line, "is-enabled") {
			return "disabled\n", errors.New("exit status 1")
		}
		return "inactive\n", errors.New("exit status 3")
	}
}

func TestProbeSystemInstance(t *testing.T) {
	const user = "alice"
	sys := SystemUnitFor(user)
	for _, tc := range []struct {
		name    string
		states  map[string]string
		files   func(t *testing.T, d Deps, home string)
		want    InstanceProbe
		warning bool
	}{
		{
			name: "template only",
			files: func(t *testing.T, d Deps, _ string) {
				writeFile(t, filepath.Join(d.PackagedUnitDirs[0], TemplateUnitName))
				writeFile(t, filepath.Join(d.SystemUnitDir, TemplateUnitName))
			},
			want: InstanceProbe{Manager: ManagerNone, Scope: ScopeUser},
		},
		{
			name:   "enabled but stopped system instance",
			states: map[string]string{"systemctl is-enabled " + sys: "enabled"},
			want:   InstanceProbe{Manager: ManagerSystemd, Scope: ScopeSystem, Unit: sys},
		},
		{
			name: "system instance drop-in only",
			files: func(t *testing.T, d Deps, _ string) {
				writeFile(t, filepath.Join(d.SystemUnitDir, sys+".d", "minion.conf"))
			},
			want: InstanceProbe{Manager: ManagerSystemd, Scope: ScopeSystem, Unit: sys},
		},
		{
			name:   "active user unit",
			states: map[string]string{"systemctl --user is-active " + UserUnitName: "active"},
			want:   InstanceProbe{Manager: ManagerSystemd, Scope: ScopeUser, Unit: UserUnitName},
		},
		{
			name: "user unit file only",
			files: func(t *testing.T, _ Deps, home string) {
				writeFile(t, filepath.Join(home, ".config", "systemd", "user", UserUnitName))
			},
			want: InstanceProbe{Manager: ManagerSystemd, Scope: ScopeUser, Unit: UserUnitName},
		},
		{
			name: "both scopes",
			states: map[string]string{
				"systemctl is-active " + sys:                  "active",
				"systemctl --user is-enabled " + UserUnitName: "enabled",
			},
			want:    InstanceProbe{Manager: ManagerSystemd, Scope: ScopeSystem, Unit: sys},
			warning: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{answer: systemctlAnswers(tc.states)}
			d := testDeps(t, r)
			home := t.TempDir()
			if tc.files != nil {
				tc.files(t, d, home)
			}
			got := d.ProbeSystemInstance(context.Background(), user, home)
			if got.Manager != tc.want.Manager || got.Scope != tc.want.Scope || got.Unit != tc.want.Unit {
				t.Fatalf("probe = %+v, want {%s %s %s}", got, tc.want.Manager, tc.want.Scope, tc.want.Unit)
			}
			if (got.Warning != "") != tc.warning {
				t.Fatalf("warning = %q, want warning %t", got.Warning, tc.warning)
			}
			if got.Manager != ManagerNone && got.Evidence == "" {
				t.Fatal("no evidence named")
			}
			for _, l := range r.lines() {
				if query := strings.Contains(l, " is-active ") || strings.Contains(l, " is-enabled "); !strings.HasPrefix(l, "systemctl ") || !query {
					t.Fatalf("the probe ran %q; it may only query", l)
				}
			}
		})
	}
}

func TestProbeSystemInstanceRefusesABadUser(t *testing.T) {
	r := &fakeRunner{}
	got := testDeps(t, r).ProbeSystemInstance(context.Background(), "../x", "")
	if got.Manager != ManagerNone || len(r.lines()) != 0 {
		t.Fatalf("probe = %+v, calls %v", got, r.lines())
	}
}

func writeFile(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}
