package localapi

import (
	"errors"
	"strings"
	"testing"

	"github.com/patrick-1984/ai-minion/internal/config"
)

const (
	userSID  = "S-1-5-21-1004336348-1177238915-682003330-1001"
	otherSID = "S-1-5-21-1004336348-1177238915-682003330-1002"
)

func TestValidSID(t *testing.T) {
	for _, ok := range []string{userSID, SIDSystem, SIDAdministrators, "S-1-1-0", "S-1-5-32-545"} {
		if !ValidSID(ok) {
			t.Errorf("ValidSID(%q) = false", ok)
		}
	}
	for _, bad := range []string{
		"", "S-1-5", "S-1-5-", "s-1-5-18", "S-2-5-18", "S-1-5-18\\", "S-1-5-18)(A;;GA;;;WD", "S-1-5-18 ",
		"S-1-5-18\x00", "..\\S-1-5-18", "S-1-5-18/../x", "S-1-5-21-a", strings.Repeat("S-1-5-1", 3),
		"S-1-5" + strings.Repeat("-1", 16), // more than 15 sub-authorities
	} {
		if ValidSID(bad) {
			t.Errorf("ValidSID(%q) = true", bad)
		}
	}
}

func TestPipeNameCarriesTheUserSID(t *testing.T) {
	got, err := PipeName(userSID)
	if err != nil {
		t.Fatal(err)
	}
	if want := `\\.\pipe\minion-worker-` + userSID; got != want {
		t.Fatalf("PipeName = %q, want %q", got, want)
	}
	if _, err := PipeName(`..\evil`); err == nil {
		t.Fatal("PipeName accepted a name that is not a SID")
	}
}

func TestParsePipeNameRoundTripsAndRejectsOtherNames(t *testing.T) {
	name, _ := PipeName(userSID)
	sid, err := ParsePipeName(name)
	if err != nil || sid != userSID {
		t.Fatalf("ParsePipeName = %q, %v", sid, err)
	}
	for _, bad := range []string{
		"", `\\.\pipe\minion-worker-`, `\\.\pipe\foreign-` + userSID, `\\.\pipe\minion-worker-` + userSID + `\x`,
		`\\server\pipe\minion-worker-` + userSID, `minion-worker-` + userSID, `\\.\pipe\minion-worker-S-1-5`,
		"/run/user/1000/minion/worker.sock",
	} {
		if _, err := ParsePipeName(bad); err == nil {
			t.Errorf("ParsePipeName(%q) succeeded", bad)
		}
	}
}

// The DACL of the pipe names the user and SYSTEM and nobody else, and is
// protected so that nothing is inherited (AD-22: restricted to the user).
func TestPipeSDDLAdmitsTheUserAndSystemOnly(t *testing.T) {
	got, err := PipeSDDL(userSID)
	if err != nil {
		t.Fatal(err)
	}
	if want := "D:P(A;;GA;;;" + userSID + ")(A;;GA;;;SY)"; got != want {
		t.Fatalf("PipeSDDL = %q, want %q", got, want)
	}
	for _, forbidden := range []string{"WD", "BU", "AU", "AN", "NU", "IU", "BA", "(D;"} {
		if strings.Contains(got, ";;;"+forbidden+")") || strings.Contains(got, forbidden+";") {
			t.Errorf("SDDL %q grants or denies %s", got, forbidden)
		}
	}
	if !strings.HasPrefix(got, "D:P") {
		t.Errorf("SDDL %q is not a protected DACL", got)
	}
	if n := strings.Count(got, "(A;"); n != 2 {
		t.Errorf("SDDL %q has %d allow entries, want 2", got, n)
	}
}

func TestPipeSDDLForSystemHasOneEntry(t *testing.T) {
	got, err := PipeSDDL(SIDSystem)
	if err != nil {
		t.Fatal(err)
	}
	if want := "D:P(A;;GA;;;" + SIDSystem + ")"; got != want {
		t.Fatalf("PipeSDDL(SYSTEM) = %q, want %q", got, want)
	}
}

func TestPipeSDDLRejectsAnythingButASID(t *testing.T) {
	// A value that could add an entry to the DACL must never reach the SDDL.
	for _, bad := range []string{"", "WD", userSID + ")(A;;GA;;;WD", "S-1-5-18)(A;;GA;;;WD"} {
		if s, err := PipeSDDL(bad); err == nil {
			t.Errorf("PipeSDDL(%q) = %q, want an error", bad, s)
		}
	}
}

func TestTrustedPipeOwner(t *testing.T) {
	for _, c := range []struct {
		name, owner string
		want        bool
	}{
		{"the user", userSID, true},
		{"SYSTEM (service)", SIDSystem, true},
		{"Administrators (elevated service or Worker)", SIDAdministrators, true},
		{"another user", otherSID, false},
		{"Everyone", "S-1-1-0", false},
		{"Users group", "S-1-5-32-545", false},
		{"nobody", "", false},
	} {
		if got := TrustedPipeOwner(c.owner, userSID); got != c.want {
			t.Errorf("%s: TrustedPipeOwner(%q) = %v, want %v", c.name, c.owner, got, c.want)
		}
	}
}

func TestPipeServedBy(t *testing.T) {
	if !pipeServedBy(userSID, userSID) {
		t.Error("a user cannot serve their own pipe")
	}
	if pipeServedBy(userSID, otherSID) {
		t.Error("a user can serve the pipe of another user")
	}
	if !pipeServedBy(userSID, SIDSystem) {
		t.Error("the service account cannot serve the pipe of the installing user")
	}
}

func TestPipePathUsesTheEnvironmentSIDFirst(t *testing.T) {
	self := func() (string, error) { return otherSID, nil }
	getenv := func(k string) string {
		if k == PipeUserSIDEnv {
			return userSID
		}
		return ""
	}
	got, err := PipePath(getenv, self)
	if err != nil || got != PipeNamePrefix+userSID {
		t.Fatalf("PipePath = %q, %v", got, err)
	}
}

func TestPipePathFallsBackToTheProcessUser(t *testing.T) {
	got, err := PipePath(func(string) string { return "" }, func() (string, error) { return userSID, nil })
	if err != nil || got != PipeNamePrefix+userSID {
		t.Fatalf("PipePath = %q, %v", got, err)
	}
	// A nil getenv is the same as an empty environment.
	if got, err = PipePath(nil, func() (string, error) { return userSID, nil }); err != nil || got != PipeNamePrefix+userSID {
		t.Fatalf("PipePath(nil getenv) = %q, %v", got, err)
	}
}

func TestPipePathErrors(t *testing.T) {
	bad := func(k string) string {
		if k == PipeUserSIDEnv {
			return `S-1-5-18)(A;;GA;;;WD`
		}
		return ""
	}
	if _, err := PipePath(bad, func() (string, error) { return userSID, nil }); err == nil || !strings.Contains(err.Error(), PipeUserSIDEnv) {
		t.Errorf("an invalid %s: err = %v", PipeUserSIDEnv, err)
	}
	if _, err := PipePath(nil, func() (string, error) { return "", errors.New("no token") }); !errors.Is(err, ErrNoUserSID) {
		t.Errorf("a failing SID source: err = %v, want ErrNoUserSID", err)
	}
	if _, err := PipePath(nil, nil); !errors.Is(err, ErrNoUserSID) {
		t.Errorf("no SID source: err = %v, want ErrNoUserSID", err)
	}
	if _, err := PipePath(nil, func() (string, error) { return "not-a-sid", nil }); err == nil {
		t.Error("an invalid process SID was accepted")
	}
}

func windowsEnv(sid string) config.PathEnv {
	return config.PathEnv{GOOS: "windows", Getenv: func(k string) string {
		if k == PipeUserSIDEnv {
			return sid
		}
		return ""
	}}
}

// On Windows the socket path is the pipe name, not a file under a runtime
// directory, and the client's candidate list has that one name.
func TestSocketPathOnWindowsIsThePipe(t *testing.T) {
	got, err := SocketPath(windowsEnv(userSID))
	if err != nil || got != PipeNamePrefix+userSID {
		t.Fatalf("SocketPath = %q, %v", got, err)
	}
	c, err := SocketCandidates(windowsEnv(userSID))
	if err != nil || len(c) != 1 || c[0] != PipeNamePrefix+userSID {
		t.Fatalf("SocketCandidates = %v, %v", c, err)
	}
}

func TestPipeNameConstantsFollowTheSpine(t *testing.T) {
	// AD-22: \\.\pipe\minion-worker-<user SID>.
	if PipeNamePrefix != `\\.\pipe\minion-worker-` {
		t.Fatalf("PipeNamePrefix = %q", PipeNamePrefix)
	}
	if PipeUserSIDEnv != "MINION_PIPE_USER_SID" {
		t.Fatalf("PipeUserSIDEnv = %q", PipeUserSIDEnv)
	}
}
