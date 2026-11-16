#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#include "mt_json.h"
#include "mt_vt.h"

static int fails;

#define FAIL(...) do { printf("FAIL line %d: ", __LINE__); printf(__VA_ARGS__); printf("\n"); fails++; } while (0)

/* ---------------------------------------------------------------- validation of the produced JSON
 * (mirrors ops.Screen.Validate) */

static int utf8_runes(const char *s, int *ctl) /* rune count, -1 when invalid */
{
	const unsigned char *p = (const unsigned char *)s;
	int n = 0;
	while (*p) {
		unsigned c = *p, need = 0, cp, min;
		if (c < 0x80) { cp = c; }
		else if (c >= 0xC2 && c <= 0xDF) { need = 1; cp = c & 0x1f; min = 0x80; }
		else if (c >= 0xE0 && c <= 0xEF) { need = 2; cp = c & 0xf; min = 0x800; }
		else if (c >= 0xF0 && c <= 0xF4) { need = 3; cp = c & 7; min = 0x10000; }
		else return -1;
		p++;
		while (need--) {
			if ((*p & 0xC0) != 0x80) return -1;
			cp = (cp << 6) | (*p++ & 0x3f);
			if (need == 0 && (cp < min || cp > 0x10FFFF || (cp >= 0xD800 && cp <= 0xDFFF))) return -1;
		}
		if (cp < 0x20 || (cp >= 0x7f && cp <= 0x9f)) *ctl = 1;
		n++;
	}
	return n;
}

static int valid_color(const char *c)
{
	size_t n = strlen(c), i;
	if (n == 7 && c[0] == '#') {
		for (i = 1; i < 7; i++)
			if (!((c[i] >= '0' && c[i] <= '9') || (c[i] >= 'a' && c[i] <= 'f'))) return 0;
		return 1;
	}
	if (n < 2 || n > 4 || c[0] != 'p') return 0;
	for (i = 1; i < n; i++)
		if (c[i] < '0' || c[i] > '9') return 0;
	if (n > 2 && c[1] == '0') return 0;
	return atoi(c + 1) <= 255;
}

static mt_jtok toks[40000];

static const char *validate(const char *js, size_t len, int want_height)
{
	int n = mt_json_parse(js, len, toks, 40000), i, lines, spans, cur, k, runes[512];
	int64_t v, h = -1, w = -1;
	static char tmp[4096];
	int prev_line = -1, prev_end = 0, nl;
	if (n < 1 || toks[0].type != MT_JT_OBJ) return "bad json";
	if ((i = mt_json_get(js, toks, n, 0, "width")) < 0 || mt_json_int(js, toks, i, &w) || w < 0) return "width";
	if ((i = mt_json_get(js, toks, n, 0, "height")) < 0 || mt_json_int(js, toks, i, &h) || h < 0) return "height";
	if (want_height >= 0 && h != want_height) return "height value";
	if ((lines = mt_json_get(js, toks, n, 0, "lines")) < 0 || toks[lines].type != MT_JT_ARR) return "lines";
	nl = toks[lines].size;
	if (h > 0 && nl != h) return "lines count != height";
	if (nl > 512) return "too many lines";
	k = lines + 1;
	for (i = 0; i < nl; i++, k++) {
		int ctl = 0;
		if (toks[k].type != MT_JT_STR || mt_json_str(js, toks, k, tmp, sizeof tmp) < 0) return "line string";
		runes[i] = utf8_runes(tmp, &ctl);
		if (runes[i] < 0) return "line invalid utf8";
		if (ctl) return "line control char";
		if (w > 0 && runes[i] > w) return "line longer than width";
	}
	if ((spans = mt_json_get(js, toks, n, 0, "spans")) < 0 || toks[spans].type != MT_JT_ARR) return "spans";
	k = spans + 1;
	for (i = 0; i < toks[spans].size; i++) {
		int o = k, a;
		int64_t ln, st, en;
		char col[16];
		if (toks[o].type != MT_JT_OBJ) return "span obj";
		if ((a = mt_json_get(js, toks, n, o, "line")) < 0 || mt_json_int(js, toks, a, &ln)) return "span line";
		if ((a = mt_json_get(js, toks, n, o, "start")) < 0 || mt_json_int(js, toks, a, &st)) return "span start";
		if ((a = mt_json_get(js, toks, n, o, "end")) < 0 || mt_json_int(js, toks, a, &en)) return "span end";
		if (ln < 0 || ln >= nl) return "span line range";
		if (st < 0 || en <= st || en > runes[ln]) return "span range";
		if (ln < prev_line || (ln == prev_line && st < prev_end)) return "span order";
		prev_line = (int)ln; prev_end = (int)en;
		if ((a = mt_json_get(js, toks, n, o, "fg")) >= 0 && (mt_json_str(js, toks, a, col, sizeof col) < 0 || !valid_color(col))) return "fg";
		if ((a = mt_json_get(js, toks, n, o, "bg")) >= 0 && (mt_json_str(js, toks, a, col, sizeof col) < 0 || !valid_color(col))) return "bg";
		k = mt_json_skip(toks, n, o);
	}
	if ((cur = mt_json_get(js, toks, n, 0, "cursor")) < 0) return "cursor";
	if (toks[cur].type == MT_JT_OBJ) {
		int a;
		if ((a = mt_json_get(js, toks, n, cur, "row")) < 0 || mt_json_int(js, toks, a, &v) || v < 0) return "cursor row";
		if ((a = mt_json_get(js, toks, n, cur, "col")) < 0 || mt_json_int(js, toks, a, &v) || v < 0) return "cursor col";
	} else if (toks[cur].type != MT_JT_PRIM) {
		return "cursor type";
	}
	return NULL;
}

/* ---------------------------------------------------------------- helpers */

typedef struct { mt_vt vt; void *mem; } T;

static void tinit(T *t, int cols, int rows, size_t sb)
{
	size_t n = mt_vt_mem_size((uint16_t)cols, (uint16_t)rows, sb);
	t->mem = malloc(n);
	if (!t->mem || mt_vt_init(&t->vt, (uint16_t)cols, (uint16_t)rows, sb, t->mem, n) != 0) { printf("init failed\n"); exit(2); }
}
static void tfree(T *t) { free(t->mem); }
static void feeds(T *t, const char *s) { mt_vt_feed(&t->vt, (const uint8_t *)s, strlen(s)); }

static char jbuf[1 << 16];

static const char *snap(T *t)
{
	mt_jw w;
	const char *e;
	mt_jw_init(&w, jbuf, sizeof jbuf);
	if (mt_vt_screen_json(&t->vt, &w) != 0) FAIL("screen_json failed");
	if ((e = validate(jbuf, w.len, t->vt.rows)) != NULL) FAIL("invalid snapshot (%s): %s", e, jbuf);
	return jbuf;
}

static const char *cap(T *t, int lines)
{
	mt_jw w;
	const char *e;
	mt_jw_init(&w, jbuf, sizeof jbuf);
	if (mt_vt_capture_json(&t->vt, lines, &w) != 0) FAIL("capture_json failed");
	if ((e = validate(jbuf, w.len, 0)) != NULL) FAIL("invalid capture (%s): %s", e, jbuf);
	return jbuf;
}

static void check_snap(int cols, int rows, const char *in, const char *want)
{
	T t;
	const char *got;
	tinit(&t, cols, rows, 1024);
	feeds(&t, in);
	got = snap(&t);
	if (strcmp(got, want) != 0) FAIL("input %s\n  got  %s\n  want %s", in, got, want);
	tfree(&t);
}

#define H10x3 "{\"width\":10,\"height\":3,\"lines\":"
#define CUR(r, c) ",\"cursor\":{\"row\":" #r ",\"col\":" #c "}}"
#define NOSPAN "],\"spans\":[]"

static void test_basic(void)
{
	check_snap(10, 3, "", H10x3 "[\"\",\"\",\"\"" NOSPAN CUR(0, 0));
	check_snap(10, 3, "hello", H10x3 "[\"hello\",\"\",\"\"" NOSPAN CUR(0, 5));
	check_snap(10, 3, "0123456789", H10x3 "[\"0123456789\",\"\",\"\"" NOSPAN CUR(0, 9)); /* deferred wrap */
	check_snap(10, 3, "0123456789AB", H10x3 "[\"0123456789\",\"AB\",\"\"" NOSPAN CUR(1, 2));
	check_snap(10, 3, "ab\r\ncd", H10x3 "[\"ab\",\"cd\",\"\"" NOSPAN CUR(1, 2));
	check_snap(10, 3, "ab\ncd", H10x3 "[\"ab\",\"  cd\",\"\"" NOSPAN CUR(1, 4)); /* LF keeps the column */
	check_snap(10, 3, "ab\x0b" "c\x0c" "d\x07", H10x3 "[\"ab\",\"  c\",\"   d\"" NOSPAN CUR(2, 4));
	check_snap(10, 3, "ab\bX", H10x3 "[\"aX\",\"\",\"\"" NOSPAN CUR(0, 2));
	check_snap(10, 3, "a\tb\t\tc", H10x3 "[\"a       bc\",\"\",\"\"" NOSPAN CUR(0, 9));
}

static void test_cursor_erase(void)
{
	check_snap(10, 3, "\033[2;3Hx", H10x3 "[\"\",\"  x\",\"\"" NOSPAN CUR(1, 3));
	check_snap(10, 3, "\033[3;1H\033[Ay\033[2Cz\033[1Bq", H10x3 "[\"\",\"y  z\",\"    q\"" NOSPAN CUR(2, 5));
	check_snap(10, 3, "abcdef\033[1;3H\033[K", H10x3 "[\"ab\",\"\",\"\"" NOSPAN CUR(0, 2));
	check_snap(10, 3, "abcdef\033[1;3H\033[1K", H10x3 "[\"   def\",\"\",\"\"" NOSPAN CUR(0, 2));
	check_snap(10, 3, "abc\r\ndef\r\nghi\033[2;2H\033[J", H10x3 "[\"abc\",\"d\",\"\"" NOSPAN CUR(1, 1));
	check_snap(10, 3, "abc\r\ndef\033[2J\033[H" "z", H10x3 "[\"z\",\"\",\"\"" NOSPAN CUR(0, 1));
	check_snap(10, 3, "abcdef\033[1;2H\033[2P", H10x3 "[\"adef\",\"\",\"\"" NOSPAN CUR(0, 1));
	check_snap(10, 3, "abcdef\033[1;2H\033[2@", H10x3 "[\"a  bcdef\",\"\",\"\"" NOSPAN CUR(0, 1));
	check_snap(10, 3, "abcdef\033[1;2H\033[3X", H10x3 "[\"a   ef\",\"\",\"\"" NOSPAN CUR(0, 1));
	check_snap(10, 3, "a\r\nb\r\nc\033[2;1H\033[L", H10x3 "[\"a\",\"\",\"b\"" NOSPAN CUR(1, 0));
	check_snap(10, 3, "a\r\nb\r\nc\033[2;1H\033[M", H10x3 "[\"a\",\"c\",\"\"" NOSPAN CUR(1, 0));
	check_snap(10, 3, "a\r\nb\r\nc\033[S", H10x3 "[\"b\",\"c\",\"\"" NOSPAN CUR(2, 1));
	check_snap(10, 3, "a\r\nb\r\nc\033[T", H10x3 "[\"\",\"a\",\"b\"" NOSPAN CUR(2, 1));
	check_snap(10, 3, "a\033" "7\033[3;5Hz\033" "8b", H10x3 "[\"ab\",\"\",\"    z\"" NOSPAN CUR(0, 2));
	check_snap(10, 3, "a\r\nb\033M\033Mz", H10x3 "[\" z\",\"a\",\"b\"" NOSPAN CUR(0, 2));
	check_snap(10, 3, "ab\033" "Dc\033" "Ed", H10x3 "[\"ab\",\"  c\",\"d\"" NOSPAN CUR(2, 1));
	check_snap(10, 3, "ab\033(0qq\033(B" "c\033)Bd", H10x3 "[\"abqqcd\",\"\",\"\"" NOSPAN CUR(0, 6));
	check_snap(10, 3, "ab\033[6n\033[?25l", H10x3 "[\"ab\",\"\",\"\"" NOSPAN ",\"cursor\":null}");
	check_snap(10, 3, "abc\033" "cz", H10x3 "[\"z\",\"\",\"\"" NOSPAN CUR(0, 1));
	check_snap(5, 2, "abcde\033[?7lfg", "{\"width\":5,\"height\":2,\"lines\":[\"abcdg\",\"\"" NOSPAN CUR(0, 4));
}

static void test_region(void)
{
	/* 10x4, region rows 2..3 (1-based): scrolling stays inside, row 1 and 4 are untouched */
	check_snap(10, 4, "T\033[4;1HB\033[2;3r\033[2;1Ha\r\nb\r\nc",
		   "{\"width\":10,\"height\":4,\"lines\":[\"T\",\"b\",\"c\",\"B\"" NOSPAN CUR(2, 1));
	T t;
	tinit(&t, 10, 4, 256);
	feeds(&t, "T\033[2;3r\033[2;1Ha\r\nb\r\nc\r\nd");
	if (strcmp(cap(&t, 10), "{\"width\":10,\"height\":0,\"lines\":[\"T\",\"c\",\"d\"],\"spans\":[],\"cursor\":null}") != 0)
		FAIL("region scroll must not enter scrollback: %s", jbuf);
	tfree(&t);
}

static void test_sgr(void)
{
	check_snap(10, 3, "\033[1;31mRed\033[0m ok",
		   H10x3 "[\"Red ok\",\"\",\"\"],\"spans\":[{\"line\":0,\"start\":0,\"end\":3,\"fg\":\"p1\",\"bold\":true}]" CUR(0, 6));
	check_snap(10, 3, "\033[38;5;200mA\033[48;2;1;2;255mB\033[39;49mC",
		   H10x3 "[\"ABC\",\"\",\"\"],\"spans\":[{\"line\":0,\"start\":0,\"end\":1,\"fg\":\"p200\"},"
		   "{\"line\":0,\"start\":1,\"end\":2,\"fg\":\"p200\",\"bg\":\"#0102ff\"}]" CUR(0, 3));
	check_snap(10, 3, "\033[92;104;2;3;4;7;9mx\033[22;23;24;27;29my",
		   H10x3 "[\"xy\",\"\",\"\"],\"spans\":[{\"line\":0,\"start\":0,\"end\":1,\"fg\":\"p10\",\"bg\":\"p12\","
		   "\"dim\":true,\"italic\":true,\"underline\":true,\"inverse\":true,\"strike\":true},"
		   "{\"line\":0,\"start\":1,\"end\":2,\"fg\":\"p10\",\"bg\":\"p12\"}]" CUR(0, 2));
	/* trailing styled blanks are trimmed unless they are visible (background colour) */
	check_snap(10, 3, "\033[31mab   ", H10x3 "[\"ab\",\"\",\"\"],\"spans\":[{\"line\":0,\"start\":0,\"end\":2,\"fg\":\"p1\"}]" CUR(0, 5));
	check_snap(10, 3, "\033[41mab  ", H10x3 "[\"ab  \",\"\",\"\"],\"spans\":[{\"line\":0,\"start\":0,\"end\":4,\"bg\":\"p1\"}]" CUR(0, 4));
	/* background erase */
	check_snap(10, 3, "\033[44mx\033[2K", H10x3 "[\"          \",\"\",\"\"],\"spans\":[{\"line\":0,\"start\":0,\"end\":10,\"bg\":\"p4\"}]" CUR(0, 1));
	/* malformed colours are ignored */
	check_snap(10, 3, "\033[38;5;999mx\033[38;2;1;2mq", H10x3 "[\"xq\",\"\",\"\"" NOSPAN CUR(0, 2));
}

static void test_utf8(void)
{
	check_snap(10, 3, "\xc3\xa9\xe2\x82\xac\xf0\x9f\x98\x80", H10x3 "[\"\xc3\xa9\xe2\x82\xac\xf0\x9f\x98\x80\",\"\",\"\"" NOSPAN CUR(0, 3));
	check_snap(10, 3, "\xe6\x97\xa5\xe6\x9c\xac", H10x3 "[\"\xe6\x97\xa5\xe6\x9c\xac\",\"\",\"\"" NOSPAN CUR(0, 2));
	/* span offsets are runes, not cells */
	check_snap(10, 3, "\xe6\x97\xa5\033[31mx",
		   H10x3 "[\"\xe6\x97\xa5x\",\"\",\"\"],\"spans\":[{\"line\":0,\"start\":1,\"end\":2,\"fg\":\"p1\"}]" CUR(0, 2));
	/* wide char that does not fit wraps; overwriting half of one blanks the other half */
	check_snap(4, 2, "abc\xe6\x97\xa5", "{\"width\":4,\"height\":2,\"lines\":[\"abc\",\"\xe6\x97\xa5\"" NOSPAN CUR(1, 1));
	check_snap(10, 3, "\xe6\x97\xa5\033[1;2Hx", H10x3 "[\" x\",\"\",\"\"" NOSPAN CUR(0, 2));
	/* invalid UTF-8 */
	check_snap(10, 3, "a\xff" "b\xc3(", H10x3 "[\"a\xef\xbf\xbd" "b\xef\xbf\xbd(\",\"\",\"\"" NOSPAN CUR(0, 5));
	check_snap(10, 3, "\xe2\x82" "x\xc0\x80\xed\xa0\x80", H10x3 "[\"\xef\xbf\xbdx\xef\xbf\xbd\xef\xbf\xbd\xef\xbf\xbd\",\"\",\"\"" NOSPAN CUR(0, 5));
	/* split sequences and characters across feeds */
	{
		T t;
		tinit(&t, 10, 3, 64);
		feeds(&t, "\xe2\x82"); feeds(&t, "\xac\033"); feeds(&t, "[3"); feeds(&t, "1mx");
		if (strcmp(snap(&t), H10x3 "[\"\xe2\x82\xac" "x\",\"\",\"\"],\"spans\":[{\"line\":0,\"start\":1,\"end\":2,\"fg\":\"p1\"}]" CUR(0, 2)) != 0)
			FAIL("split feed: %s", jbuf);
		tfree(&t);
	}
	/* C1 controls and control characters never reach a line */
	check_snap(10, 3, "a\xc2\x85" "b\x01\x7f" "c\xe2\x80\xae" "d", H10x3 "[\"abcd\",\"\",\"\"" NOSPAN CUR(0, 4));
	check_snap(10, 3, "a\"b\\c", H10x3 "[\"a\\\"b\\\\c\",\"\",\"\"" NOSPAN CUR(0, 5));
}

static void test_strings(void)
{
	check_snap(10, 3, "\033]0;title\x07hi\033]2;x\033\\!", H10x3 "[\"hi!\",\"\",\"\"" NOSPAN CUR(0, 3));
	check_snap(10, 3, "a\033Pqdata\033\\b\033_apc\x07" "c\033^pm\033\\d", H10x3 "[\"abcd\",\"\",\"\"" NOSPAN CUR(0, 4));
	check_snap(10, 3, "a\033]0;never ended", H10x3 "[\"a\",\"\",\"\"" NOSPAN CUR(0, 1));
}

static void test_alt(void)
{
	T t;
	tinit(&t, 10, 3, 256);
	feeds(&t, "abc\r\n");
	feeds(&t, "\033[?1049hxyz\r\n1\r\n2\r\n3\r\n4");
	if (strcmp(snap(&t), H10x3 "[\"2\",\"3\",\"4\"" NOSPAN CUR(2, 1)) != 0) FAIL("alt: %s", jbuf);
	feeds(&t, "\033[?1049l");
	if (strcmp(snap(&t), H10x3 "[\"\",\"\",\"\"" NOSPAN CUR(1, 0)) != 0) FAIL("alt exit: %s", jbuf); /* cursor restored */
	if (strcmp(cap(&t, 10), "{\"width\":10,\"height\":0,\"lines\":[],\"spans\":[],\"cursor\":null}") != 0)
		FAIL("alt screen must not reach scrollback: %s", jbuf);
	tfree(&t);
}

static void test_capture(void)
{
	T t;
	tinit(&t, 10, 3, 1024);
	feeds(&t, "1\r\n2\r\n3\r\n4\r\n5");
	if (strcmp(snap(&t), H10x3 "[\"3\",\"4\",\"5\"" NOSPAN CUR(2, 1)) != 0) FAIL("scroll: %s", jbuf);
	if (strcmp(cap(&t, 10), "{\"width\":10,\"height\":0,\"lines\":[\"1\",\"2\",\"3\",\"4\",\"5\"],\"spans\":[],\"cursor\":null}") != 0) FAIL("cap10: %s", jbuf);
	if (strcmp(cap(&t, 2), "{\"width\":10,\"height\":0,\"lines\":[\"4\",\"5\"],\"spans\":[],\"cursor\":null}") != 0) FAIL("cap2: %s", jbuf);
	if (strcmp(cap(&t, 4), "{\"width\":10,\"height\":0,\"lines\":[\"2\",\"3\",\"4\",\"5\"],\"spans\":[],\"cursor\":null}") != 0) FAIL("cap4: %s", jbuf);
	/* styled visible rows keep spans with capture-relative line numbers */
	feeds(&t, "\r\n\033[1mB");
	if (strcmp(cap(&t, 2), "{\"width\":10,\"height\":0,\"lines\":[\"5\",\"B\"],\"spans\":[{\"line\":1,\"start\":0,\"end\":1,\"bold\":true}],\"cursor\":null}") != 0) FAIL("cap styled: %s", jbuf);
	/* trailing blank rows do not count */
	feeds(&t, "\033[H\033[J");
	if (strcmp(cap(&t, 2), "{\"width\":10,\"height\":0,\"lines\":[\"2\",\"3\"],\"spans\":[],\"cursor\":null}") != 0) FAIL("cap blank tail: %s", jbuf);
	{
		mt_jw w;
		mt_jw_init(&w, jbuf, sizeof jbuf);
		if (mt_vt_capture_json(&t.vt, 0, &w) != -1) FAIL("capture 0 lines must fail");
		mt_jw_init(&w, jbuf, sizeof jbuf);
		if (mt_vt_capture_json(&t.vt, MT_VT_MAX_CAPTURE + 1, &w) != -1) FAIL("capture too many lines must fail");
		mt_jw_init(&w, jbuf, 8);
		if (mt_vt_screen_json(&t.vt, &w) != -1) FAIL("overflow must be reported");
	}
	feeds(&t, "\033[3J");
	tfree(&t);

	/* tiny scrollback: oldest lines are dropped whole */
	tinit(&t, 10, 2, 8);
	feeds(&t, "aa\r\nbb\r\ncc\r\ndd\r\nee\r\nff\r\ngg");
	if (strcmp(cap(&t, 50), "{\"width\":10,\"height\":0,\"lines\":[\"dd\",\"ee\",\"ff\",\"gg\"],\"spans\":[],\"cursor\":null}") != 0) FAIL("ring: %s", jbuf);
	feeds(&t, "\033[3J");
	if (strcmp(cap(&t, 50), "{\"width\":10,\"height\":0,\"lines\":[\"ff\",\"gg\"],\"spans\":[],\"cursor\":null}") != 0) FAIL("after 3J: %s", jbuf);
	tfree(&t);
	/* no scrollback at all */
	tinit(&t, 10, 2, 0);
	feeds(&t, "a\r\nb\r\nc");
	if (strcmp(cap(&t, 5), "{\"width\":10,\"height\":0,\"lines\":[\"b\",\"c\"],\"spans\":[],\"cursor\":null}") != 0) FAIL("no sb: %s", jbuf);
	tfree(&t);
}

static void test_hash_gen(void)
{
	T a, b;
	char h1[65], h2[65], h3[65];
	uint32_t g;
	tinit(&a, 10, 3, 64);
	tinit(&b, 10, 3, 64);
	mt_vt_hash(&a.vt, h1);
	if (strlen(h1) != 64) FAIL("hash length");
	g = mt_vt_generation(&a.vt);
	feeds(&a, "");
	if (mt_vt_generation(&a.vt) != g) FAIL("empty feed changed generation");
	feeds(&a, "\x07\033]0;t\x07");
	if (mt_vt_generation(&a.vt) != g) FAIL("invisible feed changed generation");
	feeds(&a, "x");
	if (mt_vt_generation(&a.vt) == g) FAIL("text did not change generation");
	mt_vt_hash(&a.vt, h2);
	if (strcmp(h1, h2) == 0) FAIL("hash did not change");
	feeds(&b, "x");
	mt_vt_hash(&b.vt, h3);
	if (strcmp(h2, h3) != 0) FAIL("equal screens, different hash");
	g = mt_vt_generation(&a.vt);
	feeds(&a, "\033[1;1H"); /* cursor move only */
	if (mt_vt_generation(&a.vt) == g) FAIL("cursor move did not change generation");
	mt_vt_hash(&a.vt, h1);
	if (strcmp(h1, h2) == 0) FAIL("cursor not in hash");
	feeds(&a, "\033[1m\033[1;1Hx"); /* same text, now bold */
	mt_vt_hash(&a.vt, h1);
	feeds(&b, "\033[1;1H");
	feeds(&b, "x");
	mt_vt_hash(&b.vt, h3);
	if (strcmp(h1, h3) == 0) FAIL("attributes not in hash");
	g = mt_vt_generation(&a.vt);
	mt_vt_reset(&a.vt);
	if (mt_vt_generation(&a.vt) == g) FAIL("reset did not change generation");
	mt_vt_hash(&a.vt, h1);
	mt_vt_hash(&b.vt, h2);
	tfree(&a);
	tfree(&b);
}

static void test_init(void)
{
	static uint8_t buf[70000];
	mt_vt vt;
	size_t n = mt_vt_mem_size(80, 24, 4096);
	if (mt_vt_mem_size(0, 24, 0) || mt_vt_mem_size(80, 0, 0) || mt_vt_mem_size(300, 24, 0) || mt_vt_mem_size(80, 300, 0))
		FAIL("bad sizes must give 0");
	if (mt_vt_init(&vt, 80, 24, 4096, buf, n - 1) == 0) FAIL("short memory accepted");
	if (mt_vt_init(&vt, 80, 24, 4096, NULL, n) == 0) FAIL("NULL memory accepted");
	if (mt_vt_init(&vt, 80, 24, 4096, buf + 1, n) != 0) FAIL("unaligned memory rejected");
	mt_vt_feed(&vt, (const uint8_t *)"x\r\ny", 4);
	if (mt_vt_init(&vt, 1, 1, 0, buf, mt_vt_mem_size(1, 1, 0)) != 0) FAIL("1x1");
	mt_vt_feed(&vt, (const uint8_t *)"ab\r\n\xe6\x97\xa5\033[2;2r\033[L\033[P\033[@\033[X\033M\033[5T\033[5S", 28);
	printf("sizeof(mt_vt)=%zu mem(80,24,4096)=%zu mem(132,43,32768)=%zu\n", sizeof(mt_vt), n, mt_vt_mem_size(132, 43, 32768));
}

/* ---------------------------------------------------------------- fuzz */

static uint32_t rng = 0x12345678u;
static uint32_t rnd(void) { rng ^= rng << 13; rng ^= rng >> 17; rng ^= rng << 5; return rng; }

static void fuzz(void)
{
	static const char *frag[] = {"\033[", "\033]", "\033", "\x07", "\033\\", "m", "H", "J", "K", "L", "M", "P", "@", "X", "S", "T", "r", "h", "l", "?", ";",
		"38;5;", "48;2;", "1049", "25", "7", "99999", "0", "1", "2", "\r\n", "\n", "\t", "\x08", "\xe6\x97\xa5", "\xf0\x9f\x98\x80", "\xc3", "\xff", "abc ", "\033[3J", "\033c",
		"\033P", "\033(", "\x18", "\x1a", "\x7f", "\xc2\x85", "\0337", "\0338", "\033M", "\033D", "\033E", "\033[?1049h", "\033[?1049l"};
	static const int sizes[][2] = {{80, 24}, {1, 1}, {2, 2}, {3, 5}, {132, 43}, {10, 3}, {256, 4}, {7, 255}, {1, 20}, {20, 1}};
	clock_t end = clock() + 3 * CLOCKS_PER_SEC;
	unsigned long iters = 0, rounds = 0;
	int before = fails;
	while (clock() < end) {
		const int *sz = sizes[rnd() % (sizeof sizes / sizeof sizes[0])];
		size_t sb = (rnd() & 1) ? (size_t)(rnd() % 4096) : 0;
		T t;
		int step;
		tinit(&t, sz[0], sz[1], sb);
		for (step = 0; step < 300; step++) {
			uint8_t chunk[256];
			size_t n = 0, i;
			int count = (int)(rnd() % 12);
			for (i = 0; i < (size_t)count && n < 200; i++) {
				if (rnd() % 4 == 0) { chunk[n++] = (uint8_t)rnd(); }
				else { const char *f = frag[rnd() % (sizeof frag / sizeof frag[0])]; size_t l = strlen(f); memcpy(chunk + n, f, l); n += l; }
			}
			mt_vt_feed(&t.vt, chunk, n);
			iters++;
			if (step % 25 == 0) {
				char h[65];
				mt_vt_hash(&t.vt, h);
				snap(&t);
				cap(&t, 1 + (int)(rnd() % 400));
				if (fails != before) { printf("fuzz stopped after failure (round %lu step %d size %dx%d)\n", rounds, step, sz[0], sz[1]); tfree(&t); return; }
			}
		}
		tfree(&t);
		rounds++;
	}
	printf("fuzz: %lu rounds, %lu feeds\n", rounds, iters);
}

int main(void)
{
	test_basic();
	test_cursor_erase();
	test_region();
	test_sgr();
	test_utf8();
	test_strings();
	test_alt();
	test_capture();
	test_hash_gen();
	test_init();
	fuzz();
	if (fails) { printf("test_vt: %d failure(s)\n", fails); return 1; }
	printf("test_vt: PASS\n");
	return 0;
}
