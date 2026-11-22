package presence

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type indexEntry struct {
	Schema string `json:"schema"`
	Dir    string `json:"dir"`
}

func (l Layout) indexName() string { return l.profileKey() + recordSuffix }

// writeDiscovery writes the owner-only pointer from the account's default
// location to the presence directory of a relocated profile, and drops pointers
// whose directory is gone. It does nothing for a profile in the default place
// and for a root layout.
func writeDiscovery(l Layout) error {
	if l.Scope != ScopeUser || l.DiscoveryDir == "" {
		return nil
	}
	dir, err := ensurePrivateDir(l.DiscoveryDir, l.UID)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(indexEntry{Schema: IndexSchema, Dir: l.PrivateDir})
	if err := writePrivate(dir, l.indexName(), b, l.UID); err != nil {
		return err
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.Name() == l.indexName() || filepath.Ext(e.Name()) != recordSuffix {
			continue
		}
		if target, err := readIndex(dir, e.Name(), l.UID); err == nil {
			if _, err := os.Stat(target); isNotExist(err) {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	return nil
}

func removeDiscovery(l Layout) error {
	if l.Scope != ScopeUser || l.DiscoveryDir == "" {
		return nil
	}
	dir, err := verifyPrivateDir(l.DiscoveryDir, l.UID)
	if err != nil {
		if isNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.Remove(filepath.Join(dir, l.indexName())); err != nil && !isNotExist(err) {
		return err
	}
	return nil
}

// readIndex reads one pointer entry (private file checks) and returns the
// directory it names.
func readIndex(dir, name string, uid int) (string, error) {
	raw, err := readPrivateFile(dir, name, uid, MaxRecordSize)
	if err != nil {
		return "", err
	}
	var e indexEntry
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return "", fmt.Errorf("not a presence index entry: %w", err)
	}
	if e.Schema != IndexSchema || !filepath.IsAbs(e.Dir) {
		return "", errors.New("not a valid presence index entry")
	}
	return e.Dir, nil
}
