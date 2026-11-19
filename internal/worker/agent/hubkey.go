package agent

import (
	"context"
	"os"
	"path/filepath"

	"github.com/patrick-1984/ai-minion/internal/identity"
	"github.com/patrick-1984/ai-minion/internal/worker/store"
)

// The values of [HubKey.State].
const (
	// HubKeyNone: this Worker is not paired; there is no pinned Hub key.
	HubKeyNone = "none"
	// HubKeyOK: the pinned key was checked (by fingerprint or invitation).
	HubKeyOK = "ok"
	// HubKeyUnverified: a key is pinned, but nobody checked it yet.
	HubKeyUnverified = "unverified"
	// HubKeyMismatch: the Worker's own verdict is that the Hub presented
	// another key (stored mismatch, or the live alarm hub_identity_changed).
	HubKeyMismatch = "mismatch"
	// HubKeyUnknown: the state or the pinned key cannot be read.
	HubKeyUnknown = "unknown"
)

// HubKey is the pinned Hub key as one state, its words and the one line the
// major Worker commands print first (owner ask A52). The status JSON carries
// it as hub_key, so that the CLI line and the tray text are byte-identical
// (tray W10b). The state is the Worker's own verdict, never a comparison made
// by a reader of the status.
type HubKey struct {
	State string `json:"state"`
	Words string `json:"words"`
	Line  string `json:"line"`
}

// The lines of the states without words.
const (
	hubKeyLineNone       = "Hub key: none (this Worker is not paired)"
	hubKeyLineNoState    = "Hub key: unknown (the Worker state cannot be read)"
	hubKeyLineNoPairing  = "Hub key: unknown (the pairing cannot be read)"
	hubKeyLineNoPinnedFP = "Hub key: unknown (the pinned key cannot be read)"
)

// HubKeyStateUnreadable is the [HubKey] of a Worker whose state database
// exists but cannot be opened.
func HubKeyStateUnreadable() HubKey {
	return HubKey{State: HubKeyUnknown, Line: hubKeyLineNoState}
}

// HubKeyOf builds the [HubKey] from the result of [Enrollments.Load] (enr,
// paired, err) and the live alarm of a running Worker ("" when none answered).
// A stored mismatch and the alarm [AlarmHubIdentityChanged] are both a
// mismatch: the Worker refuses that Hub.
func HubKeyOf(enr Enrollment, paired bool, err error, alarm string) HubKey {
	switch {
	case err != nil:
		return HubKey{State: HubKeyUnknown, Line: hubKeyLineNoPairing}
	case !paired:
		return HubKey{State: HubKeyNone, Line: hubKeyLineNone}
	}
	fp, ferr := identity.FingerprintOf(enr.Pin.Pub)
	if ferr != nil {
		return HubKey{State: HubKeyUnknown, Line: hubKeyLineNoPinnedFP}
	}
	words := fp.Words()
	if enr.Check() == store.HubCheckMismatch || alarm == AlarmHubIdentityChanged {
		return HubKey{State: HubKeyMismatch, Words: words,
			Line: "Hub key: " + words + " · MISMATCH: the Hub presented another key; the Worker refuses it (minion worker status)"}
	}
	state := HubKeyOK
	if enr.Check() == store.HubCheckNone {
		state = HubKeyUnverified
	}
	return HubKey{State: state, Words: words, Line: "Hub key: " + words + " (compare with the Hub's top bar)"}
}

// ReadHubKey reads the [HubKey] of the Worker state in stateDir read-only,
// without a running Worker. It never fails: a problem becomes the unknown
// state with its line. A missing database means not paired (and is not
// created).
func ReadHubKey(ctx context.Context, stateDir string) HubKey {
	if _, err := os.Stat(filepath.Join(stateDir, store.DBFile)); err != nil {
		return HubKeyOf(Enrollment{}, false, nil, "")
	}
	st, err := store.OpenReadOnlyContext(ctx, stateDir)
	if err != nil {
		return HubKeyStateUnreadable()
	}
	defer func() { _ = st.Close() }()
	enr, ok, err := NewEnrollments(st, stateDir).Load(ctx)
	return HubKeyOf(enr, ok, err, "")
}
