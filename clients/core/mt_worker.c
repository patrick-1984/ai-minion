/* Device half of the Minion Worker protocol 3.0. Mirrors internal/transport (handshake, channel, mux),
 * internal/worker/agent (connector, op server, events) for the subset a thin device needs. */
#include "mt_worker.h"

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "mt_crypto.h"
#include "mt_enc.h"
#include "mt_json.h"
#include "mt_keys.h"

#define HS_TIMEOUT_MS 30000
#define KEEPALIVE_MS 30000
#define DEAD_AFTER_MS 120000
#define KIND_TIMEOUT_MS 10000
#define EXPIRY_TICK_MS 30000
#define EV_DEBOUNCE_MS 300
#define EV_REOPEN_MS 2000
#define RESULT_TTL_MS 600000
#define YAMUX_WINDOW (256u * 1024u)
#define MAX_TOKENS 256
#define MAX_CHAIN 8

enum { ST_FREE = 0, ST_OPEN = 1 };
enum { K_NONE = 0, K_OP = 1, K_EV = 2 };

typedef struct {
	uint32_t id;
	int state, kind, local; /* local: opened by us */
	uint8_t kbuf[2];
	int klen;
	int64_t opened_mono;
	uint8_t *in;
	size_t in_len;
	uint32_t send_window;
	uint32_t discard; /* bytes of an oversized frame still to skip */
	int fin_rx, handled;
} stream;

typedef struct {
	char id[40];
	int64_t mono;
	char result[160];
} cached;

typedef struct {
	uint8_t magic[4];
	uint8_t enrolled;
	uint8_t root[32];
	uint64_t pin_seq;
	uint64_t max_serial;
	uint8_t cert_sha[32];
	uint8_t has_hash;
	char worker_id[40];
	uint8_t preview_only;
} persisted;

struct mt_worker {
	const mt_platform *p;
	mt_config cfg;
	mt_url url;
	mt_ws ws;
	uint8_t *rx;      /* WebSocket message buffer */
	uint8_t *out;     /* JSON result buffer */
	uint8_t *rec;     /* outgoing record plaintext + wire */
	size_t rec_len;
	uint8_t *wire;
	stream *streams;
	uint8_t *frame_pool;
	cached *results;
	int nresults;
	/* identity and state */
	uint8_t seed[32], pub[32];
	persisted st;
	int revoked, stop;
	char last_error[96];
	/* connection */
	int connected, pending;
	mt_dir w2h, h2w;
	uint8_t transcript[32], op_pub[32];
	mt_cert cert;
	int64_t deadline_mono, last_rx_mono, last_ping_mono, last_expiry_mono, ev_retry_mono;
	uint32_t ping_id, next_local_id;
	int ping_outstanding;
	/* plaintext input parser */
	int ctl_done;
	uint8_t ctl_hdr[4];
	int ctl_hdr_len;
	uint8_t *ctl_buf;
	size_t ctl_len, ctl_need;
	uint8_t yhdr[12];
	int yhdr_len;
	uint32_t ypay_left;
	stream *ycur;
	/* events */
	stream *ev;
	uint64_t ev_seq;
	/* share */
	int share_mode; /* 0 all, 1 none, 2 select, 3 later */
	char share_names[8][33];
	int share_n;
	uint64_t share_rev, op_barrier;
	/* replay protection: request ids whose UUIDv7 time is not after this floor are refused (the result of an
	 * evicted or pre-reboot request id is no longer known, so executing it again could repeat an effect) */
	int64_t replay_floor_ms;
};

static const char *OPS[] = {"sessions_list", "session_snapshot", "session_capture", "send_text", "send_keys",
                            "share_get", "share_set"};
static const char *MODES[] = {"all", "none", "select", "later"};

/* ---------------------------------------------------------------- helpers */

static void logf_(mt_worker *w, int lvl, const char *fmt, const char *a, long b)
{
	char m[192];
	snprintf(m, sizeof m, fmt, a ? a : "", b);
	if (w->p->log)
		w->p->log(w->p->ctx, lvl, m);
	if (lvl <= MT_LOG_WARN)
	{
		size_t l = strlen(m);
		if (l >= sizeof w->last_error)
			l = sizeof w->last_error - 1;
		memcpy(w->last_error, m, l);
		w->last_error[l] = 0;
	}
}

static int64_t mono(mt_worker *w) { return w->p->mono_ms(w->p->ctx); }

static void put_be32(uint8_t *p, uint32_t v)
{
	p[0] = (uint8_t)(v >> 24); p[1] = (uint8_t)(v >> 16); p[2] = (uint8_t)(v >> 8); p[3] = (uint8_t)v;
}
static uint32_t be32(const uint8_t *p)
{
	return (uint32_t)p[0] << 24 | (uint32_t)p[1] << 16 | (uint32_t)p[2] << 8 | p[3];
}

static void ts_fmt(char out[25], int64_t ms)
{
	int64_t s = ms / 1000, days = s / 86400, rem = s % 86400, z, era, doe, yoe, y, doy, mp, d, m;
	if (ms < 0) {
		ms = 0; s = 0; days = 0; rem = 0;
	}
	z = days + 719468;
	era = (z >= 0 ? z : z - 146096) / 146097;
	doe = z - era * 146097;
	yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
	y = yoe + era * 400;
	doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
	mp = (5 * doy + 2) / 153;
	d = doy - (153 * mp + 2) / 5 + 1;
	m = mp < 10 ? mp + 3 : mp - 9;
	y += m <= 2;
	{
		int v[7], wd[7] = {4, 2, 2, 2, 2, 2, 3}, i, k, o = 0;
		static const char sep[7] = {'-', '-', 'T', ':', ':', '.', 'Z'};
		v[0] = (int)(y < 0 ? 0 : y > 9999 ? 9999 : y);
		v[1] = (int)m; v[2] = (int)d; v[3] = (int)(rem / 3600); v[4] = (int)(rem / 60 % 60);
		v[5] = (int)(rem % 60); v[6] = (int)(ms % 1000);
		for (i = 0; i < 7; i++) {
			for (k = wd[i] - 1; k >= 0; k--, v[i] /= 10)
				out[o + k] = (char)('0' + v[i] % 10);
			o += wd[i];
			out[o++] = sep[i];
		}
		out[o] = 0;
	}
}

/* ---------------------------------------------------------------- persistence */

static void save_state(mt_worker *w)
{
	memcpy(w->st.magic, "MTE1", 4);
	if (w->p->store_set(w->p->ctx, MT_KEY_STATE, (const uint8_t *)&w->st, sizeof w->st) != 0)
		logf_(w, MT_LOG_ERROR, "store: cannot write the enrollment state%s", NULL, 0);
}

static void save_share(mt_worker *w)
{
	uint8_t b[1 + 8 + 1 + 8 * 33];
	int i;
	memset(b, 0, sizeof b);
	b[0] = (uint8_t)w->share_mode;
	memcpy(b + 1, &w->share_rev, 8);
	b[9] = (uint8_t)w->share_n;
	for (i = 0; i < w->share_n; i++)
		memcpy(b + 10 + 33 * i, w->share_names[i], 33);
	w->p->store_set(w->p->ctx, MT_KEY_SHARE, b, sizeof b);
}

static void load(mt_worker *w)
{
	uint8_t b[1 + 8 + 1 + 8 * 33];
	int n = w->p->store_get(w->p->ctx, MT_KEY_SEED, w->seed, sizeof w->seed);
	if (n != 32) {
		w->p->random(w->p->ctx, w->seed, 32);
		w->p->store_set(w->p->ctx, MT_KEY_SEED, w->seed, 32);
	}
	mt_ed25519_pub_from_seed(w->pub, w->seed);
	memset(&w->st, 0, sizeof w->st);
	n = w->p->store_get(w->p->ctx, MT_KEY_STATE, (uint8_t *)&w->st, sizeof w->st);
	if (n != (int)sizeof w->st || memcmp(w->st.magic, "MTE1", 4))
		memset(&w->st, 0, sizeof w->st);
	w->share_mode = 0; /* a device shares its sessions unless the owner changes it in the Hub */
	w->share_rev = 1;
	if (w->p->store_get(w->p->ctx, MT_KEY_SHARE, b, sizeof b) == (int)sizeof b && b[0] <= 3 && b[9] <= 8) {
		int i;
		w->share_mode = b[0];
		memcpy(&w->share_rev, b + 1, 8);
		w->share_n = b[9];
		for (i = 0; i < w->share_n; i++) {
			memcpy(w->share_names[i], b + 10 + 33 * i, 33);
			w->share_names[i][32] = 0;
		}
	}
}

mt_worker *mt_worker_new(const mt_platform *p, const mt_config *cfg)
{
	mt_worker *w = calloc(1, sizeof *w);
	int i;
	if (!w)
		return NULL;
	w->p = p;
	w->cfg = *cfg;
	if (w->cfg.rx_cap < 16404 + 64)
		w->cfg.rx_cap = 16404 + 64;
	if (w->cfg.frame_cap < 2048)
		w->cfg.frame_cap = 2048;
	if (w->cfg.out_cap < 4096)
		w->cfg.out_cap = 4096;
	if (w->cfg.max_streams < 2)
		w->cfg.max_streams = 2;
	if (w->cfg.results_ring < 4)
		w->cfg.results_ring = 4;
	if (mt_url_parse(&w->url, cfg->hub_url) != 0) {
		free(w);
		return NULL;
	}
	w->rx = malloc(w->cfg.rx_cap);
	w->out = malloc(w->cfg.out_cap);
	w->rec = malloc(MT_REC_MAX_PLAIN);
	w->wire = malloc(MT_REC_MAX_PLAIN + 20);
	w->ctl_buf = malloc(4096);
	w->streams = calloc((size_t)w->cfg.max_streams, sizeof(stream));
	w->frame_pool = malloc(w->cfg.frame_cap * (size_t)w->cfg.max_streams);
	w->results = calloc((size_t)w->cfg.results_ring, sizeof(cached));
	if (!w->rx || !w->out || !w->rec || !w->wire || !w->ctl_buf || !w->streams || !w->frame_pool || !w->results) {
		free(w->rx); free(w->out); free(w->rec); free(w->wire); free(w->ctl_buf); free(w->streams);
		free(w->frame_pool); free(w->results); free(w);
		return NULL;
	}
	for (i = 0; i < w->cfg.max_streams; i++)
		w->streams[i].in = w->frame_pool + w->cfg.frame_cap * (size_t)i;
	mt_ws_init(&w->ws, p, w->rx, w->cfg.rx_cap);
	load(w);
	return w;
}

void mt_worker_stop(mt_worker *w) { w->stop = 1; }

void mt_worker_status(const mt_worker *w, mt_status *st)
{
	memset(st, 0, sizeof *st);
	st->enrolled = w->st.enrolled;
	st->connected = w->connected;
	st->pending = w->pending;
	st->revoked = w->revoked;
	memcpy(st->worker_id, w->st.worker_id, sizeof st->worker_id);
	st->worker_id[sizeof st->worker_id - 1] = 0;
	if (w->st.enrolled)
		mt_fingerprint(st->hub_fp, w->st.root);
	st->max_serial = w->st.max_serial;
	snprintf(st->last_error, sizeof st->last_error, "%s", w->last_error);
}

/* ---------------------------------------------------------------- handshake frames (plaintext) */

static int hs_send(mt_worker *w, const char *json, size_t n)
{
	uint8_t *b = w->wire;
	if (n + 4 > MT_REC_MAX_PLAIN + 20)
		return -1;
	put_be32(b, (uint32_t)n);
	memcpy(b + 4, json, n);
	return mt_ws_send(&w->ws, b, n + 4);
}

/* Receive one handshake frame; returns the JSON length (JSON at w->rx + 4) and its type in `type`. */
static int hs_recv(mt_worker *w, char *type, size_t tcap, mt_jtok *t, int *ntok)
{
	int n = mt_ws_recv(&w->ws, HS_TIMEOUT_MS), it;
	uint32_t len;
	if (n <= 0 || n < 4)
		return -1;
	len = be32(w->rx);
	if (len != (uint32_t)n - 4 || len > 65536)
		return -1;
	*ntok = mt_json_parse((const char *)w->rx + 4, len, t, MAX_TOKENS);
	if (*ntok <= 0 || t[0].type != MT_JT_OBJ)
		return -1;
	it = mt_json_get((const char *)w->rx + 4, t, *ntok, 0, "type");
	if (mt_json_str((const char *)w->rx + 4, t, it, type, tcap) <= 0)
		return -1;
	return (int)len;
}

static int get_b64(const char *js, const mt_jtok *t, int nt, int obj, const char *k, uint8_t *out, size_t want)
{
	int i = mt_json_get(js, t, nt, obj, k);
	if (i < 0 || t[i].type != MT_JT_STR)
		return -1;
	return mt_b64url_decode(out, want, js + t[i].start, (size_t)(t[i].end - t[i].start)) == (int)want ? 0 : -1;
}

/* Returns 0 when the handshake succeeded and the channel keys are set. On a refused certificate/pin the error
 * text is in last_error. */
static int handshake(mt_worker *w, int enroll, const uint8_t inv_fp_hash[32])
{
	uint8_t eph[32], ephp[32], nonce[32], ephh[32], hub_pub[32], certb[200], sig[64], dh[32];
	char hello[1024], type[24], *hh;
	static mt_jtok t[MAX_TOKENS];
	int nt, n, i, r, len;
	size_t k;
	mt_jw jw;
	mt_pin pin_now;
	mt_transition chain[MAX_CHAIN];
	size_t nchain = 0;

	w->p->random(w->p->ctx, eph, 32);
	mt_x25519_public(ephp, eph);
	w->p->random(w->p->ctx, nonce, 32);
	mt_jw_init(&jw, hello, sizeof hello);
	mt_jw_obj(&jw);
	mt_jw_kstr(&jw, "type", "hello");
	mt_jw_kstr(&jw, "proto_min", "3.0");
	mt_jw_kstr(&jw, "proto_max", "3.0");
	mt_jw_key(&jw, "ops");
	mt_jw_arr(&jw);
	for (k = 0; k < sizeof OPS / sizeof OPS[0]; k++)
		mt_jw_str(&jw, OPS[k]);
	mt_jw_arr_end(&jw);
	mt_jw_key(&jw, "features");
	mt_jw_arr(&jw);
	mt_jw_str(&jw, "send_text.submit");
	mt_jw_str(&jw, "text.verbatim");
	mt_jw_arr_end(&jw);
	mt_jw_kb64(&jw, "worker_pub", w->pub, 32);
	mt_jw_kb64(&jw, "eph_w", ephp, 32);
	mt_jw_kb64(&jw, "nonce", nonce, 32);
	mt_jw_kstr(&jw, "mode", enroll ? "enroll_invite" : "connect");
	mt_jw_kstr(&jw, "name", w->cfg.name);
	mt_jw_obj_end(&jw);
	if (jw.err || hs_send(w, hello, jw.len) != 0) {
		logf_(w, MT_LOG_WARN, "handshake: cannot send hello%s", NULL, 0);
		goto fail;
	}
	len = hs_recv(w, type, sizeof type, t, &nt);
	if (len < 0) {
		logf_(w, MT_LOG_WARN, "handshake: no Hub hello%s", NULL, 0);
		goto fail;
	}
	hh = (char *)w->rx + 4;
	if (strcmp(type, "hello")) {
		char code[48] = "";
		mt_json_str(hh, t, mt_json_get(hh, t, nt, 0, "code"), code, sizeof code);
		logf_(w, MT_LOG_WARN, "handshake: Hub answered %s (unauthenticated)", code[0] ? code : type, 0);
		goto fail;
	}
	/* (a) shape */
	if (!mt_json_eq(hh, t, mt_json_get(hh, t, nt, 0, "version"), "3.0") ||
	    get_b64(hh, t, nt, 0, "hub_pub", hub_pub, 32) || get_b64(hh, t, nt, 0, "eph_h", ephh, 32) ||
	    get_b64(hh, t, nt, 0, "nonce", nonce, 32)) {
		logf_(w, MT_LOG_WARN, "handshake: malformed Hub hello%s", NULL, 0);
		goto fail;
	}
	i = mt_json_get(hh, t, nt, 0, "hub_cert");
	if (i < 0 || t[i].type != MT_JT_STR ||
	    (n = mt_b64url_decode(certb, sizeof certb, hh + t[i].start, (size_t)(t[i].end - t[i].start))) < 0) {
		logf_(w, MT_LOG_WARN, "handshake: certificate missing%s", NULL, 0);
		goto fail;
	}
	i = mt_json_get(hh, t, nt, 0, "chain");
	if (i >= 0 && t[i].type == MT_JT_ARR) {
		int e = i + 1, c;
		if (enroll && t[i].size > 0) {
			logf_(w, MT_LOG_WARN, "handshake: unexpected chain%s", NULL, 0);
			goto fail;
		}
		if (t[i].size > MAX_CHAIN)
			goto fail;
		for (c = 0; c < t[i].size; c++, e = mt_json_skip(t, nt, e)) {
			mt_transition *tr = &chain[nchain++];
			char ts[32];
			int64_t seq;
			if (mt_json_int(hh, t, mt_json_get(hh, t, nt, e, "seq"), &seq) || seq < 0 ||
			    get_b64(hh, t, nt, e, "old_pub", tr->old_pub, 32) ||
			    get_b64(hh, t, nt, e, "new_pub", tr->new_pub, 32) ||
			    get_b64(hh, t, nt, e, "sig_by_old", tr->sig, 64) ||
			    mt_json_str(hh, t, mt_json_get(hh, t, nt, e, "created_at"), ts, sizeof ts) < 0 ||
			    mt_parse_ts_ms(ts, &tr->created_ms)) {
				logf_(w, MT_LOG_WARN, "handshake: malformed chain%s", NULL, 0);
				goto fail;
			}
			tr->seq = (uint64_t)seq;
		}
	}
	r = mt_cert_parse(&w->cert, certb, (size_t)n);
	if (r) {
		logf_(w, MT_LOG_WARN, "handshake: %s", mt_err_reason(r), 0);
		goto fail;
	}
	mt_transcript(w->transcript, (const uint8_t *)hello, jw.len, (const uint8_t *)hh, (size_t)len);
	/* (b) root */
	if (enroll) {
		uint8_t fh[32];
		mt_sha256(hub_pub, 32, fh);
		if (!mt_ct_equal(fh, inv_fp_hash, 32)) {
			logf_(w, MT_LOG_WARN, "handshake: the Hub is not the one named by the invitation key%s", NULL, 0);
			goto fail;
		}
		memcpy(pin_now.pub, hub_pub, 32);
		pin_now.seq = 0;
	} else {
		mt_pin pin;
		memcpy(pin.pub, w->st.root, 32);
		pin.seq = w->st.pin_seq;
		r = mt_resolve_pin(&pin, hub_pub, chain, nchain, &pin_now);
		if (r) {
			logf_(w, MT_LOG_WARN, "handshake: %s (the Hub root does not match the pinned one)", mt_err_reason(r), 0);
			goto fail;
		}
	}
	/* (c) certificate signed by that root */
	if (!mt_ct_equal(w->cert.root_pub, hub_pub, 32)) {
		logf_(w, MT_LOG_WARN, "handshake: %s", mt_err_reason(MT_E_ROOT_MISMATCH), 0);
		goto fail;
	}
	r = mt_cert_verify(&w->cert, pin_now.pub);
	if (r) {
		logf_(w, MT_LOG_WARN, "handshake: %s", mt_err_reason(r), 0);
		goto fail;
	}
	/* (d) window, with a monotonic deadline fixed now */
	{
		int64_t now = w->p->now_ms(w->p->ctx);
		r = mt_cert_window(&w->cert, now);
		if (r) {
			logf_(w, MT_LOG_WARN, "handshake: %s (check the device clock)", mt_err_reason(r), 0);
			goto fail;
		}
		w->deadline_mono = mono(w) + (w->cert.not_after_ms - now);
	}
	/* (e) serial verdict: computed now, enforced after the Hub signature */
	{
		mt_ratchet scope;
		memset(&scope, 0, sizeof scope);
		if (!enroll && !memcmp(w->st.root, pin_now.pub, 32)) { /* a new root resets the scope */
			scope.max_serial = w->st.max_serial;
			memcpy(scope.cert_sha256, w->st.cert_sha, 32);
			scope.has_hash = w->st.has_hash;
		}
		r = mt_ratchet_check(&scope, &w->cert);
		/* (f) sig_hub by the operational key */
		len = hs_recv(w, type, sizeof type, t, &nt);
		hh = (char *)w->rx + 4;
		if (len < 0 || strcmp(type, "sig") || get_b64(hh, t, nt, 0, "sig", sig, 64) ||
		    mt_verify(sig, w->cert.op_pub, "hs-hub", w->transcript, 32) != MT_OK) {
			logf_(w, MT_LOG_WARN, "handshake: Hub signature missing or invalid%s", NULL, 0);
			goto fail;
		}
		if (r) { /* (g) close without sig_worker */
			logf_(w, MT_LOG_WARN, "handshake: %s", mt_err_reason(r), 0);
			goto fail;
		}
		mt_ratchet_raise(&scope, &w->cert);
		/* remember what to commit; committed before any op (connect) or after welcome (enroll) */
		memcpy(w->op_pub, w->cert.op_pub, 32);
		memcpy(w->st.root, pin_now.pub, 32);
		w->st.pin_seq = pin_now.seq;
		w->st.max_serial = scope.max_serial;
		memcpy(w->st.cert_sha, scope.cert_sha256, 32);
		w->st.has_hash = (uint8_t)scope.has_hash;
	}
	if (mono(w) >= w->deadline_mono) {
		logf_(w, MT_LOG_WARN, "handshake: certificate expired during the handshake%s", NULL, 0);
		goto fail;
	}
	/* (h) sig_worker, then keys */
	{
		char sf[160];
		mt_jw j2;
		mt_sign(sig, w->seed, "hs-worker", w->transcript, 32);
		mt_jw_init(&j2, sf, sizeof sf);
		mt_jw_obj(&j2);
		mt_jw_kstr(&j2, "type", "sig");
		mt_jw_kb64(&j2, "sig", sig, 64);
		mt_jw_obj_end(&j2);
		if (j2.err || hs_send(w, sf, j2.len) != 0)
			goto fail;
	}
	if (mt_x25519(dh, eph, ephh) != 0) {
		logf_(w, MT_LOG_WARN, "handshake: invalid Hub ephemeral key%s", NULL, 0);
		goto fail;
	}
	mt_derive_keys(w->w2h.key, w->h2w.key, w->transcript, dh, NULL, 0);
	w->w2h.counter = w->h2w.counter = 0;
	mt_wipe(eph, sizeof eph);
	mt_wipe(dh, sizeof dh);
	return 0;
fail:
	mt_wipe(eph, sizeof eph);
	return -1;
}

/* ---------------------------------------------------------------- channel output */

static int out_flush(mt_worker *w)
{
	int n;
	if (!w->rec_len)
		return 0;
	n = mt_rec_seal(&w->w2h, w->wire, MT_REC_MAX_PLAIN + 20, w->rec, w->rec_len);
	w->rec_len = 0;
	if (n < 0)
		return -1;
	return mt_ws_send(&w->ws, w->wire, (size_t)n);
}

static int out_write(mt_worker *w, const uint8_t *d, size_t n)
{
	while (n) {
		size_t take = MT_REC_MAX_PLAIN - w->rec_len;
		if (take > n)
			take = n;
		memcpy(w->rec + w->rec_len, d, take);
		w->rec_len += take;
		d += take;
		n -= take;
		if (w->rec_len == MT_REC_MAX_PLAIN && out_flush(w) != 0)
			return -1;
	}
	return 0;
}

static int ctl_send(mt_worker *w, const char *json, size_t n)
{
	uint8_t h[4];
	put_be32(h, (uint32_t)n);
	if (out_write(w, h, 4) || out_write(w, (const uint8_t *)json, n))
		return -1;
	return out_flush(w);
}

/* yamux */
enum { Y_DATA = 0, Y_WIN = 1, Y_PING = 2, Y_GOAWAY = 3 };
enum { F_SYN = 1, F_ACK = 2, F_FIN = 4, F_RST = 8 };

static int y_hdr(mt_worker *w, uint8_t type, uint16_t flags, uint32_t sid, uint32_t len)
{
	uint8_t h[12];
	h[0] = 0;
	h[1] = type;
	h[2] = (uint8_t)(flags >> 8);
	h[3] = (uint8_t)flags;
	put_be32(h + 4, sid);
	put_be32(h + 8, len);
	return out_write(w, h, 12);
}

static stream *s_find(mt_worker *w, uint32_t id)
{
	int i;
	for (i = 0; i < w->cfg.max_streams; i++)
		if (w->streams[i].state == ST_OPEN && w->streams[i].id == id)
			return &w->streams[i];
	return NULL;
}

static stream *s_alloc(mt_worker *w, uint32_t id, int local)
{
	int i;
	for (i = 0; i < w->cfg.max_streams; i++)
		if (w->streams[i].state == ST_FREE) {
			stream *s = &w->streams[i];
			uint8_t *in = s->in;
			memset(s, 0, sizeof *s);
			s->in = in;
			s->id = id;
			s->state = ST_OPEN;
			s->local = local;
			s->send_window = YAMUX_WINDOW;
			s->opened_mono = mono(w);
			return s;
		}
	return NULL;
}

static void s_free(mt_worker *w, stream *s)
{
	if (s == w->ev)
		w->ev = NULL;
	if (s == w->ycur)
		w->ycur = NULL;
	s->state = ST_FREE;
}

static void s_reset(mt_worker *w, stream *s)
{
	y_hdr(w, Y_WIN, F_RST, s->id, 0);
	s_free(w, s);
}

/* Write a stream payload honouring the peer's window; a reply that does not fit is refused (reset). */
static int s_send(mt_worker *w, stream *s, const uint8_t *d, size_t n, int fin)
{
	if (n > s->send_window) {
		logf_(w, MT_LOG_WARN, "stream: reply larger than the peer window (%s%ld bytes)", "", (long)n);
		s_reset(w, s);
		return -1;
	}
	while (n) {
		uint32_t take = n > 16000 ? 16000 : (uint32_t)n;
		if (y_hdr(w, Y_DATA, (uint16_t)(fin && take == n ? F_FIN : 0), s->id, take) || out_write(w, d, take))
			return -1;
		s->send_window -= take;
		d += take;
		n -= take;
		if (fin && n == 0)
			return 0;
	}
	if (fin)
		return y_hdr(w, Y_DATA, F_FIN, s->id, 0);
	return 0;
}

static int s_frame(mt_worker *w, stream *s, const char *json, size_t n, int fin)
{
	uint8_t h[4];
	put_be32(h, (uint32_t)n);
	if (n + 4 > s->send_window) {
		s_reset(w, s);
		return -1;
	}
	if (s_send(w, s, h, 4, 0))
		return -1;
	return s_send(w, s, (const uint8_t *)json, n, fin);
}

/* ---------------------------------------------------------------- sessions, share */

static int shared(mt_worker *w, const mt_session *se)
{
	int i;
	if (w->share_mode == 0)
		return 1;
	if (w->share_mode != 2)
		return 0;
	for (i = 0; i < w->share_n; i++)
		if (!strcmp(w->share_names[i], se->name))
			return 1;
	return 0;
}

static mt_session *find_session(mt_worker *w, const char *name)
{
	size_t i;
	for (i = 0; i < w->cfg.nsessions; i++)
		if (!strcmp(w->cfg.sessions[i]->name, name))
			return w->cfg.sessions[i];
	return NULL;
}

static void session_hash(mt_session *se)
{
	uint32_t g = mt_vt_generation(se->vt);
	if (g != se->seen_gen || !se->hash[0]) {
		mt_vt_hash(se->vt, se->hash);
		se->seen_gen = g;
	}
}

static void pane_json(mt_worker *w, mt_jw *j, const mt_session *se)
{
	(void)w;
	mt_jw_obj(j);
	mt_jw_kstr(j, "session", se->name);
	mt_jw_kstr(j, "id", "%0");
	mt_jw_kint(j, "window", 0);
	mt_jw_kint(j, "index", 0);
	if (se->command)
		mt_jw_kstr(j, "command", se->command);
	mt_jw_kstr(j, "kind", se->kind ? se->kind : "other");
	mt_jw_kstr(j, "state", "unknown");
	if (se->created_ms > 0)
		mt_jw_kint(j, "session_created", se->created_ms / 1000);
	mt_jw_obj_end(j);
}

static void share_rule_json(mt_worker *w, mt_jw *j)
{
	int i;
	mt_jw_obj(j);
	mt_jw_kstr(j, "mode", MODES[w->share_mode]);
	if (w->share_mode == 2) {
		mt_jw_key(j, "names");
		mt_jw_arr(j);
		for (i = 0; i < w->share_n; i++)
			mt_jw_str(j, w->share_names[i]);
		mt_jw_arr_end(j);
	}
	mt_jw_obj_end(j);
}

/* ---------------------------------------------------------------- events */

static void ev_session_json(mt_worker *w, mt_jw *j, mt_session *se)
{
	(void)w;
	session_hash(se);
	mt_jw_obj(j);
	mt_jw_kstr(j, "name", se->name);
	mt_jw_kstr(j, "kind", se->kind ? se->kind : "other");
	mt_jw_kstr(j, "state", "unknown");
	mt_jw_kstr(j, "screen_hash", se->hash);
	mt_jw_obj_end(j);
}

static int ev_send(mt_worker *w, const char *json, size_t n)
{
	if (!w->ev)
		return -1;
	if (s_frame(w, w->ev, json, n, 0)) {
		w->ev = NULL;
		return -1;
	}
	return 0;
}

static void ev_open(mt_worker *w)
{
	stream *s;
	mt_jw j;
	char ts[25];
	size_t i;
	uint32_t id = w->next_local_id;
	if (w->ev || w->pending || mono(w) < w->ev_retry_mono)
		return;
	s = s_alloc(w, id, 1);
	if (!s)
		return;
	w->next_local_id += 2;
	s->kind = K_EV;
	w->ev = s;
	w->ev_seq = 0;
	y_hdr(w, Y_WIN, F_SYN, id, 0);
	s_send(w, s, (const uint8_t *)"ev", 2, 0);
	ts_fmt(ts, w->p->now_ms(w->p->ctx));
	mt_jw_init(&j, (char *)w->out, w->cfg.out_cap);
	mt_jw_obj(&j);
	mt_jw_kstr(&j, "type", "events.snapshot");
	mt_jw_kint(&j, "seq", 0);
	mt_jw_kstr(&j, "time", ts);
	mt_jw_key(&j, "sessions");
	mt_jw_arr(&j);
	for (i = 0; i < w->cfg.nsessions; i++)
		if (shared(w, w->cfg.sessions[i]))
			ev_session_json(w, &j, w->cfg.sessions[i]);
	mt_jw_arr_end(&j);
	mt_jw_key(&j, "share");
	mt_jw_obj(&j);
	mt_jw_key(&j, "rule");
	share_rule_json(w, &j);
	mt_jw_kint(&j, "revision", (int64_t)w->share_rev);
	mt_jw_obj_end(&j);
	mt_jw_obj_end(&j);
	if (j.err || ev_send(w, (const char *)w->out, j.len))
		logf_(w, MT_LOG_WARN, "events: cannot send the snapshot%s", NULL, 0);
}

static void ev_tick(mt_worker *w)
{
	size_t i;
	int64_t now = mono(w);
	if (!w->ev)
		return;
	for (i = 0; i < w->cfg.nsessions; i++) {
		mt_session *se = w->cfg.sessions[i];
		uint32_t g = mt_vt_generation(se->vt);
		if (g == se->seen_gen || !shared(w, se))
			continue;
		if (!se->dirty_mono) {
			se->dirty_mono = now;
			continue;
		}
		if (now - se->dirty_mono < EV_DEBOUNCE_MS)
			continue;
		se->dirty_mono = 0;
		session_hash(se);
		{
			mt_jw j;
			char ts[25];
			ts_fmt(ts, w->p->now_ms(w->p->ctx));
			mt_jw_init(&j, (char *)w->out, w->cfg.out_cap);
			mt_jw_obj(&j);
			mt_jw_kstr(&j, "type", "event");
			mt_jw_kint(&j, "seq", (int64_t)++w->ev_seq);
			mt_jw_kstr(&j, "time", ts);
			mt_jw_kstr(&j, "event", "screen_changed");
			mt_jw_kstr(&j, "session", se->name);
			mt_jw_kstr(&j, "screen_hash", se->hash);
			mt_jw_obj_end(&j);
			if (!j.err)
				ev_send(w, (const char *)w->out, j.len);
		}
	}
}

/* Share changed: a fresh snapshot is the simplest correct event stream. */
static void ev_restart(mt_worker *w)
{
	if (w->ev) {
		y_hdr(w, Y_DATA, F_FIN, w->ev->id, 0);
		s_free(w, w->ev);
	}
	w->ev_retry_mono = 0;
	ev_open(w);
}

/* ---------------------------------------------------------------- op server */

static void op_error(mt_worker *w, stream *s, const char *code, const char *msg)
{
	char b[256];
	mt_jw j;
	mt_jw_init(&j, b, sizeof b);
	mt_jw_obj(&j);
	mt_jw_kstr(&j, "type", "error");
	mt_jw_kstr(&j, "code", code);
	mt_jw_kstr(&j, "message", msg);
	mt_jw_key(&j, "details");
	mt_jw_obj(&j);
	mt_jw_obj_end(&j);
	mt_jw_obj_end(&j);
	s_frame(w, s, b, j.len, 1);
}

static int target_of(const char *js, const mt_jtok *t, int nt, int args, char *wid, char *sess)
{
	int tg = mt_json_get(js, t, nt, args, "target");
	if (tg < 0)
		return -1;
	if (mt_json_str(js, t, mt_json_get(js, t, nt, tg, "worker_id"), wid, 40) < 0)
		return -1;
	if (mt_json_str(js, t, mt_json_get(js, t, nt, tg, "session_name"), sess, 33) <= 0)
		return -1;
	return 0;
}

static int64_t uuid7_ms(const char *id)
{
	uint8_t b[6];
	char h[12];
	int i, k = 0;
	for (i = 0; id[i] && k < 12; i++)
		if (id[i] != '-')
			h[k++] = id[i];
	if (k != 12 || mt_hex_decode(b, sizeof b, h, 12) != 6)
		return -1;
	return (int64_t)((uint64_t)b[0] << 40 | (uint64_t)b[1] << 32 | (uint64_t)b[2] << 24 | (uint64_t)b[3] << 16 |
	                 (uint64_t)b[4] << 8 | b[5]);
}

static const char *cached_result(mt_worker *w, const char *id)
{
	int i;
	for (i = 0; i < w->cfg.results_ring; i++)
		if (w->results[i].id[0] && !strcmp(w->results[i].id, id) &&
		    mono(w) - w->results[i].mono < RESULT_TTL_MS)
			return w->results[i].result;
	return NULL;
}

static void remember(mt_worker *w, const char *id, const char *res)
{
	cached *c = &w->results[w->nresults++ % w->cfg.results_ring];
	if (c->id[0]) { /* evicting: an id at or before the evicted one can no longer be recognised */
		int64_t ev = uuid7_ms(c->id);
		if (ev > w->replay_floor_ms)
			w->replay_floor_ms = ev;
	}
	snprintf(c->id, sizeof c->id, "%s", id);
	snprintf(c->result, sizeof c->result, "%s", res);
	c->mono = mono(w);
}

static void write_result(mt_worker *w, stream *s, const char *rid, const char *outcome, const char *reason,
                         const mt_session *se)
{
	char res[160], frame[512];
	mt_jw j;
	mt_jw_init(&j, res, sizeof res);
	mt_jw_obj(&j);
	mt_jw_kstr(&j, "outcome", outcome);
	mt_jw_kstr(&j, "reason", reason);
	mt_jw_kbool(&j, "forceable", 0);
	mt_jw_kbool(&j, "forced", 0);
	mt_jw_key(&j, "warnings");
	mt_jw_arr(&j);
	mt_jw_arr_end(&j);
	mt_jw_key(&j, "pane");
	mt_jw_null(&j);
	mt_jw_obj_end(&j);
	if (rid && rid[0] && !j.err)
		remember(w, rid, res);
	(void)se;
	mt_jw_init(&j, frame, sizeof frame);
	mt_jw_obj(&j);
	mt_jw_kstr(&j, "type", "op.result");
	mt_jw_key(&j, "result");
	mt_jw_raw(&j, res, strlen(res));
	mt_jw_obj_end(&j);
	s_frame(w, s, frame, j.len, 1);
}

static void op_request(mt_worker *w, stream *s, const char *js, mt_jtok *t, int nt)
{
	char op[32] = "", rid[40] = "", wid[40], sess[33];
	int args = mt_json_get(js, t, nt, 0, "args");
	mt_session *se = NULL;
	mt_jw j;
	if (mt_json_str(js, t, mt_json_get(js, t, nt, 0, "op"), op, sizeof op) <= 0) {
		op_error(w, s, "invalid_argument", "missing op");
		return;
	}
	if (args >= 0 && t[args].type != MT_JT_OBJ)
		args = -1;
	if (args >= 0)
		mt_json_str(js, t, mt_json_get(js, t, nt, args, "request_id"), rid, sizeof rid);
	mt_jw_init(&j, (char *)w->out, w->cfg.out_cap);

	if (!strcmp(op, "sessions_list")) {
		size_t i;
		char ts[25];
		mt_jw_obj(&j);
		mt_jw_kstr(&j, "type", "op.result");
		mt_jw_key(&j, "result");
		mt_jw_obj(&j);
		mt_jw_key(&j, "sessions");
		mt_jw_arr(&j);
		for (i = 0; i < w->cfg.nsessions; i++) {
			mt_session *x = w->cfg.sessions[i];
			if (!shared(w, x))
				continue;
			ts_fmt(ts, x->created_ms > 0 ? x->created_ms : w->p->now_ms(w->p->ctx));
			mt_jw_obj(&j);
			mt_jw_kstr(&j, "name", x->name);
			mt_jw_kstr(&j, "created", ts);
			mt_jw_kint(&j, "windows", 1);
			mt_jw_kbool(&j, "attached", 0);
			mt_jw_key(&j, "pane");
			pane_json(w, &j, x);
			mt_jw_obj_end(&j);
		}
		mt_jw_arr_end(&j);
		mt_jw_obj_end(&j);
		mt_jw_obj_end(&j);
		if (j.err)
			op_error(w, s, "internal", "result too large");
		else
			s_frame(w, s, (const char *)w->out, j.len, 1);
		return;
	}
	if (!strcmp(op, "share_get") || !strcmp(op, "share_set")) {
		size_t i;
		int wr = 0;
		if (!strcmp(op, "share_set")) {
			char mode[16] = "";
			int m;
			mt_json_str(js, t, mt_json_get(js, t, nt, args, "mode"), mode, sizeof mode);
			for (m = 0; m < 3; m++)
				if (!strcmp(mode, MODES[m]))
					break;
			if (m == 3) {
				op_error(w, s, "invalid_argument", "mode must be all, none or select");
				return;
			}
			w->share_mode = m;
			w->share_n = 0;
			if (m == 2) {
				int a = mt_json_get(js, t, nt, args, "names"), e, k;
				if (a < 0 || t[a].type != MT_JT_ARR || t[a].size > 8) {
					op_error(w, s, "invalid_argument", "names: up to 8 session names");
					return;
				}
				for (k = 0, e = a + 1; k < t[a].size; k++, e = mt_json_skip(t, nt, e))
					if (mt_json_str(js, t, e, w->share_names[w->share_n], 33) > 0)
						w->share_n++;
			}
			w->share_rev++;
			save_share(w);
			wr = 1;
		}
		mt_jw_obj(&j);
		mt_jw_kstr(&j, "type", "op.result");
		mt_jw_key(&j, "result");
		mt_jw_obj(&j);
		mt_jw_key(&j, "rule");
		share_rule_json(w, &j);
		mt_jw_key(&j, "unshared");
		mt_jw_arr(&j);
		for (i = 0; i < w->cfg.nsessions; i++)
			if (!shared(w, w->cfg.sessions[i]))
				mt_jw_str(&j, w->cfg.sessions[i]->name);
		mt_jw_arr_end(&j);
		{
			char rv[24];
			snprintf(rv, sizeof rv, "%llu", (unsigned long long)w->share_rev);
			mt_jw_kstr(&j, "revision", rv);
			snprintf(rv, sizeof rv, "%llu", (unsigned long long)w->op_barrier);
			mt_jw_kstr(&j, "op_barrier", rv);
		}
		mt_jw_obj_end(&j);
		mt_jw_obj_end(&j);
		s_frame(w, s, (const char *)w->out, j.len, 1);
		if (wr)
			ev_restart(w);
		return;
	}
	/* the rest names a target session */
	if (args < 0 || target_of(js, t, nt, args, wid, sess) || !(se = find_session(w, sess))) {
		op_error(w, s, "target_not_found", "no such session");
		return;
	}
	if (!shared(w, se)) {
		op_error(w, s, "not_shared", "the session is not shared");
		return;
	}
	if (!strcmp(op, "session_snapshot") || !strcmp(op, "session_capture")) {
		int64_t lines = 200;
		if (!strcmp(op, "session_capture")) {
			int li = mt_json_get(js, t, nt, args, "lines");
			if (li >= 0 && (mt_json_int(js, t, li, &lines) || lines < 0 || lines > 10000)) {
				op_error(w, s, "invalid_argument", "lines must be 0..10000");
				return;
			}
			if (lines == 0)
				lines = 200;
		}
		mt_jw_obj(&j);
		mt_jw_kstr(&j, "type", "op.result");
		mt_jw_key(&j, "result");
		mt_jw_obj(&j);
		mt_jw_key(&j, "pane");
		pane_json(w, &j, se);
		mt_jw_key(&j, "screen");
		if (!strcmp(op, "session_snapshot"))
			mt_vt_screen_json(se->vt, &j);
		else
			mt_vt_capture_json(se->vt, (int)lines, &j);
		mt_jw_obj_end(&j);
		mt_jw_obj_end(&j);
		if (j.err)
			op_error(w, s, "internal", "screen too large for this device");
		else
			s_frame(w, s, (const char *)w->out, j.len, 1);
		return;
	}
	if (!strcmp(op, "send_text") || !strcmp(op, "send_keys")) {
		const char *prev;
		int o = mt_json_get(js, t, nt, 0, "origin");
		if (!rid[0]) {
			op_error(w, s, "invalid_argument", "request_id is required");
			return;
		}
		if ((prev = cached_result(w, rid)) == NULL && uuid7_ms(rid) <= w->replay_floor_ms) {
			op_error(w, s, "conflict",
			         "request_id is older than this Worker's replay window; look at the session and send a new request");
			return;
		}
		if (prev != NULL) {
			char frame[512];
			mt_jw f;
			mt_jw_init(&f, frame, sizeof frame);
			mt_jw_obj(&f);
			mt_jw_kstr(&f, "type", "op.result");
			mt_jw_key(&f, "result");
			mt_jw_raw(&f, prev, strlen(prev));
			mt_jw_obj_end(&f);
			s_frame(w, s, frame, f.len, 1);
			return;
		}
		if (o < 0 || t[o].type != MT_JT_OBJ || mt_json_get(js, t, nt, o, "kind") < 0) {
			op_error(w, s, "pipe_origin_refused", "a write needs its origin");
			return;
		}
		if (w->st.preview_only) {
			op_error(w, s, "preview_only", "this Worker is preview-only");
			return;
		}
		if (se->readonly || !se->write) {
			write_result(w, s, rid, "failed_not_sent", "read_only_session", se);
			return;
		}
		if (!strcmp(op, "send_text")) {
			static char text[4096];
			int submit = 0, tl = mt_json_str(js, t, mt_json_get(js, t, nt, args, "text"), text, sizeof text);
			mt_json_bool(js, t, mt_json_get(js, t, nt, args, "submit"), &submit);
			if (tl < 0) {
				op_error(w, s, "message_too_large", "text too long for this device");
				return;
			}
			if (se->write(se, (const uint8_t *)text, (size_t)tl) != 0 ||
			    (submit && se->write(se, (const uint8_t *)"\r", 1) != 0)) {
				write_result(w, s, rid, "failed_not_sent", "session_write_failed", se);
				return;
			}
		} else {
			int a = mt_json_get(js, t, nt, args, "keys"), e, k;
			uint8_t bytes[64];
			char name[40];
			if (a < 0 || t[a].type != MT_JT_ARR || t[a].size < 1 || t[a].size > 256) {
				op_error(w, s, "invalid_argument", "keys: 1..256 key names");
				return;
			}
			for (k = 0, e = a + 1; k < t[a].size; k++, e = mt_json_skip(t, nt, e))
				if (mt_json_str(js, t, e, name, sizeof name) <= 0 || mt_key_bytes(name, strlen(name), bytes, sizeof bytes) < 0) {
					op_error(w, s, "invalid_argument", "unknown key name");
					return;
				}
			for (k = 0, e = a + 1; k < t[a].size; k++, e = mt_json_skip(t, nt, e)) {
				int bl;
				mt_json_str(js, t, e, name, sizeof name);
				bl = mt_key_bytes(name, strlen(name), bytes, sizeof bytes);
				if (se->write(se, bytes, (size_t)bl) != 0) {
					write_result(w, s, rid, "partial", "session_write_failed", se);
					return;
				}
			}
		}
		write_result(w, s, rid, "delivered", "", se);
		return;
	}
	op_error(w, s, "unsupported_op", "not supported by this Worker");
}

static void share_probe(mt_worker *w, stream *s, const char *js, mt_jtok *t, int nt)
{
	mt_jw j;
	int64_t b = 0;
	char rv[24];
	if (mt_json_int(js, t, mt_json_get(js, t, nt, 0, "op_barrier"), &b) == 0 && b > 0 && (uint64_t)b > w->op_barrier)
		w->op_barrier = (uint64_t)b;
	mt_jw_init(&j, (char *)w->out, w->cfg.out_cap);
	mt_jw_obj(&j);
	mt_jw_kstr(&j, "type", "op.result");
	mt_jw_key(&j, "result");
	mt_jw_obj(&j);
	mt_jw_key(&j, "rule");
	share_rule_json(w, &j);
	snprintf(rv, sizeof rv, "%llu", (unsigned long long)w->share_rev);
	mt_jw_kstr(&j, "revision", rv);
	snprintf(rv, sizeof rv, "%llu", (unsigned long long)w->op_barrier);
	mt_jw_kstr(&j, "op_barrier", rv);
	mt_jw_obj_end(&j);
	mt_jw_obj_end(&j);
	s_frame(w, s, (const char *)w->out, j.len, 1);
}

static int cert_expired(mt_worker *w)
{
	return mono(w) >= w->deadline_mono || w->p->now_ms(w->p->ctx) > w->cert.not_after_ms;
}

/* A live revoke: the only frame of an op stream the Hub opens, signed over this connection's transcript. */
static int live_revoke(mt_worker *w, stream *s, const char *js, mt_jtok *t, int nt)
{
	uint8_t sig[64];
	static const char ack[] = "{\"type\":\"op.result\",\"op\":\"revoke\"}";
	if (get_b64(js, t, nt, 0, "sig", sig, 64) || mt_verify(sig, w->op_pub, "revoke", w->transcript, 32) != MT_OK ||
	    cert_expired(w)) {
		logf_(w, MT_LOG_WARN, "ignored an unverifiable revoke%s", NULL, 0);
		s_reset(w, s);
		return 0;
	}
	logf_(w, MT_LOG_WARN, "the Hub revoked this Worker; enrollment and key erased%s", NULL, 0);
	memset(&w->st, 0, sizeof w->st);
	w->p->store_set(w->p->ctx, MT_KEY_STATE, NULL, 0);
	w->p->store_set(w->p->ctx, MT_KEY_SEED, NULL, 0);
	w->revoked = 1;
	s_frame(w, s, ack, sizeof ack - 1, 1);
	out_flush(w);
	return -2;
}

/* A complete frame arrived on stream s. Returns -2 after a valid revoke. */
static int stream_frame(mt_worker *w, stream *s, char *js, size_t n)
{
	static mt_jtok t[MAX_TOKENS];
	char type[24] = "";
	int nt = mt_json_parse(js, n, t, MAX_TOKENS);
	if (nt <= 0 || t[0].type != MT_JT_OBJ ||
	    mt_json_str(js, t, mt_json_get(js, t, nt, 0, "type"), type, sizeof type) <= 0) {
		s_reset(w, s);
		return 0;
	}
	if (s->kind == K_OP && !s->handled) {
		s->handled = 1;
		if (!strcmp(type, "revoke"))
			return live_revoke(w, s, js, t, nt);
		if (cert_expired(w)) { /* no operation is admitted after the certificate's end */
			s_reset(w, s);
			w->connected = -1;
			return 0;
		}
		if (!strcmp(type, "op.request"))
			op_request(w, s, js, t, nt);
		else if (!strcmp(type, "share.probe"))
			share_probe(w, s, js, t, nt);
		else
			op_error(w, s, "unsupported_op", "unknown request");
	}
	/* frames on the ev stream from the Hub, or extra frames: ignored */
	return 0;
}

/* Stream payload bytes, as they arrive. Returns -2 after a valid revoke. */
static int stream_data(mt_worker *w, stream *s, const uint8_t *d, size_t n)
{
	while (n && s->state == ST_OPEN) {
		if (s->discard) {
			size_t skip = n < s->discard ? n : s->discard;
			s->discard -= (uint32_t)skip;
			d += skip;
			n -= skip;
			continue;
		}
		if (s->kind == K_NONE) {
			s->kbuf[s->klen++] = *d++;
			n--;
			if (s->klen == 2) {
				if (!s->local && s->kbuf[0] == 'o' && s->kbuf[1] == 'p')
					s->kind = K_OP;
				else {
					s_reset(w, s);
					return 0;
				}
			}
			continue;
		}
		{
			size_t take, need;
			if (s->in_len < 4) {
				take = 4 - s->in_len;
			} else {
				uint32_t fl = be32(s->in);
				if (fl > w->cfg.frame_cap - 4) {
					if (s->kind == K_OP && !s->handled) {
						s->handled = 1;
						op_error(w, s, "message_too_large", "request too large for this device");
					}
					s->discard = fl;
					s->in_len = 0;
					continue;
				}
				need = 4 + fl;
				take = need - s->in_len;
			}
			if (take > n)
				take = n;
			memcpy(s->in + s->in_len, d, take);
			s->in_len += take;
			d += take;
			n -= take;
			if (s->in_len >= 4 && s->in_len == 4 + be32(s->in)) {
				int r = stream_frame(w, s, (char *)s->in + 4, s->in_len - 4);
				s->in_len = 0;
				if (r)
					return r;
			}
		}
	}
	return 0;
}

/* ---------------------------------------------------------------- inbound plaintext: control frame, then yamux */

static int on_control(mt_worker *w, int enroll)
{
	static mt_jtok t[64];
	char type[24] = "", *js = (char *)w->ctl_buf;
	int nt = mt_json_parse(js, w->ctl_len, t, 64);
	if (nt <= 0 || mt_json_str(js, t, mt_json_get(js, t, nt, 0, "type"), type, sizeof type) <= 0)
		return -1;
	if (!strcmp(type, "welcome")) {
		int pend = 0, pi;
		char wid[40] = "";
		mt_json_bool(js, t, mt_json_get(js, t, nt, 0, "pending"), &pend);
		mt_json_str(js, t, mt_json_get(js, t, nt, 0, "worker_id"), wid, sizeof wid);
		pi = mt_json_get(js, t, nt, 0, "policy");
		if (pi >= 0) {
			int po = 0;
			mt_json_bool(js, t, mt_json_get(js, t, nt, pi, "preview_only"), &po);
			w->st.preview_only = (uint8_t)po;
		}
		w->pending = pend;
		if (wid[0])
			snprintf(w->st.worker_id, sizeof w->st.worker_id, "%s", wid);
		w->st.enrolled = 1;
		save_state(w); /* ratchet and pin committed before any op is admitted */
		if (enroll) {
			w->p->store_set(w->p->ctx, MT_KEY_INVITE, NULL, 0);
			logf_(w, MT_LOG_INFO, "enrolled as %s; the invitation key was erased", w->st.worker_id, 0);
		}
		if (pend)
			logf_(w, MT_LOG_WARN,
			      "this Worker is waiting for approval: approve %s in the Hub (Workers page)", w->st.worker_id, 0);
		else
			logf_(w, MT_LOG_INFO, "connected as %s", w->st.worker_id, 0);
		return 0;
	}
	if (!strcmp(type, "revoke")) {
		uint8_t sig[64];
		if (get_b64(js, t, nt, 0, "sig", sig, 64) == 0 &&
		    mt_verify(sig, w->op_pub, "revoke", w->transcript, 32) == MT_OK &&
		    mono(w) < w->deadline_mono) {
			logf_(w, MT_LOG_WARN, "the Hub revoked this Worker; enrollment and key erased%s", NULL, 0);
			memset(&w->st, 0, sizeof w->st);
			w->p->store_set(w->p->ctx, MT_KEY_STATE, NULL, 0);
			w->p->store_set(w->p->ctx, MT_KEY_SEED, NULL, 0);
			w->revoked = 1;
			return -2;
		}
		logf_(w, MT_LOG_WARN, "ignored an unverifiable revoke%s", NULL, 0);
		return -1;
	}
	if (!strcmp(type, "not_enrolled")) {
		logf_(w, MT_LOG_WARN, "the Hub does not know this Worker key (not_enrolled)%s", NULL, 0);
		return -1;
	}
	{
		char code[48] = "";
		mt_json_str(js, t, mt_json_get(js, t, nt, 0, "code"), code, sizeof code);
		logf_(w, MT_LOG_WARN, "the Hub refused: %s", code[0] ? code : type, 0);
	}
	return -1;
}

static void y_frame(mt_worker *w)
{
	uint8_t type = w->yhdr[1];
	uint16_t flags = (uint16_t)(w->yhdr[2] << 8 | w->yhdr[3]);
	uint32_t sid = be32(w->yhdr + 4), len = be32(w->yhdr + 8);
	stream *s = NULL;
	w->ycur = NULL;
	w->ypay_left = 0;
	if (w->yhdr[0] != 0) {
		w->connected = -1;
		return;
	}
	if (type == Y_PING) {
		if (flags & F_SYN) {
			y_hdr(w, Y_PING, F_ACK, 0, len);
		} else if (flags & F_ACK) {
			w->ping_outstanding = 0;
		}
		return;
	}
	if (type == Y_GOAWAY) {
		logf_(w, MT_LOG_INFO, "the Hub closed the session (go away %s%ld)", "", (long)len);
		w->connected = -1;
		return;
	}
	if (type != Y_DATA && type != Y_WIN) {
		w->connected = -1;
		return;
	}
	if (flags & F_SYN) {
		if ((sid & 1) || s_find(w, sid)) { /* the Hub opens even ids */
			w->connected = -1;
			return;
		}
		s = s_alloc(w, sid, 0);
		if (!s) {
			y_hdr(w, Y_WIN, F_ACK | F_RST, sid, 0); /* an unacknowledged stream makes the Hub drop the session */
		} else {
			y_hdr(w, Y_WIN, F_ACK, sid, 0);
		}
	} else {
		s = s_find(w, sid);
	}
	if (type == Y_WIN) {
		if (s)
			s->send_window += len;
	}
	if (flags & F_RST) {
		if (s)
			s_free(w, s);
		s = NULL;
	}
	if (type == Y_DATA) {
		w->ypay_left = len;
		w->ycur = s;
	}
	if (s && (flags & F_FIN)) {
		s->fin_rx = 1;
		if (type == Y_WIN || len == 0) {
			if (s->kind != K_EV && s->handled)
				s_free(w, s);
			else if (s->kind == K_EV)
				s_free(w, s), w->ev_retry_mono = mono(w) + EV_REOPEN_MS;
		}
	}
}

static int plain_in(mt_worker *w, const uint8_t *d, size_t n, int enroll)
{
	while (n) {
		if (!w->ctl_done) {
			size_t take;
			if (w->ctl_hdr_len < 4) {
				w->ctl_hdr[w->ctl_hdr_len++] = *d++;
				n--;
				if (w->ctl_hdr_len == 4) {
					w->ctl_need = be32(w->ctl_hdr);
					w->ctl_len = 0;
					if (w->ctl_need == 0 || w->ctl_need > 4095)
						return -1;
				}
				continue;
			}
			take = w->ctl_need - w->ctl_len;
			if (take > n)
				take = n;
			memcpy(w->ctl_buf + w->ctl_len, d, take);
			w->ctl_len += take;
			d += take;
			n -= take;
			if (w->ctl_len == w->ctl_need) {
				int r = on_control(w, enroll);
				if (r)
					return r;
				w->ctl_done = 1;
			}
			continue;
		}
		if (w->ypay_left) {
			size_t take = n < w->ypay_left ? n : w->ypay_left;
			stream *s = w->ycur;
			if (s && s->state == ST_OPEN) {
				uint32_t sid = s->id;
				if (stream_data(w, s, d, take) == -2)
					return -2;
				if (s->state == ST_OPEN && s->id == sid)
					y_hdr(w, Y_WIN, 0, sid, (uint32_t)take); /* credit back at once: nothing is buffered */
			}
			w->ypay_left -= (uint32_t)take;
			d += take;
			n -= take;
			if (!w->ypay_left && s && s->state == ST_OPEN && s->fin_rx && s->kind == K_OP && s->handled)
				s_free(w, s);
			continue;
		}
		w->yhdr[w->yhdr_len++] = *d++;
		n--;
		if (w->yhdr_len == 12) {
			w->yhdr_len = 0;
			y_frame(w);
			if (w->connected < 0)
				return -1;
		}
	}
	return 0;
}

/* ---------------------------------------------------------------- connection lifetime */

static int send_invite(mt_worker *w, const char *secret)
{
	char b[160];
	mt_jw j;
	mt_jw_init(&j, b, sizeof b);
	mt_jw_obj(&j);
	mt_jw_kstr(&j, "type", "invite.secret");
	mt_jw_kstr(&j, "secret", secret);
	mt_jw_obj_end(&j);
	return j.err ? -1 : ctl_send(w, b, j.len);
}

static void reset_conn(mt_worker *w)
{
	int i;
	w->connected = 0;
	w->pending = 0;
	w->ctl_done = 0;
	w->ctl_hdr_len = 0;
	w->yhdr_len = 0;
	w->ypay_left = 0;
	w->ycur = NULL;
	w->ev = NULL;
	w->rec_len = 0;
	w->ping_outstanding = 0;
	w->next_local_id = 1;
	for (i = 0; i < w->cfg.max_streams; i++)
		w->streams[i].state = ST_FREE;
	for (i = 0; i < (int)w->cfg.nsessions; i++)
		w->cfg.sessions[i]->dirty_mono = 0;
	mt_wipe(&w->w2h, sizeof w->w2h);
	mt_wipe(&w->h2w, sizeof w->h2w);
}

/* One connection: returns 0 after a clean end, <0 on failure, -2 after a valid revoke. */
static int run_once(mt_worker *w)
{
	char invite[160] = "", secret[64] = "";
	uint8_t fphash[32];
	int enroll = 0, r;
	reset_conn(w);
	if (!w->st.enrolled) {
		int n = w->p->store_get(w->p->ctx, MT_KEY_INVITE, (uint8_t *)invite, sizeof invite - 1);
		const char *dot;
		if (n <= 0)
			return -3; /* nothing to do until an invitation key is given */
		invite[n] = 0;
		while (n > 0 && (invite[n - 1] == '\n' || invite[n - 1] == ' ' || invite[n - 1] == '\r'))
			invite[--n] = 0;
		dot = strchr(invite, '.');
		if (strncmp(invite, "mnk_", 4) || !dot || dot - invite != 4 + 52 || strlen(dot + 1) != 52 ||
		    mt_b32_decode(fphash, sizeof fphash, invite + 4, 52) != 32) {
			logf_(w, MT_LOG_ERROR, "the invitation key is malformed (expected mnk_<52>.<52>)%s", NULL, 0);
			return -3;
		}
		snprintf(secret, sizeof secret, "%s", dot + 1);
		enroll = 1;
	}
	if (w->p->now_ms(w->p->ctx) < 1735689600000LL) { /* 2025-01-01: the clock is not set yet */
		logf_(w, MT_LOG_INFO, "waiting for the clock (NTP) before connecting%s", NULL, 0);
		return -1;
	}
	if (mt_ws_connect(&w->ws, &w->url, 15000) != 0) {
		if (w->ws.http_status)
			logf_(w, MT_LOG_WARN, "connect: HTTP %ld %s", w->ws.http_reason, (long)w->ws.http_status);
		else
			logf_(w, MT_LOG_WARN, "connect: cannot reach %s", w->url.host, 0);
		return -1;
	}
	if (handshake(w, enroll, fphash) != 0) {
		mt_ws_close(&w->ws);
		return -1;
	}
	if (enroll && send_invite(w, secret) != 0) {
		mt_ws_close(&w->ws);
		return -1;
	}
	mt_wipe(secret, sizeof secret);
	mt_wipe(invite, sizeof invite);
	w->connected = 1;
	if (!w->replay_floor_ms) /* requests issued before this boot are not recognisable any more */
		w->replay_floor_ms = w->p->now_ms(w->p->ctx) - 1;
	w->last_rx_mono = w->last_ping_mono = w->last_expiry_mono = mono(w);
	for (;;) {
		int n;
		int64_t now;
		if (w->stop)
			break;
		if (w->p->poll)
			w->p->poll(w->p->ctx);
		if (w->ctl_done) {
			ev_open(w);
			ev_tick(w);
		}
		if (out_flush(w) != 0)
			break;
		n = mt_ws_recv(&w->ws, 50);
		now = mono(w);
		if (n < 0) {
			logf_(w, MT_LOG_INFO, "connection closed (%s%ld)", "", (long)n);
			break;
		}
		if (n > 0) {
			w->last_rx_mono = now;
			r = mt_rec_open(&w->h2w, w->rx, (size_t)n);
			if (r < 0) {
				logf_(w, MT_LOG_WARN, "channel: %s", mt_err_reason(r), 0);
				break;
			}
			r = plain_in(w, w->rx + 4, (size_t)r, enroll);
			if (r == -2) {
				mt_ws_close(&w->ws);
				return -2;
			}
			if (r < 0)
				break;
		}
		if (w->ctl_done && now - w->last_ping_mono >= KEEPALIVE_MS && !w->ping_outstanding) {
			y_hdr(w, Y_PING, F_SYN, 0, ++w->ping_id);
			w->ping_outstanding = 1;
			w->last_ping_mono = now;
		}
		if (now - w->last_rx_mono > DEAD_AFTER_MS) {
			logf_(w, MT_LOG_WARN, "the Hub stopped answering%s", NULL, 0);
			break;
		}
		if (now - w->last_expiry_mono >= EXPIRY_TICK_MS || now >= w->deadline_mono) {
			w->last_expiry_mono = now;
			if (now >= w->deadline_mono || w->p->now_ms(w->p->ctx) > w->cert.not_after_ms) {
				logf_(w, MT_LOG_WARN, "the Hub certificate expired; reconnecting%s", NULL, 0);
				break;
			}
		}
		/* streams the Hub opened but never named within 10 s */
		{
			int i;
			for (i = 0; i < w->cfg.max_streams; i++) {
				stream *s = &w->streams[i];
				if (s->state == ST_OPEN && s->kind == K_NONE && now - s->opened_mono > KIND_TIMEOUT_MS)
					s_reset(w, s);
			}
		}
	}
	out_flush(w);
	mt_ws_close(&w->ws);
	reset_conn(w);
	return w->stop ? 0 : -1;
}

int mt_worker_run(mt_worker *w)
{
	int64_t base = 1000;
	while (!w->stop) {
		int64_t t0 = mono(w), wait;
		int r = run_once(w);
		uint8_t j[2];
		if (r == -2 && w->p->store_get(w->p->ctx, MT_KEY_INVITE, j, 1) <= 0)
			return -2;
		if (r == -3) { /* not enrolled and no invitation key: look again later */
			w->p->sleep_ms(w->p->ctx, 5000);
			continue;
		}
		if (mono(w) - t0 > 60000)
			base = 1000; /* the connection lived: start over */
		w->p->random(w->p->ctx, j, 2);
		wait = base / 2 + (int64_t)((j[0] << 8 | j[1]) % (uint32_t)(base / 2 + 1));
		w->p->sleep_ms(w->p->ctx, (int)wait);
		if (base < 60000)
			base *= 2;
		if (base > 60000)
			base = 60000;
	}
	return 0;
}
