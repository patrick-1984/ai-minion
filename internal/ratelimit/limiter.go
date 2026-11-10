package ratelimit

import (
	"container/list"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Policy names a set of limits. The five named policies below are the ones
// the spine requires; callers may register more through Config.Policies.
type Policy string

// The named policies of AD-19.
const (
	// PolicyLogin guards password logins. The target is the account name.
	PolicyLogin Policy = "login"
	// PolicyEnroll guards Worker enrollment. The target is the invitation
	// fingerprint.
	PolicyEnroll Policy = "enroll"
	// PolicyConnect guards Worker connects. The target is the Worker key id.
	PolicyConnect Policy = "connect"
	// PolicyPairing guards pairing attempts. The target is the public
	// pairing id (or the user, for the per-user limit).
	PolicyPairing Policy = "pairing"
	// PolicyAPIKeyFail guards failed API-key attempts. The target is the
	// key prefix or id presented.
	PolicyAPIKeyFail Policy = "apikey_fail"
)

// DefaultMaxEntries is the cap of every LRU table: per policy one table of IP
// buckets and one of target buckets, plus one table of back-off entries. With
// five policies that is at most 11 tables of 10000 entries. Measured with all
// tables full, an entry costs about 160 bytes with short keys and about 230
// bytes with the longest verbatim key (128 bytes), so the defaults bound the
// limiter at roughly 25 MiB.
//
// Eviction never erases active protection. An entry is active while its bucket
// has outstanding token debt or its login back-off has not expired; only idle
// entries (full bucket, no running back-off) are evicted, least recently used
// first. When a table is full and every entry is active, a key that is not yet
// in the table is refused (fail closed) with a retry_after of the time until
// the first entry becomes idle, instead of evicting an active one. A flood of
// distinct keys can therefore briefly lock out new clients, accounts or
// targets, but it can never reset a drained bucket or a running back-off.
const DefaultMaxEntries = 10000

// unknownPolicyRetry is the retry_after reported for an unregistered policy.
const unknownPolicyRetry = time.Minute

// Clock is the limiter's time source. Tests inject a fake.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// SystemClock returns the wall clock.
func SystemClock() Clock { return systemClock{} }

// Bucket is one token bucket: it holds up to Burst tokens and gains one token
// every Interval. A request costs one token.
type Bucket struct {
	Burst    int
	Interval time.Duration
}

func (b Bucket) validate() error {
	if b.Burst < 1 {
		return errors.New("burst must be at least 1")
	}
	if b.Interval <= 0 {
		return errors.New("interval must be positive")
	}
	return nil
}

// Spec holds the two buckets of a policy.
type Spec struct {
	// IP limits one client network (IPv4 /32, IPv6 /64).
	IP Bucket
	// Target limits one target (account, fingerprint, pairing id, key id),
	// whatever address the attempts come from.
	Target Bucket
}

// BackoffSpec configures the per-account login back-off. After Free failures
// in a row, each further failure blocks the account for Base, then 2*Base, 4*Base
// and so on, never more than Max. A success resets the count; an account that
// has had no failure for Decay is forgotten.
type BackoffSpec struct {
	Free  int
	Base  time.Duration
	Max   time.Duration
	Decay time.Duration
}

func (b BackoffSpec) validate() error {
	switch {
	case b.Free < 0:
		return errors.New("free failures must not be negative")
	case b.Base <= 0:
		return errors.New("base must be positive")
	case b.Max < b.Base:
		return errors.New("max must be at least base")
	case b.Decay <= 0:
		return errors.New("decay must be positive")
	case b.Decay < b.Max:
		// Backoff checks decay before the block, so a shorter decay would lift
		// an active block early.
		return errors.New("decay must be at least max")
	}
	return nil
}

// DefaultPolicies returns the built-in limits of the named policies. A client
// gets Burst quick attempts, then one more every Interval.
func DefaultPolicies() map[Policy]Spec {
	return map[Policy]Spec{
		PolicyLogin:      {IP: Bucket{10, 6 * time.Second}, Target: Bucket{5, 12 * time.Second}},
		PolicyEnroll:     {IP: Bucket{5, 12 * time.Second}, Target: Bucket{10, 6 * time.Second}},
		PolicyConnect:    {IP: Bucket{30, time.Second}, Target: Bucket{10, 5 * time.Second}},
		PolicyPairing:    {IP: Bucket{10, 6 * time.Second}, Target: Bucket{5, 30 * time.Second}},
		PolicyAPIKeyFail: {IP: Bucket{10, 6 * time.Second}, Target: Bucket{5, 12 * time.Second}},
	}
}

// DefaultBackoff returns the built-in login back-off: three free failures,
// then 1 s doubling up to 15 min, forgotten after one hour without failures.
func DefaultBackoff() BackoffSpec {
	return BackoffSpec{Free: 3, Base: time.Second, Max: 15 * time.Minute, Decay: time.Hour}
}

// Config configures a Limiter. The zero value is valid and yields the
// defaults.
type Config struct {
	// Clock is the time source; nil means the wall clock.
	Clock Clock
	// Policies overrides or adds policies on top of DefaultPolicies.
	Policies map[Policy]Spec
	// Backoff overrides DefaultBackoff when non-nil.
	Backoff *BackoffSpec
	// MaxEntries caps every LRU table; zero means DefaultMaxEntries.
	MaxEntries int
}

// Limiter is the shared rate limiter. It is safe for concurrent use.
type Limiter struct {
	clock      Clock
	maxEntries int
	backoff    BackoffSpec

	mu       sync.Mutex
	policies map[Policy]*policyState
	failures *lru[*failEntry]
}

type policyState struct {
	spec   Spec
	byIP   *lru[*bucketState]
	target *lru[*bucketState]
}

// New builds a Limiter. It returns an error for an invalid bucket or back-off.
func New(cfg Config) (*Limiter, error) {
	clock := cfg.Clock
	if clock == nil {
		clock = SystemClock()
	}
	maxEntries := cfg.MaxEntries
	if maxEntries == 0 {
		maxEntries = DefaultMaxEntries
	}
	if maxEntries < 1 {
		return nil, errors.New("ratelimit: MaxEntries must be positive")
	}
	backoff := DefaultBackoff()
	if cfg.Backoff != nil {
		backoff = *cfg.Backoff
	}
	if err := backoff.validate(); err != nil {
		return nil, fmt.Errorf("ratelimit: backoff: %w", err)
	}
	specs := DefaultPolicies()
	for name, spec := range cfg.Policies {
		specs[name] = spec
	}
	l := &Limiter{
		clock:      clock,
		maxEntries: maxEntries,
		backoff:    backoff,
		policies:   make(map[Policy]*policyState, len(specs)),
		failures:   newLRU(maxEntries, (*failEntry).protectedUntil),
	}
	for name, spec := range specs {
		if name == "" {
			return nil, errors.New("ratelimit: empty policy name")
		}
		if err := spec.IP.validate(); err != nil {
			return nil, fmt.Errorf("ratelimit: policy %q ip bucket: %w", name, err)
		}
		if err := spec.Target.validate(); err != nil {
			return nil, fmt.Errorf("ratelimit: policy %q target bucket: %w", name, err)
		}
		l.policies[name] = &policyState{
			spec:   spec,
			byIP:   newLRU(maxEntries, (*bucketState).protectedUntil),
			target: newLRU(maxEntries, (*bucketState).protectedUntil),
		}
	}
	return l, nil
}

// bucketState is a token bucket kept as its theoretical arrival time (GCRA):
// tat is the instant at which every token spent so far has refilled. All
// arithmetic is exact integer duration arithmetic, so a client that waits the
// reported retryAfter is always admitted.
type bucketState struct {
	tat time.Time
}

// protectedUntil is the instant until which the bucket carries token debt and
// must not be evicted.
func (s *bucketState) protectedUntil() time.Time { return s.tat }

// peek reports whether one token is available at now and, if not, how long
// until it is. It does not consume.
func (s *bucketState) peek(b Bucket, now time.Time) (bool, time.Duration) {
	tat := s.tat
	if tat.Before(now) {
		tat = now
	}
	// Allowed while at most Burst-1 tokens are already spent.
	over := tat.Sub(now) - time.Duration(b.Burst-1)*b.Interval
	if over <= 0 {
		return true, 0
	}
	return false, over
}

// take consumes one token; the caller has checked peek.
func (s *bucketState) take(b Bucket, now time.Time) {
	if s.tat.Before(now) {
		s.tat = now
	}
	s.tat = s.tat.Add(b.Interval)
}

// Allow applies the policy's IP bucket and target bucket and reports whether
// the attempt may proceed. When it may not, retryAfter is how long the client
// must wait (the longer of the two buckets). A token is consumed from both
// buckets only when both have one, so a refused attempt costs nothing. An empty
// target skips the target bucket. An unregistered policy is refused (fail
// closed) with a retryAfter of one minute.
func (l *Limiter) Allow(policy Policy, ip netip.Addr, target string) (allowed bool, retryAfter time.Duration) {
	p, ok := l.policies[policy]
	if !ok {
		return false, unknownPolicyRetry
	}
	ipKey := IPKey(ip)
	tgtKey := NormalizeKey(target)

	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()

	ipState, _ := p.byIP.get(ipKey)
	var tgtState *bucketState
	if tgtKey != "" {
		tgtState, _ = p.target.get(tgtKey)
	}
	// A key that is new to a full table of active entries is refused; the wait
	// covers every table that has no room so that waiting it out is enough.
	var full time.Duration
	if ipState == nil {
		if ok, wait := p.byIP.room(now); !ok {
			full = max(full, wait)
		}
	}
	if tgtKey != "" && tgtState == nil {
		if ok, wait := p.target.room(now); !ok {
			full = max(full, wait)
		}
	}
	if full > 0 {
		return false, full
	}
	if ipState == nil {
		ipState = &bucketState{}
	}
	if tgtKey != "" && tgtState == nil {
		tgtState = &bucketState{}
	}

	okIP, waitIP := ipState.peek(p.spec.IP, now)
	okTgt, waitTgt := true, time.Duration(0)
	if tgtState != nil {
		okTgt, waitTgt = tgtState.peek(p.spec.Target, now)
	}
	if !okIP || !okTgt {
		return false, max(waitIP, waitTgt)
	}
	ipState.take(p.spec.IP, now)
	p.byIP.put(ipKey, ipState, now)
	if tgtState != nil {
		tgtState.take(p.spec.Target, now)
		p.target.put(tgtKey, tgtState, now)
	}
	return true, 0
}

// failEntry is the back-off state of one account.
type failEntry struct {
	count int
	until time.Time // blocked until; may be in the past
	last  time.Time // time of the latest failure
}

// protectedUntil is the end of the running back-off; an entry whose block has
// expired is only count memory and may be evicted.
func (e *failEntry) protectedUntil() time.Time { return e.until }

// delay returns the block applied after the count-th consecutive failure.
func (b BackoffSpec) delay(count int) time.Duration {
	n := count - b.Free
	if n <= 0 {
		return 0
	}
	d := b.Base
	for i := 1; i < n; i++ {
		if d >= b.Max/2 {
			return b.Max
		}
		d *= 2
	}
	return min(d, b.Max)
}

// Backoff reports how long the account must still wait before another login
// attempt is worth verifying; zero means it may proceed. It records nothing.
// A login endpoint should call it before checking the password, and refuse with
// a retry_after while it is non-zero. Apply it to unknown account names as
// well, so that the answer does not reveal which accounts exist.
func (l *Limiter) Backoff(account string) time.Duration {
	key := NormalizeKey(account)
	if key == "" {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	e, ok := l.failures.get(key)
	if !ok {
		// A new account that cannot be tracked because every slot holds a
		// running back-off is refused (fail closed).
		if room, wait := l.failures.room(now); !room {
			return wait
		}
		return 0
	}
	if now.Sub(e.last) >= l.backoff.Decay {
		l.failures.remove(key)
		return 0
	}
	if wait := e.until.Sub(now); wait > 0 {
		return wait
	}
	return 0
}

// BackoffFail records a failed login for the account and returns the block
// now in force (zero while the free failures last). When the table is full of
// running back-offs and the account is new, nothing is recorded and the wait
// until the first slot frees is returned.
func (l *Limiter) BackoffFail(account string) time.Duration {
	key := NormalizeKey(account)
	if key == "" {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.Now()
	e, ok := l.failures.get(key)
	if !ok {
		if room, wait := l.failures.room(now); !room {
			return wait
		}
	}
	if !ok || now.Sub(e.last) >= l.backoff.Decay {
		e = &failEntry{}
	}
	e.count++
	e.last = now
	d := l.backoff.delay(e.count)
	e.until = now.Add(d)
	l.failures.put(key, e, now)
	return d
}

// BackoffReset forgets the account's failures; call it after a successful
// login.
func (l *Limiter) BackoffReset(account string) {
	key := NormalizeKey(account)
	if key == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures.remove(key)
}

// lru is a string-keyed map with least-recently-used eviction at a fixed cap.
// Entries whose protectedUntil lies in the future are active and are never
// evicted. It is not safe for concurrent use; the Limiter's mutex guards it.
type lru[V any] struct {
	cap            int
	protectedUntil func(V) time.Time
	order          *list.List // front is most recently used
	items          map[string]*list.Element

	// nextCheck is a lower bound for the instant at which an entry can become
	// idle, valid after a full sweep found every entry active. Entries only
	// ever gain protection, so until then a full table need not be scanned
	// again.
	nextCheck time.Time
}

type lruItem[V any] struct {
	key string
	val V
}

// newLRU builds a table. protectedUntil returns the instant until which a value
// is active.
func newLRU[V any](capacity int, protectedUntil func(V) time.Time) *lru[V] {
	return &lru[V]{cap: capacity, protectedUntil: protectedUntil, order: list.New(), items: make(map[string]*list.Element)}
}

// get returns the value and marks it most recently used.
func (c *lru[V]) get(key string) (V, bool) {
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		return el.Value.(*lruItem[V]).val, true
	}
	var zero V
	return zero, false
}

// room reports whether a new key can be stored at now, evicting idle entries
// if the table is full. When every entry is active it reports false and how
// long until the first entry becomes idle.
func (c *lru[V]) room(now time.Time) (bool, time.Duration) {
	if c.order.Len() < c.cap {
		return true, 0
	}
	if now.Before(c.nextCheck) {
		return false, c.nextCheck.Sub(now)
	}
	c.sweep(now)
	if c.order.Len() < c.cap {
		return true, 0
	}
	return false, c.nextCheck.Sub(now)
}

// sweep evicts idle entries, least recently used first, until a batch of slots
// is free. If it finds none it records when the first entry becomes idle.
func (c *lru[V]) sweep(now time.Time) {
	batch := max(1, c.cap/64)
	freed := 0
	var soonest time.Time
	for el := c.order.Back(); el != nil && freed < batch; {
		prev := el.Prev()
		it := el.Value.(*lruItem[V])
		until := c.protectedUntil(it.val)
		if until.After(now) {
			if soonest.IsZero() || until.Before(soonest) {
				soonest = until
			}
		} else {
			c.order.Remove(el)
			delete(c.items, it.key)
			freed++
		}
		el = prev
	}
	if freed == 0 {
		c.nextCheck = soonest
	}
}

// put stores the value as most recently used. A new key needs a free slot;
// callers check room first, and put drops the entry when there is none rather
// than evict an active one.
func (c *lru[V]) put(key string, val V, now time.Time) {
	if el, ok := c.items[key]; ok {
		el.Value.(*lruItem[V]).val = val
		c.order.MoveToFront(el)
		return
	}
	if ok, _ := c.room(now); !ok {
		return
	}
	// Clone the key of a new entry: it may be a substring of a large caller
	// buffer, which the table must not keep alive. Lookups stay allocation-free.
	key = strings.Clone(key)
	c.items[key] = c.order.PushFront(&lruItem[V]{key: key, val: val})
}

func (c *lru[V]) remove(key string) {
	if el, ok := c.items[key]; ok {
		c.order.Remove(el)
		delete(c.items, key)
	}
}

func (c *lru[V]) len() int { return c.order.Len() }
