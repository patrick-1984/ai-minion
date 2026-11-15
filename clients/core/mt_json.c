#include "mt_json.h"

#include <string.h>

#include "mt_enc.h"

/* ---------------------------------------------------------------- writer */

void mt_jw_init(mt_jw *w, char *buf, size_t cap)
{
	w->buf = buf;
	w->cap = cap;
	w->len = 0;
	w->err = 0;
	w->need_sep = 0;
	if (cap)
		buf[0] = 0;
}

static void put(mt_jw *w, const char *s, size_t n)
{
	if (w->err)
		return;
	if (w->len + n + 1 > w->cap) {
		w->err = 1;
		return;
	}
	memcpy(w->buf + w->len, s, n);
	w->len += n;
	w->buf[w->len] = 0;
}

static void sep(mt_jw *w)
{
	if (w->need_sep)
		put(w, ",", 1);
	w->need_sep = 0;
}

void mt_jw_obj(mt_jw *w) { sep(w); put(w, "{", 1); }
void mt_jw_obj_end(mt_jw *w) { put(w, "}", 1); w->need_sep = 1; }
void mt_jw_arr(mt_jw *w) { sep(w); put(w, "[", 1); }
void mt_jw_arr_end(mt_jw *w) { put(w, "]", 1); w->need_sep = 1; }

void mt_jw_key(mt_jw *w, const char *k)
{
	sep(w);
	put(w, "\"", 1);
	put(w, k, strlen(k));
	put(w, "\":", 2);
}

void mt_jw_strn(mt_jw *w, const char *s, size_t n)
{
	static const char hex[] = "0123456789abcdef";
	size_t i, run = 0;
	sep(w);
	put(w, "\"", 1);
	for (i = 0; i < n; i++) {
		unsigned char c = (unsigned char)s[i];
		const char *esc = NULL;
		char u[6];
		if (c == '"')
			esc = "\\\"";
		else if (c == '\\')
			esc = "\\\\";
		else if (c == '\n')
			esc = "\\n";
		else if (c == '\r')
			esc = "\\r";
		else if (c == '\t')
			esc = "\\t";
		else if (c < 0x20 || c == 0x7f) {
			u[0] = '\\'; u[1] = 'u'; u[2] = '0'; u[3] = '0';
			u[4] = hex[c >> 4]; u[5] = hex[c & 15];
		}
		if (esc || c < 0x20 || c == 0x7f) {
			put(w, s + i - run, run);
			run = 0;
			if (esc)
				put(w, esc, strlen(esc));
			else
				put(w, u, 6);
		} else {
			run++;
		}
	}
	put(w, s + n - run, run);
	put(w, "\"", 1);
	w->need_sep = 1;
}

void mt_jw_str(mt_jw *w, const char *s) { mt_jw_strn(w, s ? s : "", s ? strlen(s) : 0); }

void mt_jw_uint(mt_jw *w, uint64_t v)
{
	char b[24];
	int i = 23;
	b[i] = 0;
	do {
		b[--i] = (char)('0' + v % 10);
		v /= 10;
	} while (v);
	sep(w);
	put(w, b + i, (size_t)(23 - i));
	w->need_sep = 1;
}

void mt_jw_int(mt_jw *w, int64_t v)
{
	if (v < 0) {
		sep(w);
		put(w, "-", 1);
		mt_jw_uint(w, (uint64_t)(-(v + 1)) + 1);
	} else {
		mt_jw_uint(w, (uint64_t)v);
	}
}

void mt_jw_bool(mt_jw *w, int v) { sep(w); put(w, v ? "true" : "false", v ? 4 : 5); w->need_sep = 1; }
void mt_jw_null(mt_jw *w) { sep(w); put(w, "null", 4); w->need_sep = 1; }
void mt_jw_raw(mt_jw *w, const char *json, size_t n) { sep(w); put(w, json, n); w->need_sep = 1; }

void mt_jw_b64(mt_jw *w, const uint8_t *data, size_t n)
{
	char tmp[96];
	size_t done = 0;
	sep(w);
	put(w, "\"", 1);
	while (done < n) { /* 72 input bytes -> 96 chars, keeps chunks aligned to 3 */
		size_t take = n - done > 72 ? 72 : n - done;
		size_t k = mt_b64url_encode(tmp, sizeof tmp, data + done, take);
		put(w, tmp, k);
		done += take;
	}
	put(w, "\"", 1);
	w->need_sep = 1;
}

void mt_jw_kstr(mt_jw *w, const char *k, const char *v) { mt_jw_key(w, k); mt_jw_str(w, v); }
void mt_jw_kint(mt_jw *w, const char *k, int64_t v) { mt_jw_key(w, k); mt_jw_int(w, v); }
void mt_jw_kbool(mt_jw *w, const char *k, int v) { mt_jw_key(w, k); mt_jw_bool(w, v); }
void mt_jw_kb64(mt_jw *w, const char *k, const uint8_t *d, size_t n) { mt_jw_key(w, k); mt_jw_b64(w, d, n); }

/* ---------------------------------------------------------------- tokenizer */

typedef struct {
	const char *js;
	size_t len, pos;
	mt_jtok *t;
	int max, n;
} parser;

static int new_tok(parser *p, uint8_t type, int start, int parent)
{
	if (p->n >= p->max)
		return -2;
	p->t[p->n].type = type;
	p->t[p->n].start = start;
	p->t[p->n].end = -1;
	p->t[p->n].size = 0;
	p->t[p->n].parent = parent;
	return p->n++;
}

static void ws(parser *p)
{
	while (p->pos < p->len) {
		char c = p->js[p->pos];
		if (c != ' ' && c != '\t' && c != '\n' && c != '\r')
			break;
		p->pos++;
	}
}

static int parse_value(parser *p, int parent, int depth);

static int parse_string(parser *p, int parent)
{
	int i;
	size_t start = ++p->pos;
	while (p->pos < p->len) {
		unsigned char c = (unsigned char)p->js[p->pos];
		if (c == '"') {
			i = new_tok(p, MT_JT_STR, (int)start, parent);
			if (i < 0)
				return i;
			p->t[i].end = (int)p->pos;
			p->pos++;
			return i;
		}
		if (c < 0x20)
			return -1;
		if (c == '\\') {
			p->pos++;
			if (p->pos >= p->len)
				return -1;
			c = (unsigned char)p->js[p->pos];
			if (c == 'u') {
				int k;
				for (k = 1; k <= 4; k++) {
					char h;
					if (p->pos + (size_t)k >= p->len)
						return -1;
					h = p->js[p->pos + (size_t)k];
					if (!((h >= '0' && h <= '9') || (h >= 'a' && h <= 'f') || (h >= 'A' && h <= 'F')))
						return -1;
				}
				p->pos += 4;
			} else if (!strchr("\"\\/bfnrt", c)) {
				return -1;
			}
		}
		p->pos++;
	}
	return -1;
}

static int parse_prim(parser *p, int parent)
{
	size_t start = p->pos;
	int i;
	while (p->pos < p->len) {
		char c = p->js[p->pos];
		if (c == ',' || c == ']' || c == '}' || c == ' ' || c == '\t' || c == '\n' || c == '\r')
			break;
		if ((unsigned char)c < 0x20 || c == '"' || c == '{' || c == '[')
			return -1;
		p->pos++;
	}
	if (p->pos == start)
		return -1;
	{ /* accept true/false/null/numbers only */
		const char *s = p->js + start;
		size_t n = p->pos - start, k;
		if (!((n == 4 && !memcmp(s, "true", 4)) || (n == 5 && !memcmp(s, "false", 5)) ||
		      (n == 4 && !memcmp(s, "null", 4)))) {
			for (k = 0; k < n; k++)
				if (!strchr("0123456789+-.eE", s[k]))
					return -1;
		}
	}
	i = new_tok(p, MT_JT_PRIM, (int)start, parent);
	if (i < 0)
		return i;
	p->t[i].end = (int)p->pos;
	return i;
}

static int parse_container(parser *p, int parent, int depth, int is_obj)
{
	int me = new_tok(p, is_obj ? MT_JT_OBJ : MT_JT_ARR, (int)p->pos, parent), r;
	char close = is_obj ? '}' : ']';
	if (me < 0)
		return me;
	if (depth > 32)
		return -1;
	p->pos++;
	ws(p);
	if (p->pos < p->len && p->js[p->pos] == close) {
		p->pos++;
		p->t[me].end = (int)p->pos;
		return me;
	}
	for (;;) {
		ws(p);
		if (is_obj) {
			if (p->pos >= p->len || p->js[p->pos] != '"')
				return -1;
			r = parse_string(p, me);
			if (r < 0)
				return r;
			ws(p);
			if (p->pos >= p->len || p->js[p->pos] != ':')
				return -1;
			p->pos++;
			ws(p);
			r = parse_value(p, r, depth + 1);
		} else {
			r = parse_value(p, me, depth + 1);
		}
		if (r < 0)
			return r;
		p->t[me].size++;
		ws(p);
		if (p->pos >= p->len)
			return -1;
		if (p->js[p->pos] == ',') {
			p->pos++;
			continue;
		}
		if (p->js[p->pos] == close) {
			p->pos++;
			p->t[me].end = (int)p->pos;
			return me;
		}
		return -1;
	}
}

static int parse_value(parser *p, int parent, int depth)
{
	ws(p);
	if (p->pos >= p->len)
		return -1;
	switch (p->js[p->pos]) {
	case '{': return parse_container(p, parent, depth, 1);
	case '[': return parse_container(p, parent, depth, 0);
	case '"': return parse_string(p, parent);
	default: return parse_prim(p, parent);
	}
}

int mt_json_parse(const char *js, size_t len, mt_jtok *toks, int max_toks)
{
	parser p = {js, len, 0, toks, max_toks, 0};
	int r = parse_value(&p, -1, 0);
	if (r < 0)
		return r;
	ws(&p);
	if (p.pos != len)
		return -1;
	return p.n;
}

int mt_json_skip(const mt_jtok *t, int ntok, int i)
{
	int end = t[i].end, j = i + 1;
	while (j < ntok && t[j].start < end)
		j++;
	return j;
}

int mt_json_eq(const char *js, const mt_jtok *t, int i, const char *s)
{
	size_t n = strlen(s);
	return i >= 0 && t[i].type == MT_JT_STR && (size_t)(t[i].end - t[i].start) == n &&
	       memcmp(js + t[i].start, s, n) == 0;
}

int mt_json_get(const char *js, const mt_jtok *t, int ntok, int obj, const char *key)
{
	int i, k;
	if (obj < 0 || obj >= ntok || t[obj].type != MT_JT_OBJ)
		return -1;
	i = obj + 1;
	for (k = 0; k < t[obj].size && i < ntok; k++) {
		int v = i + 1;
		if (mt_json_eq(js, t, i, key))
			return v < ntok ? v : -1;
		i = mt_json_skip(t, ntok, v);
	}
	return -1;
}

static int hexv(char c)
{
	if (c >= '0' && c <= '9') return c - '0';
	if (c >= 'a' && c <= 'f') return c - 'a' + 10;
	if (c >= 'A' && c <= 'F') return c - 'A' + 10;
	return -1;
}

int mt_json_str(const char *js, const mt_jtok *t, int i, char *out, size_t cap)
{
	int p;
	size_t o = 0;
	if (i < 0 || t[i].type != MT_JT_STR || cap == 0)
		return -1;
	for (p = t[i].start; p < t[i].end; p++) {
		char c = js[p];
		uint32_t cp;
		if (c != '\\') {
			if (o + 1 >= cap) return -1;
			out[o++] = c;
			continue;
		}
		c = js[++p];
		switch (c) {
		case 'n': cp = '\n'; break;
		case 'r': cp = '\r'; break;
		case 't': cp = '\t'; break;
		case 'b': cp = '\b'; break;
		case 'f': cp = '\f'; break;
		case 'u':
			cp = (uint32_t)(hexv(js[p + 1]) << 12 | hexv(js[p + 2]) << 8 | hexv(js[p + 3]) << 4 | hexv(js[p + 4]));
			p += 4;
			if (cp >= 0xD800 && cp <= 0xDBFF && p + 6 < t[i].end && js[p + 1] == '\\' && js[p + 2] == 'u') {
				uint32_t lo = (uint32_t)(hexv(js[p + 3]) << 12 | hexv(js[p + 4]) << 8 | hexv(js[p + 5]) << 4 |
				                         hexv(js[p + 6]));
				if (lo >= 0xDC00 && lo <= 0xDFFF) {
					cp = 0x10000 + ((cp - 0xD800) << 10) + (lo - 0xDC00);
					p += 6;
				}
			}
			if (cp >= 0xD800 && cp <= 0xDFFF)
				cp = 0xFFFD;
			break;
		default: cp = (uint32_t)(unsigned char)c; break;
		}
		if (cp == 0)
			return -1; /* no NUL inside C strings */
		if (cp < 0x80) {
			if (o + 1 >= cap) return -1;
			out[o++] = (char)cp;
		} else if (cp < 0x800) {
			if (o + 2 >= cap) return -1;
			out[o++] = (char)(0xC0 | cp >> 6);
			out[o++] = (char)(0x80 | (cp & 0x3F));
		} else if (cp < 0x10000) {
			if (o + 3 >= cap) return -1;
			out[o++] = (char)(0xE0 | cp >> 12);
			out[o++] = (char)(0x80 | ((cp >> 6) & 0x3F));
			out[o++] = (char)(0x80 | (cp & 0x3F));
		} else {
			if (o + 4 >= cap) return -1;
			out[o++] = (char)(0xF0 | cp >> 18);
			out[o++] = (char)(0x80 | ((cp >> 12) & 0x3F));
			out[o++] = (char)(0x80 | ((cp >> 6) & 0x3F));
			out[o++] = (char)(0x80 | (cp & 0x3F));
		}
	}
	out[o] = 0;
	return (int)o;
}

int mt_json_int(const char *js, const mt_jtok *t, int i, int64_t *out)
{
	int p, neg = 0;
	uint64_t v = 0;
	if (i < 0 || t[i].type != MT_JT_PRIM)
		return -1;
	p = t[i].start;
	if (js[p] == '-') {
		neg = 1;
		p++;
	}
	if (p >= t[i].end)
		return -1;
	for (; p < t[i].end; p++) {
		if (js[p] < '0' || js[p] > '9')
			return -1;
		if (v > (UINT64_MAX - 9) / 10)
			return -1;
		v = v * 10 + (uint64_t)(js[p] - '0');
	}
	if (v > (uint64_t)INT64_MAX)
		return -1;
	*out = neg ? -(int64_t)v : (int64_t)v;
	return 0;
}

int mt_json_bool(const char *js, const mt_jtok *t, int i, int *out)
{
	if (i < 0 || t[i].type != MT_JT_PRIM)
		return -1;
	if (t[i].end - t[i].start == 4 && !memcmp(js + t[i].start, "true", 4)) {
		*out = 1;
		return 0;
	}
	if (t[i].end - t[i].start == 5 && !memcmp(js + t[i].start, "false", 5)) {
		*out = 0;
		return 0;
	}
	return -1;
}
