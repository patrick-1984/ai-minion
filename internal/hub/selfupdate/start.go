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
	"sync"
	"time"
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
// channelTag is the image tag of the configured update channel ("0.9", "0",
// "latest"): the container must run that tag (as Compose created it) or a
// digest of the release repository (after an earlier self-update). It
// returns a reason in plain words when it cannot.
func Detect(ctx context.Context, d *Docker, socket, channelTag string) (Self, string) {
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
	ref := c.ConfigString("Image")
	repo, tag := splitImage(ref)
	if repo != Repository {
		return Self{}, fmt.Sprintf("the Hub runs the image %q, not %s", ref, Repository)
	}
	digest := strings.Contains(ref, "@")
	switch {
	case digest:
		// Created by an earlier self-update: the channel tag was moved then.
	case !channelTagRE.MatchString(tag):
		return Self{}, fmt.Sprintf("the Hub's image tag %q pins one version; use the channel tag %s:%s (set MINION_TAG=%s in .env) so that docker compose keeps the update", tag, Repository, channelTag, channelTag)
	case tag != channelTag:
		return Self{}, fmt.Sprintf("the Hub runs the tag %q but update_channel follows %q; set MINION_TAG=%s in .env (or change update_channel) so that docker compose and the update agree", tag, channelTag, channelTag)
	}
	tag = channelTag
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
	prepMu.Lock() // one preparation per Hub process at a time
	defer prepMu.Unlock()
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

// prepMu serialises the preparation (lock, pull, helper start) inside one Hub.
var prepMu sync.Mutex

// prepMark is the lock content while the Hub prepares (before the helper id
// replaces it).
const prepMark = "preparing"

// acquireLock creates the lock file, or reports ErrBusy. A lock is stale only
// when it names a helper container that no longer exists, or a preparation
// older than the longest one (an earlier Hub process that died while pulling).
func acquireLock(ctx context.Context, d *Docker, dataDir string) error {
	p := filepath.Join(dataDir, LockFile)
	create := func() error {
		f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, werr := f.WriteString(prepMark)
		return errors.Join(werr, f.Close())
	}
	err := create()
	if err == nil {
		return nil
	}
	if !os.IsExist(err) {
		return err
	}
	b, rerr := os.ReadFile(p)
	fi, serr := os.Stat(p)
	if rerr != nil || serr != nil {
		return ErrBusy
	}
	content := strings.TrimSpace(string(b))
	stale := false
	switch {
	case content == prepMark || content == "":
		stale = time.Since(fi.ModTime()) > 30*time.Minute
	default:
		if _, ierr := d.Inspect(ctx, content); IsNotFound(ierr) {
			stale = true
		}
	}
	if !stale {
		return ErrBusy
	}
	_ = os.Remove(p)
	if err := create(); err != nil {
		return ErrBusy
	}
	return nil
}
