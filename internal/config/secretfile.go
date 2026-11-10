package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// maxSecretFileSize bounds a secret file. A secret is a password, a key or a
// token; anything larger is a wrong path, not a secret.
const maxSecretFileSize = 64 << 10

// ReadSecretFile returns the contents of the file at path with one trailing
// newline ("\n" or "\r\n") removed. A missing, unreadable, oversized or empty
// file is an error. The errors name the path and never include the contents.
func ReadSecretFile(path string) (string, error) {
	return readSecretFile(readLimited, path)
}

func readSecretFile(read func(string) ([]byte, error), path string) (string, error) {
	if path == "" {
		return "", errors.New("secret file path is empty")
	}
	data, err := read(path)
	if err != nil {
		// os errors carry the path and the reason, not the contents.
		return "", fmt.Errorf("cannot read secret file: %w", err)
	}
	if len(data) > maxSecretFileSize {
		return "", fmt.Errorf("secret file %q is larger than %d bytes", path, maxSecretFileSize)
	}
	v := string(data)
	if t, ok := strings.CutSuffix(v, "\n"); ok {
		v = strings.TrimSuffix(t, "\r")
	}
	if v == "" {
		return "", fmt.Errorf("secret file %q is empty", path)
	}
	return v, nil
}

// readLimited is the default file reader of the resolver: os.ReadFile with a
// size cap, so that pointing a *_FILE variable at a huge file cannot exhaust
// memory.
func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, maxSecretFileSize+1))
}
