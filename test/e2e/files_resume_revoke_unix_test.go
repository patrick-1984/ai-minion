//go:build e2e && unix

package e2e

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/ops"
	"github.com/patrick-1984/ai-minion/internal/proto"
	"github.com/patrick-1984/ai-minion/internal/worker/agent"
)

// WP11: resume of a single file with Range and If-Range on the Hub's public
// leg endpoint (FR-19, NFR-5), and revocation while the bytes move (SEC-2:
// the commit gate withholds the trailers, nothing is published).

// legGet admits a pull of path with bearer and GETs its leg with the extra
// headers. It returns the admission and the response.
func (e *fxE2E) legGet(bearer string, tgt ops.Target, path string, h map[string]string) (ops.FileTransferResult, *http.Response) {
	e.t.Helper()
	var adm ops.FileTransferResult
	if err := e.call(bearer, ops.OpFilePull, ops.FilePullArgs{Target: tgt, Path: path}, &adm); err != nil {
		e.t.Fatalf("file_pull admission: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, "https://"+e.ph.addr+proto.FilesLegPath(adm.TransferID), nil)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set(proto.HeaderLegToken, adm.LegToken)
	req.Header.Set("Accept-Encoding", "identity")
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := e.bulk.Do(req)
	if err != nil {
		e.t.Fatalf("GET of the leg: %v", err)
	}
	return adm, resp
}

// receipt sends the receipt of a raw leg GET, as a receiving CLI does:
// the transfer then gives its concurrency slots back at once.
func (e *fxE2E) receipt(bearer string, adm ops.FileTransferResult, outcome ops.Outcome, n int64, trailer http.Header) {
	e.t.Helper()
	r := ops.FileReceipt{Outcome: outcome, Files: 1, Bytes: n}
	if md, ok := proto.ParseDigestField(trailer.Get(proto.HeaderManifestDigest)); ok {
		r.ManifestDigest = hex.EncodeToString(md)
	}
	if outcome != ops.OutcomeDelivered {
		r = ops.FileReceipt{Outcome: outcome, Reason: string(proto.CodeConnectionLost)}
	}
	if err := e.call(bearer, ops.OpFileReceipt, ops.FileReceiptArgs{TransferID: adm.TransferID, LegToken: adm.LegToken, FileReceipt: r}, nil); err != nil {
		e.t.Fatalf("file_receipt: %v", err)
	}
}

func TestFilesResumeOfASingleFileWithRangeAndIfRange(t *testing.T) {
	e := startFilesE2E(t)
	w := e.startWorker("wa", fxWorkerOptions{}, "src")
	w.ahead.Store(int64(10 * time.Second)) // every file is stable
	bearer := e.key("files", fileActions, w)
	tgt := target(w, "src")
	content := noise(4 << 20)
	writeFile(t, w.dirs["src"], "big.iso", content)

	// The first download is interrupted after half of the file.
	adm, resp := e.legGet(bearer, tgt, "big.iso", nil)
	if resp.StatusCode != http.StatusOK || adm.ETag == "" || adm.Size != int64(len(content)) {
		t.Fatalf("first GET: %d, admission %+v", resp.StatusCode, adm)
	}
	half := len(content) / 2
	first := make([]byte, half)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	e.receipt(bearer, adm, ops.OutcomeFailedNotSent, 0, nil)

	// The resume: a new admission, Range from where it stopped, If-Range
	// with the strong ETag.
	adm2, resp := e.legGet(bearer, tgt, "big.iso", map[string]string{"Range": "bytes=" + strconv.Itoa(half) + "-", "If-Range": adm.ETag})
	if resp.StatusCode != http.StatusPartialContent || adm2.ETag != adm.ETag {
		t.Fatalf("resume GET: %d %v, etag %q vs %q", resp.StatusCode, resp.Header, adm2.ETag, adm.ETag)
	}
	want := "bytes " + strconv.Itoa(half) + "-" + strconv.Itoa(len(content)-1) + "/" + strconv.Itoa(len(content))
	if got := resp.Header.Get("Content-Range"); got != want {
		t.Fatalf("Content-Range %q, want %q", got, want)
	}
	rest, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := append(first, rest...); !bytes.Equal(got, content) {
		t.Fatalf("resumed file differs: %s vs %s", sum(got), sum(content))
	}
	e.receipt(bearer, adm2, ops.OutcomeDelivered, int64(len(content)), resp.Trailer)
	if rd := resp.Trailer.Get(proto.HeaderReprDigest); rd == "" {
		t.Logf("no Repr-Digest trailer on the 206 (headers %v)", resp.Header)
	}

	// A changed file forces a restart: If-Range does not match, the whole
	// file comes with 200.
	changed := append(bytes.Clone(content[:1000]), noise(5000)...)
	writeFile(t, w.dirs["src"], "big.iso", changed)
	adm3, resp := e.legGet(bearer, tgt, "big.iso", map[string]string{"Range": "bytes=" + strconv.Itoa(half) + "-", "If-Range": adm.ETag})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	e.receipt(bearer, adm3, ops.OutcomeDelivered, int64(len(body)), resp.Trailer)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, changed) {
		t.Fatalf("GET of a changed file with the old If-Range: %d, %d bytes", resp.StatusCode, len(body))
	}
	// A range past the end is 416.
	adm4, resp := e.legGet(bearer, tgt, "big.iso", map[string]string{"Range": "bytes=999999999-"})
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	_ = e.call(bearer, ops.OpFileReceipt, ops.FileReceiptArgs{TransferID: adm4.TransferID, LegToken: adm4.LegToken,
		FileReceipt: ops.FileReceipt{Outcome: ops.OutcomeFailedNotSent, Reason: string(proto.CodeInvalidArgument)}}, nil)
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("GET past the end: %d", resp.StatusCode)
	}
	// The leg token is single use.
	req, _ := http.NewRequest(http.MethodGet, "https://"+e.ph.addr+proto.FilesLegPath(adm.TransferID), nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set(proto.HeaderLegToken, adm.LegToken)
	resp, err = e.bulk.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent {
		t.Fatalf("a used leg token was served again: %d", resp.StatusCode)
	}
}

// pauseTransport stops the response body after skip bytes until release is
// closed, and tells paused when it got there.
type pauseTransport struct {
	base    http.RoundTripper
	skip    int64
	paused  chan struct{}
	release chan struct{}
}

func (p pauseTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := p.base.RoundTrip(r)
	if err != nil {
		return resp, err
	}
	resp.Body = &pauseBody{rc: resp.Body, p: p}
	return resp, nil
}

type pauseBody struct {
	rc   io.ReadCloser
	p    pauseTransport
	n    int64
	once sync.Once
}

func (b *pauseBody) Read(p []byte) (int, error) {
	if b.n >= b.p.skip {
		b.once.Do(func() { close(b.p.paused) })
		<-b.p.release
	}
	if rem := b.p.skip - b.n; rem > 0 && int64(len(p)) > rem {
		p = p[:rem]
	}
	n, err := b.rc.Read(p)
	b.n += int64(n)
	return n, err
}

func (b *pauseBody) Close() error { return b.rc.Close() }

// pausedPull starts a pull of path whose caller stops reading after 1 MiB;
// revoke runs while it is stopped. It returns the result.
func (e *fxE2E) pausedPull(c *fxCaller, tgt ops.Target, path string, revoke func()) (ops.FileTransferResult, error) {
	e.t.Helper()
	pt := pauseTransport{skip: 1 << 20, paused: make(chan struct{}), release: make(chan struct{})}
	leg := c.Leg
	c.Leg = func(ctx context.Context, adm ops.FileTransferResult) (*agent.FileLeg, error) {
		l, err := leg(ctx, adm)
		if err != nil {
			return nil, err
		}
		cl := *l.Client
		pt.base = l.Client.Transport
		if pt.base == nil {
			pt.base = http.DefaultTransport
		}
		cl.Transport = pt
		l.Client = &cl
		return l, nil
	}
	defer func() { c.Leg = leg }()
	type out struct {
		res ops.FileTransferResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		res, err := agent.CallerPull(ctxT(e.t), c.FileCaller, ops.FilePullArgs{Target: tgt, Path: path})
		done <- out{res, err}
	}()
	select {
	case <-pt.paused:
	case o := <-done:
		e.t.Fatalf("the pull ended before it was paused: %+v %v", o.res, o.err)
	case <-time.After(30 * time.Second):
		e.t.Fatal("the pull never reached the pause")
	}
	revoke()
	close(pt.release)
	select {
	case o := <-done:
		return o.res, o.err
	case <-time.After(60 * time.Second):
		e.t.Fatal("the pull did not end after the revocation")
		return ops.FileTransferResult{}, nil
	}
}

func TestFilesRevocationDuringATransferPublishesNothing(t *testing.T) {
	e := startFilesE2E(t)
	w := e.startWorker("wa", fxWorkerOptions{}, "src")
	tgt := target(w, "src")
	content := noise(24 << 20) // more than the windows and buffers of the path
	writeFile(t, w.dirs["src"], "big.bin", content)

	t.Run("hub policy off", func(t *testing.T) {
		bearer := e.key("files-policy", fileActions, w)
		c := e.caller(bearer)
		res, err := e.pausedPull(c, tgt, "big.bin", func() {
			code, body := e.ph.do("POST", "/api/v1/ops/"+ops.OpSettingsSet, ops.SettingsSetArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Key: config.KeyFileTransferPolicy, Value: ops.FileModeOff}, e.ph.admin)
			if code != http.StatusOK {
				t.Fatalf("settings_set: %d %s", code, body)
			}
		})
		t.Cleanup(func() {
			_, _ = e.ph.do("POST", "/api/v1/ops/"+ops.OpSettingsSet, ops.SettingsSetArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Key: config.KeyFileTransferPolicy, Unset: true}, e.ph.admin)
		})
		if err == nil && res.Outcome == ops.OutcomeDelivered {
			t.Fatalf("the pull was delivered after the policy was switched off: %+v", res)
		}
		if exists(c.dir, "big.bin") {
			t.Fatal("the pull was published after the policy was switched off")
		}
		if err == nil {
			waitFor(t, "the record to end failed_not_sent", func() bool {
				return e.transfer(res.TransferID).State == ops.FileStateFailedNotSent
			})
		}
	})
	_, _ = e.ph.do("POST", "/api/v1/ops/"+ops.OpSettingsSet, ops.SettingsSetArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, Key: config.KeyFileTransferPolicy, Unset: true}, e.ph.admin)

	t.Run("api key revoked", func(t *testing.T) {
		bearer := e.key("files-revoke", fileActions, w)
		var keys ops.APIKeyListResult
		code, body := e.ph.do("POST", "/api/v1/ops/"+ops.OpAPIKeyList, map[string]any{}, e.owner)
		if code != http.StatusOK {
			t.Fatalf("apikey_list: %d %s", code, body)
		}
		if err := json.Unmarshal([]byte(body), &keys); err != nil {
			t.Fatal(err)
		}
		id := ""
		for _, k := range keys.Keys {
			if k.Name == "files-revoke" {
				id = k.ID
			}
		}
		c := e.caller(bearer)
		res, err := e.pausedPull(c, tgt, "big.bin", func() {
			code, body := e.ph.do("POST", "/api/v1/ops/"+ops.OpAPIKeyRevoke, ops.APIKeyRevokeArgs{WriteArgs: ops.WriteArgs{RequestID: ops.NewUUIDv7()}, ID: id}, e.owner)
			if code != http.StatusOK {
				t.Fatalf("apikey_revoke: %d %s", code, body)
			}
		})
		if err == nil && res.Outcome == ops.OutcomeDelivered {
			t.Fatalf("the pull was delivered after its key was revoked: %+v", res)
		}
		if exists(c.dir, "big.bin") {
			t.Fatal("the pull was published after its key was revoked")
		}
	})
}
