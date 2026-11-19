package agent

import (
	"context"
	"encoding/json"
	"path/filepath"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

// Live apply of a saved setting (tray W4b). Only the tighten-only
// allow_new_sessions can be applied to a running Worker; every other setting
// is used at the next start.
const (
	// OpLocalConfigApply asks the running Worker to apply one saved setting.
	OpLocalConfigApply = "worker_config_apply"
	// FeatureConfigApply is its local capability.
	FeatureConfigApply = "config_apply.v1"
)

// WorkerConfigFile is the name of the Worker config file in the state
// directory.
const WorkerConfigFile = "config.json"

// LocalConfigApplyArgs are the arguments of [OpLocalConfigApply]: the key and
// the instance that answered worker_status.
type LocalConfigApplyArgs struct {
	Key      string `json:"key"`
	Instance string `json:"instance"`
}

// ConfigApplyResult is the reply of [OpLocalConfigApply]: the value now in
// force and the local sources that restrict it.
type ConfigApplyResult struct {
	Key       string   `json:"key"`
	Effective string   `json:"effective"`
	Sources   []string `json:"sources"`
}

// RereadTightenOnly resolves the tighten-only key from the config file as it
// is on disk now, with the flags and the environment of s (the Worker's
// original snapshot), never a cached copy of the file. An unreadable file
// restricts.
func RereadTightenOnly(s Settings, key string) config.Tightened {
	res := &config.Resolver{Role: config.ScopeWorker}
	if s.Resolver != nil {
		res.Registry, res.Flags, res.LookupEnv = s.Resolver.Registry, s.Resolver.Flags, s.Resolver.LookupEnv
	}
	unreadable := false
	if s.StateDir != "" {
		if f, err := config.LoadFile(filepath.Join(s.StateDir, WorkerConfigFile)); err == nil {
			res.File = f
		} else {
			unreadable = true
		}
	}
	t, err := config.ResolveTightenOnly(res, key)
	if err != nil {
		unreadable = true
	}
	if unreadable {
		t.Blocked = true
		t.Sources = append(t.Sources, config.SourceFile)
	}
	return t
}

// configApplier applies a saved setting in the running Worker.
type configApplier struct {
	settings Settings
	ex       *Executor
}

// apply rereads key and puts it in force.
func (a configApplier) apply(key string) (ConfigApplyResult, error) {
	if key != config.KeyAllowNewSessions {
		return ConfigApplyResult{}, apiErr(proto.CodeInvalidArgument, "",
			"only allow_new_sessions can be applied to a running Worker; restart the Worker to apply "+key)
	}
	// The file is reread under the exclusive create lock, not before it: a
	// handler that waited there for the creations in flight must not install
	// a value that another CLI saved over meanwhile (W4b, gate 1).
	var res ConfigApplyResult
	a.ex.ApplyNewSessions(func() (bool, []string) {
		t := RereadTightenOnly(a.settings, key)
		src := sourceNames(t.Sources)
		res = ConfigApplyResult{Key: key, Effective: t.Value(key), Sources: src}
		return t.Blocked, src
	})
	return res, nil
}

// configApplyOp serves [OpLocalConfigApply].
func (l localExecutor) configApplyOp(_ context.Context, rq localapi.Request) (json.RawMessage, error) {
	if l.configApply == nil {
		return nil, apiErr(proto.CodeUnsupportedOp, "", "this Worker cannot apply settings live")
	}
	var args LocalConfigApplyArgs
	if err := json.Unmarshal(rq.Args, &args); err != nil || args.Key == "" {
		return nil, apiErr(proto.CodeInvalidArgument, "", "invalid config apply request: give the key")
	}
	if args.Instance != "" && args.Instance != l.instance {
		return nil, apiErr(proto.CodeConflict, "state_changed", "the Worker restarted since its status was read; nothing was applied")
	}
	res, err := l.configApply(args.Key)
	if err != nil {
		return nil, err
	}
	return json.Marshal(res)
}
