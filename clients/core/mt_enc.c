#include "mt_enc.h"

#include <string.h>

#include "mt_crypto.h"

static const char B64[] = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
static const char B32[] = "abcdefghijklmnopqrstuvwxyz234567";

size_t mt_b64url_encode(char *out, size_t cap, const uint8_t *in, size_t n)
{
	size_t need = (n / 3) * 4 + (n % 3 ? n % 3 + 1 : 0), i, o = 0;
	if (need > cap)
		return 0;
	for (i = 0; i + 2 < n; i += 3) {
		uint32_t v = (uint32_t)in[i] << 16 | (uint32_t)in[i + 1] << 8 | in[i + 2];
		out[o++] = B64[v >> 18];
		out[o++] = B64[(v >> 12) & 63];
		out[o++] = B64[(v >> 6) & 63];
		out[o++] = B64[v & 63];
	}
	if (n - i == 1) {
		uint32_t v = (uint32_t)in[i] << 16;
		out[o++] = B64[v >> 18];
		out[o++] = B64[(v >> 12) & 63];
	} else if (n - i == 2) {
		uint32_t v = (uint32_t)in[i] << 16 | (uint32_t)in[i + 1] << 8;
		out[o++] = B64[v >> 18];
		out[o++] = B64[(v >> 12) & 63];
		out[o++] = B64[(v >> 6) & 63];
	}
	return o;
}

static int b64v(char c)
{
	if (c >= 'A' && c <= 'Z') return c - 'A';
	if (c >= 'a' && c <= 'z') return c - 'a' + 26;
	if (c >= '0' && c <= '9') return c - '0' + 52;
	if (c == '-') return 62;
	if (c == '_') return 63;
	return -1;
}

int mt_b64url_decode(uint8_t *out, size_t cap, const char *in, size_t n)
{
	size_t i, o = 0, rem = n % 4;
	if (rem == 1)
		return -1;
	if ((n / 4) * 3 + (rem ? rem - 1 : 0) > cap)
		return -1;
	for (i = 0; i + 4 <= n; i += 4) {
		int a = b64v(in[i]), b = b64v(in[i + 1]), c = b64v(in[i + 2]), d = b64v(in[i + 3]);
		uint32_t v;
		if ((a | b | c | d) < 0)
			return -1;
		v = (uint32_t)a << 18 | (uint32_t)b << 12 | (uint32_t)c << 6 | (uint32_t)d;
		out[o++] = (uint8_t)(v >> 16);
		out[o++] = (uint8_t)(v >> 8);
		out[o++] = (uint8_t)v;
	}
	if (rem == 2) {
		int a = b64v(in[i]), b = b64v(in[i + 1]);
		if ((a | b) < 0 || (b & 15))
			return -1; /* non-canonical: unused bits set */
		out[o++] = (uint8_t)((a << 2) | (b >> 4));
	} else if (rem == 3) {
		int a = b64v(in[i]), b = b64v(in[i + 1]), c = b64v(in[i + 2]);
		if ((a | b | c) < 0 || (c & 3))
			return -1;
		out[o++] = (uint8_t)((a << 2) | (b >> 4));
		out[o++] = (uint8_t)(((b & 15) << 4) | (c >> 2));
	}
	return (int)o;
}

size_t mt_b32_encode(char *out, size_t cap, const uint8_t *in, size_t n)
{
	size_t need = (n * 8 + 4) / 5, i, o = 0;
	uint32_t buf = 0;
	int bits = 0;
	if (need > cap)
		return 0;
	for (i = 0; i < n; i++) {
		buf = (buf << 8) | in[i];
		bits += 8;
		while (bits >= 5) {
			out[o++] = B32[(buf >> (bits - 5)) & 31];
			bits -= 5;
		}
	}
	if (bits > 0)
		out[o++] = B32[(buf << (5 - bits)) & 31];
	return o;
}

int mt_b32_decode(uint8_t *out, size_t cap, const char *in, size_t n)
{
	size_t i, o = 0;
	uint32_t buf = 0;
	int bits = 0;
	for (i = 0; i < n; i++) {
		int v;
		char c = in[i];
		if (c >= 'a' && c <= 'z')
			v = c - 'a';
		else if (c >= '2' && c <= '7')
			v = c - '2' + 26;
		else
			return -1;
		buf = (buf << 5) | (uint32_t)v;
		bits += 5;
		if (bits >= 8) {
			if (o >= cap)
				return -1;
			out[o++] = (uint8_t)(buf >> (bits - 8));
			bits -= 8;
		}
	}
	if (bits >= 5 || (buf & ((1u << bits) - 1)))
		return -1; /* a whole spare symbol, or non-zero padding bits: not canonical */
	return (int)o;
}

size_t mt_hex_encode(char *out, size_t cap, const uint8_t *in, size_t n)
{
	static const char h[] = "0123456789abcdef";
	size_t i;
	if (n * 2 > cap)
		return 0;
	for (i = 0; i < n; i++) {
		out[2 * i] = h[in[i] >> 4];
		out[2 * i + 1] = h[in[i] & 15];
	}
	return n * 2;
}

static int hv(char c)
{
	if (c >= '0' && c <= '9') return c - '0';
	if (c >= 'a' && c <= 'f') return c - 'a' + 10;
	if (c >= 'A' && c <= 'F') return c - 'A' + 10;
	return -1;
}

int mt_hex_decode(uint8_t *out, size_t cap, const char *in, size_t n)
{
	size_t i;
	if (n % 2 || n / 2 > cap)
		return -1;
	for (i = 0; i < n / 2; i++) {
		int a = hv(in[2 * i]), b = hv(in[2 * i + 1]);
		if ((a | b) < 0)
			return -1;
		out[i] = (uint8_t)(a << 4 | b);
	}
	return (int)(n / 2);
}

void mt_fingerprint(char out[53], const uint8_t pub[32])
{
	uint8_t h[32];
	mt_sha256(pub, 32, h);
	mt_b32_encode(out, 52, h, 32);
	out[52] = 0;
}
