#include "mt_crypto.h"

#include <string.h>

#include "third_party/monocypher/monocypher-ed25519.h"
#include "third_party/monocypher/monocypher.h"

/* ---------------------------------------------------------------- SHA-256 (FIPS 180-4) */

static const uint32_t K256[64] = {
	0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
	0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
	0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
	0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
	0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
	0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
	0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
	0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
};

#define ROR(x, n) (((x) >> (n)) | ((x) << (32 - (n))))

static void sha256_block(uint32_t h[8], const uint8_t *p)
{
	uint32_t w[64], a, b, c, d, e, f, g, k, t1, t2;
	int i;
	for (i = 0; i < 16; i++)
		w[i] = (uint32_t)p[4 * i] << 24 | (uint32_t)p[4 * i + 1] << 16 | (uint32_t)p[4 * i + 2] << 8 |
		       (uint32_t)p[4 * i + 3];
	for (i = 16; i < 64; i++) {
		uint32_t s0 = ROR(w[i - 15], 7) ^ ROR(w[i - 15], 18) ^ (w[i - 15] >> 3);
		uint32_t s1 = ROR(w[i - 2], 17) ^ ROR(w[i - 2], 19) ^ (w[i - 2] >> 10);
		w[i] = w[i - 16] + s0 + w[i - 7] + s1;
	}
	a = h[0]; b = h[1]; c = h[2]; d = h[3]; e = h[4]; f = h[5]; g = h[6]; k = h[7];
	for (i = 0; i < 64; i++) {
		t1 = k + (ROR(e, 6) ^ ROR(e, 11) ^ ROR(e, 25)) + ((e & f) ^ (~e & g)) + K256[i] + w[i];
		t2 = (ROR(a, 2) ^ ROR(a, 13) ^ ROR(a, 22)) + ((a & b) ^ (a & c) ^ (b & c));
		k = g; g = f; f = e; e = d + t1; d = c; c = b; b = a; a = t1 + t2;
	}
	h[0] += a; h[1] += b; h[2] += c; h[3] += d; h[4] += e; h[5] += f; h[6] += g; h[7] += k;
}

void mt_sha256_init(mt_sha256_ctx *c)
{
	static const uint32_t iv[8] = {0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a,
	                               0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19};
	memcpy(c->h, iv, sizeof iv);
	c->len = 0;
	c->fill = 0;
}

void mt_sha256_update(mt_sha256_ctx *c, const void *data, size_t n)
{
	const uint8_t *p = data;
	c->len += n;
	while (n > 0) {
		size_t take = 64 - c->fill;
		if (take > n)
			take = n;
		memcpy(c->buf + c->fill, p, take);
		c->fill += take;
		p += take;
		n -= take;
		if (c->fill == 64) {
			sha256_block(c->h, c->buf);
			c->fill = 0;
		}
	}
}

void mt_sha256_final(mt_sha256_ctx *c, uint8_t out[32])
{
	uint64_t bits = c->len * 8;
	uint8_t pad = 0x80, zero = 0, lenb[8];
	int i;
	mt_sha256_update(c, &pad, 1);
	while (c->fill != 56)
		mt_sha256_update(c, &zero, 1);
	for (i = 0; i < 8; i++)
		lenb[i] = (uint8_t)(bits >> (56 - 8 * i));
	mt_sha256_update(c, lenb, 8);
	for (i = 0; i < 8; i++) {
		out[4 * i] = (uint8_t)(c->h[i] >> 24);
		out[4 * i + 1] = (uint8_t)(c->h[i] >> 16);
		out[4 * i + 2] = (uint8_t)(c->h[i] >> 8);
		out[4 * i + 3] = (uint8_t)c->h[i];
	}
	mt_wipe(c, sizeof *c);
}

void mt_sha256(const void *data, size_t n, uint8_t out[32])
{
	mt_sha256_ctx c;
	mt_sha256_init(&c);
	mt_sha256_update(&c, data, n);
	mt_sha256_final(&c, out);
}

/* ---------------------------------------------------------------- HMAC / HKDF */

void mt_hmac_sha256(const uint8_t *key, size_t key_len, const uint8_t *msg, size_t msg_len, uint8_t out[32])
{
	uint8_t k[64], pad[64], inner[32];
	mt_sha256_ctx c;
	int i;
	memset(k, 0, sizeof k);
	if (key_len > 64)
		mt_sha256(key, key_len, k);
	else if (key_len)
		memcpy(k, key, key_len);
	for (i = 0; i < 64; i++)
		pad[i] = k[i] ^ 0x36;
	mt_sha256_init(&c);
	mt_sha256_update(&c, pad, 64);
	mt_sha256_update(&c, msg, msg_len);
	mt_sha256_final(&c, inner);
	for (i = 0; i < 64; i++)
		pad[i] = k[i] ^ 0x5c;
	mt_sha256_init(&c);
	mt_sha256_update(&c, pad, 64);
	mt_sha256_update(&c, inner, 32);
	mt_sha256_final(&c, out);
	mt_wipe(k, sizeof k);
	mt_wipe(pad, sizeof pad);
	mt_wipe(inner, sizeof inner);
}

void mt_hkdf_extract(const uint8_t *salt, size_t salt_len, const uint8_t *ikm, size_t ikm_len, uint8_t prk[32])
{
	static const uint8_t zero[32];
	if (salt == NULL || salt_len == 0) {
		salt = zero;
		salt_len = sizeof zero;
	}
	mt_hmac_sha256(salt, salt_len, ikm, ikm_len, prk);
}

int mt_hkdf_expand(const uint8_t prk[32], const uint8_t *info, size_t info_len, uint8_t *out, size_t out_len)
{
	uint8_t t[32], buf[32 + 256 + 1];
	size_t tlen = 0, done = 0;
	uint8_t ctr = 1;
	if (out_len > 255 * 32 || info_len > 256)
		return -1;
	while (done < out_len) {
		size_t n = 0, take;
		memcpy(buf, t, tlen);
		n = tlen;
		if (info_len)
			memcpy(buf + n, info, info_len);
		n += info_len;
		buf[n++] = ctr++;
		mt_hmac_sha256(prk, 32, buf, n, t);
		tlen = 32;
		take = out_len - done < 32 ? out_len - done : 32;
		memcpy(out + done, t, take);
		done += take;
	}
	mt_wipe(t, sizeof t);
	mt_wipe(buf, sizeof buf);
	return 0;
}

/* ---------------------------------------------------------------- Monocypher wrappers */

void mt_x25519_public(uint8_t pub[32], const uint8_t secret[32])
{
	crypto_x25519_public_key(pub, secret);
}

int mt_x25519(uint8_t shared[32], const uint8_t secret[32], const uint8_t peer_pub[32])
{
	static const uint8_t zero[32];
	crypto_x25519(shared, secret, peer_pub);
	return crypto_verify32(shared, zero) == 0 ? -1 : 0;
}

void mt_ed25519_pub_from_seed(uint8_t pub[32], const uint8_t seed[32])
{
	uint8_t s[32], sk[64];
	memcpy(s, seed, 32); /* crypto_ed25519_key_pair wipes the seed it is given */
	crypto_ed25519_key_pair(sk, pub, s);
	mt_wipe(sk, sizeof sk);
}

void mt_ed25519_sign(uint8_t sig[64], const uint8_t seed[32], const uint8_t *msg, size_t len)
{
	uint8_t s[32], sk[64], pub[32];
	memcpy(s, seed, 32);
	crypto_ed25519_key_pair(sk, pub, s);
	crypto_ed25519_sign(sig, sk, msg, len);
	mt_wipe(sk, sizeof sk);
}

int mt_ed25519_verify(const uint8_t sig[64], const uint8_t pub[32], const uint8_t *msg, size_t len)
{
	return crypto_ed25519_check(sig, pub, msg, len) == 0 ? 0 : -1;
}

void mt_aead_seal(uint8_t *out, const uint8_t key[32], const uint8_t nonce[12], const uint8_t *ad, size_t ad_len,
                  const uint8_t *plain, size_t len)
{
	crypto_aead_ctx ctx; /* fresh context per message: Monocypher re-keys after each write */
	crypto_aead_init_ietf(&ctx, key, nonce);
	crypto_aead_write(&ctx, out, out + len, ad, ad_len, plain, len);
	crypto_wipe(&ctx, sizeof ctx);
}

int mt_aead_open(uint8_t *out, const uint8_t key[32], const uint8_t nonce[12], const uint8_t *ad, size_t ad_len,
                 const uint8_t *cipher, size_t len_with_tag)
{
	crypto_aead_ctx ctx;
	int r;
	if (len_with_tag < 16)
		return -1;
	crypto_aead_init_ietf(&ctx, key, nonce);
	r = crypto_aead_read(&ctx, out, cipher + len_with_tag - 16, ad, ad_len, cipher, len_with_tag - 16);
	crypto_wipe(&ctx, sizeof ctx);
	return r == 0 ? 0 : -1;
}

void mt_wipe(void *p, size_t n)
{
	crypto_wipe(p, n);
}

int mt_ct_equal(const uint8_t *a, const uint8_t *b, size_t n)
{
	uint8_t d = 0;
	size_t i;
	for (i = 0; i < n; i++)
		d |= a[i] ^ b[i];
	return d == 0;
}
