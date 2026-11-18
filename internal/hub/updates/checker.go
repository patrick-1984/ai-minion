// Package updates is the Hub's release check (v0.9.0, A10): once a day it asks
// GitHub for the newest release in the configured channel and keeps the answer
// in memory. Pages read Latest(); nothing on a request path calls GitHub.
// The design follows Arcane's version service (cached answer, a failed check
// keeps the last good one) and Syncthing (stay inside a channel).
package updates

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/config"
	"github.com/patrick-1984/ai-minion/internal/version"
)

// ReleasesURL is the page of a release on GitHub.
const ReleasesURL = "https://github.com/patrick-1984/ai-minion/releases/tag/"

// Result is the last answer of the check.
type Result struct {
	// Latest is the newest release in the channel ("v0.9.2"); empty before
	// the first successful check.
	Latest string
	// Newer is true when Latest is newer than the running Hub.
	Newer     bool
	URL       string
	Channel   string
	CheckedAt time.Time
	// Err is the error of the last check; the last good answer stays.
	Err string
}

// Checker runs the daily check.
type Checker struct {
	// Source lists the releases (version.NewGitHubSource).
	Source version.Source
	// Current is the running version (version.Version).
	Current string
	// Setting resolves a Hub setting on every check (live: a change in the
	// panel applies at the next check). An error skips the check: a failed
	// lookup must never turn the check on or widen the channel.
	Setting func(key string) (string, error)
	Log     *slog.Logger
	// Interval is 24 h; FirstDelay the wait after start (a few minutes).
	Interval, FirstDelay time.Duration
	// Poll is how often Run looks (locally) whether update_check or
	// update_channel changed since the last answer; a change is checked
	// then (after MinRefresh since the last call to GitHub) instead of at
	// the next daily check. Zero means 15 seconds.
	Poll time.Duration
	// Life ends with the Hub: a check started by Refresh (detached from the
	// request that asked for it) is canceled when it ends, so a Hub stop never
	// waits for GitHub. Nil means no such bound.
	Life context.Context
	// MinRefresh is the shortest time between two checks asked for with
	// Refresh (the "Check now" button); zero means one minute. GitHub allows
	// 60 unauthenticated calls an hour per address.
	MinRefresh time.Duration

	mu       sync.RWMutex
	last     Result
	runMu    sync.Mutex // one check at a time
	checking atomic.Bool
	lastAt   time.Time // when the last check (any trigger) ran
}

// Latest returns the last answer; it never blocks on the network.
func (c *Checker) Latest() Result {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.last
}

// Run checks after FirstDelay and then every Interval (plus up to an hour of
// jitter) until ctx ends. In between it checks when update_check or
// update_channel no longer match the last answer (a change in the panel),
// at most once per MinRefresh like Refresh.
func (c *Checker) Run(ctx context.Context) {
	t := time.NewTimer(c.FirstDelay)
	defer t.Stop()
	poll := c.Poll
	if poll <= 0 {
		poll = 15 * time.Second
	}
	p := time.NewTicker(poll)
	defer p.Stop()
	started := false // the first scheduled attempt ran (also with the check off)
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.C:
			if (started || c.checked()) && c.settingsChanged() {
				c.fetch(ctx, true, false)
			}
			continue
		case <-t.C:
		}
		started = true
		c.CheckOnce(ctx)
		t.Reset(c.Interval + time.Duration(rand.Int64N(int64(time.Hour))))
	}
}

// checked reports whether a check ran (any trigger): the baseline that a
// settings change is compared with.
func (c *Checker) checked() bool {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	return !c.lastAt.IsZero()
}

// settingsChanged reports whether the last answer was found with other
// settings than the current ones: another channel, or the check turned on
// since. A failed lookup is no change.
func (c *Checker) settingsChanged() bool {
	check, err1 := c.Setting(config.KeyUpdateCheck)
	channel, err2 := c.Setting(config.KeyUpdateChannel)
	if err1 != nil || err2 != nil {
		return false
	}
	last := c.Latest()
	if check != "on" {
		return last != (Result{})
	}
	return last.Channel != channel
}

// Refresh checks now, for an administrator's "Check now". It reports whether
// GitHub was asked: not when the last call to GitHub (any trigger) was less
// than MinRefresh ago, the check is off or the settings cannot be read; the
// caller then shows the answer it has.
func (c *Checker) Refresh(ctx context.Context) bool {
	return c.fetch(ctx, true, true)
}

// StartRefresh starts a check in the background for "Check now" and returns
// at once: true when a check runs (started now or already running), false
// when the cooldown, update_check off or unreadable settings keep GitHub out
// of it. The check is bounded by Life (a Hub stop cancels it) and by 30 s;
// no request ever waits for GitHub.
func (c *Checker) StartRefresh() bool {
	if !c.runMu.TryLock() {
		return true // a check is running; its answer comes as usual
	}
	due := c.dueLocked()
	c.runMu.Unlock()
	if !due {
		return false
	}
	life := c.Life
	if life == nil {
		life = context.Background()
	}
	c.checking.Store(true)
	go func() {
		defer c.checking.Store(false)
		c.fetch(life, true, false)
	}()
	return true
}

// Checking reports whether a check started by StartRefresh still runs.
func (c *Checker) Checking() bool { return c.checking.Load() }

// dueLocked reports whether a cooldown-bound check would ask GitHub now; the
// caller holds runMu.
func (c *Checker) dueLocked() bool {
	check, err1 := c.Setting(config.KeyUpdateCheck)
	_, err2 := c.Setting(config.KeyUpdateChannel)
	if err1 != nil || err2 != nil || check != "on" {
		return false
	}
	gap := c.MinRefresh
	if gap <= 0 {
		gap = time.Minute
	}
	return c.lastAt.IsZero() || time.Since(c.lastAt) >= gap
}

// CheckOnce runs one check now, unless update_check is off (the daily check).
func (c *Checker) CheckOnce(ctx context.Context) {
	c.fetch(ctx, false, false)
}

// fetch runs one check and reports whether it asked GitHub. With cooldown it
// skips the call when the previous one was less than MinRefresh ago; the
// decision and the call happen under runMu, so concurrent triggers cannot
// both pass. With detach the call ignores ctx's cancellation (a browser that
// goes away does not leave a "canceled" answer behind); background checks
// keep it, so a Hub shutdown does not wait for GitHub. Bounded to 30 s.
func (c *Checker) fetch(ctx context.Context, cooldown, detach bool) bool {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	check, err1 := c.Setting(config.KeyUpdateCheck)
	channel, err2 := c.Setting(config.KeyUpdateChannel)
	if err1 != nil || err2 != nil {
		c.Log.Debug("release check skipped: the settings could not be read", "error", errors.Join(err1, err2))
		return false
	}
	if check != "on" {
		c.mu.Lock()
		c.last = Result{}
		c.mu.Unlock()
		return false
	}
	gap := c.MinRefresh
	if gap <= 0 {
		gap = time.Minute
	}
	if cooldown && !c.lastAt.IsZero() && time.Since(c.lastAt) < gap {
		return false
	}
	if detach {
		ctx = context.WithoutCancel(ctx)
		if c.Life != nil {
			var stop context.CancelFunc
			ctx, stop = context.WithCancel(ctx)
			defer stop()
			unhook := context.AfterFunc(c.Life, stop)
			defer unhook()
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c.lastAt = time.Now()
	res, err := c.check(ctx, channel)
	if err != nil && ctx.Err() != nil && errors.Is(context.Cause(ctx), context.Canceled) {
		// Shutdown: keep the last answer as it was.
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		if c.last.Channel != channel {
			// A notice found on another channel no longer applies.
			c.last = Result{Channel: channel}
		}
		c.last.Err, c.last.CheckedAt = err.Error(), time.Now()
		c.Log.Debug("release check failed; keeping the last answer", "error", err)
		return true
	}
	c.last = res
	c.Log.Debug("release check", "channel", channel, "latest", res.Latest, "newer", res.Newer)
	return true
}

func (c *Checker) check(ctx context.Context, channel string) (Result, error) {
	cur, curErr := version.ParseVersion(c.Current)
	sel, err := SelectorFor(channel, cur, curErr == nil)
	if err != nil {
		return Result{}, err
	}
	rels, err := c.Source.Releases(ctx)
	if err != nil {
		return Result{}, err
	}
	rel, v, err := sel.Resolve(rels)
	if err != nil {
		return Result{}, err
	}
	return Result{Latest: rel.Tag, URL: ReleasesURL + rel.Tag, Channel: channel, CheckedAt: time.Now(),
		Newer: curErr == nil && v.Compare(cur) > 0}, nil
}

// ImageTag is the Hub image tag that follows a channel, relative to the
// running version cur: X.Y (minor), X (major) or latest.
func ImageTag(channel string, cur version.SemVer) string {
	switch channel {
	case config.UpdateChannelMinor:
		return fmt.Sprintf("%d.%d", cur.Major, cur.Minor)
	case config.UpdateChannelMajor:
		return strconv.Itoa(int(cur.Major))
	}
	return "latest"
}

// SelectorFor is the version selector of a channel, relative to the running
// version cur (minor: X.Y, major: X). A development build follows latest.
func SelectorFor(channel string, cur version.SemVer, released bool) (version.Selector, error) {
	if !released {
		channel = config.UpdateChannelLatest
	}
	switch channel {
	case config.UpdateChannelMinor:
		return version.ParseSelector(fmt.Sprintf("%d.%d", cur.Major, cur.Minor))
	case config.UpdateChannelMajor:
		return version.ParseSelector(strconv.Itoa(int(cur.Major)))
	case config.UpdateChannelLatest:
		return version.ParseSelector("latest")
	}
	return version.Selector{}, fmt.Errorf("unknown update channel %q", channel)
}
