package config

import (
	"fmt"
	"strings"
)

// Values of allow_new_sessions.
const (
	AllowNewSessionsOn  = "on"
	AllowNewSessionsOff = "off"
)

// tightenOnly are the settings whose local sources are merged tighten-only
// instead of by precedence: a restriction from any source holds.
var tightenOnly = map[string]string{
	KeyAllowNewSessions: AllowNewSessionsOn, // the value that allows
}

// IsTightenOnly reports whether key is merged tighten-only.
func IsTightenOnly(key string) bool { _, ok := tightenOnly[key]; return ok }

// Tightened is the tighten-only merge of one setting over the local sources.
type Tightened struct {
	// Blocked is true when any source restricts: it says the restricting
	// value, or a value that is not valid (an unreadable value never allows).
	Blocked bool
	// Sources are the sources that restrict, in the order flag, env, file;
	// empty when nothing restricts.
	Sources []Source
	// Values are the values of every source that holds one.
	Values map[Source]string
}

// Value returns the merged value: the allowing value, or the restricting
// one.
func (t Tightened) Value(key string) string {
	if t.Blocked {
		return AllowNewSessionsOff
	}
	return tightenOnly[key]
}

// ResolveTightenOnly merges a tighten-only setting over the flag, the
// environment and the config file of res (tray architecture 4.2): unlike the
// usual precedence (flag > env > file) a restriction from any source holds,
// so a block saved in the file is not lifted by a service environment that
// says on. A value that is not one of the setting's choices restricts too.
func ResolveTightenOnly(res *Resolver, key string) (Tightened, error) {
	allow, ok := tightenOnly[key]
	if !ok {
		return Tightened{}, fmt.Errorf("setting %q is not tighten-only", key)
	}
	s, ok := res.registry().Lookup(key)
	if !ok {
		return Tightened{}, fmt.Errorf("unknown setting %q", key)
	}
	t := Tightened{Values: map[Source]string{}}
	add := func(src Source, v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		t.Values[src] = v
		if v != allow {
			t.Blocked = true
			t.Sources = append(t.Sources, src)
		}
	}
	if s.HasFlag() && res.Flags != nil {
		v, ok, err := res.Flags.Lookup(key)
		if err != nil {
			return Tightened{}, fmt.Errorf("setting %q: flag: %w", key, err)
		}
		if ok {
			add(SourceFlag, v)
		}
	}
	if v, ok := res.lookupEnv(s.EnvName()); ok {
		add(SourceEnv, v)
	}
	if res.File != nil {
		v, ok, err := res.File.Lookup(key)
		if err != nil {
			// An unreadable file value never allows.
			t.Blocked = true
			t.Sources = append(t.Sources, SourceFile)
		} else if ok {
			add(SourceFile, v)
		}
	}
	return t, nil
}
