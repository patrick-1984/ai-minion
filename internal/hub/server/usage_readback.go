package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
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

	mu    sync.Mutex
	cache map[string]usageReadbackEntry
}

type usageReadbackEntry struct {
	key  string
	at   time.Time
	doc  ops.UsageMeters
	hash string
}

func newUsageReadback(st *store.Store) *usageReadback {
	return &usageReadback{st: st, now: time.Now, cache: map[string]usageReadbackEntry{}}
}

// Document returns the document for the Workers of owner and the hash of its
// stable content (independent of the generation time). The owner must be the
// authenticated principal's own id (the preview) or the Hub owner of an
// approved Worker (delivery); callers decide that before they ask.
func (u *usageReadback) Document(ctx context.Context, owner string) (ops.UsageMeters, string, error) {
	specs, err := u.st.UsageReadbackSlots(ctx, owner)
	if err != nil {
		return ops.UsageMeters{}, "", err
	}
	sources := make([][]store.UsagePublishableSource, len(specs))
	h := sha256.New()
	for i, sp := range specs {
		if sources[i], err = u.st.UsagePublishableSources(ctx, sp.Publisher, sp.Kind, sp.BucketID); err != nil {
			return ops.UsageMeters{}, "", err
		}
		b, _ := json.Marshal([]any{sp, sources[i]})
		h.Write(b)
	}
	key := hex.EncodeToString(h.Sum(nil))
	now := u.now()

	u.mu.Lock()
	if e, ok := u.cache[owner]; ok && e.key == key && now.Sub(e.at) < usageReadbackTTL {
		u.mu.Unlock()
		return e.doc, e.hash, nil
	}
	u.mu.Unlock()

	doc := ops.UsageMeters{Schema: ops.UsageMetersSchema, GeneratedAt: now.UnixMilli(), Slots: []ops.UsageSlotDoc{}}
	for i, sp := range specs {
		data, err := u.st.ReadNominatedUsage(ctx, sp.Kind, sources[i])
		if err != nil {
			return ops.UsageMeters{}, "", err
		}
		var meters []ops.UsageMeter
		var omitted int
		if sp.Kind == "provider" {
			meters, omitted = ops.ProviderMeters(data.Accounts, now)
		} else {
			meters, omitted = ops.SessionMeters(data.Sessions, data.Linked, now)
		}
		doc.Slots = append(doc.Slots, ops.NewUsageSlot(sp.Scope, sp.Kind, sp.Label, meters, omitted, now))
	}
	if err := doc.Fit(ops.UsageMetersMaxBytes); err != nil {
		return ops.UsageMeters{}, "", err
	}
	hash := doc.ContentHash()

	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.cache) > 1024 {
		for k, e := range u.cache {
			if now.Sub(e.at) >= usageReadbackTTL {
				delete(u.cache, k)
			}
		}
	}
	u.cache[owner] = usageReadbackEntry{key: key, at: now, doc: doc, hash: hash}
	return doc, hash, nil
}
