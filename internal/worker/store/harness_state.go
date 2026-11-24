package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/worker/agentdetect"
)

// HarnessEvent contains only projected, grant-scoped identifiers. A non-nil
// Registry replaces the observed runtime task set, including an empty snapshot.
type HarnessEvent struct {
	ProcessPID                                     int
	ProcessBirth                                   string
	Session, Pane, Harness, NativeID, Kind, Entity string
	Created                                        int64
	At                                             time.Time
	Registry                                       map[string]string
}

type HarnessState struct {
	RuntimeKnown          bool // local-only completeness of the Claude runtime registry
	ProcessPID            int  // local-only foreground correlation
	State, Reason, Source string
	LastEvent             time.Time
}

const (
	harnessSilence   = 15 * time.Minute
	harnessMaxAge    = 30 * time.Minute
	maxHarnessEvents = 1024
)

func harnessDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func (s *Store) RecordHarnessEvent(ctx context.Context, grant string, e HarnessEvent) error {
	if grant == "" || e.ProcessPID <= 0 || e.ProcessBirth == "" || len(e.ProcessBirth) > 128 || !ops.ValidSessionName(e.Session) || len(e.Pane) > 16 || e.Pane == "" || e.Created <= 0 || e.At.Unix() < e.Created || e.At.After(time.Now()) ||
		(e.Harness != "codex" && e.Harness != "claude") || !harnessDigest(e.NativeID) || (e.Entity != "" && !harnessDigest(e.Entity)) || len(e.Registry) > 256 {
		return errors.New("invalid harness event binding")
	}
	switch e.Kind {
	case "task_start", "task_end", "subagent_start", "subagent_end":
		if e.Entity == "" {
			return errors.New("missing harness entity")
		}
	case "session_start", "session_end", "turn_start", "turn_end", "wait_permission", "wait_input", "resume", "batch_end", "idle", "heartbeat":
	default:
		return errors.New("unknown harness event")
	}
	for id, kind := range e.Registry {
		if !harnessDigest(id) || !slices.Contains([]string{"shell", "workflow", "subagent", "background task"}, kind) {
			return errors.New("invalid runtime task")
		}
	}
	registry, err := json.Marshal(e.Registry)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Fence changes of ownership or monitoring consent inside the writer transaction.
	change, err := tx.ExecContext(ctx, `INSERT INTO harness_events(session,pane,created,pid,birth,harness,native,kind,entity,registry,at)
 SELECT ?,?,?,?,?,?,?,?,?,?,? FROM hub_enrollment WHERE id=1 AND usage_policy=1 AND worker_id<>'' AND owner<>''
 AND hex(hub_pub)||':'||worker_id||':'||owner_id||':'||owner=?`, e.Session, e.Pane, e.Created, e.ProcessPID, e.ProcessBirth, e.Harness, e.NativeID, e.Kind, e.Entity, string(registry), e.At.UnixNano(), grant)
	if err != nil {
		return err
	}
	if n, _ := change.RowsAffected(); n != 1 {
		return errors.New("harness monitoring grant expired")
	}
	// Compact liveness independently so a rapidly redrawn status line cannot
	// crowd lifecycle observations out of the bounded replay window.
	if e.Kind == "heartbeat" {
		if _, err = tx.ExecContext(ctx, `DELETE FROM harness_events WHERE session=? AND pane=? AND created=? AND harness=? AND native=? AND kind='heartbeat' AND seq<>(SELECT seq FROM harness_events WHERE session=? AND pane=? AND created=? AND harness=? AND native=? AND kind='heartbeat' ORDER BY at DESC,seq DESC LIMIT 1)`, e.Session, e.Pane, e.Created, e.Harness, e.NativeID, e.Session, e.Pane, e.Created, e.Harness, e.NativeID); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM harness_events WHERE seq IN (SELECT seq FROM harness_events WHERE at<? ORDER BY at LIMIT 128)`, time.Now().Add(-24*time.Hour).UnixNano()); err != nil {
		return err
	}
	return tx.Commit()
}

// NativeHarnessState reads one pane generation. Overflow falls back unless
// replay reaches a session start that makes the truncated prefix immaterial.
func (s *Store) NativeHarnessState(ctx context.Context, session, pane, harness string, created int64, now time.Time) (HarnessState, bool, error) {
	// An old turn in a long-lived pane cannot displace newer observations
	// from the bounded replay. A session-start fence remains effective even
	// when its own state has aged out. Read both from one SQLite snapshot.
	rows, err := s.db.QueryContext(ctx, `WITH latest AS (
      SELECT native,pid,birth FROM harness_events WHERE session=?1 AND pane=?2 AND created=?3 AND harness=?4
        AND at<=?5 ORDER BY at DESC,seq DESC LIMIT 1
    ), generation AS (
      SELECT e.native,e.at,e.seq FROM harness_events e JOIN latest l USING(pid,birth)
      WHERE session=?1 AND pane=?2 AND created=?3 AND harness=?4 AND kind='session_start' AND at<=?5
      ORDER BY at DESC,seq DESC LIMIT 1
    )
    SELECT e.native,e.kind,e.entity,e.registry,e.at,e.pid,e.birth FROM harness_events e
      JOIN latest l USING(pid,birth) LEFT JOIN generation g ON TRUE
      WHERE session=?1 AND pane=?2 AND created=?3 AND harness=?4 AND e.at BETWEEN ?6 AND ?5
        AND e.native=coalesce(g.native,l.native) AND (g.at IS NULL OR (e.at,e.seq)>=(g.at,g.seq))
        AND EXISTS(SELECT 1 FROM hub_enrollment WHERE id=1 AND usage_policy=1)
      ORDER BY e.at DESC,e.seq DESC LIMIT ?7`, session, pane, created, harness, now.UnixNano(), now.Add(-harnessMaxAge).UnixNano(), maxHarnessEvents+1)
	if err != nil {
		return HarnessState{}, false, err
	}
	defer func() { _ = rows.Close() }()
	var events []HarnessEvent
	complete := false
	for rows.Next() {
		var e HarnessEvent
		var registry string
		var at int64
		if err := rows.Scan(&e.NativeID, &e.Kind, &e.Entity, &registry, &at, &e.ProcessPID, &e.ProcessBirth); err != nil {
			return HarnessState{}, false, err
		}
		if err := json.Unmarshal([]byte(registry), &e.Registry); err != nil {
			return HarnessState{}, false, err
		}
		e.At = time.Unix(0, at)
		events = append(events, e)
		if e.Kind == "session_start" {
			complete = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return HarnessState{}, false, err
	}
	if len(events) == 0 || (len(events) > maxHarnessEvents && !complete) {
		return HarnessState{}, false, nil
	}
	latest := events[0]
	if agentdetect.ProcessBirth(latest.ProcessPID) != latest.ProcessBirth {
		return HarnessState{}, false, nil
	}
	slices.Reverse(events)
	state, ok := projectHarness(events, harness, now)
	state.ProcessPID = latest.ProcessPID
	return state, ok, nil
}

type harnessFact struct {
	kind string
	at   time.Time
}

func projectHarness(events []HarnessEvent, harness string, now time.Time) (HarnessState, bool) {
	var out HarnessState
	out.Source = harness + "-hooks"
	native := events[0].NativeID
	foreground, turn := "", ""
	var fgAt, lastSeen time.Time
	ended := false
	var registryAt time.Time
	tasks := make(map[string]harnessFact)
	agents := make(map[string]harnessFact)
	waits := make(map[string]harnessFact)
	runtimeTasks := make(map[string]harnessFact)
	for _, e := range events {
		if e.NativeID != native || ended {
			continue
		}
		lastSeen = e.At
		if e.Kind == "heartbeat" {
			continue
		}
		if e.Kind == "turn_end" && e.Entity != "" && turn != "" && e.Entity != turn {
			continue
		}
		if e.Registry != nil {
			registryAt = e.At
			clear(runtimeTasks)
			clear(agents)
			// A full parent registry supersedes earlier detached work.
			for id, k := range e.Registry {
				runtimeTasks[id] = harnessFact{k, e.At}
			}
		}
		switch e.Kind {
		case "session_start", "idle":
			foreground, turn, fgAt = ops.PaneStateStable, "", e.At
			delete(waits, "")
		case "session_end":
			ended = true
		case "turn_start":
			foreground, turn, fgAt = ops.PaneStateBusy, e.Entity, e.At
			delete(waits, "")
		case "turn_end":
			foreground, turn, fgAt = ops.PaneStateStable, "", e.At
			delete(waits, "")
		case "wait_permission", "wait_input":
			waits[e.Entity] = harnessFact{e.Kind, e.At}
		case "batch_end", "resume":
			_, waiting := waits[e.Entity]
			activeBatch := e.Kind == "batch_end" && e.Entity == "" && foreground == ops.PaneStateBusy
			if !waiting && !activeBatch {
				continue
			}
			delete(waits, e.Entity)
			if e.Entity == "" {
				foreground, fgAt = ops.PaneStateBusy, e.At
			}
		case "task_start":
			tasks[e.Entity] = harnessFact{"open task", e.At}
		case "task_end":
			delete(tasks, e.Entity)
		case "subagent_start":
			agents[e.Entity] = harnessFact{"subagent", e.At}
		case "subagent_end":
			delete(waits, e.Entity)
			delete(agents, e.Entity)
			delete(runtimeTasks, e.Entity)
		}
		out.LastEvent = e.At
	}
	if ended || now.Sub(lastSeen) > harnessSilence || out.LastEvent.IsZero() || now.Sub(out.LastEvent) > harnessMaxAge {
		return HarnessState{}, false
	}
	out.RuntimeKnown = !registryAt.IsZero() && now.Sub(registryAt) <= harnessMaxAge
	// A parent may finish its foreground turn while a background agent
	// still has a permission dialog. Give permission deterministic priority.
	for _, kind := range []string{"wait_permission", "wait_input"} {
		for _, f := range waits {
			if f.kind == kind && now.Sub(f.at) <= harnessMaxAge {
				out.State, out.Reason = ops.PaneStateDialog, "Input requested"
				if kind == "wait_permission" {
					out.Reason = "Permission requested"
				}
				return out, true
			}
		}
	}
	counts := map[string]int{}
	for _, set := range []map[string]harnessFact{runtimeTasks, tasks} {
		for _, f := range set {
			if now.Sub(f.at) <= harnessMaxAge {
				counts[f.kind]++
			}
		}
	}
	for id, f := range agents {
		if _, ok := runtimeTasks[id]; !ok && now.Sub(f.at) <= harnessMaxAge {
			counts["subagent"]++
		}
	}
	var reasons []string
	for _, k := range []string{"shell", "workflow", "subagent", "background task", "open task"} {
		if n := counts[k]; n > 0 {
			label := k
			if n > 1 {
				label += "s"
			}
			reasons = append(reasons, fmt.Sprintf("%d %s", n, label))
		}
	}
	out.Reason = strings.Join(reasons, ", ")
	// An explicit runtime registry distinguishes pending planning items from actual execution.
	plannerOnly := len(counts) == 1 && counts["open task"] > 0 && out.RuntimeKnown
	if len(reasons) > 0 && !plannerOnly {
		out.State = ops.PaneStateBusy
		return out, true
	}
	if now.Sub(fgAt) > harnessMaxAge || foreground == "" {
		return HarnessState{}, false
	}
	out.State = foreground
	return out, true
}
