package config

import (
	"slices"
	"testing"
)

func TestResolveTightenOnlyAnyOffBlocks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flag    string
		env     string
		file    string
		blocked bool
		sources []Source
	}{
		{name: "nothing set", blocked: false},
		{name: "all on", flag: "on", env: "on", file: "on", blocked: false},
		{name: "file off, env on", env: "on", file: "off", blocked: true, sources: []Source{SourceFile}},
		{name: "flag on does not unblock the file", flag: "on", file: "off", blocked: true, sources: []Source{SourceFile}},
		{name: "env off", env: "off", file: "on", blocked: true, sources: []Source{SourceEnv}},
		{name: "unparsable blocks", file: "maybe", blocked: true, sources: []Source{SourceFile}},
		{name: "two sources", flag: "off", env: "OFF", blocked: true, sources: []Source{SourceFlag, SourceEnv}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := &Resolver{Role: ScopeWorker,
				Flags:     MapSource(nonEmpty(KeyAllowNewSessions, tc.flag)),
				File:      MapSource(nonEmpty(KeyAllowNewSessions, tc.file)),
				LookupEnv: func(k string) (string, bool) { return tc.env, k == "MINION_ALLOW_NEW_SESSIONS" && tc.env != "" },
			}
			got, err := ResolveTightenOnly(res, KeyAllowNewSessions)
			if err != nil {
				t.Fatal(err)
			}
			if got.Blocked != tc.blocked || !slices.Equal(got.Sources, tc.sources) {
				t.Fatalf("got blocked %t sources %v, want %t %v", got.Blocked, got.Sources, tc.blocked, tc.sources)
			}
			wc, err := ResolveWorkerCore(&Resolver{Role: ScopeWorker, Flags: res.Flags, File: res.File, LookupEnv: func(k string) (string, bool) {
				if k == "MINION_STATE_DIR" {
					return t.TempDir(), true
				}
				if k == "MINION_TMUX_SOCKET" {
					return "/tmp/x.sock", true
				}
				return res.LookupEnv(k)
			}})
			if err != nil {
				t.Fatal(err)
			}
			if wc.NewSessions.Blocked != tc.blocked {
				t.Fatalf("WorkerCore.NewSessions = %+v", wc.NewSessions)
			}
		})
	}
	if _, err := ResolveTightenOnly(&Resolver{Role: ScopeWorker}, KeyName); err == nil {
		t.Fatal("name is not tighten-only")
	}
	if !IsTightenOnly(KeyAllowNewSessions) || IsTightenOnly(KeyPreviewOnly) {
		t.Fatal("IsTightenOnly")
	}
}

func nonEmpty(k, v string) map[string]string {
	if v == "" {
		return map[string]string{}
	}
	return map[string]string{k: v}
}
