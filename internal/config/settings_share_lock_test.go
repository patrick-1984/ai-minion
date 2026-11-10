package config

import "testing"

func TestShareLockSettingIsDeclaredAsASwitch(t *testing.T) {
	s, ok := Default.Lookup(KeyShareLock)
	if !ok {
		t.Fatal("share_lock is not declared")
	}
	if s.Scope != ScopeWorker || !s.HasFlag() || s.FlagName() != "share-lock" || s.EnvName() != "MINION_SHARE_LOCK" || s.Help == "" || s.Default != "false" {
		t.Fatalf("share_lock: scope %s flag %q env %q default %q", s.Scope, s.FlagName(), s.EnvName(), s.Default)
	}
	if len(s.Choices) != 2 {
		t.Fatalf("choices %v: a switch has true and false", s.Choices)
	}
}

func TestResolveWorkerCoreShareLockPrecedence(t *testing.T) {
	state := map[string]string{"MINION_STATE_DIR": t.TempDir()}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range state {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	wc, err := ResolveWorkerCore(workerResolver(nil, state, nil))
	if err != nil || wc.ShareLock {
		t.Fatalf("default: %+v %v (the lock is off unless asked for)", wc, err)
	}
	// File, then env over file, then flag over env.
	wc, err = ResolveWorkerCore(workerResolver(nil, state, map[string]string{KeyShareLock: "true"}))
	if err != nil || !wc.ShareLock {
		t.Fatalf("file: %+v %v", wc, err)
	}
	wc, err = ResolveWorkerCore(workerResolver(nil, with(map[string]string{"MINION_SHARE_LOCK": "false"}), map[string]string{KeyShareLock: "true"}))
	if err != nil || wc.ShareLock {
		t.Fatalf("env over file: %+v %v", wc, err)
	}
	wc, err = ResolveWorkerCore(workerResolver(map[string]string{KeyShareLock: "true"}, with(map[string]string{"MINION_SHARE_LOCK": "false"}), nil))
	if err != nil || !wc.ShareLock {
		t.Fatalf("flag over env: %+v %v", wc, err)
	}
	if _, err := ResolveWorkerCore(workerResolver(map[string]string{KeyShareLock: "perhaps"}, state, nil)); err == nil {
		t.Fatal("an invalid value was accepted")
	}
}
