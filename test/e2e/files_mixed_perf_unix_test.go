//go:build e2e && unix

package e2e

import (
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// WP11: mixed versions (a Worker without files.v1, as v0.23: the Hub answers
// unsupported_op without opening a stream, and the connection keeps working)
// and the terminal's responsiveness on the Worker connection under a bulk
// transfer (NFR-3).

func TestFilesOldWorkerWithoutFilesV1GetsUnsupportedOpAndKeepsWorking(t *testing.T) {
	e := startFilesE2E(t)
	old := e.startWorker("old", fxWorkerOptions{noFiles: true}, "src")
	cur := e.startWorker("cur", fxWorkerOptions{}, "src")
	writeFile(t, old.dirs["src"], "a.txt", []byte("a"))
	bearer := e.key("files", fileActions, old, cur)

	on, ok := e.ph.wsv.Registry().Get(old.id)
	if !ok || on.HasFeature(proto.FeatureFilesV1) || slices.Contains(on.Ops, ops.OpFilePull) {
		t.Fatalf("the old Worker announces file transfer: %+v", on)
	}
	if on, ok := e.ph.wsv.Registry().Get(cur.id); !ok || !on.HasFeature(proto.FeatureFilesV1) {
		t.Fatal("the current Worker does not announce files.v1")
	}
	tgt := target(old, "src")
	for _, c := range []struct {
		op   string
		args any
	}{
		{ops.OpFileList, ops.FileListArgs{Target: tgt}},
		{ops.OpFilePull, ops.FilePullArgs{Target: tgt, Path: "a.txt"}},
		{ops.OpFilePush, map[string]any{"target": tgt, "path": "a.txt", "as": "a.txt", "kind": "file", "size": 1}},
	} {
		if err := e.call(bearer, c.op, c.args, nil); codeOf(err) != proto.CodeUnsupportedOp {
			t.Fatalf("%s on an old Worker: %v", c.op, err)
		}
	}
	// The caller end gets the same answer and publishes nothing.
	cl := e.caller(bearer)
	if _, err := agent.CallerPull(ctxT(t), cl.FileCaller, ops.FilePullArgs{Target: tgt, Path: "a.txt"}); codeOf(err) != proto.CodeUnsupportedOp {
		t.Fatalf("caller pull from an old Worker: %v", err)
	}
	if exists(cl.dir, "a.txt") {
		t.Fatal("a refused pull was published")
	}
	// The old Worker's connection still serves its ops: no stream error.
	m := hubWrite(t, e.ph.wsv, old.id, ops.OpSessionSnapshot, []ops.Action{ops.ActionRead}, ops.SessionSnapshotArgs{Target: ops.Target{SessionName: "src"}})
	if k := frameKind(m); k != proto.TypeOpResult {
		t.Fatalf("session_snapshot after the refusals: %v", m)
	}
	if !e.ph.wsv.Registry().IsOnline(old.id) {
		t.Fatal("the old Worker went offline")
	}
}

// snapshotLatencies measures n session_snapshot round trips on the Worker
// connection, as the Hub's terminal view does them.
func snapshotLatencies(t *testing.T, e *fxE2E, w *fxWorker, n int, stop <-chan struct{}) []time.Duration {
	t.Helper()
	out := make([]time.Duration, 0, n)
	for range n {
		select {
		case <-stop:
			return out
		default:
		}
		start := time.Now()
		m := hubWrite(t, e.ph.wsv, w.id, ops.OpSessionSnapshot, []ops.Action{ops.ActionRead}, ops.SessionSnapshotArgs{Target: ops.Target{SessionName: "src"}})
		if k := frameKind(m); k != proto.TypeOpResult {
			t.Fatalf("session_snapshot: %v", m)
		}
		out = append(out, time.Since(start))
		time.Sleep(20 * time.Millisecond)
	}
	return out
}

func p99(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := slices.Clone(d)
	slices.Sort(s)
	i := (len(s)*99 + 99) / 100
	return s[min(i, len(s))-1]
}

func TestFilesTerminalLatencyUnderABulkTransfer(t *testing.T) {
	if testing.Short() {
		t.Skip("bulk transfer")
	}
	e := startFilesE2E(t)
	w := e.startWorker("wa", fxWorkerOptions{}, "src")
	bearer := e.key("files", fileActions, w)
	size := 192 << 20
	content := make([]byte, size)
	_, _ = rand.NewChaCha8([32]byte{1}).Read(content) // incompressible: identity on the wire, the pacer is the only brake
	writeFile(t, w.dirs["src"], "bulk.bin", content)

	base := snapshotLatencies(t, e, w, 40, nil)

	c := e.caller(bearer)
	type out struct {
		res ops.FileTransferResult
		err error
		dur time.Duration
	}
	done := make(chan out, 1)
	stop := make(chan struct{})
	go func() {
		start := time.Now()
		res, err := agent.CallerPull(ctxT(t), c.FileCaller, ops.FilePullArgs{Target: target(w, "src"), Path: "bulk.bin"})
		close(stop)
		done <- out{res, err, time.Since(start)}
	}()
	time.Sleep(100 * time.Millisecond) // the leg is open and saturating
	loaded := snapshotLatencies(t, e, w, 60, stop)
	o := <-done
	if o.err != nil || o.res.Outcome != ops.OutcomeDelivered {
		t.Fatalf("bulk pull: %+v %v", o.res, o.err)
	}
	mbps := float64(size) / o.dur.Seconds() / 1e6
	added := p99(loaded) - p99(base)
	t.Logf("bulk pull of %d MiB in %s (%.1f MB/s); session_snapshot p99 %s idle, %s under load (%d samples), added %s",
		size>>20, o.dur.Round(time.Millisecond), mbps, p99(base), p99(loaded), len(loaded), added)
	if len(loaded) < 10 {
		t.Skipf("the transfer ended after %d samples: too fast to measure here", len(loaded))
	}
	// NFR-3: the added p99 latency of the terminal on the same Worker
	// connection is at most 50 ms (BRD target at 10 ms RTT; this is
	// loopback, so the bound is the same and the RTT adds nothing).
	if added > 50*time.Millisecond {
		t.Fatalf("session_snapshot p99 grew by %s under a bulk transfer (NFR-3: <= 50ms)", added)
	}
}
