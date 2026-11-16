package selfupdate

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Repository is the release image repository.
const Repository = "ghcr.io/patrick-1984/minion"

// channelTagRE are the tags that follow a channel (they move with releases):
// latest, X and X.Y. An exact vX.Y.Z tag pins the version: then a later
// "docker compose up" would put the old release back, so the Hub refuses.
var channelTagRE = regexp.MustCompile(`^(latest|\d+|\d+\.\d+)$`)

// Self is the Hub's own container, when the update is possible.
type Self struct {
	Container  Container
	Repo, Tag  string
	DataDir    string
	SocketPath string
}

// Detect finds the Hub's own container and checks that it can replace itself.
// It returns a reason in plain words when it cannot.
func Detect(ctx context.Context, d *Docker, socket string) (Self, string) {
	if _, err := os.Stat(socket); err != nil {
		return Self{}, "the Docker socket is not mounted into the Hub container (use deploy/compose/hub.self-update.yaml)"
	}
	if err := d.Ping(ctx); err != nil {
		return Self{}, "the Docker socket does not answer (is the Hub in the docker group? set DOCKER_GID): " + err.Error()
	}
	c, err := findSelf(ctx, d)
	if err != nil {
		return Self{}, "the Hub's own container was not found: " + err.Error()
	}
	repo, tag := splitImage(c.ConfigString("Image"))
	if repo != Repository {
		return Self{}, fmt.Sprintf("the Hub runs the image %q, not %s", c.ConfigString("Image"), Repository)
	}
	if !channelTagRE.MatchString(tag) {
		return Self{}, fmt.Sprintf("the Hub's image tag %q pins one version; use a channel tag (for example %s:0.9, set MINION_TAG in .env) so that docker compose keeps the update", tag, Repository)
	}
	data := envValue(c.Env(), "MINION_DATA_DIR")
	if data == "" {
		return Self{}, "the Hub container sets no MINION_DATA_DIR"
	}
	return Self{Container: c, Repo: repo, Tag: tag, DataDir: data, SocketPath: socket}, ""
}

// findSelf inspects the container whose id or hostname this process has.
func findSelf(ctx context.Context, d *Docker) (Container, error) {
	if id := containerIDFromMountinfo("/proc/self/mountinfo"); id != "" {
		if c, err := d.Inspect(ctx, id); err == nil {
			return c, nil
		}
	}
	host, err := os.Hostname()
	if err != nil {
		return Container{}, err
	}
	return d.Inspect(ctx, host)
}

var containerIDRE = regexp.MustCompile(`/containers/([0-9a-f]{64})/`)

func containerIDFromMountinfo(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := containerIDRE.FindStringSubmatch(sc.Text()); m != nil {
			return m[1]
		}
	}
	return ""
}

// splitImage splits "repo:tag" (a digest ref has no tag).
func splitImage(ref string) (repo, tag string) {
	if i := strings.Index(ref, "@"); i >= 0 {
		return ref[:i], ""
	}
	slash := strings.LastIndex(ref, "/")
	if i := strings.LastIndex(ref, ":"); i > slash {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}

// ErrBusy says an update is already running.
var ErrBusy = errors.New("a Hub update is already running")

// StartHelper pulls the verified image and starts the helper container that
// replaces this Hub. It returns at once; this process is stopped by the
// helper moments later.
func StartHelper(ctx context.Context, d *Docker, self Self, imageRef, from string) (string, error) {
	if err := acquireLock(ctx, d, self.DataDir); err != nil {
		return "", err
	}
	release := func() { _ = os.Remove(filepath.Join(self.DataDir, LockFile)) }
	if err := d.Pull(ctx, imageRef); err != nil {
		release()
		return "", fmt.Errorf("pull %s: %w", imageRef, err)
	}
	var groups json.RawMessage = self.Container.HostConfig["GroupAdd"]
	if len(groups) == 0 {
		groups = json.RawMessage("null")
	}
	body := map[string]any{
		"Image": imageRef,
		"Cmd": []string{"hub", "upgrade-helper", "--container", self.Container.ID, "--image", imageRef,
			"--channel", self.Repo + ":" + self.Tag, "--from", from},
		"Labels": map[string]string{HelperLabel: helperValue},
		"HostConfig": map[string]any{
			// The helper sees the Hub's volumes (data directory, the Docker
			// socket) at the same paths and has the same groups.
			"VolumesFrom": []string{self.Container.ID},
			"GroupAdd":    groups,
			"NetworkMode": networkMode(self.Container),
			"AutoRemove":  true,
		},
	}
	id, err := d.Create(ctx, "", body)
	if err != nil {
		release()
		return "", fmt.Errorf("create the update helper: %w", err)
	}
	if err := os.WriteFile(filepath.Join(self.DataDir, LockFile), []byte(id), 0o600); err != nil {
		_ = d.Remove(ctx, id)
		release()
		return "", err
	}
	if err := d.Start(ctx, id); err != nil {
		_ = d.Remove(ctx, id)
		release()
		return "", fmt.Errorf("start the update helper: %w", err)
	}
	return id, nil
}

// acquireLock creates the lock file, or reports ErrBusy when a helper still
// exists. A lock left by a helper that no longer exists is stale and taken.
func acquireLock(ctx context.Context, d *Docker, dataDir string) error {
	p := filepath.Join(dataDir, LockFile)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		return f.Close()
	}
	if !os.IsExist(err) {
		return err
	}
	if ids, lerr := d.ListByLabel(ctx, HelperLabel, helperValue); lerr == nil && len(ids) == 0 {
		_ = os.Remove(p)
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return ErrBusy
		}
		return f.Close()
	}
	return ErrBusy
}
