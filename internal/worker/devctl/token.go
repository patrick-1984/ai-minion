package devctl

import (
	"io"
	"regexp"
	"strings"
	"sync/atomic"
)

var tokenRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Token holds the bridge token of this start. The zero value holds none.
type Token struct{ v atomic.Pointer[string] }

// Get returns the token, "" when none was read.
func (t *Token) Get() string {
	if p := t.v.Load(); p != nil {
		return *p
	}
	return ""
}

// ReadLine reads the one line the app writes to the Worker's stdin at launch
// ("MINION_ADB_BRIDGE_TOKEN=<64 hex>") and keeps the token. It reads at most
// one short line, so a stdin that carries something else is left alone after
// it; a line that is not a token is dropped. It returns when the line is read
// or r ends; run it in its own goroutine.
func (t *Token) ReadLine(r io.Reader) {
	// One byte at a time: nothing after the line is consumed.
	var line []byte
	var b [1]byte
	for len(line) < 256 {
		n, err := r.Read(b[:])
		if n == 1 {
			if b[0] == '\n' {
				break
			}
			line = append(line, b[0])
		}
		if err != nil {
			break
		}
	}
	v, ok := strings.CutPrefix(strings.TrimRight(string(line), "\r"), TokenEnvPrefix)
	if !ok || !tokenRE.MatchString(v) {
		return
	}
	t.v.Store(&v)
}
