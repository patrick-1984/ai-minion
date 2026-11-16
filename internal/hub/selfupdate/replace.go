package selfupdate

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Labels and names of the self-update.
const (
	// HelperLabel marks the helper container.
	HelperLabel = "io.minion.role"
	helperValue = "upgrade-helper"
	// composeImageLabel is the label Docker Compose compares with the image
	// of the service; it must name the new image, or a later "compose up"
	// recreates the container needlessly.
	composeImageLabel = "com.docker.compose.image"
	// backupPrefix names the copy of the data directory taken while the old
	// Hub is stopped; one is kept.
	backupPrefix = ".pre-upgrade-"
	// LockFile in the data directory excludes a second update.
	LockFile = ".upgrade.lock"
)

// Replace parameters, for the helper.
type ReplaceParams struct {
	// OldID is the Hub container to replace.
	OldID string
	// ImageRef is the verified image, "repo@sha256:...".
	ImageRef string
	// ChannelRepo and ChannelTag are the repository and tag the old
	// container was created from ("ghcr.io/patrick-1984/minion", "0.9"); the
	// tag is moved to the new image locally, as "docker pull" would.
	ChannelRepo, ChannelTag string
	// From is the running release (for the backup name).
	From string
	Log  *slog.Logger
	// ReadyTimeout bounds the wait for /readyz; StopTimeout the stop.
	ReadyTimeout, StopTimeout time.Duration
	// Ready probes the new Hub; nil uses HTTP(S) /readyz on its address.
	Ready func(ctx context.Context, c Container) bool
}

// Replace stops the old Hub, keeps a copy of its data directory, starts the
// new one with the same configuration and the new image, and waits until it
// is ready. On any failure after the stop it removes the new container,
// restores the data directory and starts the old Hub again.
func Replace(ctx context.Context, d *Docker, p ReplaceParams) (err error) {
	old, err := d.Inspect(ctx, p.OldID)
	if err != nil {
		return fmt.Errorf("inspect the Hub container: %w", err)
	}
	dataDir := envValue(old.Env(), "MINION_DATA_DIR")
	if dataDir == "" {
		return errors.New("the Hub container sets no MINION_DATA_DIR; refusing to update without a known data directory")
	}
	name := strings.TrimPrefix(old.Name, "/")
	newImageID, err := d.ImageID(ctx, p.ImageRef)
	if err != nil {
		return fmt.Errorf("the verified image is not available locally: %w", err)
	}

	p.Log.Info("stopping the Hub", "container", name)
	if err := d.Stop(ctx, old.ID, p.StopTimeout); err != nil {
		return fmt.Errorf("stop the Hub: %w", err)
	}
	backup := filepath.Join(dataDir, backupPrefix+safeName(p.From))
	restart := func(reason error) error {
		p.Log.Error("update failed; restoring the previous Hub", "error", reason)
		_ = d.Rename(ctx, old.ID, name)
		if rerr := restoreDir(dataDir, backup); rerr != nil {
			p.Log.Error("cannot restore the data directory", "error", rerr, "backup", backup)
		}
		if serr := d.Start(ctx, old.ID); serr != nil {
			return fmt.Errorf("%w; and the previous Hub did not start again: %v", reason, serr)
		}
		return reason
	}
	if err := copyDir(dataDir, backup); err != nil {
		return restart(fmt.Errorf("copy the data directory: %w", err))
	}
	if err := d.Rename(ctx, old.ID, name+"-old"); err != nil {
		return restart(fmt.Errorf("rename the old container: %w", err))
	}
	body, err := recreateBody(old, p.ImageRef, newImageID)
	if err != nil {
		return restart(err)
	}
	newID, err := d.Create(ctx, name, body)
	if err != nil {
		return restart(fmt.Errorf("create the new container: %w", err))
	}
	fail := func(reason error) error {
		_ = d.Remove(context.WithoutCancel(ctx), newID)
		return restart(reason)
	}
	for net := range old.NetworkSettings.Networks {
		if net == networkMode(old) {
			continue
		}
		if err := d.Connect(ctx, net, newID, old.NetworkSettings.Networks[net]); err != nil {
			return fail(fmt.Errorf("connect network %s: %w", net, err))
		}
	}
	if err := d.Start(ctx, newID); err != nil {
		return fail(fmt.Errorf("start the new container: %w", err))
	}
	ready := p.Ready
	if ready == nil {
		ready = readyz
	}
	if !waitReady(ctx, d, newID, ready, p.ReadyTimeout) {
		return fail(errors.New("the new Hub did not become ready"))
	}
	if p.ChannelTag != "" {
		if err := d.Tag(ctx, newImageID, p.ChannelRepo, p.ChannelTag); err != nil {
			p.Log.Warn("cannot move the image tag; docker compose may recreate the Hub", "tag", p.ChannelRepo+":"+p.ChannelTag, "error", err)
		}
	}
	if err := d.Remove(ctx, old.ID); err != nil {
		p.Log.Warn("the previous Hub container could not be removed", "error", err)
	}
	removeOtherBackups(dataDir, backup)
	p.Log.Info("the Hub runs the new release", "container", name, "image", p.ImageRef)
	return nil
}

// recreateBody is the create body of the new container: the old Config and
// HostConfig unchanged except the image and the Compose image label, every
// resolved named volume pinned (an anonymous volume would otherwise be
// replaced by an empty one), and the endpoint of the primary network.
func recreateBody(old Container, imageRef, imageID string) (map[string]any, error) {
	body := map[string]any{}
	for k, v := range old.Config {
		body[k] = v
	}
	body["Image"] = imageRef
	var labels map[string]string
	if raw, ok := old.Config["Labels"]; ok {
		_ = json.Unmarshal(raw, &labels)
	}
	if labels == nil {
		labels = map[string]string{}
	}
	if _, ok := labels[composeImageLabel]; ok {
		labels[composeImageLabel] = imageID
	}
	body["Labels"] = labels

	host := map[string]any{}
	for k, v := range old.HostConfig {
		host[k] = v
	}
	var mounts []map[string]any
	if raw, ok := old.HostConfig["Mounts"]; ok {
		_ = json.Unmarshal(raw, &mounts)
	}
	var binds []string
	if raw, ok := old.HostConfig["Binds"]; ok {
		_ = json.Unmarshal(raw, &binds)
	}
	covered := map[string]bool{}
	for _, b := range binds {
		if parts := strings.Split(b, ":"); len(parts) >= 2 {
			covered[parts[1]] = true
		}
	}
	for _, m := range mounts {
		if t, ok := m["Target"].(string); ok {
			covered[t] = true
		}
	}
	for _, m := range old.Mounts {
		if m.Type == "volume" && m.Name != "" && !covered[m.Destination] {
			mounts = append(mounts, map[string]any{"Type": "volume", "Source": m.Name, "Target": m.Destination, "ReadOnly": !m.RW})
		}
	}
	if len(mounts) > 0 {
		host["Mounts"] = mounts
	}
	body["HostConfig"] = host
	if nm := networkMode(old); nm != "" {
		if ep, ok := old.NetworkSettings.Networks[nm]; ok {
			body["NetworkingConfig"] = map[string]any{"EndpointsConfig": map[string]any{nm: endpoint(ep)}}
		}
	}
	return body, nil
}

// endpoint keeps the user-set parts of an endpoint (aliases, static IPs).
func endpoint(ep map[string]json.RawMessage) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for _, k := range []string{"Aliases", "IPAMConfig", "Links", "DriverOpts"} {
		if v, ok := ep[k]; ok && string(v) != "null" {
			out[k] = v
		}
	}
	return out
}

func networkMode(c Container) string {
	var nm string
	_ = json.Unmarshal(c.HostConfig["NetworkMode"], &nm)
	if nm == "default" {
		return "bridge"
	}
	return nm
}

// Connect attaches a container to one more network with its endpoint settings.
func (d *Docker) Connect(ctx context.Context, network, id string, ep map[string]json.RawMessage) error {
	body := map[string]any{"Container": id, "EndpointConfig": endpoint(ep)}
	return d.do(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/connect", nil, body, nil)
}

// waitReady polls ready until it answers three times in a row (sustained,
// not the first green), the container stops, or the timeout ends.
func waitReady(ctx context.Context, d *Docker, id string, ready func(context.Context, Container) bool, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ok := 0
	for {
		c, err := d.Inspect(ctx, id)
		if err == nil && c.State.Running && ready(ctx, c) {
			ok++
			if ok >= 3 {
				return true
			}
		} else {
			ok = 0
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(3 * time.Second):
		}
	}
}

// readyz asks the new Hub's /readyz on its address in its first network.
func readyz(ctx context.Context, c Container) bool {
	host := ""
	for _, ep := range c.NetworkSettings.Networks {
		var ip string
		_ = json.Unmarshal(ep["IPAddress"], &ip)
		if ip != "" {
			host = ip
			break
		}
	}
	if networkMode(c) == "host" || host == "" {
		host = "127.0.0.1"
	}
	env := c.Env()
	listen := envValue(env, "MINION_LISTEN")
	port := "8443"
	if i := strings.LastIndex(listen, ":"); i >= 0 && listen[i+1:] != "" {
		port = listen[i+1:]
	}
	scheme := "https"
	if envValue(env, "MINION_TLS") == "off" {
		scheme = "http"
	}
	// A local probe of the container just created: its certificate is the
	// Hub's own (often self-signed), so it is not verified here.
	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // local readiness probe
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, scheme+"://"+host+":"+port+"/readyz", nil)
	if err != nil {
		return false
	}
	resp, err := hc.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func envValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			return r
		}
		return '_'
	}, s)
}

// skipEntry reports the entries of the data directory that the backup
// never copies or restores: earlier backups and the update lock.
func skipEntry(name string) bool {
	return strings.HasPrefix(name, backupPrefix) || name == LockFile
}

// copyDir copies the regular files and directories of src into dst (created
// fresh), skipping backups and the lock. Links are not followed.
func copyDir(src, dst string) error {
	_ = os.RemoveAll(dst)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if skipEntry(e.Name()) {
			continue
		}
		if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyTree(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case fi.IsDir():
		if err := os.MkdirAll(dst, fi.Mode().Perm()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	case fi.Mode().IsRegular():
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	}
	return nil // links, sockets and devices are not part of the Hub data
}

// restoreDir replaces the content of dataDir (except backups and the lock)
// with the backup.
func restoreDir(dataDir, backup string) error {
	if _, err := os.Stat(backup); err != nil {
		return err
	}
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if skipEntry(e.Name()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dataDir, e.Name())); err != nil {
			return err
		}
	}
	entries, err = os.ReadDir(backup)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := copyTree(filepath.Join(backup, e.Name()), filepath.Join(dataDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func removeOtherBackups(dataDir, keep string) {
	entries, _ := os.ReadDir(dataDir)
	for _, e := range entries {
		p := filepath.Join(dataDir, e.Name())
		if strings.HasPrefix(e.Name(), backupPrefix) && p != keep {
			_ = os.RemoveAll(p)
		}
	}
}
