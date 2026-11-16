/* Minion thin Worker: identity and handshake primitives of the Worker protocol 3.0.
 *
 * Mirrors internal/identity (sign.go, hubcert.go, chain.go), internal/transport (transcript.go, kdf.go,
 * channel.go, cert.go). Every check here decides trust; keep it in step with the Go code. */
#ifndef MT_PROTO_H
#define MT_PROTO_H

#include <stddef.h>
#include <stdint.h>

#define MT_CERT_SIZE 153
#define MT_CERT_PAYLOAD 89
#define MT_CERT_TOLERANCE_MS 3600000LL
/* Hub PR #24 raises the Hub's maximum validity from 400 to 3650 days; accept both kinds of Hub. */
#define MT_CERT_MAX_VALIDITY_MS (3650LL * 24 * 3600 * 1000)

enum mt_err {
	MT_OK = 0,
	MT_E_MALFORMED = -1,     /* cert_malformed */
	MT_E_ROOT_MISMATCH = -2, /* cert_root_mismatch */
	MT_E_BAD_SIGNATURE = -3, /* cert_bad_signature */
	MT_E_NOT_YET_VALID = -4, /* cert_not_yet_valid */
	MT_E_EXPIRED = -5,       /* cert_expired */
	MT_E_SERIAL_LOW = -6,    /* cert_serial_too_low */
	MT_E_SERIAL_CONFLICT = -7,
	MT_E_CHAIN = -8,         /* chain invalid */
	MT_E_PIN_MISMATCH = -9,
	MT_E_PROTOCOL = -10,
	MT_E_AUTH = -11,         /* record or signature authentication failed */
	MT_E_LIMIT = -12,
	MT_E_IO = -13,
};

const char *mt_err_reason(int err);

/* Domain-separated signatures: "minion/v1/" || purpose || 0x00 || data. */
void mt_sign(uint8_t sig[64], const uint8_t seed[32], const char *purpose, const uint8_t *data, size_t len);
int mt_verify(const uint8_t sig[64], const uint8_t pub[32], const char *purpose, const uint8_t *data, size_t len);

typedef struct {
	uint8_t root_pub[32];
	uint8_t op_pub[32];
	uint64_t serial;
	int64_t not_before_ms, not_after_ms;
	uint8_t sig[64];
	uint8_t sha256[32]; /* identity of the certificate for the ratchet */
} mt_cert;

int mt_cert_parse(mt_cert *c, const uint8_t *b, size_t n);   /* structure only: MT_E_MALFORMED */
int mt_cert_verify(const mt_cert *c, const uint8_t root[32]); /* root equality, then signature */
int mt_cert_window(const mt_cert *c, int64_t now_ms);        /* not_before - 1 h <= now <= not_after */

typedef struct {
	uint64_t max_serial;
	uint8_t cert_sha256[32];
	int has_hash;
} mt_ratchet;

int mt_ratchet_check(const mt_ratchet *r, const mt_cert *c);
void mt_ratchet_raise(mt_ratchet *r, const mt_cert *c); /* call only after the handshake succeeded */

typedef struct {
	uint64_t seq;
	uint8_t old_pub[32], new_pub[32];
	int64_t created_ms;
	uint8_t sig[64];
} mt_transition;

typedef struct {
	uint8_t pub[32];
	uint64_t seq;
} mt_pin;

/* Parse "2006-01-02T15:04:05.000Z" (UTC, millisecond precision). */
int mt_parse_ts_ms(const char *s, int64_t *out);
int mt_transition_verify(const mt_transition *t);
/* Apply the part of `chain` that starts at the pinned key and require the result to equal hub_pub.
 * On success *out is the new pin (equal to *pin when there was nothing to apply). */
int mt_resolve_pin(const mt_pin *pin, const uint8_t hub_pub[32], const mt_transition *chain, size_t n, mt_pin *out);

/* Transcript: SHA-256("minion-hs-v1" || be32(len wh) || wh || be32(len hh) || hh). */
void mt_transcript(uint8_t out[32], const uint8_t *wh, size_t wh_len, const uint8_t *hh, size_t hh_len);
/* Keys: prk = HKDF-Extract(salt = transcript, ikm = dh [|| isk]); w2h/h2w = HKDF-Expand(prk, label, 32). */
void mt_derive_keys(uint8_t w2h[32], uint8_t h2w[32], const uint8_t transcript[32], const uint8_t dh[32],
                    const uint8_t *isk, size_t isk_len);

/* Record layer: wire = be32(n) || ciphertext(n), n <= 16384 + 16; nonce = 4 zero bytes || be64(counter);
 * AAD = the 4-byte header. One record per WebSocket message. */
#define MT_REC_MAX_PLAIN 16384
typedef struct {
	uint8_t key[32];
	uint64_t counter;
} mt_dir;

/* Seal len (<= MT_REC_MAX_PLAIN) bytes into out (needs len + 20 bytes). Returns the wire length or < 0. */
int mt_rec_seal(mt_dir *d, uint8_t *out, size_t cap, const uint8_t *plain, size_t len);
/* Open one wire record in place: returns the plaintext length (plaintext at wire + 4) or < 0. */
int mt_rec_open(mt_dir *d, uint8_t *wire, size_t n);

#endif
