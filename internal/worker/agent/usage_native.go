package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

type NativeUsageOptions struct {
	ObservedAt                                                   time.Time
	Format, Session, NativeID, Harness, Model, Provider, EventID string
	Account                                                      *ops.UsageAccount
}

type claudeUsageWindow struct {
	Percent *float64 `json:"used_percentage"`
	Reset   *int64   `json:"resets_at"`
}

func claudeUsage(raw []byte, s *ops.UsageSession, a *ops.UsageAccount) (string, error) {
	var n struct {
		Session string `json:"session_id"`
		Version string `json:"version"`
		Model   struct {
			ID string `json:"id"`
		} `json:"model"`
		Workspace struct {
			Dir string `json:"current_dir"`
		} `json:"workspace"`
		Cost struct {
			Estimated *float64 `json:"total_cost_usd"`
			Duration  *int64   `json:"total_duration_ms"`
		} `json:"cost"`
		Context struct {
			Current *int64 `json:"total_input_tokens"`
			Limit   *int64 `json:"context_window_size"`
		} `json:"context_window"`
		Cache struct {
			Writes *int64 `json:"cache_write_tokens"`
		} `json:"prompt_cache"`
		Limits struct {
			Five *claudeUsageWindow `json:"five_hour"`
			Week *claudeUsageWindow `json:"seven_day"`
		} `json:"rate_limits"`
	}
	if err := json.Unmarshal(raw, &n); err != nil {
		return "", err
	}
	s.Harness = "claude"
	s.Version = n.Version
	s.Model = n.Model.ID
	s.Folder = n.Workspace.Dir
	s.EstimatedCostUSD = n.Cost.Estimated
	s.DurationMS = n.Cost.Duration
	s.ContextTokens = n.Context.Current
	s.ContextLimit = n.Context.Limit
	s.CacheWriteTokens = n.Cache.Writes
	if a != nil && a.AuthMode == "subscription" && a.Provider == "Anthropic" {
		a.Source = "claude-statusline"
		a.ObservedAt = s.ObservedAt
		for i, window := range []*claudeUsageWindow{n.Limits.Five, n.Limits.Week} {
			if window == nil {
				continue
			}
			duration := []int64{5 * 3600000, 7 * 86400000}[i]
			w := ops.UsageWindow{Scope: "general", Period: "rolling", DurationMS: &duration, UsedPercent: window.Percent}
			if window.Reset != nil && *window.Reset > 0 && *window.Reset < 253402300800 {
				x := *window.Reset * 1000
				w.ResetsAt = &x
			}
			a.Windows = append(a.Windows, w)
		}
	}
	return n.Session, nil
}

func codexUsage(raw []byte, s *ops.UsageSession) (string, error) {
	var n struct {
		Method string `json:"method"`
		Params struct {
			Thread string `json:"threadId"`
			Tokens struct {
				Total struct {
					Input     *int64 `json:"inputTokens"`
					Output    *int64 `json:"outputTokens"`
					Read      *int64 `json:"cachedInputTokens"`
					Write     *int64 `json:"cacheWriteInputTokens"`
					Reasoning *int64 `json:"reasoningOutputTokens"`
				} `json:"total"`
				Limit *int64 `json:"modelContextWindow"`
			} `json:"tokenUsage"`
		} `json:"params"`
	}
	if err := json.Unmarshal(raw, &n); err != nil || n.Method != "thread/tokenUsage/updated" {
		return "", errors.New("expected a Codex token usage notification")
	}
	t := n.Params.Tokens.Total
	s.Harness = "codex"
	s.InputTokens = t.Input
	s.OutputTokens = t.Output
	s.CacheReadTokens = t.Read
	s.CacheWriteTokens = t.Write
	s.ReasoningTokens = t.Reasoning
	s.ContextLimit = n.Params.Tokens.Limit
	return n.Params.Thread, nil
}

func sumNativeModels(models []ops.UsageModel, pick func(ops.UsageCounters) *int64) *int64 {
	var sum int64
	for _, m := range models {
		v := pick(m.UsageCounters)
		if v == nil || *v < 0 || *v > math.MaxInt64-sum {
			return nil
		}
		sum += *v
	}
	return &sum
}

func geminiUsage(raw []byte, s *ops.UsageSession) error {
	type model struct {
		Tokens struct {
			Input    *int64 `json:"prompt"`
			Output   *int64 `json:"candidates"`
			Cached   *int64 `json:"cached"`
			Thoughts *int64 `json:"thoughts"`
		} `json:"tokens"`
	}
	var n struct {
		Stats struct {
			Models map[string]model `json:"models"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(raw, &n); err != nil || len(n.Stats.Models) == 0 || len(n.Stats.Models) > 32 {
		return errors.New("expected Gemini per-model statistics")
	}
	for name, m := range n.Stats.Models {
		s.Models = append(s.Models, ops.UsageModel{Model: name, UsageCounters: ops.UsageCounters{InputTokens: m.Tokens.Input, OutputTokens: m.Tokens.Output, CacheReadTokens: m.Tokens.Cached, ReasoningTokens: m.Tokens.Thoughts}})
	}
	sort.Slice(s.Models, func(i, j int) bool { return s.Models[i].Model < s.Models[j].Model })
	s.Harness = "gemini"
	if len(s.Models) == 1 {
		s.Model = s.Models[0].Model
	}
	s.InputTokens = sumNativeModels(s.Models, func(c ops.UsageCounters) *int64 { return c.InputTokens })
	s.OutputTokens = sumNativeModels(s.Models, func(c ops.UsageCounters) *int64 { return c.OutputTokens })
	s.CacheReadTokens = sumNativeModels(s.Models, func(c ops.UsageCounters) *int64 { return c.CacheReadTokens })
	s.ReasoningTokens = sumNativeModels(s.Models, func(c ops.UsageCounters) *int64 { return c.ReasoningTokens })
	return nil
}

func claudeHook(raw []byte, s *ops.UsageSession) (nativeID, kind, eventID string, err error) {
	var n struct {
		Session string `json:"session_id"`
		Event   string `json:"hook_event_name"`
		Agent   string `json:"agent_id"`
		Task    string `json:"task_id"`
		Cwd     string `json:"cwd"`
	}
	if err = json.Unmarshal(raw, &n); err != nil {
		return
	}
	kind = map[string]string{"SessionStart": "session_started", "SessionEnd": "session_stopped", "UserPromptSubmit": "turn_started", "Stop": "turn_finished", "SubagentStart": "subagent_started", "SubagentStop": "subagent_finished", "TaskCreated": "task_started", "TaskCompleted": "task_finished", "CwdChanged": "folder_changed", "PostModelSwitch": "model_changed"}[n.Event]
	if kind == "" {
		err = errors.New("unsupported lifecycle hook")
		return
	}
	s.Harness = "claude"
	s.Folder = n.Cwd
	switch kind {
	case "subagent_started", "subagent_finished":
		eventID = n.Agent
	case "task_started", "task_finished":
		eventID = n.Task
	}
	return n.Session, kind, eventID, nil
}

// ParseNativeUsage decodes only recognized, typed metrics from the document
// deliberately supplied to the local exporter. It never opens the transcript
// or paths named by a native status document.
func ParseNativeUsage(raw []byte, opts NativeUsageOptions, now time.Time) (ops.UsageReport, error) {
	var zero ops.UsageReport
	if len(raw) > 256<<10 || len(bytes.TrimSpace(raw)) == 0 || opts.Session == "" {
		return zero, errors.New("expected a session name and at most 256 KiB of JSON")
	}
	s := ops.UsageSession{Name: opts.Session, Source: opts.Format, Harness: opts.Harness, Provider: opts.Provider, Model: opts.Model, AuthMode: "unknown", ObservedAt: now.UnixMilli()}
	s.TimeSource = "worker_received"
	if !opts.ObservedAt.IsZero() {
		s.TimeSource = "native"
		s.ObservedAt = opts.ObservedAt.UnixMilli()
		if s.ObservedAt <= 0 || opts.ObservedAt.After(now) {
			return zero, errors.New("invalid native observation time")
		}
	}
	var a *ops.UsageAccount
	if opts.Account != nil {
		x := *opts.Account
		x.Windows = nil
		x.ObservedAt = s.ObservedAt
		x.TimeSource = s.TimeSource
		a = &x
		s.AccountID = x.ID
		s.Provider = x.Provider
		s.AuthMode = x.AuthMode
	}
	native := opts.NativeID
	var kind, eventID string
	var err error
	switch opts.Format {
	case "claude-statusline":
		native, err = claudeUsage(raw, &s, a)
	case "codex-token-usage":
		native, err = codexUsage(raw, &s)
	case "gemini-result":
		err = geminiUsage(raw, &s)
	case "gemini-quota":
		var quota struct {
			Buckets []struct {
				Model    string   `json:"modelId"`
				Unit     string   `json:"tokenType"`
				Fraction *float64 `json:"remainingFraction"`
				Reset    string   `json:"resetTime"`
			} `json:"buckets"`
		}
		if a == nil || a.AuthMode != "subscription" || a.Provider != "Google" {
			return zero, errors.New("gemini quota needs a Google subscription identity")
		}
		if err = json.Unmarshal(raw, &quota); err == nil && len(quota.Buckets) <= 32 {
			a.Source = "gemini-quota"
			s.Harness = "gemini"
			for _, b := range quota.Buckets {
				if b.Model == "" {
					continue
				}
				w := ops.UsageWindow{Scope: b.Model, Period: "fixed", Unit: b.Unit}
				if b.Fraction != nil && *b.Fraction >= 0 && *b.Fraction <= 1 {
					pct := (1 - *b.Fraction) * 100
					w.UsedPercent = &pct
				}
				if reset, err := time.Parse(time.RFC3339Nano, b.Reset); err == nil {
					ms := reset.UnixMilli()
					w.ResetsAt = &ms
				}
				a.Windows = append(a.Windows, w)
			}
		} else {
			err = errors.New("invalid Gemini quota")
		}
	case "claude-hook":
		native, kind, eventID, err = claudeHook(raw, &s)
	case "deepseek-response":
		var n struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage struct {
				Input   *int64 `json:"prompt_tokens"`
				Output  *int64 `json:"completion_tokens"`
				Cache   *int64 `json:"prompt_cache_hit_tokens"`
				Details struct {
					Reasoning *int64 `json:"reasoning_tokens"`
				} `json:"completion_tokens_details"`
			} `json:"usage"`
		}
		err = json.Unmarshal(raw, &n)
		if n.ID == "" || n.Usage.Input == nil || n.Usage.Output == nil {
			err = errors.New("expected native response usage")
		}
		s.Model = n.Model
		s.Provider = "DeepSeek"
		s.InputTokens = n.Usage.Input
		s.OutputTokens = n.Usage.Output
		s.CacheReadTokens = n.Usage.Cache
		s.ReasoningTokens = n.Usage.Details.Reasoning
		kind = "turn_finished"
		eventID = n.ID
	default:
		return zero, errors.New("unsupported usage format")
	}
	if err != nil || native == "" {
		return zero, errors.New("native document has no valid usage identity or supported fields")
	}
	s.NativeID = ops.UsageID("native-session", s.Harness, native)
	s.ID = s.NativeID
	r := ops.UsageReport{Version: ops.UsageVersion, ObservedAt: now.UnixMilli(), Sessions: []ops.UsageSession{s}}
	if a != nil {
		r.Accounts = []ops.UsageAccount{*a}
	}
	if kind != "" {
		if opts.EventID != "" {
			eventID = opts.EventID
		}
		if eventID == "" {
			eventID = strconv.FormatInt(now.UnixNano(), 10)
		}
		r.Events = []ops.UsageEvent{{ID: ops.UsageID("event", s.NativeID, kind, eventID), NativeID: s.NativeID, AccountID: s.AccountID, Kind: kind, Source: s.Source, ObservedAt: s.ObservedAt}}
	}
	if err := r.Check(now); err != nil {
		return zero, err
	}
	return r, nil
}
