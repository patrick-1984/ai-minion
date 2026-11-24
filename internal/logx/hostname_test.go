package logx

import "testing"

func TestLogHostnameEnvOverride(t *testing.T) {
	t.Setenv(HostnameEnv, "minion-hub")
	if got := LogHostname(); got != "minion-hub" {
		t.Errorf("got %q", got)
	}
	t.Setenv(HostnameEnv, "bad name")
	if got := LogHostname(); got == "bad name" {
		t.Error("a name with a space was accepted")
	}
}
