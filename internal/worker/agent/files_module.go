package agent

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/filexfer"
	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Module name and start order of the file transfer end (files.v1). It starts
// after the core module (order 100), whose executor and host it needs, and
// before the connector (order 200), so that the first hello already says
// whether this Worker announces files.v1.
const (
	// ModuleFiles is the name of the file transfer module.
	ModuleFiles = "files"
	// OrderFiles is its start order.
	OrderFiles = 150
)

// FilesProbeArgs are the arguments of the Worker's own executable that run the
// files probe child (filexfer.ProbeChild): "minion worker files-probe".
var FilesProbeArgs = []string{"worker", "files-probe"}

// filesProbeTimeout bounds the probe child.
const filesProbeTimeout = 10 * time.Second

func init() { RegisterModule(ModuleFiles, OrderFiles, startFiles) }

// filesProbe runs the probe of the resolution mode (BRD 5.3) and applies it to
// the process. It is a variable so that tests of other modules, whose binary
// is a test binary that must not be run as a probe child, never start one.
var filesProbe = func(ctx context.Context, stateDir string) filexfer.ProbeResult {
	if runtime.GOOS == "windows" {
		return filexfer.ProbeResult{Mode: filexfer.ResolveUnavailable, Reason: "File transfer is not available on Windows Workers in this version"}
	}
	if testing.Testing() {
		return filexfer.ProbeResult{Mode: filexfer.ResolveUnavailable, Reason: "the files probe does not run in tests"}
	}
	exe, err := os.Executable()
	if err != nil {
		return filexfer.ProbeResult{Mode: filexfer.ResolveUnavailable, Reason: "the Worker cannot find its own executable for the files probe"}
	}
	pctx, cancel := context.WithTimeout(ctx, filesProbeTimeout)
	defer cancel()
	return filexfer.Probe(pctx, filexfer.ProbeCommand{Path: exe, Args: FilesProbeArgs}, stateDir)
}

// startFiles probes the resolution, builds the file transfer end and attaches
// it to the executor. A Worker whose probe fails still runs: it does not
// announce files.v1 and reports why (Files.Status).
func startFiles(ctx context.Context, env *Env) (StopFunc, error) {
	s := env.Settings()
	log := s.logger()
	ex, err := Require[*Executor](env)
	if err != nil {
		return nil, err
	}
	pr := filesProbe(ctx, s.StateDir)
	filexfer.ApplyProbe(pr)
	home, _ := os.UserHomeDir()
	cfgPath := filepath.Join(s.StateDir, config.WorkerConfigFile)
	cfg := FilesConfig{
		Home: home, StateDir: s.StateDir, Logger: log,
		Mode: func() string { return config.ResolveFileTransfer(s.Resolver, cfgPath) },
	}
	if pr.Mode == filexfer.ResolveUnavailable {
		cfg.Unavailable = pr.Reason
		if cfg.Unavailable == "" {
			cfg.Unavailable = "this system offers no safe way to confine file transfers"
		}
	}
	floors := &floorCache{base: filexfer.FloorConfig{Home: home, StateDir: s.StateDir}}
	cfg.Floor = func() *filexfer.Floor {
		return floors.get(config.ResolveFileTransferLocal(s.Resolver, cfgPath))
	}
	if runtime.GOOS == "android" && home != "" {
		cfg.Confine, cfg.ConfineLabel = AndroidConfine(home), ConfineLabelAndroid
		if err := os.Mkdir(cfg.Confine, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			log.Warn("cannot create the Minion files folder; file transfer is off", "error", err.Error())
			cfg.Unavailable = "the Minion files folder cannot be created"
		}
	}
	f := NewFiles(ex, cfg)
	ex.AttachFiles(f)
	if runtime.GOOS == "android" && cfg.Confine != "" && s.StateDir != "" {
		// The app of this build reads the app install requests (v0.25.0).
		ex.AttachAppInstall(s.StateDir, cfg.Confine, cfg.Floor)
	}
	if st := f.Status(); st != "" {
		log.Info("file transfer is not available on this Worker", "reason", st)
	} else {
		log.Info("file transfer available", "resolve", pr.Mode.String())
	}
	if s.StateDir != "" && cfg.Unavailable == "" {
		// Staged files of a transfer that ended with the process (BRD 5.8).
		if n, err := filexfer.NewStagingLog(s.StateDir).Sweep(proto.StaleStagingAge, time.Now()); err == nil && n > 0 {
			log.Info("removed stale staged files", "files", n)
		}
	}
	return nil, nil
}

// AndroidConfine returns the "Minion files" folder of the Android app Worker
// under its home folder (<noBackupFilesDir>/home, BRD 5.4).
func AndroidConfine(home string) string { return filepath.Join(home, ConfineLabelAndroid) }

// floorCache keeps the identity floor and records it again when the local
// relaxations change (BRD 5.3: refreshed when the config changes).
type floorCache struct {
	base  filexfer.FloorConfig
	mu    sync.Mutex
	local config.FileTransferLocal
	floor *filexfer.Floor
}

func (c *floorCache) get(local config.FileTransferLocal) *filexfer.Floor {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.floor == nil || c.local != local {
		cfg := c.base
		cfg.AllowHome, cfg.ProtectedOff = local.AllowHome, local.ProtectedOff
		c.floor, c.local = filexfer.NewFloor(cfg), local
	}
	return c.floor
}
