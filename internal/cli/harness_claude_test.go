package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestClaudeStopProjectsNativeRegistry(t *testing.T) {
	raw := []byte(`{"session_id":"private-id","prompt_id":"private-turn","hook_event_name":"Stop","last_assistant_message":"private message","background_tasks":[{"id":"private-shell","type":"shell","command":"private command"},{"id":"private-workflow","type":"workflow","name":"private name"},{"id":"private-monitor","type":"monitor","server":"private server"}],"session_crons":[{"prompt":"private schedule"}]}`)
	event, ok := parseClaudeHarnessEvent("Stop", raw, "grant", time.Now())
	if !ok || event.Kind != "turn_end" || len(event.Entity) != 64 || len(event.Registry) != 3 {
		t.Fatalf("registry: %+v", event)
	}
	seen := map[string]int{}
	for _, kind := range event.Registry {
		seen[kind]++
	}
	if seen["shell"] != 1 || seen["workflow"] != 1 || seen["background task"] != 1 {
		t.Fatalf("types: %v", seen)
	}
	projected, _ := json.Marshal(event)
	if strings.Contains(string(projected), "private") {
		t.Fatal("unprojected input retained")
	}
	for _, tc := range []struct {
		raw    string
		absent bool
	}{
		{`{"session_id":"x","hook_event_name":"Stop","background_tasks":[]}`, false},
		{`{"session_id":"x","hook_event_name":"Stop"}`, true},
	} {
		event, ok := parseClaudeHarnessEvent("Stop", []byte(tc.raw), "grant", time.Now())
		if !ok || (event.Registry == nil) != tc.absent {
			t.Fatalf("absent/empty lost: %+v", event)
		}
	}
	if _, ok := parseClaudeHarnessEvent("SubagentStop", raw, "grant", time.Now()); ok {
		t.Fatal("wrong hook accepted")
	}
}

func TestClaudeNativeWaitEvents(t *testing.T) {
	for notification, kind := range map[string]string{"permission_prompt": "wait_permission", "idle_prompt": "idle", "elicitation_dialog": "wait_input", "elicitation_response": "resume", "auth_success": ""} {
		raw := []byte(`{"hook_event_name":"Notification","session_id":"s","notification_type":"` + notification + `","message":"never read this"}`)
		e, ok := parseClaudeHarnessEvent("Notification", raw, "grant", time.Now())
		if ok != (kind != "") || e.Kind != kind {
			t.Fatalf("%s: %+v (%t)", notification, e, ok)
		}
	}
}
