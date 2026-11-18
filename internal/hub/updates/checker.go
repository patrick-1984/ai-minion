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
	// update_channel changed since the last answer; a change checks at once
	// instead of at the next daily check. Zero means one minute.
	Poll time.Duration
	// MinRefresh is the shortest time between two checks asked for with
	// Refresh (the "Check now" button); zero means one minute. GitHub allows
	// 60 unauthenticated calls an hour per address.
	MinRefresh time.Duration

	mu     sync.RWMutex
	last   Result
	runMu  sync.Mutex // one check at a time
	lastAt time.Time  // when the last check (any trigger) ran
}

// Latest returns the last answer; it never blocks on the network.
func (c *Checker) Latest() Result {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.last
}

// Run checks after FirstDelay and then every Interval (plus up to an hour of
// jitter) until ctx ends. In between it checks at once when update_check or
// update_channel no longer match the last answer (a change in the panel).
func (c *Checker) Run(ctx context.Context) {
	t := time.NewTimer(c.FirstDelay)
	defer t.Stop()
	poll := c.Poll
	if poll <= 0 {
		poll = time.Minute
	}
	p := time.NewTicker(poll)
	defer p.Stop()
	started := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.C:
			if !started || !c.settingsChanged() {
				continue
			}
			c.CheckOnce(ctx)
			continue
		case <-t.C:
		}
		started = true
		c.CheckOnce(ctx)
		t.Reset(c.Interval + time.Duration(rand.Int64N(int64(time.Hour))))
	}
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

// Refresh checks now, for an administrator's "Check now". It returns false
// without calling GitHub when the last check ran less than MinRefresh ago;
// the caller then shows the answer it has.
func (c *Checker) Refresh(ctx context.Context) bool {
	min := c.MinRefresh
	if min <= 0 {
		min = time.Minute
	}
	c.runMu.Lock()
	recent := !c.lastAt.IsZero() && time.Since(c.lastAt) < min
	c.runMu.Unlock()
	if recent && !c.settingsChanged() {
		return false
	}
	c.CheckOnce(ctx)
	return true
}

// CheckOnce runs one check now, unless update_check is off.
func (c *Checker) CheckOnce(ctx context.Context) {
	c.runMu.Lock()
	defer c.runMu.Unlock()
	check, err1 := c.Setting(config.KeyUpdateCheck)
	channel, err2 := c.Setting(config.KeyUpdateChannel)
	if err1 != nil || err2 != nil {
		c.Log.Debug("release check skipped: the settings could not be read", "error", errors.Join(err1, err2))
		return
	}
	if check != "on" {
		c.mu.Lock()
		c.last = Result{}
		c.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	c.lastAt = time.Now()
	res, err := c.check(ctx, channel)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		if c.last.Channel != channel {
			// A notice found on another channel no longer applies.
			c.last = Result{Channel: channel}
		}
		c.last.Err, c.last.CheckedAt = err.Error(), time.Now()
		c.Log.Debug("release check failed; keeping the last answer", "error", err)
		return
	}
	c.last = res
	c.Log.Debug("release check", "channel", channel, "latest", res.Latest, "newer", res.Newer)
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
