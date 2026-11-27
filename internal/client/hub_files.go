package client

import (
	"net/http"
	"strings"
)

// FileLeg returns what a REST caller's data leg (files.v1) needs from this
// backend: its HTTP client (with the backend's TLS settings), the absolute URL
// of path (proto.FilesLegPath) on the Hub, and the Authorization header with
// the API key, read from the key file now. The key is never logged and never
// part of an error.
func (b *HubBackend) FileLeg(path string) (*http.Client, string, http.Header, error) {
	key, err := readAPIKey(b.keyFile)
	if err != nil {
		return nil, "", nil, err
	}
	u := *b.base
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	h := http.Header{}
	h.Set("Authorization", "Bearer "+key)
	if b.userAgent != "" {
		h.Set("User-Agent", b.userAgent)
	}
	return b.hc, u.String(), h, nil
}
