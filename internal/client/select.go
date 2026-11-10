package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Kind says which backend [Select] chose.
type Kind string

const (
	// KindHub is the Hub REST backend.
	KindHub Kind = "hub"
	// KindLocal is the local control socket.
	KindLocal Kind = "local"
)

// ErrNoBackend wraps the "no Worker" failure of the local backend when no Hub
// is configured either, so that the user learns both ways to get a backend.
// It matches [ErrWorkerNotRunning] too.
var ErrNoBackend = errors.New("no Hub is configured and no local Worker is running")

// Select chooses the backend of one command or MCP server (AD-22).
//
//   - A Hub that is configured (a URL or a key file) always wins, also when a
//     local control socket exists. When it cannot be used (a missing key file,
//     an unreadable CA, a bad URL) the error is returned: there is no fallback
//     to the local Worker and no other Hub, because that would run the request
//     on a different machine than the one the user chose.
//   - Without a Hub the local backend is used. When no Worker runs there, its
//     error also says that no Hub is configured and how to configure one; the
//     client never invents a server.
//
// A key file without a URL is an error, not "local": the user asked for a Hub
// and left the address out.
func Select(cfg HubConfig, local Backend, opts ...HubOption) (Backend, Kind, error) {
	if cfg.URL != "" || cfg.APIKeyFile != "" {
		if cfg.URL == "" {
			return nil, KindHub, fmt.Errorf("%w: MINION_HUB_URL is not set although an API key file is configured", ErrHubURL)
		}
		h, err := NewHub(cfg, opts...)
		if err != nil {
			return nil, KindHub, err
		}
		return h, KindHub, nil
	}
	if local == nil {
		return nil, KindLocal, ErrNoBackend
	}
	return NoHubHint(local), KindLocal, nil
}

// NoHubHint wraps a local backend so that its "no Worker is running" failure
// says that no Hub is configured either. The wrapped error still matches
// [ErrWorkerNotRunning] and [ErrNoBackend].
func NoHubHint(b Backend) Backend {
	if _, ok := b.(noHubHint); ok {
		return b
	}
	return noHubHint{b}
}

type noHubHint struct{ Backend }

func (n noHubHint) Call(ctx context.Context, op string, args any) (json.RawMessage, error) {
	r, err := n.Backend.Call(ctx, op, args)
	return r, hint(err)
}

func (n noHubHint) Stream(ctx context.Context, op string, args any, onProgress func(json.RawMessage) error) (json.RawMessage, error) {
	r, err := n.Backend.Stream(ctx, op, args, onProgress)
	return r, hint(err)
}

func hint(err error) error {
	if err == nil || !errors.Is(err, ErrWorkerNotRunning) || errors.Is(err, ErrNoBackend) {
		return err
	}
	return fmt.Errorf("%w; %w (to use a Hub set MINION_HUB_URL and MINION_API_KEY_FILE)", err, ErrNoBackend)
}
