package agentdetect

import (
	"context"
	"errors"
	"sync"
	"time"
)

// SnapshotTimeout bounds the one process-table query of a snapshot.
const SnapshotTimeout = 5 * time.Second

// errSnapshotTimeout reports a process table that did not answer in time.
var errSnapshotTimeout = errors.New("agentdetect: process table query timed out")

// Snapshot returns a Detector that reads the process table at most once, on
// first use, bounded by timeout (zero means SnapshotTimeout), and shares the
// answer (or the error) across all later classifications. A source that
// blocks past the bound counts as unavailable, so the panes fall back to the
// current command alone instead of stalling the caller.
func (d *Detector) Snapshot(timeout time.Duration) *Detector {
	if d.procs == nil {
		return d
	}
	if timeout <= 0 {
		timeout = SnapshotTimeout
	}
	c := *d
	c.procs = &snapshotSource{inner: d.procs, timeout: timeout}
	return &c
}

type snapshotSource struct {
	inner   ProcessSource
	timeout time.Duration
	once    sync.Once
	procs   []Proc
	err     error
}

// Processes implements ProcessSource.
func (s *snapshotSource) Processes(ctx context.Context) ([]Proc, error) {
	s.once.Do(func() {
		qctx, cancel := context.WithTimeout(ctx, s.timeout)
		defer cancel()
		type res struct {
			p   []Proc
			err error
		}
		ch := make(chan res, 1)
		go func() {
			p, err := s.inner.Processes(qctx)
			ch <- res{p, err}
		}()
		select {
		case r := <-ch:
			s.procs, s.err = r.p, r.err
		case <-qctx.Done():
			s.err = errSnapshotTimeout
		}
	})
	return s.procs, s.err
}
