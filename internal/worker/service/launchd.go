package service

import (
	"context"
	"encoding/xml"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// LaunchdLabel is the label of the launchd agent.
const LaunchdLabel = "io.github.patrick-1984.minion-worker"

// launchdPath is the PATH of the agent. launchd starts agents with a minimal
// PATH that lacks the Homebrew directories where tmux lives.
const launchdPath = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

// PlistParams are the inputs of the launchd agent plist.
type PlistParams struct {
	// Binary is the absolute path of the minion binary.
	Binary string
	// Home is the home directory of the user; the Worker starts there.
	Home string
	// LogFile is where the agent's stdout and stderr go.
	LogFile string
	// StateDir, when set, is pinned as MINION_STATE_DIR in the agent's
	// environment.
	StateDir string
}

// Plist returns the launchd agent property list: it runs `minion worker run`
// when the user logs in and keeps it running (KeepAlive), restarting no more
// often than every 5 seconds.
func Plist(p PlistParams) (string, error) {
	if err := validateBinary(p.Binary); err != nil {
		return "", err
	}
	stateEnv := ""
	if p.StateDir != "" {
		if err := validateStateDir(p.StateDir); err != nil {
			return "", err
		}
		stateEnv = "\n\t\t<key>" + stateEnvName + "</key>\n\t\t<string>" + esc(p.StateDir) + "</string>"
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + esc(LaunchdLabel) + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + esc(p.Binary) + `</string>
		<string>worker</string>
		<string>run</string>
	</array>
	<key>WorkingDirectory</key>
	<string>` + esc(p.Home) + `</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>` + esc(launchdPath) + `</string>` + stateEnv + `
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>5</integer>
	<key>StandardOutPath</key>
	<string>` + esc(p.LogFile) + `</string>
	<key>StandardErrorPath</key>
	<string>` + esc(p.LogFile) + `</string>
</dict>
</plist>
`)
	return b.String(), nil
}

func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// PlistPath returns where the agent plist of the user at home lives.
func PlistPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", LaunchdLabel+".plist")
}

func (d Deps) installLaunchd(ctx context.Context, req Request) (Result, error) {
	params := PlistParams{
		Binary:  req.Binary,
		Home:    req.Target.Home,
		LogFile: filepath.Join(req.StateDir, "worker.log"),
	}
	if req.PinStateDir {
		params.StateDir = req.StateDir
	}
	plist, err := Plist(params)
	if err != nil {
		return Result{}, err
	}
	path := PlistPath(req.Target.Home)
	if err := d.writePlist(path, plist, req.Target); err != nil {
		return Result{}, err
	}

	domain := "gui/" + strconv.Itoa(req.Target.UID)
	target := domain + "/" + LaunchdLabel
	// An agent that is loaded already must be unloaded first, or bootstrap
	// fails; "not loaded" is not an error here.
	_, _ = d.Runner.Run(ctx, "launchctl", "bootout", target)
	for _, step := range [][]string{
		{"bootstrap", domain, path},
		{"enable", target},
		{"kickstart", "-k", target},
	} {
		if out, err := d.Runner.Run(ctx, "launchctl", step...); err != nil {
			return Result{}, fmt.Errorf("launchctl %s failed: %w%s", strings.Join(step, " "), err, tail(out))
		}
	}
	if err := d.waitLaunchdRunning(ctx, target); err != nil {
		return Result{}, err
	}
	return Result{Manager: ManagerLaunchd, Scope: ScopeUser, Unit: LaunchdLabel, Path: path}, nil
}

func (d Deps) waitLaunchdRunning(ctx context.Context, target string) error {
	state := "not loaded"
	for i := 0; i < d.activePolls(); i++ {
		out, err := d.Runner.Run(ctx, "launchctl", "print", target)
		if err == nil {
			state = "loaded"
			if strings.Contains(out, "state = running") {
				d.sleep(ctx, d.settle())
				again, err := d.Runner.Run(ctx, "launchctl", "print", target)
				if err == nil && strings.Contains(again, "state = running") {
					return nil
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		d.sleep(ctx, d.pollInterval())
	}
	return fmt.Errorf("the launchd agent %s is not running (%s); see its log or run: launchctl print %s", LaunchdLabel, state, target)
}

// writePlist writes the agent plist of the target user. Running as root on
// behalf of that user, the directory is opened through [OpenTrustedDir]: the
// user owns ~/Library and could replace a directory by a link to another
// account between a check and the write. Without a [Chowner] the user is the
// caller and nothing is trusted or given away.
func (d Deps) writePlist(path, plist string, t Target) error {
	if d.Chown == nil {
		if err := writeIfChanged(path, plist, 0o755, 0o644); err != nil {
			return fmt.Errorf("install the launchd agent: %w", err)
		}
		return nil
	}
	root, err := OpenTrustedDir(DirSpec{Path: filepath.Dir(path), Create: true, Mode: 0o755, UID: t.UID, GID: t.GID, Chown: d.Chown, LinkOwner: d.LinkOwner})
	if err != nil {
		return fmt.Errorf("open the launchd agent directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	if err := WriteFileIn(root, filepath.Base(path), plist, 0o644, t.UID, t.GID, d.Chown); err != nil {
		return fmt.Errorf("install the launchd agent: %w", err)
	}
	return nil
}
