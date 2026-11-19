package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// JoinPinFile is the provenance record of the Hub settings that a join saved
// in the Worker's config file (tray tickets v3, C11). It says whether the
// join created the hub_fingerprint expectation (and the server address), and
// with which value, so that cancelling a background pairing (W8b) and leaving
// the Hub (W9) remove only what a join set, and only while it is unchanged. A
// fingerprint that was provisioned before the join, or edited after it, is
// never removed by them. No record means "not known" (an older join): kept.
const JoinPinFile = "join-pin.json"

// JoinPinVersion is the format version of [JoinPin].
const JoinPinVersion = 1

// Sources of a [JoinPin].
const (
	// JoinPinSourceBackground: "minion worker join <url> --background".
	JoinPinSourceBackground = "join_background"
)

// JoinPin is the content of [JoinPinFile].
type JoinPin struct {
	Version int `json:"version"`
	// Source is the command that wrote the record (JoinPinSource*).
	Source string `json:"source"`
	// Server is the server address the join saved; ServerCreated says the
	// config file named no server, or this one from an earlier join, before.
	Server        string `json:"server"`
	ServerCreated bool   `json:"server_created"`
	// HubFingerprint is the hub_fingerprint value in the config file after
	// the join ("" when none); HubFingerprintCreated says the join created
	// that expectation (the file had none before, or the one an earlier join
	// created).
	HubFingerprint        string `json:"hub_fingerprint"`
	HubFingerprintCreated bool   `json:"hub_fingerprint_created"`
	// At is when the join saved it.
	At time.Time `json:"at"`
}

// CreatedFingerprint reports whether the join created the hub_fingerprint
// expectation and it still has the value current.
func (p JoinPin) CreatedFingerprint(current string) bool {
	return p.HubFingerprintCreated && p.HubFingerprint != "" && p.HubFingerprint == current
}

// CreatedServer reports whether the join created the server setting and it
// still has the value current.
func (p JoinPin) CreatedServer(current string) bool {
	return p.ServerCreated && p.Server != "" && p.Server == current
}

// ReadJoinPin reads the provenance record of stateDir; ok is false when there
// is none.
func ReadJoinPin(stateDir string) (pin JoinPin, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(stateDir, JoinPinFile))
	if errors.Is(err, fs.ErrNotExist) {
		return JoinPin{}, false, nil
	}
	if err != nil {
		return JoinPin{}, false, fmt.Errorf("read %s: %w", JoinPinFile, err)
	}
	if err := json.Unmarshal(b, &pin); err != nil {
		return JoinPin{}, false, fmt.Errorf("the join record %s is not valid: %w", filepath.Join(stateDir, JoinPinFile), err)
	}
	if pin.Version != JoinPinVersion {
		return JoinPin{}, false, fmt.Errorf("the join record %s has the unknown version %d", filepath.Join(stateDir, JoinPinFile), pin.Version)
	}
	return pin, true, nil
}

// NextJoinPin computes the record of a join that saves server and fingerprint
// ("" for none) into a config file that held fileServer and fileFingerprint
// before ("" for none). A value is "created" when the file had none, or held
// exactly the one an earlier join created (prev, if any). A value that was
// there before any join, or edited since, is not.
func NextJoinPin(prev JoinPin, hadPrev bool, fileServer, fileFingerprint, server, fingerprint string, now time.Time) JoinPin {
	next := JoinPin{Version: JoinPinVersion, Source: JoinPinSourceBackground, Server: server, At: now.UTC()}
	next.ServerCreated = fileServer == "" || (hadPrev && prev.CreatedServer(fileServer))
	switch {
	case fingerprint == "":
		// The join set none: what the file holds stays as it was.
		next.HubFingerprint = fileFingerprint
		next.HubFingerprintCreated = hadPrev && prev.CreatedFingerprint(fileFingerprint)
	case fileFingerprint == "":
		next.HubFingerprint, next.HubFingerprintCreated = fingerprint, true
	default:
		next.HubFingerprint = fingerprint
		next.HubFingerprintCreated = hadPrev && prev.CreatedFingerprint(fileFingerprint)
	}
	return next
}

// WriteJoinPin writes the provenance record durably and owner-only
// (temporary file, fsync, rename, fsync of the directory). The caller holds
// the config lock, under which it also wrote the config file.
func WriteJoinPin(stateDir string, pin JoinPin) error {
	return writeStateJSON(stateDir, JoinPinFile, ".join-pin-*.tmp", pin)
}

// RemoveJoinPin removes the provenance record; a missing one is fine.
func RemoveJoinPin(stateDir string) error {
	if err := os.Remove(filepath.Join(stateDir, JoinPinFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", JoinPinFile, err)
	}
	return nil
}
