//go:build linux

package presence

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const maxServiceFile = 64 << 10

// serviceStateDirs returns the state directories the account's own Worker
// registrations pin: the user unit and its drop-ins (MINION_STATE_DIR, or an
// XDG_STATE_HOME that gives <dir>/minion) and the login entry. A service whose
// environment differs from a fresh login is found this way even when no
// discovery pointer was written (a Worker of an older release). The files are
// data: only regular files of this account (or root) are read, bounded, and
// every directory found is verified like any other by the reader.
func serviceStateDirs(home string, uid int) []string {
	if home == "" {
		return nil
	}
	var files []string
	unit := filepath.Join(home, ".config", "systemd", "user", "minion-worker.service")
	files = append(files, unit)
	drop, _ := filepath.Glob(unit + ".d/*.conf")
	files = append(files, drop...)
	var out []string
	add := func(d string) {
		if filepath.IsAbs(d) && !strings.ContainsAny(d, "%\r\n") {
			out = append(out, filepath.Clean(d))
		}
	}
	for _, f := range files {
		raw := readServiceFile(f, uid)
		for _, line := range strings.Split(raw, "\n") {
			rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Environment=")
			if !ok {
				continue
			}
			for _, tok := range envTokens(rest) {
				if v, ok := strings.CutPrefix(tok, "MINION_STATE_DIR="); ok {
					add(v)
				} else if v, ok := strings.CutPrefix(tok, "XDG_STATE_HOME="); ok && filepath.IsAbs(v) {
					add(filepath.Join(v, "minion"))
				}
			}
		}
	}
	// The login entry: first line the executable, second its state directory.
	if lines := strings.Split(readServiceFile(filepath.Join(home, ".config", "minion", "worker-login"), uid), "\n"); len(lines) >= 2 {
		add(strings.TrimRight(lines[1], "\r"))
	}
	return out
}

// envTokens splits the value of an Environment= line: words separated by
// blanks, a word that starts with a double quote is a C-style quoted string.
func envTokens(s string) []string {
	var out []string
	for s = strings.TrimSpace(s); s != ""; s = strings.TrimSpace(s) {
		if s[0] == '"' {
			end := -1
			for i := 1; i < len(s); i++ {
				if s[i] == '\\' {
					i++
				} else if s[i] == '"' {
					end = i
					break
				}
			}
			if end < 0 {
				return out
			}
			if u, err := strconv.Unquote(s[:end+1]); err == nil {
				out = append(out, u)
			}
			s = s[end+1:]
			continue
		}
		i := strings.IndexAny(s, " \t")
		if i < 0 {
			i = len(s)
		}
		out = append(out, s[:i])
		s = s[i:]
	}
	return out
}

func readServiceFile(path string, uid int) string {
	// Non-blocking: a FIFO in this place must not hang discovery before the
	// handle is checked.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o022 != 0 {
		return ""
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || (int(st.Uid) != uid && st.Uid != 0) {
		return ""
	}
	b, err := readBounded(f, maxServiceFile)
	if err != nil {
		return ""
	}
	return string(b)
}
