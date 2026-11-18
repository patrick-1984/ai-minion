package updates

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/version"
)

type fakeSource struct {
	rels  []version.Release
	err   error
	calls atomic.Int32
}

func (f *fakeSource) Releases(context.Context) ([]version.Release, error) {
	f.calls.Add(1)
	return f.rels, f.err
}
func (f *fakeSource) Open(context.Context, version.Asset) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}

func rels(tags ...string) []version.Release {
	out := make([]version.Release, 0, len(tags))
	for _, t := range tags {
		out = append(out, version.Release{Tag: t, Prerelease: len(t) > 7})
	}
	return out
}

func newChecker(src version.Source, current string, settings map[string]string) *Checker {
	return &Checker{Source: src, Current: current, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Setting: func(k string) (string, error) {
			if v, ok := settings[k]; ok {
				return v, nil
			}
			switch k {
			case config.KeyUpdateCheck:
				return "on", nil
			case config.KeyUpdateChannel:
				return config.UpdateChannelMinor, nil
			}
			return "", nil
		}}
}

func TestChannelsPickTheNewestReleaseInsideTheChannel(t *testing.T) {
	src := &fakeSource{rels: rels("v0.8.2", "v0.9.0", "v0.9.3", "v1.0.0", "v1.1.0-rc.1")}
	for _, tc := range []struct {
		channel, want string
		newer         bool
	}{
		{config.UpdateChannelMinor, "v0.9.3", true},
		{config.UpdateChannelMajor, "v0.9.3", true},
		{config.UpdateChannelLatest, "v1.0.0", true},
	} {
		c := newChecker(src, "v0.9.0", map[string]string{config.KeyUpdateChannel: tc.channel})
		c.CheckOnce(context.Background())
		if r := c.Latest(); r.Latest != tc.want || r.Newer != tc.newer || r.URL != ReleasesURL+tc.want {
			t.Errorf("%s: %+v", tc.channel, r)
		}
	}
}

func TestSameOrOlderIsNotNewerAndDevNeverIs(t *testing.T) {
	src := &fakeSource{rels: rels("v0.9.0")}
	c := newChecker(src, "v0.9.0", map[string]string{config.KeyUpdateChannel: "minor"})
	c.CheckOnce(context.Background())
	if c.Latest().Newer {
		t.Fatal("same version is newer")
	}
	c = newChecker(src, "dev", nil)
	c.CheckOnce(context.Background())
	if r := c.Latest(); r.Newer || r.Latest != "v0.9.0" {
		t.Fatalf("dev: %+v", r)
	}
}

// A failed check keeps the last good answer; off contacts nothing.
func TestFailureKeepsTheLastAnswerAndOffMakesNoCall(t *testing.T) {
	src := &fakeSource{rels: rels("v0.9.1")}
	c := newChecker(src, "v0.9.0", map[string]string{config.KeyUpdateChannel: "minor"})
	c.CheckOnce(context.Background())
	src.err = errors.New("offline")
	c.CheckOnce(context.Background())
	if r := c.Latest(); r.Latest != "v0.9.1" || !r.Newer || r.Err == "" {
		t.Fatalf("%+v", r)
	}
	off := newChecker(src, "v0.9.0", map[string]string{config.KeyUpdateCheck: "off"})
	before := src.calls.Load()
	off.CheckOnce(context.Background())
	if src.calls.Load() != before {
		t.Fatal("update_check=off called GitHub")
	}
}

// A failed settings lookup skips the check (never turns it on), and a cached
// notice of another channel is dropped when the new channel's check fails.
func TestSettingErrorsSkipAndChannelChangeDropsTheNotice(t *testing.T) {
	src := &fakeSource{rels: rels("v0.9.0", "v1.0.0")}
	c := newChecker(src, "v0.9.0", map[string]string{config.KeyUpdateChannel: config.UpdateChannelLatest})
	c.CheckOnce(context.Background())
	if r := c.Latest(); r.Latest != "v1.0.0" || !r.Newer {
		t.Fatalf("%+v", r)
	}
	c.Setting = func(string) (string, error) { return "", errors.New("db locked") }
	before := src.calls.Load()
	c.CheckOnce(context.Background())
	if src.calls.Load() != before {
		t.Fatal("a failed settings lookup called GitHub")
	}
	c.Setting = func(k string) (string, error) {
		if k == config.KeyUpdateChannel {
			return config.UpdateChannelMinor, nil
		}
		return "on", nil
	}
	src.err = errors.New("offline")
	c.CheckOnce(context.Background())
	if r := c.Latest(); r.Newer || r.Latest != "" {
		t.Fatalf("the latest-channel notice survived the switch to minor: %+v", r)
	}
}

// "Check now" asks GitHub, but not twice within MinRefresh; a changed
// channel is checked anyway.
func TestRefreshIsRateLimitedExceptAfterAChannelChange(t *testing.T) {
	src := &fakeSource{rels: rels("v0.11.0", "v0.13.0")}
	settings := map[string]string{config.KeyUpdateChannel: config.UpdateChannelMinor}
	c := newChecker(src, "v0.11.0", settings)
	c.MinRefresh = time.Hour
	if !c.Refresh(context.Background()) || src.calls.Load() != 1 {
		t.Fatalf("first refresh: calls = %d", src.calls.Load())
	}
	if c.Refresh(context.Background()) || src.calls.Load() != 1 {
		t.Fatalf("second refresh within MinRefresh called GitHub: calls = %d", src.calls.Load())
	}
	settings[config.KeyUpdateChannel] = config.UpdateChannelLatest
	if !c.Refresh(context.Background()) || src.calls.Load() != 2 {
		t.Fatalf("refresh after a channel change: calls = %d", src.calls.Load())
	}
	if r := c.Latest(); r.Latest != "v0.13.0" || !r.Newer || r.Channel != config.UpdateChannelLatest {
		t.Fatalf("latest = %+v", r)
	}
}

// Run checks at once when update_channel changes, not only at the next
// daily check.
func TestRunChecksAgainWhenTheChannelChanges(t *testing.T) {
	src := &fakeSource{rels: rels("v0.11.0", "v0.13.0")}
	var mu sync.Mutex
	channel := config.UpdateChannelMinor
	c := newChecker(src, "v0.11.0", nil)
	c.Setting = func(k string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if k == config.KeyUpdateChannel {
			return channel, nil
		}
		return "on", nil
	}
	c.Interval, c.FirstDelay, c.Poll = time.Hour, time.Millisecond, 5*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	waitFor(t, func() bool { return c.Latest().Channel == config.UpdateChannelMinor })
	if c.Latest().Newer {
		t.Fatal("minor channel offered v0.13.0")
	}
	mu.Lock()
	channel = config.UpdateChannelLatest
	mu.Unlock()
	waitFor(t, func() bool { r := c.Latest(); return r.Channel == config.UpdateChannelLatest && r.Latest == "v0.13.0" })
	if n := src.calls.Load(); n != 2 {
		t.Fatalf("calls = %d, want 2 (start, channel change)", n)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
