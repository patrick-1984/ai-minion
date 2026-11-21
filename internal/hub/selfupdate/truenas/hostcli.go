package truenas

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	imageIDRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	refRE     = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]*@sha256:[0-9a-f]{64}$`)
	snapRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,100}$`)
)

// DockerCLI implements [Docker] with the host docker CLI.
type DockerCLI struct{ Runner Runner }

// Pull implements Docker.
func (d DockerCLI) Pull(ctx context.Context, ref string) error {
	if !refRE.MatchString(ref) {
		return fmt.Errorf("docker pull: invalid reference")
	}
	_, err := d.Runner.Run(ctx, nil, "docker", "pull", "--quiet", ref)
	return err
}

// ImageID implements Docker.
func (d DockerCLI) ImageID(ctx context.Context, ref string) (string, error) {
	out, err := d.Runner.Run(ctx, nil, "docker", "image", "inspect", "--format", "{{.Id}}", ref)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", ErrNoImage
	}
	id := strings.TrimSpace(string(out))
	if !imageIDRE.MatchString(id) {
		return "", fmt.Errorf("docker image inspect: unexpected image id")
	}
	return id, nil
}

// Containers implements Docker.
func (d DockerCLI) Containers(ctx context.Context, project string) ([]Container, error) {
	out, err := d.Runner.Run(ctx, nil, "docker", "ps", "-a", "--no-trunc", "--quiet",
		"--filter", "label=com.docker.compose.project="+project)
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil, nil
	}
	args := append([]string{"inspect", "--format",
		`{{.Id}}|{{.Image}}|{{index .Config.Labels "com.docker.compose.service"}}|{{.State.Running}}`}, ids...)
	out, err = d.Runner.Run(ctx, nil, "docker", args...)
	if err != nil {
		return nil, err
	}
	var cs []Container
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) != 4 {
			return nil, fmt.Errorf("docker inspect: unexpected output")
		}
		run, _ := strconv.ParseBool(f[3])
		cs = append(cs, Container{ID: f[0], ImageID: f[1], Service: f[2], Running: run})
	}
	return cs, nil
}

// ZFSCLI implements [ZFS] with the zfs CLI.
type ZFSCLI struct{ Runner Runner }

func (z ZFSCLI) snap(dataset, name string) (string, error) {
	if !datasetRE.MatchString(dataset) || !snapRE.MatchString(name) {
		return "", fmt.Errorf("zfs: invalid dataset or snapshot name")
	}
	return dataset + "@" + name, nil
}

// Available implements ZFS.
func (z ZFSCLI) Available(ctx context.Context, dataset string) (uint64, error) {
	out, err := z.Runner.Run(ctx, nil, "zfs", "list", "-Hp", "-o", "available", dataset)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
}

// Snapshot implements ZFS (not recursive).
func (z ZFSCLI) Snapshot(ctx context.Context, dataset, name string) error {
	s, err := z.snap(dataset, name)
	if err != nil {
		return err
	}
	_, err = z.Runner.Run(ctx, nil, "zfs", "snapshot", s)
	return err
}

// Hold implements ZFS.
func (z ZFSCLI) Hold(ctx context.Context, dataset, name string) error {
	s, err := z.snap(dataset, name)
	if err != nil {
		return err
	}
	_, err = z.Runner.Run(ctx, nil, "zfs", "hold", "minion-hub-updater", s)
	return err
}

// Release implements ZFS.
func (z ZFSCLI) Release(ctx context.Context, dataset, name string) error {
	s, err := z.snap(dataset, name)
	if err != nil {
		return err
	}
	_, err = z.Runner.Run(ctx, nil, "zfs", "release", "minion-hub-updater", s)
	return err
}

// SnapshotExists implements ZFS.
func (z ZFSCLI) SnapshotExists(ctx context.Context, dataset, name string) (bool, error) {
	s, err := z.snap(dataset, name)
	if err != nil {
		return false, err
	}
	out, err := z.Runner.Run(ctx, nil, "zfs", "list", "-H", "-t", "snapshot", "-o", "name", "-d", "1", dataset)
	if err != nil {
		return false, err
	}
	for _, l := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(l) == s {
			return true, nil
		}
	}
	return false, nil
}

// Rollback implements ZFS (no -r, no -f).
func (z ZFSCLI) Rollback(ctx context.Context, dataset, name string) error {
	s, err := z.snap(dataset, name)
	if err != nil {
		return err
	}
	_, err = z.Runner.Run(ctx, nil, "zfs", "rollback", s)
	return err
}

// Destroy implements ZFS.
func (z ZFSCLI) Destroy(ctx context.Context, dataset, name string) error {
	s, err := z.snap(dataset, name)
	if err != nil {
		return err
	}
	_, err = z.Runner.Run(ctx, nil, "zfs", "destroy", s)
	return err
}

// HTTPProber implements [Prober]: GET url must answer 200 within 5 s; no
// redirects are followed.
type HTTPProber struct {
	URL    string
	Client *http.Client
}

// Ready implements Prober.
func (p HTTPProber) Ready(ctx context.Context) error {
	c := p.Client
	if c == nil {
		c = &http.Client{
			Timeout:       5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("not ready")
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("not ready (status %d)", resp.StatusCode)
	}
	return nil
}
