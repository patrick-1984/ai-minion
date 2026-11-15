/* Protocol primitives against RFC vectors and the Go repository's golden vectors. */
#include <stdio.h>
#include <string.h>

#include "mt_crypto.h"
#include "mt_enc.h"
#include "mt_json.h"
#include "mt_proto.h"
#include "vectors.h"

static int fails;
#define CHECK(c, ...)                                                                                              \
	do {                                                                                                       \
		if (!(c)) {                                                                                        \
			fails++;                                                                                   \
			printf("FAIL %s:%d: ", __FILE__, __LINE__);                                                \
			printf(__VA_ARGS__);                                                                       \
			printf("\n");                                                                              \
		}                                                                                                  \
	} while (0)

static void hex(const char *h, uint8_t *out, size_t n)
{
	if (mt_hex_decode(out, n, h, strlen(h)) != (int)n) {
		printf("bad hex in test\n");
		fails++;
	}
}

static void test_rfc(void)
{
	uint8_t d[64], exp[64], k[32], buf[256];
	/* SHA-256 FIPS 180-2 */
	mt_sha256("abc", 3, d);
	hex("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", exp, 32);
	CHECK(!memcmp(d, exp, 32), "sha256 abc");
	mt_sha256("abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq", 56, d);
	hex("248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1", exp, 32);
	CHECK(!memcmp(d, exp, 32), "sha256 448 bits");
	/* HMAC RFC 4231 case 2 */
	mt_hmac_sha256((const uint8_t *)"Jefe", 4, (const uint8_t *)"what do ya want for nothing?", 28, d);
	hex("5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843", exp, 32);
	CHECK(!memcmp(d, exp, 32), "hmac 4231-2");
	/* HKDF RFC 5869 case 1 */
	{
		uint8_t ikm[22], salt[13], info[10], prk[32], okm[42];
		memset(ikm, 0x0b, 22);
		hex("000102030405060708090a0b0c", salt, 13);
		hex("f0f1f2f3f4f5f6f7f8f9", info, 10);
		mt_hkdf_extract(salt, 13, ikm, 22, prk);
		hex("077709362c2e32df0ddc3f0dc47bba6390b6c73bb50f9c3122ec844ad7c2b3e5", exp, 32);
		CHECK(!memcmp(prk, exp, 32), "hkdf prk");
		mt_hkdf_expand(prk, info, 10, okm, 42);
		hex("3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf34007208d5b887185865", buf, 42);
		CHECK(!memcmp(okm, buf, 42), "hkdf okm");
	}
	/* X25519 RFC 7748 6.1 */
	{
		uint8_t a[32], b[32], ap[32], bp[32], s1[32], s2[32];
		hex("77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a", a, 32);
		hex("5dab087e624a8a4b79e17f8b83800ee66f3bb1292618b6fd1c2f8b27ff88e0eb", b, 32);
		mt_x25519_public(ap, a);
		mt_x25519_public(bp, b);
		hex("8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a", exp, 32);
		CHECK(!memcmp(ap, exp, 32), "x25519 pub a");
		CHECK(mt_x25519(s1, a, bp) == 0 && mt_x25519(s2, b, ap) == 0, "x25519 ok");
		hex("4a5d9d5ba4ce2de1728e3bf480350f25e07e21c947d19e3376f09b3c1e161742", exp, 32);
		CHECK(!memcmp(s1, exp, 32) && !memcmp(s2, exp, 32), "x25519 shared");
	}
	/* Ed25519 RFC 8032 test 2 */
	{
		uint8_t seed[32], pub[32], sig[64], m = 0x72;
		hex("4ccd089b28ff96da9db6c346ec114e0f5b8a319f35aba624da8cf6ed4fb8a6fb", seed, 32);
		mt_ed25519_pub_from_seed(pub, seed);
		hex("3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c", exp, 32);
		CHECK(!memcmp(pub, exp, 32), "ed25519 pub");
		mt_ed25519_sign(sig, seed, &m, 1);
		hex("92a009a9f0d4cab8720e820b5f642540a2b27b5416503f8fb3762223ebdb69da085ac1e43e15996e458f3613d0f11d8c387b2eaeb4302aeeb00d291612bb0c00",
		    exp, 64);
		CHECK(!memcmp(sig, exp, 64), "ed25519 sig");
		CHECK(mt_ed25519_verify(sig, pub, &m, 1) == 0, "ed25519 verify");
		sig[0] ^= 1;
		CHECK(mt_ed25519_verify(sig, pub, &m, 1) != 0, "ed25519 reject");
	}
	/* ChaCha20-Poly1305 RFC 8439 2.8.2 */
	{
		static const char pt[] = "Ladies and Gentlemen of the class of '99: If I could offer you only one tip for "
		                         "the future, sunscreen would be it.";
		uint8_t nonce[12], ad[12], out[200], back[200];
		size_t n = sizeof pt - 1;
		hex("808182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9f", k, 32);
		hex("070000004041424344454647", nonce, 12);
		hex("50515253c0c1c2c3c4c5c6c7", ad, 12);
		mt_aead_seal(out, k, nonce, ad, 12, (const uint8_t *)pt, n);
		hex("1ae10b594f09e26a7e902ecbd0600691", exp, 16);
		CHECK(!memcmp(out + n, exp, 16), "aead tag");
		hex("d31a8d34648e60db7b86afbc53ef7ec2", exp, 16);
		CHECK(!memcmp(out, exp, 16), "aead ct");
		CHECK(mt_aead_open(back, k, nonce, ad, 12, out, n + 16) == 0 && !memcmp(back, pt, n), "aead open");
		out[3] ^= 1;
		CHECK(mt_aead_open(back, k, nonce, ad, 12, out, n + 16) != 0, "aead reject");
	}
}

static void test_encodings(void)
{
	uint8_t b[64];
	char s[128];
	size_t n = mt_b64url_encode(s, sizeof s, (const uint8_t *)"\xfb\xff", 2);
	CHECK(n == 3 && !memcmp(s, "-_8", 3), "b64 encode");
	CHECK(mt_b64url_decode(b, sizeof b, "-_8", 3) == 2 && b[0] == 0xfb && b[1] == 0xff, "b64 decode");
	CHECK(mt_b64url_decode(b, sizeof b, "-_9", 3) < 0, "b64 non-canonical");
	CHECK(mt_b64url_decode(b, sizeof b, "AA==", 4) < 0, "b64 padding refused");
	CHECK(mt_b64url_decode(b, sizeof b, "A", 1) < 0, "b64 length 1");
	CHECK(mt_b32_decode(b, sizeof b, "my", 2) == 1 && b[0] == 'f', "b32 f");
	CHECK(mt_b32_decode(b, sizeof b, "mz", 2) < 0, "b32 non-canonical");
	CHECK(mt_b32_decode(b, sizeof b, "MY", 2) < 0, "b32 upper refused");
}

static void test_certs(void)
{
	size_t i;
	for (i = 0; i < NELEM(CERT_VECS); i++) {
		const cert_vec *v = &CERT_VECS[i];
		uint8_t raw[200], root[32];
		mt_cert c;
		int n = mt_b64url_decode(raw, sizeof raw, v->cert_b64, strlen(v->cert_b64)), r;
		uint8_t sha[32];
		CHECK(n > 0, "%s decode", v->name);
		mt_sha256(raw, (size_t)n, sha);
		CHECK(!memcmp(sha, v->sha, 32), "%s sha", v->name);
		mt_ed25519_pub_from_seed(root, v->root_seed);
		r = mt_cert_parse(&c, raw, (size_t)n);
		if (r == MT_OK)
			r = mt_cert_verify(&c, root);
		if (r == MT_OK && v->has_now)
			r = mt_cert_window(&c, v->now_ms);
		CHECK(!strcmp(mt_err_reason(r), v->expect), "%s: got %s want %s", v->name, mt_err_reason(r), v->expect);
	}
	printf("cert vectors: %zu\n", NELEM(CERT_VECS));
}

static void test_keys_sigs(void)
{
	size_t i, j;
	for (i = 0; i < NELEM(KEY_VECS); i++) {
		const key_vec *k = &KEY_VECS[i];
		uint8_t pub[32], sig[64];
		char fp[53];
		mt_ed25519_pub_from_seed(pub, k->seed);
		CHECK(!memcmp(pub, k->pub, 32), "%s pub", k->name);
		mt_fingerprint(fp, k->pub);
		CHECK(!strcmp(fp, k->fp), "%s fingerprint %s", k->name, fp);
		for (j = 0; j < k->nsig; j++) {
			const sig_vec *s = &k->sigs[j];
			mt_sign(sig, k->seed, s->purpose, s->data, s->len);
			CHECK(!memcmp(sig, s->sig, 64), "%s sig %s", k->name, s->purpose);
			CHECK(mt_verify(s->sig, k->pub, s->purpose, s->data, s->len) == MT_OK, "%s verify %s", k->name,
			      s->purpose);
			CHECK(mt_verify(s->sig, k->pub, "other", s->data, s->len) != MT_OK, "%s purpose bound", k->name);
		}
	}
	{
		mt_transition t;
		mt_pin pin, out;
		t.seq = TRANSITION.seq;
		memcpy(t.old_pub, TRANSITION.old_pub, 32);
		memcpy(t.new_pub, TRANSITION.new_pub, 32);
		memcpy(t.sig, TRANSITION.sig, 64);
		CHECK(mt_parse_ts_ms(TRANSITION.created, &t.created_ms) == 0 && t.created_ms == 1700000000000LL,
		      "created_at parse");
		CHECK(mt_transition_verify(&t) == MT_OK, "transition verify");
		memcpy(pin.pub, t.old_pub, 32);
		pin.seq = 0;
		CHECK(mt_resolve_pin(&pin, t.new_pub, &t, 1, &out) == MT_OK && out.seq == 1 &&
		          !memcmp(out.pub, t.new_pub, 32),
		      "resolve pin through chain");
		CHECK(mt_resolve_pin(&pin, t.old_pub, NULL, 0, &out) == MT_OK, "resolve pin no chain");
		CHECK(mt_resolve_pin(&pin, t.new_pub, NULL, 0, &out) == MT_E_PIN_MISMATCH, "pin mismatch");
		t.sig[5] ^= 1;
		CHECK(mt_resolve_pin(&pin, t.new_pub, &t, 1, &out) == MT_E_CHAIN, "chain bad sig");
	}
}

static void test_handshake_vectors(void)
{
	size_t i;
	for (i = 0; i < NELEM(TR_VECS); i++) {
		uint8_t h[32];
		mt_transcript(h, (const uint8_t *)TR_VECS[i].wh, strlen(TR_VECS[i].wh), (const uint8_t *)TR_VECS[i].hh,
		              strlen(TR_VECS[i].hh));
		CHECK(!memcmp(h, TR_VECS[i].hash, 32), "transcript %s", TR_VECS[i].name);
	}
	for (i = 0; i < NELEM(KDF_VECS); i++) {
		const kdf_vec *v = &KDF_VECS[i];
		uint8_t p[32], dh[32], w2h[32], h2w[32];
		mt_x25519_public(p, v->ew);
		CHECK(!memcmp(p, v->ewp, 32), "%s eph_w pub", v->name);
		CHECK(mt_x25519(dh, v->ew, v->ehp) == 0 && !memcmp(dh, v->dh, 32), "%s dh", v->name);
		mt_derive_keys(w2h, h2w, v->th, v->dh, v->isk_len ? v->isk : NULL, v->isk_len);
		CHECK(!memcmp(w2h, v->w2h, 32) && !memcmp(h2w, v->h2w, 32), "%s keys", v->name);
	}
	for (i = 0; i < NELEM(REC_VECS); i++) {
		const rec_vec *v = &REC_VECS[i];
		mt_dir d;
		uint8_t out[1200];
		int n;
		memcpy(d.key, v->key, 32);
		d.counter = v->counter;
		n = mt_rec_seal(&d, out, sizeof out, v->plain, v->plen);
		CHECK(n == (int)v->wlen && !memcmp(out, v->wire, v->wlen), "record seal %zu", i);
		d.counter = v->counter;
		n = mt_rec_open(&d, out, v->wlen);
		CHECK(n == (int)v->plen && !memcmp(out + 4, v->plain, v->plen), "record open %zu", i);
		d.counter = v->counter; /* replay with a later counter must fail */
		d.counter++;
		memcpy(out, v->wire, v->wlen);
		CHECK(mt_rec_open(&d, out, v->wlen) < 0, "record wrong counter %zu", i);
	}
}

static void test_ratchet(void)
{
	mt_ratchet r = {0, {0}, 0};
	mt_cert a, b;
	memset(&a, 0, sizeof a);
	memset(&b, 0, sizeof b);
	a.serial = 5;
	a.sha256[0] = 1;
	b.serial = 5;
	b.sha256[0] = 2;
	CHECK(mt_ratchet_check(&r, &a) == MT_OK, "fresh ratchet");
	mt_ratchet_raise(&r, &a);
	CHECK(mt_ratchet_check(&r, &a) == MT_OK, "same cert");
	CHECK(mt_ratchet_check(&r, &b) == MT_E_SERIAL_CONFLICT, "same serial other cert");
	b.serial = 4;
	CHECK(mt_ratchet_check(&r, &b) == MT_E_SERIAL_LOW, "lower serial");
	b.serial = 6;
	CHECK(mt_ratchet_check(&r, &b) == MT_OK, "higher serial");
}

static void test_json(void)
{
	const char *js = "{\"type\":\"hello\",\"n\":-42,\"ok\":true,\"a\":[1,{\"x\":\"\\u00e9\\n\"}],\"s\":\"q\\\"\"}";
	mt_jtok t[32];
	int n = mt_json_parse(js, strlen(js), t, 32), i;
	char s[32];
	int64_t v;
	int b;
	char buf[128];
	mt_jw w;
	CHECK(n > 0, "json parse");
	CHECK(mt_json_eq(js, t, mt_json_get(js, t, n, 0, "type"), "hello"), "json get");
	CHECK(mt_json_int(js, t, mt_json_get(js, t, n, 0, "n"), &v) == 0 && v == -42, "json int");
	CHECK(mt_json_bool(js, t, mt_json_get(js, t, n, 0, "ok"), &b) == 0 && b == 1, "json bool");
	i = mt_json_get(js, t, n, 0, "a");
	CHECK(i > 0 && t[i].type == MT_JT_ARR && t[i].size == 2, "json array");
	CHECK(mt_json_str(js, t, mt_json_get(js, t, n, 0, "s"), s, sizeof s) == 2 && !strcmp(s, "q\""), "json esc");
	CHECK(mt_json_get(js, t, n, 0, "missing") < 0, "json missing");
	CHECK(mt_json_parse("{\"a\":1}x", 8, t, 32) < 0, "json trailing");
	CHECK(mt_json_parse("{\"a\":01x}", 9, t, 32) < 0, "json bad prim");
	mt_jw_init(&w, buf, sizeof buf);
	mt_jw_obj(&w);
	mt_jw_kstr(&w, "t", "a\"b\x01");
	mt_jw_kint(&w, "n", -7);
	mt_jw_key(&w, "l");
	mt_jw_arr(&w);
	mt_jw_int(&w, 1);
	mt_jw_bool(&w, 0);
	mt_jw_arr_end(&w);
	mt_jw_obj_end(&w);
	CHECK(!w.err && !strcmp(buf, "{\"t\":\"a\\\"b\\u0001\",\"n\":-7,\"l\":[1,false]}"), "json writer: %s", buf);
}

int main(void)
{
	test_rfc();
	test_encodings();
	test_certs();
	test_keys_sigs();
	test_handshake_vectors();
	test_ratchet();
	test_json();
	if (fails) {
		printf("FAILED: %d\n", fails);
		return 1;
	}
	printf("PASS test_proto\n");
	return 0;
}
