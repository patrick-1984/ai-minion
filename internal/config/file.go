package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
)

// FileMode is the permission of the Worker config file. It can hold secrets
// (for example an invitation key written by the wizard), so only the owner
// may read it.
const FileMode fs.FileMode = 0o600

// dirMode is the permission of directories created for the config file.
const dirMode fs.FileMode = 0o700

// maxFileSize bounds the config file; a settings file is a few lines.
const maxFileSize = 1 << 20

type fileDoc = map[string]json.RawMessage

// ConfigFile is the Worker config file: one JSON object that maps setting
// keys to values. Strings, numbers and booleans are accepted when reading and
// kept as their text; values are always written back as JSON strings.
// Create it with [LoadFile]. It implements [FileSource] and is safe for
// concurrent use.
type ConfigFile struct {
	path string

	saveMu sync.Mutex // serializes Save
	mu     sync.RWMutex
	vals   map[string]string
}

// LoadFile reads the config file at path. A missing file is not an error: it
// yields an empty file that [ConfigFile.Save] creates on first write. On Unix
// a file that is readable by group or others is refused, because it can hold
// secrets.
func LoadFile(path string) (*ConfigFile, error) {
	f := &ConfigFile{path: path, vals: map[string]string{}}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("config file %q is not a regular file", path)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("config file %q has mode %04o; it may hold secrets, run: chmod 600 %s",
			path, info.Mode().Perm(), path)
	}
	fh, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config file: %w", err)
	}
	defer func() { _ = fh.Close() }()
	data, err := io.ReadAll(io.LimitReader(fh, maxFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("config file %q: %w", path, err)
	}
	if len(data) > maxFileSize {
		return nil, fmt.Errorf("config file %q is larger than %d bytes", path, maxFileSize)
	}
	vals, err := parseFile(data)
	if err != nil {
		return nil, fmt.Errorf("config file %q: %w", path, err)
	}
	f.vals = vals
	return f, nil
}

func parseFile(data []byte) (map[string]string, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]string{}, nil
	}
	var doc fileDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("invalid JSON object: %w", err)
	}
	vals := make(map[string]string, len(doc))
	for k, raw := range doc {
		var v any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return nil, fmt.Errorf("key %q: %w", k, err)
		}
		switch t := v.(type) {
		case string:
			vals[k] = t
		case json.Number:
			vals[k] = t.String()
		case bool:
			vals[k] = fmt.Sprint(t)
		default:
			return nil, fmt.Errorf("key %q: value must be a string, number or boolean", k)
		}
	}
	return vals, nil
}

// Path returns the file path.
func (f *ConfigFile) Path() string { return f.path }

// Lookup implements [ValueSource].
func (f *ConfigFile) Lookup(key string) (string, bool, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	v, ok := f.vals[key]
	return v, ok, nil
}

// Set stores a value in memory; call [ConfigFile.Save] to persist it.
func (f *ConfigFile) Set(key, value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vals[key] = value
}

// Delete removes a key in memory.
func (f *ConfigFile) Delete(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.vals, key)
}

// Keys returns the keys in the file, sorted.
func (f *ConfigFile) Keys() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	keys := make([]string, 0, len(f.vals))
	for k := range f.vals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// UnknownKeys returns the keys in the file that reg does not declare for
// role, sorted. The caller decides whether that is an error (a typo) or a
// warning.
func (f *ConfigFile) UnknownKeys(reg *Registry, role Scope) []string {
	var out []string
	for _, k := range f.Keys() {
		s, ok := reg.Lookup(k)
		if !ok || !s.Scope.appliesTo(role) {
			out = append(out, k)
		}
	}
	return out
}

// Save writes the file atomically with mode 0600: it writes a temporary file
// in the same directory (created 0700 when missing), syncs it and renames it
// over the target, so a crash never leaves a half-written file.
func (f *ConfigFile) Save() error {
	// saveMu serializes saves from snapshot to rename, so a slower save of an
	// older snapshot can never replace a newer file.
	f.saveMu.Lock()
	defer f.saveMu.Unlock()

	f.mu.RLock()
	data, err := json.MarshalIndent(f.vals, "", "  ")
	f.mu.RUnlock()
	if err != nil {
		return fmt.Errorf("config file: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxFileSize {
		return fmt.Errorf("config file %q would be %d bytes, larger than the %d byte limit; the existing file is unchanged",
			f.path, len(data), maxFileSize)
	}

	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("config file: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp") // created 0600
	if err != nil {
		return fmt.Errorf("config file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("config file: %w", err)
	}
	if err := tmp.Chmod(FileMode); err != nil && runtime.GOOS != "windows" {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("config file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("config file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("config file: %w", err)
	}
	if err := os.Rename(tmpName, f.path); err != nil {
		cleanup()
		return fmt.Errorf("config file: %w", err)
	}
	return nil
}
