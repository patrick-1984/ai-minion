package cli

import (
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

func claudeStateEnabled(dir string, lookup func(string) (string, bool)) bool {
	cfg, err := config.LoadFile(filepath.Join(dir, config.WorkerConfigFile))
	if err != nil {
		return false
	}
	if value, _, err := cfg.Lookup(config.KeyUsageClaudeExporter); err != nil || value == "off" {
		return false
	}
	res := config.Resolver{Role: config.ScopeWorker, File: cfg, LookupEnv: lookup}
	v, err := res.Resolve(config.KeyUsageClaudeExporter)
	return err == nil && v.Value == "auto"
}

func parseClaudeHarnessEvent(mode string, raw []byte, grant string, at time.Time) (store.HarnessEvent, bool) {
	var n struct {
		Session      string `json:"session_id"`
		Event        string `json:"hook_event_name"`
		Agent        string `json:"agent_id"`
		Task         string `json:"task_id"`
		Prompt       string `json:"prompt_id"`
		Source       string `json:"source"`
		Notification string `json:"notification_type"`
		Background   []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"background_tasks"`
	}
	if len(raw) > maxClaudeUsagePayload || json.Unmarshal(raw, &n) != nil || n.Session == "" || len(n.Session) > 256 || (mode != "status-line" && mode != n.Event) {
		return store.HarnessEvent{}, false
	}
	e := store.HarnessEvent{Harness: "claude", NativeID: ops.UsageID("claude-state-session", grant, n.Session), At: at}
	id := func(value string) string {
		if value == "" || len(value) > 256 {
			return ""
		}
		return ops.UsageID("claude-state-entity", grant, n.Session, value)
	}
	switch mode {
	case "status-line":
		e.Kind = "heartbeat"
	case "SessionStart":
		if n.Source == "compact" {
			return e, false
		}
		e.Kind = "session_start"
	case "SessionEnd":
		e.Kind = "session_end"
	case "UserPromptSubmit":
		e.Kind, e.Entity = "turn_start", id(n.Prompt)
	case "Stop", "StopFailure":
		e.Kind, e.Entity = "turn_end", id(n.Prompt)
	case "TaskCreated":
		e.Kind, e.Entity = "task_start", id(n.Task)
	case "TaskCompleted":
		e.Kind, e.Entity = "task_end", id(n.Task)
	case "SubagentStart":
		e.Kind, e.Entity = "subagent_start", id(n.Agent)
	case "SubagentStop":
		e.Kind, e.Entity = "subagent_end", id(n.Agent)
	case "PermissionRequest":
		e.Kind = "wait_permission"
	case "Elicitation":
		e.Kind = "wait_input"
	case "PostToolBatch":
		e.Kind = "batch_end"
	case "ElicitationResult", "PermissionDenied":
		e.Kind = "resume"
	case "Notification":
		switch n.Notification {
		case "permission_prompt":
			e.Kind = "wait_permission"
		case "elicitation_dialog", "elicitation_url_dialog", "agent_needs_input":
			e.Kind = "wait_input"
		case "idle_prompt":
			e.Kind = "idle"
		case "elicitation_complete", "elicitation_response":
			e.Kind = "resume"
		default:
			return e, false
		}
	default:
		return e, false
	}
	// Tool batches run independently in each agent. Only the agent
	// whose batch completed can release that agent's pending wait.
	switch e.Kind {
	case "wait_permission", "wait_input", "resume", "batch_end":
		e.Entity = id(n.Agent)
	}
	if n.Background != nil && (mode == "Stop" || mode == "SubagentStop") {
		if len(n.Background) > 256 {
			return e, false
		}
		e.Registry = make(map[string]string, len(n.Background))
		for _, task := range n.Background {
			key := id(task.ID)
			if key == "" {
				return e, false
			}
			kind := task.Type
			switch kind {
			case "shell", "subagent", "workflow":
			default:
				kind = "background task"
			}
			e.Registry[key] = kind
		}
	}
	return e, true
}
