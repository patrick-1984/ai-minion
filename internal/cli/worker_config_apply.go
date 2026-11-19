package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/patrick-1984/ai-minion/internal/client"
	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

// tightenApplier puts a saved tighten-only setting (allow_new_sessions) in
// force (tray W4b). The order is: save (done by the caller), then probe the
// lifecycle lock. A free lock means no Worker runs, and the next one rereads
// the file when it takes the lock, so the saved value is what starts. A held
// lock means a Worker runs (or is starting): it is asked to apply the change
// until a deadline; only its confirmation that the requested value is in
// force is a success.
type tightenApplier struct {
	lockState func(stateDir string) (io.Closer, error)
	call      func(ctx context.Context, op string, args any) (json.RawMessage, error)
	// deadline bounds the live apply (5 s); poll is the pause between tries.
	deadline time.Duration
	poll     time.Duration
}

func defaultTightenApplier() tightenApplier {
	return tightenApplier{
		lockState: agent.LockLifecycle,
		call: func(ctx context.Context, op string, args any) (json.RawMessage, error) {
			return client.Local().Call(ctx, op, args)
		},
		deadline: 5 * time.Second,
		poll:     200 * time.Millisecond,
	}
}

// tightenOutcome is what applyTighten found.
type tightenOutcome struct {
	// Live is true when a running Worker confirmed the value in force.
	Live      bool
	Effective string
	Sources   []string
}

// errTightenStale is a live apply that met another Worker instance twice.
var errTightenStale = errors.New("the Worker restarted while the change was applied")

// applyTighten returns the value in force for key after a change to
// requested. A Worker that runs but cannot confirm in time gives
// saved_not_confirmed (22); one too old to apply it gives saved_not_in_force
// (22).
func (a tightenApplier) applyTighten(ctx context.Context, stateDir, key string, offline func() config.Tightened) (tightenOutcome, error) {
	if l, err := a.lockState(stateDir); err == nil {
		_ = l.Close()
		t := offline()
		return tightenOutcome{Effective: t.Value(key), Sources: sourceStrings(t.Sources)}, nil
	} else if !errors.Is(err, agent.ErrStateInUse) {
		return tightenOutcome{}, fmt.Errorf("cannot take the lifecycle lock of %s: %w", stateDir, err)
	}
	ctx, cancel := context.WithTimeout(ctx, a.deadline)
	defer cancel()
	stale := 0
	var last string
	for {
		inst, ok := a.instance(ctx, stateDir)
		if ok {
			raw, err := a.call(ctx, localapi.QualifiedOp(agent.OpLocalConfigApply, []string{agent.FeatureConfigApply}),
				agent.LocalConfigApplyArgs{Key: key, Instance: inst})
			var pe *proto.Error
			switch {
			case err == nil:
				var r agent.ConfigApplyResult
				if err := json.Unmarshal(raw, &r); err != nil || r.Effective == "" {
					return tightenOutcome{}, &resultError{Exit: ExitNotInForce, Code: "saved_not_confirmed",
						Message: "the running Worker returned an invalid result; the value is saved, restart the Worker to apply it"}
				}
				return tightenOutcome{Live: true, Effective: r.Effective, Sources: r.Sources}, nil
			case errors.Is(err, client.ErrWorkerTooOld) || (errors.As(err, &pe) && pe.Code == proto.CodeUnsupportedOp):
				return tightenOutcome{}, &resultError{Exit: ExitNotInForce, Code: "saved_not_in_force",
					Message: "the value is saved, but the running Worker is too old to apply it now; restart the Worker to apply it"}
			case errors.As(err, &pe) && pe.Code == proto.CodeConflict:
				stale++
				if stale > 1 {
					return tightenOutcome{}, &resultError{Exit: ExitStateChanged, Message: errTightenStale.Error() + "; the value is saved, check minion worker status", Err: errTightenStale}
				}
				continue // a restarted Worker: once more with its instance
			default:
				last = err.Error()
			}
		}
		select {
		case <-ctx.Done():
			msg := "the value is saved, but the running Worker did not confirm it in time"
			if last != "" {
				msg += " (" + last + ")"
			}
			return tightenOutcome{}, &resultError{Exit: ExitNotInForce, Code: "saved_not_confirmed", Message: msg + "; check minion worker status"}
		case <-time.After(a.poll):
		}
	}
}

// instance returns the instance of the Worker that answers for stateDir.
func (a tightenApplier) instance(ctx context.Context, stateDir string) (string, bool) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	raw, err := a.call(cctx, agent.OpLocalStatus, nil)
	if err != nil {
		return "", false
	}
	var r agent.LiveStatusReply
	if json.Unmarshal(raw, &r) != nil || r.StateDir == "" || !sameDir(r.StateDir, stateDir) || r.Instance == "" {
		return "", false
	}
	return r.Instance, true
}

func sourceStrings(src []config.Source) []string {
	out := make([]string, 0, len(src))
	for _, s := range src {
		out = append(out, string(s))
	}
	return out
}

// applyTightenChange is the end of "config set|unset" for a tighten-only key:
// exit 0 only when the value in force is the one now saved.
func (d workerConfigDeps) applyTightenChange(ctx context.Context, ce *configEnv, key string) error {
	saved := config.ResolveTightenOnly
	offline := func() config.Tightened {
		t, err := saved(&config.Resolver{Registry: d.reg(), Role: config.ScopeWorker, File: ce.file, LookupEnv: d.lookupEnv}, key)
		if err != nil {
			return config.Tightened{Blocked: true, Sources: []config.Source{config.SourceFile}}
		}
		return t
	}
	want := config.AllowNewSessionsOn
	if v, ok, _ := ce.file.Lookup(key); ok && strings.TrimSpace(v) != config.AllowNewSessionsOn {
		want = config.AllowNewSessionsOff
	}
	out, err := d.tighten.applyTighten(ctx, ce.stateDir, key, offline)
	fields := map[string]any{"key": key, "saved": want, "effective": out.Effective, "sources": nonNilStrings(out.Sources), "live": out.Live}
	if err != nil {
		var re *resultError
		if errors.As(err, &re) {
			if ce.json {
				if re.Code == "" {
					re.Code = resultCode(re.Exit)
				}
				return emitResult(ce.rawOut, ce.errOut, true, fields, re)
			}
			_ = writef(ce.errOut, "Error: %s\n", re.Error())
			return &exitCodeError{re.Exit}
		}
		if ce.json {
			return emitResult(ce.rawOut, ce.errOut, true, fields, err)
		}
		return err
	}
	if out.Effective != want {
		src := strings.Join(out.Sources, ", ")
		msg := fmt.Sprintf("the value is saved, but %s stays %s: it is set to %s by %s, which the config file cannot override (tighten-only)", key, out.Effective, out.Effective, src)
		if ce.json {
			return emitResult(ce.rawOut, ce.errOut, true, fields, &resultError{Exit: ExitNotInForce, Code: "saved_overridden", Message: msg})
		}
		_ = writef(ce.errOut, "Error: %s.\n", msg)
		return &exitCodeError{ExitNotInForce}
	}
	if ce.json {
		return emitResult(ce.rawOut, ce.errOut, true, fields, nil)
	}
	if out.Live {
		return writef(ce.out, "The running Worker applied it: %s is %s.\n", key, out.Effective)
	}
	if slices.Contains(out.Sources, string(config.SourceEnv)) {
		_ = writef(ce.errOut, "note: %s in this shell's environment restricts it too.\n", "MINION_ALLOW_NEW_SESSIONS")
	}
	return writef(ce.out, "No Worker is running on this state directory; it reads %s when it starts.\n", key)
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
