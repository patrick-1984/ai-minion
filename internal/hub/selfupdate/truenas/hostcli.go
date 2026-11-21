package truenas

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
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
		`{{.Id}}|{{.Image}}|{{index .Config.Labels "com.docker.compose.service"}}|{{.State.Running}}|{{.State.StartedAt}}|{{.HostConfig.NetworkMode}}|{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}|{{if .State.Health}}{{.State.Health.Status}}{{end}}|{{range .Config.Env}}{{if eq . "MINION_MAINTENANCE=readonly"}}readonly{{end}}{{end}}`}, ids...)
	out, err = d.Runner.Run(ctx, nil, "docker", args...)
	if err != nil {
		return nil, err
	}
	var cs []Container
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) != 9 {
			return nil, fmt.Errorf("docker inspect: unexpected output")
		}
		run, _ := strconv.ParseBool(f[3])
		ip := ""
		// Host networking cannot tie a listener to this container. Refuse it
		// instead of accepting a different Hub listening on host loopback.
		if ips := strings.Fields(f[6]); f[5] != "host" && len(ips) > 0 {
			ip = ips[0]
		}
		cs = append(cs, Container{ID: f[0], ImageID: f[1], Service: f[2], Running: run, StartedAt: f[4], IP: ip, Health: f[7], Maintenance: f[8] == "readonly"})
	}
	return cs, nil
}

// MountSource implements Docker: the host source of the bind mount at dest.
func (d DockerCLI) MountSource(ctx context.Context, containerID, dest string) (string, error) {
	out, err := d.Runner.Run(ctx, nil, "docker", "inspect", "--format",
		`{{range .Mounts}}{{.Destination}}|{{.Type}}|{{.Source}}{{"\n"}}{{end}}`, containerID)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.SplitN(strings.TrimSpace(line), "|", 3)
		if len(f) == 3 && f[0] == dest && f[1] == "bind" {
			return f[2], nil
		}
	}
	return "", fmt.Errorf("docker inspect: no bind mount at %s", dest)
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

// DatasetAt implements ZFS: the filesystem mounted exactly at path.
func (z ZFSCLI) DatasetAt(ctx context.Context, path string) (DatasetInfo, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return DatasetInfo{}, err
	}
	out, err := z.Runner.Run(ctx, nil, "findmnt", "--json", "--target", real, "--output", "SOURCE,FSTYPE,TARGET")
	if err != nil {
		return DatasetInfo{}, err
	}
	var mounts struct {
		Filesystems []struct {
			Source string `json:"source"`
			FSType string `json:"fstype"`
			Target string `json:"target"`
		} `json:"filesystems"`
	}
	if err := json.Unmarshal(out, &mounts); err != nil || len(mounts.Filesystems) != 1 {
		return DatasetInfo{}, fmt.Errorf("zfs: cannot resolve the effective data filesystem")
	}
	m := mounts.Filesystems[0]
	if m.FSType != "zfs" || m.Target != real || !datasetRE.MatchString(m.Source) {
		return DatasetInfo{}, fmt.Errorf("zfs: no dedicated dataset is mounted at the data path")
	}
	out, err = z.Runner.Run(ctx, nil, "zfs", "get", "-Hp", "-o", "value", "guid", m.Source)
	if err != nil {
		return DatasetInfo{}, err
	}
	guid := strings.TrimSpace(string(out))
	if _, err := strconv.ParseUint(guid, 10, 64); err != nil {
		return DatasetInfo{}, fmt.Errorf("zfs: invalid dataset identity")
	}
	return DatasetInfo{Name: m.Source, GUID: guid}, nil
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
// redirects are followed. The URL is the container's own address, whose
// certificate is the Hub's own (often self-signed), so it is not verified.
type HTTPProber struct{ Client *http.Client }

// Ready implements Prober.
func (p HTTPProber) Ready(ctx context.Context, url string) error {
	c := p.Client
	if c == nil {
		c = &http.Client{
			Timeout:       5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // local probe of the container itself
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
