// Package selfupdate replaces the Hub's own Docker container with a newer
// release (v0.9.0, A10, opt-in). The Hub pulls the release image by the digest
// its signed release manifest names and starts a short-lived helper container
// from that image; the helper stops the Hub, keeps a copy of the data
// directory, recreates the container with the same configuration and the new
// image, waits until it is ready and rolls everything back otherwise. The
// approach follows Arcane's upgrade helper (getarcaneapp/arcane,
// backend/cli/upgrade/upgrade.go).
package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

// DefaultSocket is the Docker Engine socket mounted into the Hub container.
const DefaultSocket = "/var/run/docker.sock"

// apiVersion is the Engine API version the client speaks (Docker 20.10+).
const apiVersion = "v1.41"

// Docker is a minimal Docker Engine API client over a Unix socket: only the
// calls the self-update needs.
type Docker struct {
	hc *http.Client
}

// NewDocker returns a client for the Engine at socket.
func NewDocker(socket string) *Docker {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}
	return &Docker{hc: &http.Client{Transport: tr, Timeout: 0}}
}

// APIError is an error answer of the Engine.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("docker: %d %s", e.Status, e.Message) }

// IsNotFound reports a 404 answer.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

func (d *Docker) do(ctx context.Context, method, path string, q url.Values, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	u := "http://docker/" + apiVersion + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotModified {
		var m struct {
			Message string `json:"message"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = json.Unmarshal(b, &m)
		return &APIError{Status: resp.StatusCode, Message: m.Message}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<20))
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}

// Ping checks that the Engine answers.
func (d *Docker) Ping(ctx context.Context) error { return d.do(ctx, http.MethodGet, "/_ping", nil, nil, nil) }

// Container is the part of a container inspect the self-update uses. Config,
// HostConfig and the network endpoints are kept as raw JSON so that every
// field survives the recreation unchanged.
type Container struct {
	ID              string                     `json:"Id"`
	Name            string                     `json:"Name"`
	Image           string                     `json:"Image"`
	Config          map[string]json.RawMessage `json:"Config"`
	HostConfig      map[string]json.RawMessage `json:"HostConfig"`
	Mounts          []Mount                    `json:"Mounts"`
	NetworkSettings struct {
		Networks map[string]map[string]json.RawMessage `json:"Networks"`
	} `json:"NetworkSettings"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
}

// Mount is one resolved mount of a container.
type Mount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

// ConfigString returns a string member of Config ("Image", "Hostname").
func (c Container) ConfigString(key string) string {
	var s string
	_ = json.Unmarshal(c.Config[key], &s)
	return s
}

// Env returns the environment of the container's Config.
func (c Container) Env() []string {
	var env []string
	_ = json.Unmarshal(c.Config["Env"], &env)
	return env
}

// Inspect returns a container by id or name.
func (d *Docker) Inspect(ctx context.Context, idOrName string) (Container, error) {
	var c Container
	err := d.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(idOrName)+"/json", nil, nil, &c)
	return c, err
}

// Pull pulls ref ("repo@sha256:..."). The progress stream is drained.
func (d *Docker) Pull(ctx context.Context, ref string) error {
	return d.do(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {ref}}, nil, nil)
}

// ImageID returns the local image id of ref.
func (d *Docker) ImageID(ctx context.Context, ref string) (string, error) {
	var im struct {
		ID string `json:"Id"`
	}
	err := d.do(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil, &im)
	return im.ID, err
}

// Tag tags image id as repo:tag.
func (d *Docker) Tag(ctx context.Context, image, repo, tag string) error {
	return d.do(ctx, http.MethodPost, "/images/"+url.PathEscape(image)+"/tag", url.Values{"repo": {repo}, "tag": {tag}}, nil, nil)
}

// Create creates a container from body and returns its id.
func (d *Docker) Create(ctx context.Context, name string, body any) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	q := url.Values{}
	if name != "" {
		q.Set("name", name)
	}
	err := d.do(ctx, http.MethodPost, "/containers/create", q, body, &out)
	return out.ID, err
}

// Start starts a container.
func (d *Docker) Start(ctx context.Context, id string) error {
	return d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil)
}

// Stop stops a container, waiting up to timeout.
func (d *Docker) Stop(ctx context.Context, id string, timeout time.Duration) error {
	q := url.Values{"t": {fmt.Sprint(int(timeout.Seconds()))}}
	return d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop", q, nil, nil)
}

// Rename renames a container.
func (d *Docker) Rename(ctx context.Context, id, name string) error {
	return d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/rename", url.Values{"name": {name}}, nil, nil)
}

// Remove removes a container (force).
func (d *Docker) Remove(ctx context.Context, id string) error {
	return d.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), url.Values{"force": {"1"}}, nil, nil)
}

// ListByLabel lists the ids of containers (also stopped) with label key=value.
func (d *Docker) ListByLabel(ctx context.Context, key, value string) ([]string, error) {
	f, _ := json.Marshal(map[string][]string{"label": {key + "=" + value}})
	var list []struct {
		ID string `json:"Id"`
	}
	if err := d.do(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"1"}, "filters": {string(f)}}, nil, &list); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(list))
	for _, c := range list {
		ids = append(ids, c.ID)
	}
	return ids, nil
}
