package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/patrick-1984/ai-minion/internal/hub/store"
	"github.com/patrick-1984/ai-minion/internal/ops"
)

// usageReadbackTTL is how long the computed meters of one recipient are reused.
const usageReadbackTTL = 30 * time.Second

// usageReadback builds the nominated usage document of the Workers of one
// owner: the Hub-wide pair and the owner's personal pair. The authorization is
// in the data: every slot reads only the sources its publisher may publish.
//
// A cached document is reused for usageReadbackTTL, but only while the
// authority behind it is unchanged. The slots and their publishable sources
// are read (cheaply) on every call and are part of the cache key, so a
// withdrawn nomination, a removed administrator or a withdrawn permission
// takes effect at once and only the aggregation is deferred.
type usageReadback struct {
	st  *store.Store
	now func() time.Time
	// access lists the Worker ids the publisher may view now, by the same
	// rules as the ordinary Usage views. Nil allows only the publisher's own
	// Workers.
	access func(ctx context.Context, publisher string) ([]string, error)

	// gen counts changes of publication authority (consent, nominations,
	// buckets). The operations bump it before and after they write; a document
	// built across a bump is discarded and built again.
	gen atomic.Uint64

	mu    sync.Mutex
	cache map[string]usageReadbackEntry
}

func (u *usageReadback) bump()              { u.gen.Add(1) }
func (u *usageReadback) generation() uint64 { return u.gen.Load() }

var errReadbackChanging = errors.New("usage publication is changing; try again")

type usageReadbackEntry struct {
	key string
	at  time.Time
	// until is when the entry stops being served: the TTL, or earlier when a
	// meter of the document expires (a reset must not leave a stale headline).
	until time.Time
	doc   ops.UsageMeters
	hash  string
}

func newUsageReadback(st *store.Store) *usageReadback {
	return &usageReadback{st: st, now: time.Now, cache: map[string]usageReadbackEntry{}}
}

// visibleFor resolves, once per publisher, what the publisher may view.
func (u *usageReadback) visibleFor(ctx context.Context, slots []store.UsageSlotSpec) (map[string][]string, error) {
	out := map[string][]string{}
	if u.access == nil {
		return out, nil
	}
	for _, sp := range slots {
		if _, ok := out[sp.Publisher]; ok {
			continue
		}
		ids, err := u.access(ctx, sp.Publisher)
		if err != nil {
			return nil, err
		}
		out[sp.Publisher] = ids
	}
	return out, nil
}

// snapshot reads the slots of owner in one transaction. The publishers'
// visibility is resolved outside the transaction, so a snapshot that carries
// data resolves it again afterwards: when a role or an ownership changed
// meanwhile (a demoted publisher), the snapshot is read again.
func (u *usageReadback) snapshot(ctx context.Context, owner string, withData bool) ([]store.ReadbackSlot, error) {
	for range 3 {
		specs, err := u.st.UsageReadbackSlots(ctx, owner)
		if err != nil {
			return nil, err
		}
		visible, err := u.visibleFor(ctx, specs)
		if err != nil {
			return nil, err
		}
		slots, err := u.st.UsageReadbackSnapshot(ctx, owner, visible, withData)
		if err != nil || !withData {
			return slots, err
		}
		again, err := u.visibleFor(ctx, specs)
		if err != nil {
			return nil, err
		}
		if sameVisible(visible, again) {
			return slots, nil
		}
	}
	return nil, errReadbackChanging
}

func sameVisible(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, x := range a {
		y, ok := b[k]
		if !ok || len(x) != len(y) {
			return false
		}
		xs, ys := slices.Clone(x), slices.Clone(y)
		slices.Sort(xs)
		slices.Sort(ys)
		if !slices.Equal(xs, ys) {
			return false
		}
	}
	return true
}

func readbackKey(slots []store.ReadbackSlot) string {
	h := sha256.New()
	for _, sl := range slots {
		b, _ := json.Marshal([]any{sl.UsageSlotSpec, sl.Sources})
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Document returns the document for the Workers of owner and the hash of its
// stable content (independent of the generation time). The owner must be the
// authenticated principal's own id (the preview) or the Hub owner of an
// approved Worker (delivery); callers decide that before they ask.
func (u *usageReadback) Document(ctx context.Context, owner string) (ops.UsageMeters, string, error) {
	for range 3 {
		g := u.generation()
		doc, hash, err := u.document(ctx, owner)
		if err != nil || u.generation() == g {
			return doc, hash, err
		}
		// The publication authority changed while the document was built (a
		// consent withdrawn, a nomination replaced): it may hold data that is
		// no longer allowed. Build again from the new state.
	}
	return ops.UsageMeters{}, "", errReadbackChanging
}

func (u *usageReadback) document(ctx context.Context, owner string) (ops.UsageMeters, string, error) {
	cheap, err := u.snapshot(ctx, owner, false)
	if err != nil {
		return ops.UsageMeters{}, "", err
	}
	key := readbackKey(cheap)

	u.mu.Lock()
	if e, ok := u.cache[owner]; ok && e.key == key && u.now().Before(e.until) {
		u.mu.Unlock()
		return e.doc, e.hash, nil
	}
	u.mu.Unlock()

	// The sources and the observations come from one snapshot: consent
	// withdrawn meanwhile cannot let a later observation into the document.
	slots, err := u.snapshot(ctx, owner, true)
	if err != nil {
		return ops.UsageMeters{}, "", err
	}
	key = readbackKey(slots)
	// The clock is read after the data: a meter that reset while the snapshot
	// was read is not chosen as the headline.
	now := u.now()
	doc := ops.UsageMeters{Schema: ops.UsageMetersSchema, GeneratedAt: now.UnixMilli(), Slots: []ops.UsageSlotDoc{}}
	for _, sl := range slots {
		var meters []ops.UsageMeter
		var omitted int
		if sl.Kind == "provider" {
			meters, omitted = ops.ProviderMeters(sl.Data.Accounts, now)
		} else {
			meters, omitted = ops.SessionMeters(sl.Data.Sessions, sl.Data.Linked, now)
		}
		doc.Slots = append(doc.Slots, ops.NewUsageSlot(sl.Scope, sl.Kind, sl.Label, meters, omitted, now))
	}
	if err := doc.Fit(ops.UsageMetersMaxBytes); err != nil {
		return ops.UsageMeters{}, "", err
	}
	hash := doc.ContentHash()
	until := now.Add(usageReadbackTTL)
	for _, sl := range doc.Slots {
		for _, m := range sl.Meters {
			if e := time.UnixMilli(m.ExpiresAt); e.After(now) && e.Before(until) {
				until = e
			}
		}
	}

	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.cache) > 1024 {
		for k, e := range u.cache {
			if !now.Before(e.until) {
				delete(u.cache, k)
			}
		}
	}
	u.cache[owner] = usageReadbackEntry{key: key, at: now, until: until, doc: doc, hash: hash}
	return doc, hash, nil
}
