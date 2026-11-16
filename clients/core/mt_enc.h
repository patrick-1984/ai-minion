/* Minion thin Worker: strict encodings used on the wire (match internal/identity b64.go and fingerprint.go). */
#ifndef MT_ENC_H
#define MT_ENC_H

#include <stddef.h>
#include <stdint.h>

/* base64url without padding. encode returns the number of chars written (no NUL), 0 if cap is too small. */
size_t mt_b64url_encode(char *out, size_t cap, const uint8_t *in, size_t n);
/* Strict decode: alphabet A-Z a-z 0-9 - _, no padding, canonical (unused trailing bits zero).
 * Returns the decoded length or -1. */
int mt_b64url_decode(uint8_t *out, size_t cap, const char *in, size_t n);

/* RFC 4648 base32, lower-case alphabet a-z2-7, no padding (fingerprints, invitation secrets). */
size_t mt_b32_encode(char *out, size_t cap, const uint8_t *in, size_t n);
int mt_b32_decode(uint8_t *out, size_t cap, const char *in, size_t n); /* strict + canonical, -1 on error */

size_t mt_hex_encode(char *out, size_t cap, const uint8_t *in, size_t n);
int mt_hex_decode(uint8_t *out, size_t cap, const char *in, size_t n);

/* Fingerprint of a 32-byte Ed25519 public key: base32(SHA-256(pub)), 52 chars + NUL. */
void mt_fingerprint(char out[53], const uint8_t pub[32]);

#endif
