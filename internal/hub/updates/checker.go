// Package updates is the Hub's release check (v0.9.0, A10): once a day it asks
// GitHub for the newest release in the configured channel and keeps the answer
// in memory. Pages read Latest(); nothing on a request path calls GitHub.
// The design follows Arcane's version service (cached answer, a failed check
// keeps the last good one) and Syncthing (stay inside a channel).
package updates

import (
	"context"
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
	// panel applies at the next check).
	Setting func(key string) string
	Log     *slog.Logger
	// Interval is 24 h; FirstDelay the wait after start (a few minutes).
	Interval, FirstDelay time.Duration

	mu   sync.RWMutex
	last Result
}

// Latest returns the last answer; it never blocks on the network.
func (c *Checker) Latest() Result {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.last
}

// Run checks after FirstDelay and then every Interval (plus up to an hour of
// jitter) until ctx ends.
func (c *Checker) Run(ctx context.Context) {
	t := time.NewTimer(c.FirstDelay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.CheckOnce(ctx)
		t.Reset(c.Interval + time.Duration(rand.Int64N(int64(time.Hour))))
	}
}

// CheckOnce runs one check now, unless update_check is off.
func (c *Checker) CheckOnce(ctx context.Context) {
	if c.Setting(config.KeyUpdateCheck) == "off" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	channel := c.Setting(config.KeyUpdateChannel)
	res, err := c.check(ctx, channel)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
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
	case config.UpdateChannelLatest, "":
		return version.ParseSelector("latest")
	}
	return version.Selector{}, fmt.Errorf("unknown update channel %q", channel)
}
