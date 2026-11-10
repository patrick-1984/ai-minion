package identity

import "testing"

const (
	pinnedSID = "S-1-5-21-1004336348-1177238915-682003330-1001"
	otherSID  = "S-1-5-21-1004336348-1177238915-682003330-1002"
)

func TestPrivateUserSID(t *testing.T) {
	cases := []struct {
		name            string
		process, pinned string
		want            string
	}{
		{"SYSTEM with a pinned user serves that user", SIDSystem, pinnedSID, pinnedSID},
		{"SYSTEM without a pin stays SYSTEM", SIDSystem, "", SIDSystem},
		{"SYSTEM with an invalid pin stays SYSTEM", SIDSystem, "not-a-sid", SIDSystem},
		{"SYSTEM with a pin that could inject SDDL stays SYSTEM", SIDSystem, pinnedSID + ")(A;;GA;;;WD", SIDSystem},
		{"a user ignores the pin", otherSID, pinnedSID, otherSID},
		{"a user without a pin", otherSID, "", otherSID},
		{"administrators group is not SYSTEM", "S-1-5-32-544", pinnedSID, "S-1-5-32-544"},
	}
	for _, tc := range cases {
		if got := PrivateUserSID(tc.process, tc.pinned); got != tc.want {
			t.Errorf("%s: PrivateUserSID(%q, %q) = %q, want %q", tc.name, tc.process, tc.pinned, got, tc.want)
		}
	}
}

func TestValidSID(t *testing.T) {
	for _, ok := range []string{SIDSystem, "S-1-5-32-544", pinnedSID, "S-1-5-21-1-2-3-4"} {
		if !ValidSID(ok) {
			t.Errorf("ValidSID(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "S-1-5", "s-1-5-18", "S-1-5-18\n", "S-2-5-18", "S-1-5-18-", `S-1-5-18)(A;;GA;;;WD`, "S-1-5-18 "} {
		if ValidSID(bad) {
			t.Errorf("ValidSID(%q) = true", bad)
		}
	}
}

// The pin is read from this name by internal/worker/localapi, service and
// store; a rename would silently unpin the service user.
func TestPipeUserSIDEnvName(t *testing.T) {
	if PipeUserSIDEnv != "MINION_PIPE_USER_SID" {
		t.Fatalf("PipeUserSIDEnv = %q", PipeUserSIDEnv)
	}
}
