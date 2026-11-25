package tmux

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/patrick-1984/ai-minion/internal/worker/term"
)

// The instance id of a tmux session (study K1, section 9.1).
//
// A session name is not an identity: a name that is killed and created again,
// even inside the same second, is another terminal. tmux already knows the
// difference: #{session_id} ("$N") stays the same when a session is renamed and
// is a new number for every session a server creates. It restarts at $0 with a
// new server, so the server is part of the identity (#{pid} and #{start_time}),
// and a server pid and start second can repeat after a reboot of a machine
// without a clock, so the boot id of the kernel is part of it too.
//
// The id is a hash over those four values: deterministic, computed on every
// listing, with nothing stored. It therefore survives a Worker restart, and it
// ends, correctly, with the tmux server or the boot. Where one of the four
// values cannot be read the id is empty: no id is better than one that two
// terminals could share.
const (
	instancePrefix = "t1-"
	instanceDomain = "minion-instance-v1\x00tmux\x00"
	instanceHexLen = 32
)

// psmuxNoIdentity removes the identity fields from a list-panes format.
var psmuxNoIdentity = strings.NewReplacer("#{pid}", "", "#{start_time}", "", "#{session_id}", "")

// instanceID returns the instance id of the session sessionID ("$N") on the
// tmux server with the pid serverPID that started at startTime (Unix seconds),
// on the boot bootID. The three tmux values are taken as tmux prints them. It
// returns "" unless every part is present and well formed.
func instanceID(bootID, serverPID, startTime, sessionID string) string {
	if !validBootID(bootID) || !positiveDecimal(serverPID) || !positiveDecimal(startTime) ||
		len(sessionID) < 2 || sessionID[0] != '$' || !decimal(sessionID[1:]) {
		return ""
	}
	sum := sha256.Sum256([]byte(instanceDomain + bootID + "\x00" + serverPID + "\x00" + startTime + "\x00" + sessionID))
	return instancePrefix + hex.EncodeToString(sum[:])[:instanceHexLen]
}

// decimal reports whether v is one to 19 ASCII digits in canonical form (no
// leading zero except "0" itself), so that one number has one spelling.
func decimal(v string) bool {
	if v == "" || len(v) > 19 || (len(v) > 1 && v[0] == '0') {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return false
		}
	}
	return true
}

// positiveDecimal reports whether v is a canonical decimal number above zero.
func positiveDecimal(v string) bool { return decimal(v) && v != "0" }

// validBootID reports whether v looks like a boot id: lower-case hex digits
// and dashes (the kernel prints a UUID), at most 64 bytes.
func validBootID(v string) bool {
	if v == "" || len(v) > 64 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && c != '-' {
			return false
		}
	}
	return true
}

// bootID returns the boot id of this machine, "" when it cannot be read. A
// value that was read is kept: it does not change while the Worker runs. A
// failed read is tried again on the next listing.
func (h *Host) bootID() string {
	if p := h.boot.Load(); p != nil {
		return *p
	}
	read := h.readBoot
	if read == nil {
		read = readBootID
	}
	id := strings.TrimSpace(read())
	if !validBootID(id) {
		return ""
	}
	h.boot.Store(&id)
	return id
}

// noteInstances records whether the listing gave every session an instance id.
func (h *Host) noteInstances(sessions []term.Session) {
	gap := false
	for i := range sessions {
		if sessions[i].Instance == "" {
			gap = true
			break
		}
	}
	h.instanceGap.Store(gap)
}

// ReportsInstances implements term.InstanceReporter: the host fills
// Session.Instance. It is true for tmux on a machine whose boot id is readable
// (Linux), and false for psmux, whose identity fields are not measured. It
// turns false while the latest listing had a session without an id, which is
// what a tmux server that does not know #{start_time} produces; before the
// first listing the host is taken to be able, and a session it then cannot
// identify is reported with an empty id, which the reader must treat as
// unverifiable.
func (h *Host) ReportsInstances() bool {
	return !h.psmux && h.bootID() != "" && !h.instanceGap.Load()
}
