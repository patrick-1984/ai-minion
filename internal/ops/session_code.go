package ops

import (
	"strconv"
	"strings"
)

// SessionCode is the short code of a session that the Hub gives out, written
// "<worker number>-<session number>", for example "5-667": digits only, so it
// is easy to read aloud and to dictate. The worker number is given by the Hub
// to each Worker and never given again; the session number is given by the
// Hub to each session of a Worker the first time the Hub lists it, and never
// given again within that Worker. A code is a Hub feature: only the Hub
// resolves it, and a Worker knows nothing of it.
type SessionCode struct {
	Worker  int64
	Session int64
}

// String returns the code in the form "5-667".
func (c SessionCode) String() string {
	return strconv.FormatInt(c.Worker, 10) + "-" + strconv.FormatInt(c.Session, 10)
}

// maxCodePartDigits bounds each number of a code (well inside int64).
const maxCodePartDigits = 12

// ParseSessionCode reads a session code given as a target: "5-667", also
// written "#5-667" or "s5-667" (a leading "#", "s" or "S"), optionally
// followed by ":window.pane" ("5-667:0.1"). ok is false for anything else,
// which is then an ordinary target. A Hub target always names its Worker
// ("worker:session"), and a session name never contains "." or ":", so no
// target that the Hub accepted before reads as a code.
func ParseSessionCode(s string) (code SessionCode, pane string, ok bool) {
	head, rest, hasPane := strings.Cut(s, ":")
	if hasPane {
		if !validPane(rest) {
			return SessionCode{}, "", false
		}
		pane = rest
	}
	if len(head) > 0 && (head[0] == '#' || head[0] == 's' || head[0] == 'S') {
		head = head[1:]
	}
	w, sess, found := strings.Cut(head, "-")
	if !found {
		return SessionCode{}, "", false
	}
	wn, okW := codeNumber(w)
	sn, okS := codeNumber(sess)
	if !okW || !okS {
		return SessionCode{}, "", false
	}
	return SessionCode{Worker: wn, Session: sn}, pane, true
}

// codeNumber parses one positive decimal number of a code.
func codeNumber(s string) (int64, bool) {
	if s == "" || len(s) > maxCodePartDigits {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}
