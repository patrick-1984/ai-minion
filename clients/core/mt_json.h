/* Minion thin Worker: tiny JSON writer (fixed buffer) and tokenizer (jsmn style, no allocation). */
#ifndef MT_JSON_H
#define MT_JSON_H

#include <stddef.h>
#include <stdint.h>

/* ---------------------------------------------------------------- writer */

typedef struct {
	char *buf;
	size_t cap, len;
	int err;      /* set once the buffer overflowed; all later calls are no-ops */
	int need_sep; /* internal: a comma is due before the next value */
} mt_jw;

void mt_jw_init(mt_jw *w, char *buf, size_t cap);
void mt_jw_obj(mt_jw *w);               /* { */
void mt_jw_obj_end(mt_jw *w);           /* } */
void mt_jw_arr(mt_jw *w);               /* [ */
void mt_jw_arr_end(mt_jw *w);           /* ] */
void mt_jw_key(mt_jw *w, const char *k); /* "k": (k is trusted ASCII, not escaped) */
void mt_jw_str(mt_jw *w, const char *s);                 /* escaped string value (UTF-8 kept, controls \u00XX) */
void mt_jw_strn(mt_jw *w, const char *s, size_t n);      /* same with explicit length */
void mt_jw_int(mt_jw *w, int64_t v);
void mt_jw_uint(mt_jw *w, uint64_t v);
void mt_jw_bool(mt_jw *w, int v);
void mt_jw_null(mt_jw *w);
void mt_jw_raw(mt_jw *w, const char *json, size_t n);    /* pre-encoded JSON value */
void mt_jw_b64(mt_jw *w, const uint8_t *data, size_t n); /* base64url, no padding, as a string value */
/* key + value helpers */
void mt_jw_kstr(mt_jw *w, const char *k, const char *v);
void mt_jw_kint(mt_jw *w, const char *k, int64_t v);
void mt_jw_kbool(mt_jw *w, const char *k, int v);
void mt_jw_kb64(mt_jw *w, const char *k, const uint8_t *data, size_t n);

/* ---------------------------------------------------------------- tokenizer */

enum { MT_JT_UNDEF = 0, MT_JT_OBJ = 1, MT_JT_ARR = 2, MT_JT_STR = 3, MT_JT_PRIM = 4 };

typedef struct {
	uint8_t type;
	int start, end; /* byte offsets into the source; strings exclude the quotes */
	int size;       /* children: members (keys) of an object, elements of an array */
	int parent;
} mt_jtok;

/* Returns the number of tokens, or -1 (invalid JSON), -2 (too many tokens). The whole input must be one value. */
int mt_json_parse(const char *js, size_t len, mt_jtok *toks, int max_toks);

/* Lookup helpers. `obj` is the index of an object token. Return the token index of the value or -1. */
int mt_json_get(const char *js, const mt_jtok *t, int ntok, int obj, const char *key);
/* Index of the token that follows the subtree rooted at `i`. */
int mt_json_skip(const mt_jtok *t, int ntok, int i);
int mt_json_eq(const char *js, const mt_jtok *t, int i, const char *s); /* string token equals s (raw bytes) */
/* Decode a string token (escapes resolved, \uXXXX to UTF-8) into out (NUL-terminated). -1 on overflow/invalid. */
int mt_json_str(const char *js, const mt_jtok *t, int i, char *out, size_t cap);
int mt_json_int(const char *js, const mt_jtok *t, int i, int64_t *out); /* integer primitive */
int mt_json_bool(const char *js, const mt_jtok *t, int i, int *out);    /* true/false primitive */

#endif
