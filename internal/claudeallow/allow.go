// Package claudeallow keeps the standing allow rules of "minion claude allow":
// the Minion side of an owner-approved permission for Claude Code.
//
// Claude Code allow rules for MCP tools name a tool, never its arguments, so
// a rule such as mcp__minion__send_text cannot be limited to one Worker or
// session. The target limit is therefore kept here, in a file of the owner
// (the allowlist, [DefaultPath]), and enforced by "minion mcp": when the file
// exists, a write tool call (and a read tool call that a rule names) whose
// target is outside every rule is refused before anything is sent. The server
// reads the file on every such call, so a revoke takes effect at the next call.
//
// The allowlist never widens what Minion itself allows: the Hub and the
// Worker decide every call exactly as before (authz.Decide, session grants,
// approvals). It only narrows what a Claude Code session may send without a
// prompt.
package claudeallow

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
)

// Version is the format version of the allowlist file.
const Version = 1

// FileName is the base name of the allowlist file.
const FileName = "claude-allow.json"

// maxFile bounds the allowlist file.
const maxFile = 1 << 20

// File is the allowlist.
type File struct {
	Version int    `json:"version"`
	Rules   []Rule `json:"rules"`
}

// Rule is one standing allow rule.
type Rule struct {
	// ID is a UUIDv7; the Hub audit names it (client_rule).
	ID string `json:"id"`
	// Created is when the owner approved the rule (RFC 3339, UTC).
	Created time.Time `json:"created"`
	// Ops are the MCP tools (catalog op names) the rule allows.
	Ops []string `json:"ops"`
	// Targets are the Workers, and optionally sessions, the rule reaches.
	Targets []Target `json:"targets"`
	// Settings is the Claude Code settings file the rule wrote its entries to.
	Settings string `json:"settings"`
	// Entries are the permissions.allow entries the rule needs in Settings.
	Entries []string `json:"entries"`
	// Added are the entries the rule added: those that were not in Settings
	// before. A revoke removes only these, and only when no remaining rule
	// needs them.
	Added []string `json:"added,omitempty"`
}

// Target is one Worker of a rule. Without sessions the rule reaches every
// session of the Worker.
type Target struct {
	WorkerID string   `json:"worker_id"`
	Sessions []string `json:"sessions,omitempty"`
}

// LockName is the base name of the lock file next to the allowlist: every
// change of the allowlist ("minion claude allow" and "revoke", from any Claude
// Code profile) holds it.
const LockName = ".claude-allow.lock"

// LockPath returns the lock file of the allowlist at path.
func LockPath(path string) string {
	return filepath.Join(filepath.Dir(path), LockName)
}

// DefaultPath returns the path of the allowlist file: minion/claude-allow.json
// in the user configuration directory of the platform (os.UserConfigDir):
// $XDG_CONFIG_HOME or ~/.config on Linux, ~/Library/Application Support on
// macOS, %AppData% on Windows.
func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find the user configuration directory: %w", err)
	}
	return filepath.Join(dir, "minion", FileName), nil
}

// Load reads the allowlist at path. A missing file returns (nil, nil): no
// standing rule exists and "minion mcp" behaves as without this package. So
// does a path that cannot hold a file because one of its directories is a
// file (ENOTDIR). Any other error (a file that exists but cannot be read, or a
// directory that cannot be searched, so its existence is unknown) is returned.
func Load(path string) (*File, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxFile {
		return nil, errors.New("the Minion allowlist is not a regular file of a bounded size")
	}
	f, err := os.Open(path) //nolint:gosec // the path is the owner's configuration file
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse decodes and validates an allowlist.
func Parse(raw []byte) (*File, error) {
	var file File
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&file); err != nil {
		return nil, fmt.Errorf("the Minion allowlist is not valid: %w", err)
	}
	if file.Version != Version {
		return nil, fmt.Errorf("the Minion allowlist has version %d; this minion reads version %d", file.Version, Version)
	}
	seen := map[string]bool{}
	for _, r := range file.Rules {
		if err := r.Validate(); err != nil {
			return nil, err
		}
		if seen[r.ID] {
			return nil, fmt.Errorf("the Minion allowlist names rule %s twice", r.ID)
		}
		seen[r.ID] = true
	}
	return &file, nil
}

// Validate checks one rule.
func (r Rule) Validate() error {
	if !ops.ValidUUIDv7(r.ID) {
		return errors.New("a rule of the Minion allowlist has an id that is not a UUIDv7")
	}
	if len(r.Ops) == 0 || len(r.Targets) == 0 {
		return fmt.Errorf("rule %s of the Minion allowlist names no op or no target", r.ID)
	}
	for _, op := range r.Ops {
		if _, ok := ops.Lookup(op); !ok {
			return fmt.Errorf("rule %s of the Minion allowlist names the unknown op %q", r.ID, op)
		}
	}
	for _, t := range r.Targets {
		if !ops.ValidUUIDv7(t.WorkerID) {
			return fmt.Errorf("rule %s of the Minion allowlist names a Worker id that is not a UUIDv7", r.ID)
		}
		for _, s := range t.Sessions {
			if (ops.Target{SessionName: s}).Validate() != nil {
				return fmt.Errorf("rule %s of the Minion allowlist names an invalid session name", r.ID)
			}
		}
	}
	return nil
}

// Encode returns the file as indented JSON with a final newline.
func (f *File) Encode() ([]byte, error) {
	if f.Rules == nil {
		f.Rules = []Rule{}
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Rule returns the rule with id, if any.
func (f *File) Rule(id string) (Rule, bool) {
	if f == nil {
		return Rule{}, false
	}
	for _, r := range f.Rules {
		if r.ID == id {
			return r, true
		}
	}
	return Rule{}, false
}

// Remove deletes the rule with id and reports whether it existed.
func (f *File) Remove(id string) bool {
	for i, r := range f.Rules {
		if r.ID == id {
			f.Rules = append(f.Rules[:i], f.Rules[i+1:]...)
			return true
		}
	}
	return false
}

// Entries returns the sorted union of the settings entries of every rule.
func (f *File) Entries() []string {
	if f == nil {
		return nil
	}
	set := map[string]bool{}
	for _, r := range f.Rules {
		for _, e := range r.Entries {
			set[e] = true
		}
	}
	out := make([]string, 0, len(set))
	for e := range set {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}

// GrantsWrites reports whether any rule allows an op that changes state.
func (f *File) GrantsWrites() bool {
	if f == nil {
		return false
	}
	for _, r := range f.Rules {
		for _, name := range r.Ops {
			if op, ok := ops.Lookup(name); ok && op.ChangesState() {
				return true
			}
		}
	}
	return false
}

// Names reports whether any rule allows op.
func (f *File) Names(op string) bool {
	if f == nil {
		return false
	}
	for _, r := range f.Rules {
		if slices.Contains(r.Ops, op) {
			return true
		}
	}
	return false
}

// Match returns the first rule that allows op on the session of the Worker
// workerID. An empty session matches only a rule target without sessions (a
// whole Worker); an empty workerID matches nothing, because a rule always
// names its Workers by id.
func (f *File) Match(op, workerID, session string) (Rule, bool) {
	if f == nil || workerID == "" {
		return Rule{}, false
	}
	for _, r := range f.Rules {
		if !slices.Contains(r.Ops, op) {
			continue
		}
		for _, t := range r.Targets {
			if t.WorkerID != workerID {
				continue
			}
			if len(t.Sessions) == 0 || (session != "" && slices.Contains(t.Sessions, session)) {
				return r, true
			}
		}
	}
	return Rule{}, false
}

// Describe returns the targets of r in one line: "<worker-id> (all sessions)"
// or "<worker-id>: a, b", separated by "; ".
func (r Rule) Describe() string {
	parts := make([]string, 0, len(r.Targets))
	for _, t := range r.Targets {
		if len(t.Sessions) == 0 {
			parts = append(parts, t.WorkerID+" (all sessions)")
			continue
		}
		parts = append(parts, t.WorkerID+": "+strings.Join(t.Sessions, ", "))
	}
	return strings.Join(parts, "; ")
}
