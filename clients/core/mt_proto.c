#include "mt_proto.h"

#include <string.h>

#include "mt_crypto.h"

const char *mt_err_reason(int err)
{
	switch (err) {
	case MT_OK: return "ok";
	case MT_E_MALFORMED: return "cert_malformed";
	case MT_E_ROOT_MISMATCH: return "cert_root_mismatch";
	case MT_E_BAD_SIGNATURE: return "cert_bad_signature";
	case MT_E_NOT_YET_VALID: return "cert_not_yet_valid";
	case MT_E_EXPIRED: return "cert_expired";
	case MT_E_SERIAL_LOW: return "cert_serial_too_low";
	case MT_E_SERIAL_CONFLICT: return "cert_serial_conflict";
	case MT_E_CHAIN: return "chain_invalid";
	case MT_E_PIN_MISMATCH: return "pin_mismatch";
	case MT_E_PROTOCOL: return "protocol";
	case MT_E_AUTH: return "authentication_failed";
	case MT_E_LIMIT: return "limit";
	case MT_E_IO: return "io";
	default: return "error";
	}
}

static uint64_t be64(const uint8_t *p)
{
	uint64_t v = 0;
	int i;
	for (i = 0; i < 8; i++)
		v = v << 8 | p[i];
	return v;
}

static void put_be64(uint8_t *p, uint64_t v)
{
	int i;
	for (i = 7; i >= 0; i--) {
		p[i] = (uint8_t)v;
		v >>= 8;
	}
}

static void put_be32(uint8_t *p, uint32_t v)
{
	p[0] = (uint8_t)(v >> 24);
	p[1] = (uint8_t)(v >> 16);
	p[2] = (uint8_t)(v >> 8);
	p[3] = (uint8_t)v;
}

/* ---------------------------------------------------------------- signatures */

#define SIG_PREFIX "minion/v1/"
#define SIG_MAX_DATA 128 /* every signed message of the protocol is small */

static size_t sig_msg(uint8_t *m, const char *purpose, const uint8_t *data, size_t len)
{
	size_t p = strlen(SIG_PREFIX), q = strlen(purpose);
	memcpy(m, SIG_PREFIX, p);
	memcpy(m + p, purpose, q);
	m[p + q] = 0;
	memcpy(m + p + q + 1, data, len);
	return p + q + 1 + len;
}

void mt_sign(uint8_t sig[64], const uint8_t seed[32], const char *purpose, const uint8_t *data, size_t len)
{
	uint8_t m[sizeof SIG_PREFIX + 32 + SIG_MAX_DATA];
	if (len > SIG_MAX_DATA || strlen(purpose) > 32) {
		memset(sig, 0, 64);
		return;
	}
	mt_ed25519_sign(sig, seed, m, sig_msg(m, purpose, data, len));
}

int mt_verify(const uint8_t sig[64], const uint8_t pub[32], const char *purpose, const uint8_t *data, size_t len)
{
	uint8_t m[sizeof SIG_PREFIX + 32 + SIG_MAX_DATA];
	if (len > SIG_MAX_DATA || strlen(purpose) > 32)
		return MT_E_LIMIT;
	return mt_ed25519_verify(sig, pub, m, sig_msg(m, purpose, data, len)) == 0 ? MT_OK : MT_E_AUTH;
}

/* ---------------------------------------------------------------- Hub certificate */

int mt_cert_parse(mt_cert *c, const uint8_t *b, size_t n)
{
	uint64_t nb, na;
	if (n != MT_CERT_SIZE || b[0] != 1)
		return MT_E_MALFORMED;
	memcpy(c->root_pub, b + 1, 32);
	memcpy(c->op_pub, b + 33, 32);
	c->serial = be64(b + 65);
	nb = be64(b + 73);
	na = be64(b + 81);
	memcpy(c->sig, b + 89, 64);
	mt_sha256(b, MT_CERT_SIZE, c->sha256);
	if (!memcmp(c->root_pub, c->op_pub, 32))
		return MT_E_MALFORMED;
	if (c->serial < 1 || c->serial > (uint64_t)INT64_MAX)
		return MT_E_MALFORMED;
	if (nb > (uint64_t)INT64_MAX) /* not_before < 0 */
		return MT_E_MALFORMED;
	c->not_before_ms = (int64_t)nb;
	c->not_after_ms = (int64_t)na; /* two's complement, as Go's int64(Uint64) */
	/* not_after - not_before must not overflow, must be > 0 and <= the maximum validity */
	if (c->not_after_ms <= c->not_before_ms)
		return MT_E_MALFORMED;
	if ((uint64_t)c->not_after_ms - (uint64_t)c->not_before_ms > (uint64_t)MT_CERT_MAX_VALIDITY_MS)
		return MT_E_MALFORMED;
	return MT_OK;
}

int mt_cert_verify(const mt_cert *c, const uint8_t root[32])
{
	uint8_t payload[MT_CERT_PAYLOAD];
	if (!mt_ct_equal(c->root_pub, root, 32))
		return MT_E_ROOT_MISMATCH;
	payload[0] = 1;
	memcpy(payload + 1, c->root_pub, 32);
	memcpy(payload + 33, c->op_pub, 32);
	put_be64(payload + 65, c->serial);
	put_be64(payload + 73, (uint64_t)c->not_before_ms);
	put_be64(payload + 81, (uint64_t)c->not_after_ms);
	return mt_verify(c->sig, c->root_pub, "hub-cert", payload, sizeof payload) == MT_OK ? MT_OK
	                                                                                 : MT_E_BAD_SIGNATURE;
}

int mt_cert_window(const mt_cert *c, int64_t now_ms)
{
	int64_t lo = c->not_before_ms >= INT64_MIN + MT_CERT_TOLERANCE_MS ? c->not_before_ms - MT_CERT_TOLERANCE_MS
	                                                                  : INT64_MIN;
	if (now_ms < lo)
		return MT_E_NOT_YET_VALID;
	if (now_ms > c->not_after_ms)
		return MT_E_EXPIRED;
	return MT_OK;
}

int mt_ratchet_check(const mt_ratchet *r, const mt_cert *c)
{
	if (c->serial < r->max_serial)
		return MT_E_SERIAL_LOW;
	if (c->serial == r->max_serial && r->has_hash && !mt_ct_equal(r->cert_sha256, c->sha256, 32))
		return MT_E_SERIAL_CONFLICT;
	return MT_OK;
}

void mt_ratchet_raise(mt_ratchet *r, const mt_cert *c)
{
	if (c->serial > r->max_serial || !r->has_hash) {
		r->max_serial = c->serial;
		memcpy(r->cert_sha256, c->sha256, 32);
		r->has_hash = 1;
	}
}

/* ---------------------------------------------------------------- root transitions */

static int64_t days_from_civil(int64_t y, unsigned m, unsigned d)
{
	int64_t era;
	unsigned yoe, doy, doe;
	y -= m <= 2;
	era = (y >= 0 ? y : y - 399) / 400;
	yoe = (unsigned)(y - era * 400);
	doy = (153 * (m + (m > 2 ? (unsigned)-3 : 9)) + 2) / 5 + d - 1;
	doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
	return era * 146097 + (int64_t)doe - 719468;
}

static int num(const char *s, int n, int *out)
{
	int i, v = 0;
	for (i = 0; i < n; i++) {
		if (s[i] < '0' || s[i] > '9')
			return -1;
		v = v * 10 + (s[i] - '0');
	}
	*out = v;
	return 0;
}

int mt_parse_ts_ms(const char *s, int64_t *out)
{
	int Y, M, D, h, mi, sec, ms;
	static const unsigned mdays[] = {31, 29, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31};
	if (strlen(s) != 24 || s[4] != '-' || s[7] != '-' || s[10] != 'T' || s[13] != ':' || s[16] != ':' ||
	    s[19] != '.' || s[23] != 'Z')
		return -1;
	if (num(s, 4, &Y) || num(s + 5, 2, &M) || num(s + 8, 2, &D) || num(s + 11, 2, &h) || num(s + 14, 2, &mi) ||
	    num(s + 17, 2, &sec) || num(s + 20, 3, &ms))
		return -1;
	if (M < 1 || M > 12 || D < 1 || (unsigned)D > mdays[M - 1] || h > 23 || mi > 59 || sec > 59)
		return -1;
	*out = ((days_from_civil(Y, (unsigned)M, (unsigned)D) * 24 + h) * 60 + mi) * 60000LL + sec * 1000LL + ms;
	return 0;
}

int mt_transition_verify(const mt_transition *t)
{
	uint8_t d[80];
	if (!memcmp(t->old_pub, t->new_pub, 32))
		return MT_E_CHAIN;
	put_be64(d, t->seq);
	memcpy(d + 8, t->old_pub, 32);
	memcpy(d + 40, t->new_pub, 32);
	put_be64(d + 72, (uint64_t)t->created_ms);
	return mt_verify(t->sig, t->old_pub, "transition", d, sizeof d) == MT_OK ? MT_OK : MT_E_CHAIN;
}

int mt_resolve_pin(const mt_pin *pin, const uint8_t hub_pub[32], const mt_transition *chain, size_t n, mt_pin *out)
{
	mt_pin cur = *pin;
	size_t i, start = n;
	for (i = 0; i < n; i++)
		if (!memcmp(chain[i].old_pub, pin->pub, 32)) {
			start = i;
			break;
		}
	for (i = start; i < n; i++) {
		const mt_transition *t = &chain[i];
		if (i == start && pin->seq == 0) {
			if (t->seq < 1)
				return MT_E_CHAIN;
		} else if (t->seq <= cur.seq) {
			return MT_E_CHAIN;
		} else if (t->seq != cur.seq + 1) {
			return MT_E_CHAIN;
		}
		if (memcmp(t->old_pub, cur.pub, 32))
			return MT_E_CHAIN;
		if (mt_transition_verify(t) != MT_OK)
			return MT_E_CHAIN;
		memcpy(cur.pub, t->new_pub, 32);
		cur.seq = t->seq;
	}
	if (!mt_ct_equal(cur.pub, hub_pub, 32))
		return MT_E_PIN_MISMATCH;
	*out = cur;
	return MT_OK;
}

/* ---------------------------------------------------------------- transcript, keys, records */

void mt_transcript(uint8_t out[32], const uint8_t *wh, size_t wh_len, const uint8_t *hh, size_t hh_len)
{
	mt_sha256_ctx c;
	uint8_t l[4];
	mt_sha256_init(&c);
	mt_sha256_update(&c, "minion-hs-v1", 12);
	put_be32(l, (uint32_t)wh_len);
	mt_sha256_update(&c, l, 4);
	mt_sha256_update(&c, wh, wh_len);
	put_be32(l, (uint32_t)hh_len);
	mt_sha256_update(&c, l, 4);
	mt_sha256_update(&c, hh, hh_len);
	mt_sha256_final(&c, out);
}

void mt_derive_keys(uint8_t w2h[32], uint8_t h2w[32], const uint8_t transcript[32], const uint8_t dh[32],
                    const uint8_t *isk, size_t isk_len)
{
	static const char lw[] = "minion-hs-v1 key worker-to-hub", lh[] = "minion-hs-v1 key hub-to-worker";
	uint8_t ikm[32 + 64], prk[32];
	size_t n = 32;
	memcpy(ikm, dh, 32);
	if (isk && isk_len && isk_len <= 64) {
		memcpy(ikm + 32, isk, isk_len);
		n += isk_len;
	}
	mt_hkdf_extract(transcript, 32, ikm, n, prk);
	mt_hkdf_expand(prk, (const uint8_t *)lw, sizeof lw - 1, w2h, 32);
	mt_hkdf_expand(prk, (const uint8_t *)lh, sizeof lh - 1, h2w, 32);
	mt_wipe(ikm, sizeof ikm);
	mt_wipe(prk, sizeof prk);
}

static void nonce_of(uint8_t n[12], uint64_t ctr)
{
	memset(n, 0, 4);
	put_be64(n + 4, ctr);
}

int mt_rec_seal(mt_dir *d, uint8_t *out, size_t cap, const uint8_t *plain, size_t len)
{
	uint8_t nonce[12];
	if (len > MT_REC_MAX_PLAIN || cap < len + 20)
		return MT_E_LIMIT;
	if (d->counter == UINT64_MAX)
		return MT_E_LIMIT;
	put_be32(out, (uint32_t)(len + 16));
	nonce_of(nonce, d->counter);
	mt_aead_seal(out + 4, d->key, nonce, out, 4, plain, len);
	d->counter++;
	return (int)(len + 20);
}

int mt_rec_open(mt_dir *d, uint8_t *wire, size_t n)
{
	uint8_t nonce[12];
	uint32_t clen;
	if (n < 20 || n > MT_REC_MAX_PLAIN + 20)
		return MT_E_LIMIT;
	clen = (uint32_t)wire[0] << 24 | (uint32_t)wire[1] << 16 | (uint32_t)wire[2] << 8 | wire[3];
	if (clen != n - 4 || d->counter == UINT64_MAX)
		return MT_E_PROTOCOL;
	nonce_of(nonce, d->counter);
	if (mt_aead_open(wire + 4, d->key, nonce, wire, 4, wire + 4, clen) != 0)
		return MT_E_AUTH;
	d->counter++;
	return (int)(clen - 16);
}
