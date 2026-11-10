package ratelimit

import (
	"fmt"
	"net/netip"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

var (
	ip1 = netip.MustParseAddr("192.0.2.1")
	ip2 = netip.MustParseAddr("192.0.2.2")
)

// testPolicy is a small policy: IP burst 3 per 10 s, target burst 2 per 20 s.
const testPolicy Policy = "test"

func newTestLimiter(t *testing.T, clk Clock, mutate func(*Config)) *Limiter {
	t.Helper()
	cfg := Config{
		Clock: clk,
		Policies: map[Policy]Spec{
			testPolicy: {
				IP:     Bucket{Burst: 3, Interval: 10 * time.Second},
				Target: Bucket{Burst: 2, Interval: 20 * time.Second},
			},
		},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	l, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func mustAllow(t *testing.T, l *Limiter, p Policy, ip netip.Addr, target string) {
	t.Helper()
	if ok, wait := l.Allow(p, ip, target); !ok {
		t.Fatalf("Allow(%s, %v, %q) refused, retry in %v", p, ip, target, wait)
	}
}

func mustRefuse(t *testing.T, l *Limiter, p Policy, ip netip.Addr, target string) time.Duration {
	t.Helper()
	ok, wait := l.Allow(p, ip, target)
	if ok {
		t.Fatalf("Allow(%s, %v, %q) allowed, want refused", p, ip, target)
	}
	if wait <= 0 {
		t.Fatalf("refused with non-positive retry_after %v", wait)
	}
	return wait
}

func TestAllowBurstThenRefill(t *testing.T) {
	clk := newFakeClock()
	l := newTestLimiter(t, clk, nil)
	// Empty target: only the IP bucket (burst 3, one token per 10 s) applies.
	for i := 0; i < 3; i++ {
		mustAllow(t, l, testPolicy, ip1, "")
	}
	wait := mustRefuse(t, l, testPolicy, ip1, "")
	if wait != 10*time.Second {
		t.Fatalf("retry_after = %v, want 10s", wait)
	}
	clk.Advance(4 * time.Second)
	if wait := mustRefuse(t, l, testPolicy, ip1, ""); wait != 6*time.Second {
		t.Fatalf("retry_after after 4s = %v, want 6s", wait)
	}
	// Waiting exactly the reported time admits exactly one attempt.
	clk.Advance(6 * time.Second)
	mustAllow(t, l, testPolicy, ip1, "")
	mustRefuse(t, l, testPolicy, ip1, "")
	// A long idle period refills to the burst and no further.
	clk.Advance(time.Hour)
	for i := 0; i < 3; i++ {
		mustAllow(t, l, testPolicy, ip1, "")
	}
	mustRefuse(t, l, testPolicy, ip1, "")
}

func TestAllowPerIPAndPerTarget(t *testing.T) {
	clk := newFakeClock()
	l := newTestLimiter(t, clk, nil)

	// Target bucket (burst 2) is shared by different IPs.
	mustAllow(t, l, testPolicy, ip1, "alice")
	mustAllow(t, l, testPolicy, ip2, "alice")
	wait := mustRefuse(t, l, testPolicy, netip.MustParseAddr("192.0.2.3"), "alice")
	if wait != 20*time.Second {
		t.Fatalf("target retry_after = %v, want 20s", wait)
	}
	// Target keys are case-insensitive.
	mustRefuse(t, l, testPolicy, ip1, "ALICE")
	// Another target is unaffected.
	mustAllow(t, l, testPolicy, ip1, "bob")

	// IP bucket (burst 3) is shared by different targets. ip1 spent 2 tokens.
	mustAllow(t, l, testPolicy, ip1, "carol")
	wait = mustRefuse(t, l, testPolicy, ip1, "dave")
	if wait != 10*time.Second {
		t.Fatalf("ip retry_after = %v, want 10s", wait)
	}
	// Another IP is unaffected.
	mustAllow(t, l, testPolicy, netip.MustParseAddr("192.0.2.9"), "dave")
}

func TestAllowRefusalCostsNothing(t *testing.T) {
	clk := newFakeClock()
	l := newTestLimiter(t, clk, nil)
	mustAllow(t, l, testPolicy, ip1, "alice")
	mustAllow(t, l, testPolicy, ip1, "alice")
	// alice is exhausted. Hammering with ip1 must not drain ip1's own bucket.
	for i := 0; i < 50; i++ {
		mustRefuse(t, l, testPolicy, ip1, "alice")
	}
	// ip1 spent 2 of 3; one token must still be there for another target.
	mustAllow(t, l, testPolicy, ip1, "bob")
	// And refused attempts must not extend the wait of the exhausted target.
	if wait := mustRefuse(t, l, testPolicy, ip2, "alice"); wait != 20*time.Second {
		t.Fatalf("retry_after = %v, want 20s", wait)
	}
}

func TestAllowReturnsLongerWait(t *testing.T) {
	clk := newFakeClock()
	l := newTestLimiter(t, clk, nil)
	mustAllow(t, l, testPolicy, ip1, "alice")
	mustAllow(t, l, testPolicy, ip1, "alice") // target empty, ip has 1 token
	// ip1 has 1 token left, alice has none: target wait (20s) dominates.
	if wait := mustRefuse(t, l, testPolicy, ip1, "alice"); wait != 20*time.Second {
		t.Fatalf("retry_after = %v, want 20s", wait)
	}
	mustAllow(t, l, testPolicy, ip1, "bob") // ip1 now empty
	// Fresh target but empty ip: ip wait (10s) applies.
	if wait := mustRefuse(t, l, testPolicy, ip1, "erin"); wait != 10*time.Second {
		t.Fatalf("retry_after = %v, want 10s", wait)
	}
}

func TestAllowIPv6GroupedBy64(t *testing.T) {
	clk := newFakeClock()
	l := newTestLimiter(t, clk, nil)
	a := netip.MustParseAddr("2001:db8:1:2::1")
	b := netip.MustParseAddr("2001:db8:1:2:ffff:ffff:ffff:ffff")
	other := netip.MustParseAddr("2001:db8:1:3::1")
	mustAllow(t, l, testPolicy, a, "")
	mustAllow(t, l, testPolicy, b, "")
	mustAllow(t, l, testPolicy, a, "")
	// Three attempts from three addresses of one /64 exhausted the bucket.
	mustRefuse(t, l, testPolicy, b, "")
	// A different /64 is a different client.
	mustAllow(t, l, testPolicy, other, "")
	// An IPv4-mapped form shares the bucket of the plain IPv4 address.
	for i := 0; i < 3; i++ {
		mustAllow(t, l, testPolicy, ip1, "")
	}
	mustRefuse(t, l, testPolicy, netip.MustParseAddr("::ffff:192.0.2.1"), "")
}

func TestInvalidAddrSharesOneBucket(t *testing.T) {
	l := newTestLimiter(t, newFakeClock(), nil)
	for i := 0; i < 3; i++ {
		mustAllow(t, l, testPolicy, netip.Addr{}, "")
	}
	mustRefuse(t, l, testPolicy, netip.Addr{}, "")
}

func TestNamedPoliciesAreIndependentAndRegistered(t *testing.T) {
	clk := newFakeClock()
	l, err := New(Config{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	named := []Policy{PolicyLogin, PolicyEnroll, PolicyConnect, PolicyPairing, PolicyAPIKeyFail}
	wantNames := []string{"login", "enroll", "connect", "pairing", "apikey_fail"}
	for i, p := range named {
		if string(p) != wantNames[i] {
			t.Fatalf("policy constant %d = %q, want %q", i, p, wantNames[i])
		}
		mustAllow(t, l, p, ip1, "t")
	}
	// Exhaust login for ip1; other policies keep their tokens.
	burst := DefaultPolicies()[PolicyLogin].IP.Burst
	for i := 1; i < burst; i++ {
		mustAllow(t, l, PolicyLogin, ip1, "")
	}
	mustRefuse(t, l, PolicyLogin, ip1, "")
	mustAllow(t, l, PolicyEnroll, ip1, "other")
}

func TestDefaultPoliciesAreValid(t *testing.T) {
	for name, spec := range DefaultPolicies() {
		if err := spec.IP.validate(); err != nil {
			t.Errorf("%s ip: %v", name, err)
		}
		if err := spec.Target.validate(); err != nil {
			t.Errorf("%s target: %v", name, err)
		}
	}
	if err := DefaultBackoff().validate(); err != nil {
		t.Error(err)
	}
}

func TestUnknownPolicyFailsClosed(t *testing.T) {
	l := newTestLimiter(t, newFakeClock(), nil)
	ok, wait := l.Allow("nope", ip1, "x")
	if ok || wait != time.Minute {
		t.Fatalf("Allow(unknown) = %v, %v; want false, 1m", ok, wait)
	}
}

func TestNewValidation(t *testing.T) {
	bad := []Config{
		{Policies: map[Policy]Spec{"x": {IP: Bucket{0, time.Second}, Target: Bucket{1, time.Second}}}},
		{Policies: map[Policy]Spec{"x": {IP: Bucket{1, time.Second}, Target: Bucket{1, 0}}}},
		{Policies: map[Policy]Spec{"": {IP: Bucket{1, time.Second}, Target: Bucket{1, time.Second}}}},
		{MaxEntries: -1},
		{Backoff: &BackoffSpec{Free: -1, Base: time.Second, Max: time.Second, Decay: time.Hour}},
		{Backoff: &BackoffSpec{Base: 0, Max: time.Second, Decay: time.Hour}},
		{Backoff: &BackoffSpec{Base: time.Minute, Max: time.Second, Decay: time.Hour}},
		{Backoff: &BackoffSpec{Base: time.Second, Max: time.Minute, Decay: 0}},
		{Backoff: &BackoffSpec{Base: 15 * time.Minute, Max: 15 * time.Minute, Decay: 10 * time.Minute}},
	}
	for i, cfg := range bad {
		if _, err := New(cfg); err == nil {
			t.Errorf("config %d accepted, want error", i)
		}
	}
	if _, err := New(Config{}); err != nil {
		t.Fatalf("zero Config rejected: %v", err)
	}
}

func TestLimiterLRUEviction(t *testing.T) {
	clk := newFakeClock()
	l := newTestLimiter(t, clk, func(c *Config) { c.MaxEntries = 3 })
	p := l.policies[testPolicy]

	// Fill the target table with three targets, each one token spent.
	for _, tgt := range []string{"a", "b", "c"} {
		mustAllow(t, l, testPolicy, ip1, tgt)
		clk.Advance(time.Second)
	}
	if p.target.len() != 3 {
		t.Fatalf("target table has %d entries, want 3", p.target.len())
	}
	// All three still carry debt (20 s per token), so a new target is refused
	// instead of evicting one.
	if _, wait := l.Allow(testPolicy, ip2, "d"); wait <= 0 {
		t.Fatal("new target admitted into a table full of active entries")
	}
	// Let every target become idle, touch "a" so that "b" is the least
	// recently used, then add "d".
	clk.Advance(time.Minute)
	mustAllow(t, l, testPolicy, ip2, "a")
	mustAllow(t, l, testPolicy, ip2, "d")
	if p.target.len() != 3 {
		t.Fatalf("target table has %d entries after eviction, want 3", p.target.len())
	}
	if _, ok := p.target.items["b"]; ok {
		t.Fatal("least recently used idle target b was not evicted")
	}
	for _, k := range []string{"a", "c", "d"} {
		if _, ok := p.target.items[k]; !ok {
			t.Fatalf("target %q was evicted", k)
		}
	}
}

func TestLimiterIPTableIsBounded(t *testing.T) {
	clk := newFakeClock()
	l := newTestLimiter(t, clk, func(c *Config) { c.MaxEntries = 3 })
	p := l.policies[testPolicy]
	for i := 0; i < 100; i++ {
		l.Allow(testPolicy, netip.MustParseAddr(fmt.Sprintf("198.51.100.%d", i)), "")
		clk.Advance(time.Minute) // every earlier entry is idle again
	}
	if p.byIP.len() > 3 {
		t.Fatalf("ip table has %d entries, want at most 3", p.byIP.len())
	}
}

func TestBackoffTableIsBounded(t *testing.T) {
	clk := newFakeClock()
	l := newTestLimiter(t, clk, func(c *Config) { c.MaxEntries = 5 })
	for i := 0; i < 100; i++ {
		l.BackoffFail(fmt.Sprintf("user%d", i))
		clk.Advance(time.Second)
	}
	if n := l.failures.len(); n > 5 {
		t.Fatalf("failure table has %d entries, want at most 5", n)
	}
}

// TestEvictionNeverErasesActiveProtection is the flood scenario: a drained
// target bucket and a 15-minute account back-off survive 10000 distinct
// names from distinct IPv6 /64 networks at a fixed clock.
func TestEvictionNeverErasesActiveProtection(t *testing.T) {
	for _, maxEntries := range []int{0, 64} { // 0 is the default cap of 10000
		t.Run(fmt.Sprintf("cap%d", maxEntries), func(t *testing.T) {
			clk := newFakeClock()
			l := newTestLimiter(t, clk, func(c *Config) {
				c.MaxEntries = maxEntries
				c.Policies[PolicyLogin] = DefaultPolicies()[PolicyLogin]
			})
			victimIP := netip.MustParseAddr("192.0.2.77")
			for i := 0; i < 5; i++ { // login target burst is 5
				mustAllow(t, l, PolicyLogin, victimIP, "victim")
			}
			mustRefuse(t, l, PolicyLogin, victimIP, "victim")
			var want time.Duration
			for i := 0; i < 20; i++ { // reach the 15 minute cap
				want = l.BackoffFail("victim")
			}
			if want != 15*time.Minute {
				t.Fatalf("victim back-off = %v, want 15m", want)
			}

			pol := l.policies[PolicyLogin]
			tgtEl, ok := pol.target.items["victim"]
			if !ok {
				t.Fatal("victim target bucket missing before the flood")
			}
			wantTgtTat := tgtEl.Value.(*lruItem[*bucketState]).val.tat
			ipEl, ok := pol.byIP.items[IPKey(victimIP)]
			if !ok {
				t.Fatal("victim IP bucket missing before the flood")
			}
			wantIPTat := ipEl.Value.(*lruItem[*bucketState]).val.tat

			for i := 0; i < 10000; i++ {
				name := fmt.Sprintf("flood%d", i)
				addr := netip.MustParseAddr(fmt.Sprintf("2001:db8:%x:%x::1", i>>8, i&0xff))
				l.Allow(PolicyLogin, addr, name)
				for j := 0; j < 5; j++ { // make the flood back-offs active too
					l.BackoffFail(name)
				}
			}

			// The active victim buckets must be the very same entries, with the
			// same debt, in both tables.
			if el, ok := pol.target.items["victim"]; !ok {
				t.Fatal("victim target bucket was evicted by the flood")
			} else if got := el.Value.(*lruItem[*bucketState]).val.tat; !got.Equal(wantTgtTat) {
				t.Fatalf("victim target tat changed by the flood: %v, want %v", got, wantTgtTat)
			}
			if el, ok := pol.byIP.items[IPKey(victimIP)]; !ok {
				t.Fatal("victim IP bucket was evicted by the flood")
			} else if got := el.Value.(*lruItem[*bucketState]).val.tat; !got.Equal(wantIPTat) {
				t.Fatalf("victim IP tat changed by the flood: %v, want %v", got, wantIPTat)
			}
			// victimIP still has tokens and is already in its table, so only
			// the drained victim target bucket can refuse this probe (12s);
			// a full IP table would give 6s instead.
			if wait := mustRefuse(t, l, PolicyLogin, victimIP, "victim"); wait != 12*time.Second {
				t.Fatalf("probe retry_after = %v, want 12s from the victim target bucket", wait)
			}
			if got := l.Backoff("victim"); got != 15*time.Minute {
				t.Fatalf("victim back-off after flood = %v, want 15m", got)
			}
			if e, ok := l.failures.items["victim"]; !ok || e.Value.(*lruItem[*failEntry]).val.count < 20 {
				t.Fatal("victim failure entry was evicted or reset")
			}
			limit := maxEntries
			if limit == 0 {
				limit = DefaultMaxEntries
			}
			if n := l.failures.len(); n > limit {
				t.Fatalf("failure table has %d entries, cap %d", n, limit)
			}
			if n := l.policies[PolicyLogin].target.len(); n > limit {
				t.Fatalf("target table has %d entries, cap %d", n, limit)
			}
		})
	}
}

func TestFullTableOfActiveEntriesFailsClosedThenRecovers(t *testing.T) {
	clk := newFakeClock()
	l := newTestLimiter(t, clk, func(c *Config) { c.MaxEntries = 2 })
	mustAllow(t, l, testPolicy, ip1, "a")
	mustAllow(t, l, testPolicy, ip2, "b")
	_, wait := l.Allow(testPolicy, netip.MustParseAddr("192.0.2.3"), "c")
	if wait <= 0 {
		t.Fatal("new key admitted although every slot is active")
	}
	// The wait is honest: after it the new key is admitted.
	clk.Advance(wait)
	mustAllow(t, l, testPolicy, netip.MustParseAddr("192.0.2.3"), "c")
	if n := l.policies[testPolicy].target.len(); n > 2 {
		t.Fatalf("target table has %d entries, cap 2", n)
	}

	// Back-off: two running blocks fill the table, a third account is refused.
	l2 := newTestLimiter(t, clk, func(c *Config) { c.MaxEntries = 2 })
	for _, name := range []string{"x", "y"} {
		for i := 0; i < 4; i++ {
			l2.BackoffFail(name)
		}
	}
	if w := l2.Backoff("z"); w <= 0 {
		t.Fatal("new account not refused while the table is full of back-offs")
	}
	if w := l2.BackoffFail("z"); w <= 0 || l2.failures.len() != 2 {
		t.Fatalf("BackoffFail admitted a new account into a full table (wait %v, len %d)", w, l2.failures.len())
	}
	clk.Advance(time.Hour)
	if w := l2.Backoff("z"); w != 0 {
		t.Fatalf("Backoff(z) = %v after the blocks expired", w)
	}
	l2.BackoffFail("z")
	if _, ok := l2.failures.items["z"]; !ok {
		t.Fatal("z was not recorded after the blocks expired")
	}
}

func TestLongKeysDoNotGrowMemoryPerKey(t *testing.T) {
	l := newTestLimiter(t, newFakeClock(), nil)
	huge := make([]byte, 1<<20)
	for i := range huge {
		huge[i] = 'x'
	}
	mustAllow(t, l, testPolicy, ip1, string(huge))
	for k := range l.policies[testPolicy].target.items {
		if len(k) > 100 {
			t.Fatalf("stored key of %d bytes", len(k))
		}
	}
}

func TestStoredKeysDoNotAliasCallerInput(t *testing.T) {
	l := newTestLimiter(t, newFakeClock(), nil)
	// A short target cut from a large buffer, and a short one padded by space.
	body := []byte("name=alice&" + strings.Repeat("x", 1<<20))
	sub := string(body)[5:10]
	padded := strings.Repeat(" ", 1<<20) + "bob"
	mustAllow(t, l, testPolicy, ip1, sub)
	mustAllow(t, l, testPolicy, ip1, padded)
	l.BackoffFail(sub)
	l.BackoffFail(padded)
	for k := range l.policies[testPolicy].target.items {
		if unsafe.StringData(k) == unsafe.StringData(sub) || unsafe.StringData(k) == unsafe.StringData(padded[len(padded)-3:]) {
			t.Fatalf("target key %q aliases the caller's input", k)
		}
	}
	for k := range l.failures.items {
		if unsafe.StringData(k) == unsafe.StringData(sub) || unsafe.StringData(k) == unsafe.StringData(padded[len(padded)-3:]) {
			t.Fatalf("failure key %q aliases the caller's input", k)
		}
	}
	if len(l.policies[testPolicy].target.items) != 2 || len(l.failures.items) != 2 {
		t.Fatal("expected two stored keys in each table")
	}
}

func TestPaddedKeysDoNotRetainLargeAllocations(t *testing.T) {
	clk := newFakeClock()
	l := newTestLimiter(t, clk, nil)
	const n = 24
	before := heapInUse()
	for i := 0; i < n; i++ {
		// A 1 MiB name: short lower-case content padded with space, so TrimSpace returns a substring.
		name := strings.Repeat(" ", 1<<20) + fmt.Sprintf("name%d", i) + " "
		mustAllow(t, l, testPolicy, ip1, name)
		l.BackoffFail(name)
		clk.Advance(time.Minute)
		if k := NormalizeKey(name); len(k) > maxKeyLen {
			t.Fatalf("normalized key has %d bytes", len(k))
		}
	}
	after := heapInUse()
	// Keeping the inputs would retain n MiB each; allow generous slack.
	if grew := int64(after) - int64(before); grew > 4<<20 {
		t.Fatalf("heap grew by %d bytes while storing %d short keys from 1 MiB inputs", grew, n)
	}
	runtime.KeepAlive(l)
	for k := range l.policies[testPolicy].target.items {
		if len(k) > maxKeyLen {
			t.Fatalf("stored key of %d bytes", len(k))
		}
	}
}

func heapInUse() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

func TestLRUBasics(t *testing.T) {
	now := time.Unix(1000, 0)
	idle := func(int) time.Time { return time.Time{} }
	c := newLRU(2, idle)
	c.put("a", 1, now)
	c.put("b", 2, now)
	if v, ok := c.get("a"); !ok || v != 1 {
		t.Fatalf("get a = %v, %v", v, ok)
	}
	c.put("c", 3, now) // evicts b
	if _, ok := c.get("b"); ok {
		t.Fatal("b should be evicted")
	}
	c.put("a", 10, now) // update, no growth
	if v, _ := c.get("a"); v != 10 || c.len() != 2 {
		t.Fatalf("update failed: v=%d len=%d", v, c.len())
	}
	c.remove("a")
	c.remove("missing")
	if c.len() != 1 {
		t.Fatalf("len = %d, want 1", c.len())
	}
}

func TestLRUKeepsActiveEntries(t *testing.T) {
	now := time.Unix(1000, 0)
	active := func(v int) time.Time { return now.Add(time.Duration(v) * time.Second) }
	c := newLRU(2, active)
	c.put("a", 10, now)
	c.put("b", 20, now)
	if ok, wait := c.room(now); ok || wait != 10*time.Second {
		t.Fatalf("room = %v, %v; want false, 10s", ok, wait)
	}
	c.put("c", 5, now) // dropped, not admitted
	if _, ok := c.get("c"); ok || c.len() != 2 {
		t.Fatal("new key displaced an active entry")
	}
	// The cached bound keeps answering without a rescan.
	if ok, wait := c.room(now.Add(4 * time.Second)); ok || wait != 6*time.Second {
		t.Fatalf("room = %v, %v; want false, 6s", ok, wait)
	}
	// After 10 s only "a" is idle.
	if ok, _ := c.room(now.Add(10 * time.Second)); !ok {
		t.Fatal("no room after a became idle")
	}
	if _, ok := c.get("a"); ok {
		t.Fatal("idle a should have been evicted")
	}
	if _, ok := c.get("b"); !ok {
		t.Fatal("active b was evicted")
	}
}

func TestBackoffGrowthCapAndReset(t *testing.T) {
	clk := newFakeClock()
	spec := BackoffSpec{Free: 2, Base: time.Second, Max: 10 * time.Second, Decay: time.Hour}
	l := newTestLimiter(t, clk, func(c *Config) { c.Backoff = &spec })

	if w := l.Backoff("alice"); w != 0 {
		t.Fatalf("fresh account blocked for %v", w)
	}
	// Two free failures, then 1s, 2s, 4s, 8s, then capped at 10s.
	want := []time.Duration{0, 0, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
	for i, d := range want {
		if got := l.BackoffFail("alice"); got != d {
			t.Fatalf("failure %d: delay %v, want %v", i+1, got, d)
		}
		if got := l.Backoff("alice"); got != d {
			t.Fatalf("failure %d: Backoff %v, want %v", i+1, got, d)
		}
		clk.Advance(d) // the account waits the delay out before the next attempt
		if got := l.Backoff("alice"); got != 0 {
			t.Fatalf("failure %d: still blocked %v after waiting", i+1, got)
		}
	}
	// The wait counts down.
	l.BackoffFail("alice")
	clk.Advance(3 * time.Second)
	if got := l.Backoff("alice"); got != 7*time.Second {
		t.Fatalf("Backoff after 3s = %v, want 7s", got)
	}
	// Other accounts are unaffected; account names are case-insensitive.
	if got := l.Backoff("bob"); got != 0 {
		t.Fatalf("bob blocked %v", got)
	}
	if got := l.Backoff("ALICE"); got != 7*time.Second {
		t.Fatalf("ALICE = %v, want 7s", got)
	}
	// A success resets.
	l.BackoffReset("alice")
	if got := l.Backoff("alice"); got != 0 {
		t.Fatalf("blocked %v after reset", got)
	}
	// The sequence starts over: two free failures again.
	for i := 0; i < 2; i++ {
		if got := l.BackoffFail("alice"); got != 0 {
			t.Fatalf("post-reset failure %d delayed %v", i+1, got)
		}
	}
	if got := l.BackoffFail("alice"); got != time.Second {
		t.Fatalf("post-reset third failure delay %v, want 1s", got)
	}
}

func TestBackoffDecay(t *testing.T) {
	clk := newFakeClock()
	spec := BackoffSpec{Free: 0, Base: time.Second, Max: time.Minute, Decay: 10 * time.Minute}
	l := newTestLimiter(t, clk, func(c *Config) { c.Backoff = &spec })
	for i := 0; i < 4; i++ {
		l.BackoffFail("alice") // 1s, 2s, 4s, 8s
	}
	clk.Advance(10 * time.Minute)
	if got := l.Backoff("alice"); got != 0 {
		t.Fatalf("blocked %v after decay", got)
	}
	if l.failures.len() != 0 {
		t.Fatal("decayed entry not dropped")
	}
	// A failure after decay starts at the beginning, not at the old count.
	l.BackoffFail("alice") // forgotten by Backoff above; start fresh
	clk.Advance(10 * time.Minute)
	if got := l.BackoffFail("alice"); got != time.Second {
		t.Fatalf("failure after decay delayed %v, want 1s", got)
	}
}

func TestBackoffDefaultsAndEmptyAccount(t *testing.T) {
	clk := newFakeClock()
	l, err := New(Config{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	if got := l.BackoffFail(""); got != 0 || l.failures.len() != 0 {
		t.Fatal("empty account must be ignored")
	}
	l.BackoffReset("")
	if l.Backoff("") != 0 {
		t.Fatal("empty account must never be blocked")
	}
	// Defaults: three free failures, then 1s.
	for i := 0; i < 3; i++ {
		if d := l.BackoffFail("u"); d != 0 {
			t.Fatalf("failure %d delayed %v", i+1, d)
		}
	}
	if d := l.BackoffFail("u"); d != time.Second {
		t.Fatalf("fourth failure delay %v, want 1s", d)
	}
	// Enormous failure counts stay at the cap without overflow.
	for i := 0; i < 200; i++ {
		l.BackoffFail("u")
	}
	if d := l.BackoffFail("u"); d != 15*time.Minute {
		t.Fatalf("delay %v, want the 15m cap", d)
	}
}

func TestBackoffDelayTable(t *testing.T) {
	b := BackoffSpec{Free: 1, Base: 3 * time.Second, Max: 20 * time.Second, Decay: time.Hour}
	want := map[int]time.Duration{
		0: 0, 1: 0, 2: 3 * time.Second, 3: 6 * time.Second, 4: 12 * time.Second,
		5: 20 * time.Second, 6: 20 * time.Second, 1000: 20 * time.Second,
	}
	for n, d := range want {
		if got := b.delay(n); got != d {
			t.Errorf("delay(%d) = %v, want %v", n, got, d)
		}
	}
}

func TestNoGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	clk := newFakeClock()
	l := newTestLimiter(t, clk, nil)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				l.Allow(testPolicy, netip.MustParseAddr(fmt.Sprintf("10.0.%d.%d", g, i%250)), fmt.Sprintf("t%d", i%7))
				l.BackoffFail(fmt.Sprintf("u%d", i%5))
				l.Backoff(fmt.Sprintf("u%d", i%5))
				if i%50 == 0 {
					l.BackoffReset("u0")
				}
				clk.Advance(time.Millisecond)
			}
		}(g)
	}
	wg.Wait()
	// The limiter spawns nothing; only the workers above existed and are gone.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("goroutines: %d before, %d after", before, after)
	}
}

func TestSystemClockDefault(t *testing.T) {
	l, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, isSys := l.clock.(systemClock); !isSys {
		t.Fatalf("default clock is %T", l.clock)
	}
	if d := time.Since(l.clock.Now()); d < 0 || d > time.Minute {
		t.Fatalf("system clock off by %v", d)
	}
	mustAllow(t, l, PolicyLogin, ip1, "x")
}
