package truenas

import (
	"context"
	"time"

	"github.com/patrick-1984/ai-minion/internal/version"
)

// JobState is a middleware job state.
type JobState string

// Job states of the TrueNAS middleware.
const (
	JobWaiting JobState = "WAITING"
	JobRunning JobState = "RUNNING"
	JobSuccess JobState = "SUCCESS"
	JobFailed  JobState = "FAILED"
	JobAborted JobState = "ABORTED"
	// JobUnknown: the middleware does not know the job (it restarted).
	JobUnknown JobState = "UNKNOWN"
)

// Active reports whether the job still runs.
func (s JobState) Active() bool { return s == JobWaiting || s == JobRunning }

// App states the executor looks at.
const (
	AppRunning   = "RUNNING"
	AppStopped   = "STOPPED"
	AppDeploying = "DEPLOYING"
)

// Middleware is the part of the TrueNAS middleware API the executor uses.
// Job calls submit and return the job id; WaitJob blocks (bounded by ctx).
// Implementations must never include configuration values in errors.
type Middleware interface {
	AppState(ctx context.Context, app string) (string, error)
	// IsCustom reports whether the app is a custom (YAML) app.
	IsCustom(ctx context.Context, app string) (bool, error)
	AppConfig(ctx context.Context, app string) (Compose, error)
	Stop(ctx context.Context, app string) (int, error)
	Start(ctx context.Context, app string) (int, error)
	Update(ctx context.Context, app string, c Compose) (int, error)
	JobState(ctx context.Context, id int) (JobState, error)
	// FindJobs lists recent jobs of a method for an app that started at or
	// after since (to find a job whose id was lost with a killed run).
	FindJobs(ctx context.Context, method, app string, since time.Time) ([]JobInfo, error)
}

// JobInfo is a middleware job found by FindJobs.
type JobInfo struct {
	ID    int
	State JobState
}

// Container is a container of the app's compose project.
type Container struct {
	ID      string
	Service string
	ImageID string
	Running bool
	// StartedAt identifies this run of the container (changes on restart).
	StartedAt string
	// IP is the address the host reaches the container on (127.0.0.1 for host networking).
	IP string
	// Health is the Docker health status ("" when the image has no health check).
	Health string
}

// Docker is the part of the Docker host CLI the executor uses.
type Docker interface {
	// Pull pulls ref (repository@digest).
	Pull(ctx context.Context, ref string) error
	// ImageID returns the local image id (sha256:...) of ref; ErrNoImage when absent.
	ImageID(ctx context.Context, ref string) (string, error)
	// Containers lists every container (any state) of a compose project.
	Containers(ctx context.Context, project string) ([]Container, error)
	// MountSource returns the host source path of the bind mount at dest.
	MountSource(ctx context.Context, containerID, dest string) (string, error)
}

// DatasetInfo identifies a ZFS filesystem.
type DatasetInfo struct{ Name, GUID string }

// ZFS is the part of the zfs CLI the executor uses. All calls name one
// dataset; none is recursive.
type ZFS interface {
	Available(ctx context.Context, dataset string) (uint64, error)
	// DatasetAt returns the filesystem whose mountpoint is exactly path.
	DatasetAt(ctx context.Context, path string) (DatasetInfo, error)
	Snapshot(ctx context.Context, dataset, name string) error
	Hold(ctx context.Context, dataset, name string) error
	Release(ctx context.Context, dataset, name string) error
	SnapshotExists(ctx context.Context, dataset, name string) (bool, error)
	// Rollback is a plain rollback to the snapshot (no -r): it fails when a
	// newer snapshot exists, which the executor reports as recovery_required.
	Rollback(ctx context.Context, dataset, name string) error
	Destroy(ctx context.Context, dataset, name string) error
}

// Prober asks a readiness URL (the container's own address) for /readyz.
type Prober interface {
	Ready(ctx context.Context, url string) error
}

// Release is a verified release (signed manifest).
type Release struct {
	Tag        string
	Prerelease bool
	Repo       string
	Digest     string
	Range      version.ProtoRange
}

// Ref is "repository@digest".
func (r Release) Ref() string { return r.Repo + "@" + r.Digest }

// ReleaseSource resolves a selector ("latest", "X", "X.Y" or "vX.Y.Z") to a
// release whose signatures are verified.
type ReleaseSource interface {
	Verified(ctx context.Context, selector string) (Release, error)
}

// Timeouts bound every wait of the executor.
type Timeouts struct {
	Pull     time.Duration // docker pull of one image
	Job      time.Duration // one middleware job
	Stop     time.Duration // until no container of the app runs
	Ready    time.Duration // total time to reach sustained readiness
	Sustain  time.Duration // continuous readiness required
	Poll     time.Duration // polling interval
	ZFS      time.Duration // one zfs command
	Reconcil time.Duration // waiting for a job of an interrupted run
	Call     time.Duration // one docker, zfs or middleware call
	Restore  time.Duration // whole budget of one restore
	Deploy   time.Duration // waiting for DEPLOYING to clear once jobs are settled
	Final    time.Duration // sustained readiness after the read-only trial ends
}

// DefaultTimeouts are the production bounds.
func DefaultTimeouts() Timeouts {
	return Timeouts{
		Pull: 15 * time.Minute, Job: 10 * time.Minute, Stop: 3 * time.Minute,
		Ready: 5 * time.Minute, Sustain: 60 * time.Second, Poll: 5 * time.Second,
		ZFS: time.Minute, Reconcil: 10 * time.Minute,
		Call: 30 * time.Second, Restore: 30 * time.Minute, Deploy: 2 * time.Minute, Final: 20 * time.Second,
	}
}
