package filexfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/patrick-1984/ai-minion/internal/proto"
)

// Manifest is the list of entries of a transfer in walk order (byte order of
// names per directory, depth first), as the sender produced them: the last
// entry of a folder archive (proto.ManifestName) and, for a single file, the
// one-entry manifest of SingleFileManifest. Paths are relative to the
// transferred folder (a single file: its name). Its canonical bytes are the
// RFC 8785 serialization of the JSON array of proto.ManifestEntry objects; the
// manifest digest is their SHA-256.
type Manifest struct {
	Entries []proto.ManifestEntry
	size    int64 // length of the canonical bytes so far
}

// manifestEntryLen is the canonical length of e.
func manifestEntryLen(e proto.ManifestEntry) int64 {
	b, err := appendManifestEntry(nil, e)
	if err != nil {
		return 0
	}
	return int64(len(b))
}

// Add appends e and checks the canonical size against maxBytes (0: the
// default proto.MaxManifestBytes).
func (m *Manifest) Add(e proto.ManifestEntry, maxBytes int64) error {
	if maxBytes <= 0 {
		maxBytes = proto.MaxManifestBytes
	}
	if m.size == 0 {
		m.size = 2 // "[]"
	}
	n := manifestEntryLen(e)
	if len(m.Entries) > 0 {
		n++ // the comma
	}
	if m.size+n > maxBytes {
		return tooLarge("the manifest of this transfer is larger than the limit")
	}
	m.size += n
	m.Entries = append(m.Entries, e)
	return nil
}

// Canonical returns the RFC 8785 bytes of the manifest.
func (m Manifest) Canonical() ([]byte, error) {
	dst := make([]byte, 0, m.size+2)
	dst = append(dst, '[')
	for i, e := range m.Entries {
		if i > 0 {
			dst = append(dst, ',')
		}
		var err error
		if dst, err = appendManifestEntry(dst, e); err != nil {
			return nil, err
		}
	}
	return append(dst, ']'), nil
}

// Digest returns the SHA-256 of the canonical bytes.
func (m Manifest) Digest() ([32]byte, error) {
	b, err := m.Canonical()
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

// DigestHex returns the manifest digest in the form of manifest_digest in
// results, receipts and audit (64 lower-case hex digits).
func DigestHex(sum [32]byte) string { return hex.EncodeToString(sum[:]) }

// Totals returns the files, directories and logical bytes of the manifest.
func (m Manifest) Totals() (files, dirs, bytes int64) {
	for _, e := range m.Entries {
		if e.Type == proto.FileTypeDir {
			dirs++
		} else {
			files++
			bytes += e.Size
		}
	}
	return files, dirs, bytes
}

// appendManifestEntry writes one entry canonically: the keys path, sha256,
// size, type are in UTF-16 order already.
func appendManifestEntry(dst []byte, e proto.ManifestEntry) ([]byte, error) {
	var err error
	dst = append(dst, `{"path":`...)
	if dst, err = appendJCSString(dst, e.Path); err != nil {
		return nil, err
	}
	dst = append(dst, `,"sha256":`...)
	if dst, err = appendJCSString(dst, e.SHA256); err != nil {
		return nil, err
	}
	dst = append(dst, `,"size":`...)
	if dst, err = appendJCSInt(dst, e.Size); err != nil {
		return nil, err
	}
	dst = append(dst, `,"type":`...)
	if dst, err = appendJCSString(dst, e.Type); err != nil {
		return nil, err
	}
	return append(dst, '}'), nil
}

// SingleFileManifest returns the manifest of a single-file transfer: one
// entry named name (the last element of the requested source path, which both
// ends know: FilePullArgs.Path / FilePushArgs.Path), with the file's size and
// SHA-256. The sender sends its digest in the Minion-Manifest-Digest trailer
// like a folder's.
func SingleFileManifest(name string, size int64, sum [32]byte) Manifest {
	e := proto.ManifestEntry{Path: name, Size: size, SHA256: hex.EncodeToString(sum[:]), Type: proto.FileTypeFile}
	m := Manifest{}
	_ = m.Add(e, 0)
	return m
}

// ParseManifest reads canonical manifest bytes. The bytes must be exactly the
// canonical serialization of a valid manifest (so unknown or duplicate keys,
// other key orders, whitespace and floats are refused), every entry must be
// valid and the manifest at most l.MaxManifestBytes long. A refusal is
// integrity_failed (or transfer_too_large for the size).
func ParseManifest(b []byte, l Limits) (Manifest, error) {
	l = l.Normalize()
	if int64(len(b)) > l.MaxManifestBytes {
		return Manifest{}, tooLarge("the manifest of this transfer is larger than the limit")
	}
	var entries []proto.ManifestEntry
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&entries); err != nil {
		return Manifest{}, integrity("the manifest is not valid JSON")
	}
	m := Manifest{}
	for _, e := range entries {
		if err := validManifestEntry(e); err != nil {
			return Manifest{}, err
		}
		if err := m.Add(e, l.MaxManifestBytes); err != nil {
			return Manifest{}, err
		}
	}
	if int64(len(entries)) > l.MaxEntries {
		return Manifest{}, tooLarge("the manifest has more entries than the limit")
	}
	c, err := m.Canonical()
	if err != nil || !bytes.Equal(c, b) {
		return Manifest{}, integrity("the manifest is not in canonical form")
	}
	return m, nil
}

func validManifestEntry(e proto.ManifestEntry) error {
	if CheckPath(e.Path, false) != nil {
		return integrity("the manifest names an invalid path")
	}
	switch e.Type {
	case proto.FileTypeDir:
		if e.Size != 0 || e.SHA256 != "" {
			return integrity("a directory of the manifest has a size or digest")
		}
	case proto.FileTypeFile:
		if e.Size < 0 || e.Size > maxSafeInteger || !proto.ValidManifestDigest(e.SHA256) {
			return integrity("a file of the manifest has an invalid size or digest")
		}
	default:
		return integrity("the manifest has an entry of an unknown type")
	}
	return nil
}

// sameEntries compares the received entries with the manifest, entry by entry
// in order.
func sameEntries(got, want []proto.ManifestEntry) error {
	if len(got) != len(want) {
		return integrity("the received entries do not match the manifest")
	}
	for i := range got {
		if got[i] != want[i] {
			return integrity("the received entries do not match the manifest")
		}
	}
	return nil
}
