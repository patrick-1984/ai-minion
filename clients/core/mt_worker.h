/* Minion thin Worker: the device half of the Worker protocol 3.0 (connect and enroll_invite). */
#ifndef MT_WORKER_H
#define MT_WORKER_H

#include <stddef.h>
#include <stdint.h>

#include "mt_platform.h"
#include "mt_proto.h"
#include "mt_vt.h"
#include "mt_ws.h"

#define MT_VERSION "0.1.0-preview.1"

/* A session the Worker serves: a PTY, a UART bridge, a built-in console. Owned by the platform. */
typedef struct mt_session {
	char name[33];       /* <= 32 bytes, no ':' or '.' */
	const char *kind;    /* "shell" or "other" (PaneInfo.kind) */
	const char *command; /* foreground command label, e.g. "ash", "uart", "console" */
	mt_vt *vt;
	int64_t created_ms;  /* Unix ms, 0 = unknown (uses the Worker start) */
	int readonly;        /* input refused (e.g. a log) */
	/* Input. Returns 0 when all n bytes were delivered, -1 when nothing was delivered, -2 when some bytes may have
	 * reached the session (the operation then reports "partial"). Must not block for long (a few seconds). */
	int (*write)(struct mt_session *s, const uint8_t *data, size_t n);
	void *ctx;
	/* Worker-private */
	uint32_t seen_gen;
	char hash[65];
	int64_t dirty_mono;
} mt_session;

typedef struct {
	const char *hub_url;
	const char *name;          /* announced Worker name (<= 56 bytes are kept by the Hub) */
	mt_session **sessions;
	size_t nsessions;
	size_t rx_cap;             /* WebSocket message buffer: >= 16404 */
	size_t frame_cap;          /* largest inbound stream frame accepted (op.request) */
	size_t out_cap;            /* largest result frame produced */
	int max_streams;           /* concurrent yamux streams (op streams + ev) */
	int results_ring;          /* remembered write results (request ids) */
	size_t tx_rec_cap;         /* plaintext bytes per outgoing record (512..16384; 0 = 16384); small saves RAM */
} mt_config;

typedef struct mt_worker mt_worker;

/* Allocates its buffers once (malloc) and loads the persistent state. NULL on failure. */
mt_worker *mt_worker_new(const mt_platform *p, const mt_config *cfg);
/* Runs forever: connects, serves, reconnects with backoff 1 s .. 60 s. Returns only after a valid signed
 * revoke left nothing to do (no invitation key), or when mt_worker_stop() was called. */
int mt_worker_run(mt_worker *w);
void mt_worker_stop(mt_worker *w);

/* Status for consoles and logs. */
typedef struct {
	int enrolled, connected, pending, revoked;
	char worker_id[40];
	char hub_fp[53];
	uint64_t max_serial;
	char last_error[96];
} mt_status;
void mt_worker_status(const mt_worker *w, mt_status *st);

/* Store keys used by the core (the platform may write "invite" to start an enrollment). */
#define MT_KEY_SEED "seed"
#define MT_KEY_STATE "state"
#define MT_KEY_INVITE "invite"
#define MT_KEY_SHARE "share"

#endif
