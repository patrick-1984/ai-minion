package service

import (
	"errors"
	"os"
	"os/user"
	"strings"
	"testing"
)

func fakeTargetEnv(euid int, cur string, env map[string]string) TargetEnv {
	accounts := map[string]*user.User{
		"root": {Username: "root", Uid: "0", Gid: "0", HomeDir: "/root"},
		"ann":  {Username: "ann", Uid: "1000", Gid: "1001", HomeDir: "/home/ann"},
		"bob":  {Username: "bob", Uid: "1002", Gid: "1002", HomeDir: "/home/bob"},
	}
	return TargetEnv{
		EUID:   euid,
		Getenv: func(k string) string { return env[k] },
		Lookup: func(n string) (*user.User, error) {
			if u, ok := accounts[n]; ok {
				return u, nil
			}
			return nil, errors.New("unknown user")
		},
		Current: func() (*user.User, error) { return accounts[cur], nil },
	}
}

func TestResolveTarget(t *testing.T) {
	cases := []struct {
		name    string
		env     TargetEnv
		asUser  string
		want    string
		wantErr string
	}{
		{"plain user", fakeTargetEnv(1000, "ann", nil), "", "ann", ""},
		{"sudo uses SUDO_USER", fakeTargetEnv(0, "root", map[string]string{"SUDO_USER": "ann"}), "", "ann", ""},
		{"--as-user beats SUDO_USER", fakeTargetEnv(0, "root", map[string]string{"SUDO_USER": "ann"}), "bob", "bob", ""},
		{"root without sudo must name a user", fakeTargetEnv(0, "root", nil), "", "", "--as-user"},
		{"root can name root explicitly", fakeTargetEnv(0, "root", nil), "root", "root", ""},
		{"as-user needs root", fakeTargetEnv(1000, "ann", nil), "bob", "", "needs root"},
		{"as-user itself is fine", fakeTargetEnv(1000, "ann", nil), "ann", "ann", ""},
		{"unknown user", fakeTargetEnv(0, "root", nil), "nobody-here", "", "nobody-here"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolveTarget(c.env, c.asUser)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil || got.User != c.want || got.Home == "" {
				t.Fatalf("got %+v, %v", got, err)
			}
		})
	}
}

func TestResolveTargetParsesIDs(t *testing.T) {
	got, err := ResolveTarget(fakeTargetEnv(0, "root", map[string]string{"SUDO_USER": "ann"}), "")
	if err != nil || got.UID != 1000 || got.GID != 1001 || got.Home != "/home/ann" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestResolveTargetWindowsSIDsAreNotIDs(t *testing.T) {
	env := TargetEnv{EUID: 0, Current: func() (*user.User, error) {
		return &user.User{Username: `PC\ann`, Uid: "S-1-5-21-1", Gid: "S-1-5-21-2", HomeDir: `C:\Users\ann`}, nil
	}}
	got, err := ResolveTarget(env, `PC\ann`)
	if err != nil || got.UID != -1 || got.GID != -1 {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestSystemTargetEnvResolvesTheCurrentUser(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root needs --as-user")
	}
	got, err := ResolveTarget(SystemTargetEnv(), "")
	if err != nil || got.UID != os.Getuid() {
		t.Fatalf("%+v %v", got, err)
	}
}
