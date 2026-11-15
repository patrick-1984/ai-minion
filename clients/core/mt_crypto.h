/* Minion thin Worker: cryptographic primitives used by the Worker protocol 3.0.
 *
 * SHA-256, HMAC-SHA-256 and HKDF-SHA-256 are implemented here; X25519, Ed25519 (RFC 8032, SHA-512) and
 * ChaCha20-Poly1305 (RFC 8439) come from the vendored Monocypher 4.0.2. Every function is constant-time where
 * secrets are involved (Monocypher) and allocation-free. */
#ifndef MT_CRYPTO_H
#define MT_CRYPTO_H

#include <stddef.h>
#include <stdint.h>

typedef struct {
	uint32_t h[8];
	uint64_t len;
	uint8_t buf[64];
	size_t fill;
} mt_sha256_ctx;

void mt_sha256_init(mt_sha256_ctx *c);
void mt_sha256_update(mt_sha256_ctx *c, const void *data, size_t n);
void mt_sha256_final(mt_sha256_ctx *c, uint8_t out[32]);
void mt_sha256(const void *data, size_t n, uint8_t out[32]);

void mt_hmac_sha256(const uint8_t *key, size_t key_len, const uint8_t *msg, size_t msg_len, uint8_t out[32]);
/* HKDF (RFC 5869) with SHA-256. Extract: prk = HMAC(salt, ikm). Expand: out_len <= 255*32. */
void mt_hkdf_extract(const uint8_t *salt, size_t salt_len, const uint8_t *ikm, size_t ikm_len, uint8_t prk[32]);
int mt_hkdf_expand(const uint8_t prk[32], const uint8_t *info, size_t info_len, uint8_t *out, size_t out_len);

/* X25519: shared = scalar * point. Returns -1 if the shared secret is all zero (low-order point). */
void mt_x25519_public(uint8_t pub[32], const uint8_t secret[32]);
int mt_x25519(uint8_t shared[32], const uint8_t secret[32], const uint8_t peer_pub[32]);

/* Ed25519 per RFC 8032. seed is the 32-byte private seed (Go's ed25519.NewKeyFromSeed). */
void mt_ed25519_pub_from_seed(uint8_t pub[32], const uint8_t seed[32]);
void mt_ed25519_sign(uint8_t sig[64], const uint8_t seed[32], const uint8_t *msg, size_t len);
int mt_ed25519_verify(const uint8_t sig[64], const uint8_t pub[32], const uint8_t *msg, size_t len); /* 0 = ok */

/* ChaCha20-Poly1305 (RFC 8439), 12-byte nonce, 16-byte tag appended after the ciphertext. In-place allowed. */
void mt_aead_seal(uint8_t *out, const uint8_t key[32], const uint8_t nonce[12], const uint8_t *ad, size_t ad_len,
                  const uint8_t *plain, size_t len);
int mt_aead_open(uint8_t *out, const uint8_t key[32], const uint8_t nonce[12], const uint8_t *ad, size_t ad_len,
                 const uint8_t *cipher, size_t len_with_tag); /* 0 = ok; out gets len_with_tag-16 bytes */

void mt_wipe(void *p, size_t n);
int mt_ct_equal(const uint8_t *a, const uint8_t *b, size_t n); /* 1 when equal, constant time */

#endif
