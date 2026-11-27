package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/worker/devctl"
	"github.com/patrick-1984/ai-minion/internal/worker/localapi"
)

// Module name and start order of device control (on-device adb). It starts
// after the core module, whose executor it extends, and before the local
// socket and the op server, so the adb_* ops are there from the first request
// and the first hello.
const (
	// ModuleDeviceControl serves the adb_* ops of an Android Worker.
	ModuleDeviceControl = "devctl"
	// OrderDeviceControl is its start order.
	OrderDeviceControl = 160
)

// deviceShareWatch is how often device control checks that the Worker is
// still unshared.
const deviceShareWatch = 5 * time.Second

func init() { RegisterModule(ModuleDeviceControl, OrderDeviceControl, startDeviceControl) }

// deviceControlGOOS is the seam through which tests start the module off
// Android.
var deviceControlGOOS = runtime.GOOS

// deviceControlStdin is where the app writes the bridge token line.
var deviceControlStdin = func() *os.File { return os.Stdin }

// startDeviceControl attaches device control to the executor on Android only.
// Elsewhere the adb_* ops are not served (unsupported_op) and the status has
// no device_control block. The bridge socket is the app's
// <runtime dir>/adb-bridge.sock; the token comes on stdin.
func startDeviceControl(ctx context.Context, env *Env) (StopFunc, error) {
	if deviceControlGOOS != "android" {
		return nil, nil
	}
	ex, err := Require[*Executor](env)
	if err != nil {
		return nil, err
	}
	s := env.Settings()
	log := s.logger()
	path, err := deviceBridgePath()
	var bridge *devctl.Client
	tok := &devctl.Token{}
	if err == nil {
		bridge = devctl.NewClient(path, tok.Get)
		if in := deviceControlStdin(); in != nil {
			go tok.ReadLine(in)
		}
	} else {
		log.Warn("device control has no bridge socket path", "error", err)
	}
	auditPath := ""
	if s.StateDir != "" {
		auditPath = filepath.Join(s.StateDir, "adb", "audit.log")
	}
	audit := devctl.NewAuditLog(auditPath, func(e devctl.Entry) {
		// The Worker log carries the entry to the remote log sinks (OTEL).
		log.Info("device control", "event", "device_control.audit", "actor", e.Actor, "session", e.Session,
			"op", e.Op, "transport", e.Transport, "result", e.Result, "duration_ms", e.DurationMS)
	})
	svc := devctl.New(devctl.Config{
		GOOS: deviceControlGOOS, Bridge: bridge, ShareMode: ex.DeviceShareMode, Panes: ex.DevicePanes,
		Audit: audit, Log: log,
	})
	ex.AttachDeviceControl(svc)
	wctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	go svc.Watch(wctx, deviceShareWatch)
	return func(context.Context) error { cancel(); return nil }, nil
}

// BridgeSocketEnv names the bridge socket the app started for this Worker.
const BridgeSocketEnv = "MINION_ADB_BRIDGE_SOCKET"

// deviceBridgePath is the bridge socket: the app's MINION_ADB_BRIDGE_SOCKET
// when it is an absolute path to an adb-bridge.sock, else adb-bridge.sock next
// to worker.sock.
func deviceBridgePath() (string, error) {
	if p := os.Getenv(BridgeSocketEnv); p != "" && filepath.IsAbs(p) && filepath.Base(p) == devctl.SocketName {
		return filepath.Clean(p), nil
	}
	sock, err := localapi.SocketPath(config.SystemPathEnv())
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(sock), devctl.SocketName), nil
}
