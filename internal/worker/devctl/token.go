package devctl

import (
	"bufio"
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

// ReadFrom reads the one line the app writes to the Worker's stdin at launch
// ("MINION_ADB_BRIDGE_TOKEN=<64 hex>") and keeps the token. It reads at most
// one short line, so a stdin that carries something else is left alone after
// it; a line that is not a token is dropped. It returns when the line is read
// or r ends; run it in its own goroutine.
func (t *Token) ReadFrom(r io.Reader) {
	br := bufio.NewReaderSize(io.LimitReader(r, 256), 256)
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		return
	}
	v, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), TokenEnvPrefix)
	if !ok || !tokenRE.MatchString(v) {
		return
	}
	t.v.Store(&v)
}
