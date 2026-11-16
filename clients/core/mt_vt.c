#include "mt_vt.h"

#include <string.h>

#include "mt_crypto.h"

enum { ST_GROUND, ST_ESC, ST_ESCI, ST_CSI, ST_STR, ST_STRE };
enum { A_BOLD = 1, A_DIM = 2, A_ITAL = 4, A_UL = 8, A_INV = 16, A_STRIKE = 32 };

#define C_RGB 0x1000000u
#define C_PAL 0x2000000u
#define CP_MASK 0x1FFFFFu
#define F_WIDE (1u << 21)
#define F_CONT (1u << 22)
#define SI_SHIFT 23
#define SI(c) ((unsigned)((c) >> SI_SHIFT))
#define CELL(cp, fl, si) ((uint32_t)(cp) | (fl) | ((uint32_t)(si) << SI_SHIFT))
#define NO_SI 0xFFFFu
#define ROWBUF (MT_VT_MAX_COLS * 4 + 4)

/* ------------------------------------------------------------ styles */

static void style_gc(mt_vt *v)
{
	uint64_t used = 1;
	size_t i, n = (size_t)v->cols * v->rows;
	for (i = 0; i < n; i++)
		used |= (uint64_t)1 << SI(v->cells[i]);
	v->alloc = used;
	v->cur_si = NO_SI;
}

/* Index of the style (fg, bg, attr), allocating one; 0 (default) when the table is exhausted. */
static unsigned style_idx(mt_vt *v, uint32_t fg, uint32_t bg, uint8_t at)
{
	unsigned i, pass;
	if (!(fg | bg | at))
		return 0;
	for (i = 1; i < MT_VT_NSTYLE; i++)
		if (((v->alloc >> i) & 1) && v->styles[i].fg == fg && v->styles[i].bg == bg && v->styles[i].attr == at)
			return i;
	for (pass = 0; pass < 2; pass++) {
		for (i = 1; i < MT_VT_NSTYLE; i++)
			if (!((v->alloc >> i) & 1)) {
				v->styles[i].fg = fg;
				v->styles[i].bg = bg;
				v->styles[i].attr = at;
				v->alloc |= (uint64_t)1 << i;
				return i;
			}
		if (pass == 0)
			style_gc(v);
	}
	return 0;
}

static unsigned cur_style(mt_vt *v)
{
	if (v->cur_si == NO_SI)
		v->cur_si = (uint16_t)style_idx(v, v->fg, v->bg, v->attr);
	return v->cur_si;
}

/* Erased cells keep the current background colour only. */
static uint32_t blank(mt_vt *v) { return CELL(' ', 0, style_idx(v, 0, v->bg, 0)); }

/* ------------------------------------------------------------ grid */

static void setc(mt_vt *v, unsigned x, unsigned y, uint32_t c)
{
	uint32_t *p = &v->cells[(size_t)y * v->cols + x];
	if (*p != c) {
		*p = c;
		v->dirty = 1;
	}
}

/* Make wide characters consistent in one row: a head needs its continuation and vice versa. */
static void fix_row(mt_vt *v, unsigned y)
{
	uint32_t *r = v->cells + (size_t)y * v->cols;
	unsigned x, c = v->cols;
	for (x = 0; x < c; x++) {
		if ((r[x] & F_WIDE) && (x + 1 >= c || !(r[x + 1] & F_CONT)))
			r[x] = CELL(' ', 0, SI(r[x]));
		else if ((r[x] & F_CONT) && (x == 0 || !(r[x - 1] & F_WIDE)))
			r[x] = CELL(' ', 0, SI(r[x]));
	}
}

static void erase(mt_vt *v, unsigned y, unsigned a, unsigned b) /* cells [a,b) of row y */
{
	uint32_t bl = blank(v);
	unsigned x;
	if (b > v->cols)
		b = v->cols;
	for (x = a; x < b; x++)
		setc(v, x, y, bl);
	fix_row(v, y);
}

static unsigned cell_trim(const mt_vt *v, uint32_t c) /* 1 when a trailing cell is invisible */
{
	const mt_vt_style *s = &v->styles[SI(c)];
	return (c & (CP_MASK | F_WIDE | F_CONT)) == ' ' && !s->bg && !(s->attr & (A_INV | A_UL | A_STRIKE));
}

static unsigned row_end(const mt_vt *v, unsigned y) /* cells to keep after trimming */
{
	const uint32_t *r = v->cells + (size_t)y * v->cols;
	unsigned e = v->cols;
	while (e && cell_trim(v, r[e - 1]))
		e--;
	return e;
}

static size_t enc8(uint32_t c, char *o)
{
	if (c < 0x80) { o[0] = (char)c; return 1; }
	if (c < 0x800) { o[0] = (char)(0xC0 | c >> 6); o[1] = (char)(0x80 | (c & 63)); return 2; }
	if (c < 0x10000) {
		o[0] = (char)(0xE0 | c >> 12); o[1] = (char)(0x80 | ((c >> 6) & 63)); o[2] = (char)(0x80 | (c & 63));
		return 3;
	}
	o[0] = (char)(0xF0 | c >> 18); o[1] = (char)(0x80 | ((c >> 12) & 63));
	o[2] = (char)(0x80 | ((c >> 6) & 63)); o[3] = (char)(0x80 | (c & 63));
	return 4;
}

static size_t row_text(const mt_vt *v, unsigned y, char *out) /* trimmed UTF-8 text, returns bytes */
{
	const uint32_t *r = v->cells + (size_t)y * v->cols;
	unsigned x, e = row_end(v, y);
	size_t n = 0;
	for (x = 0; x < e; x++)
		if (!(r[x] & F_CONT))
			n += enc8(r[x] & CP_MASK, out + n);
	return n;
}

/* The cursor column as a rune offset in its line (internal/ops/screen.go): wide characters take two cells but one
 * rune; cells past the end of the text count one rune each. */
static unsigned cursor_rune_col(const mt_vt *v)
{
	const uint32_t *r = v->cells + (size_t)v->cy * v->cols;
	unsigned x, n = 0;
	for (x = 0; x < v->cx && x < v->cols; x++)
		if (!(r[x] & F_CONT))
			n++;
	return n;
}

static void sb_push(mt_vt *v, unsigned y)
{
	char buf[ROWBUF];
	size_t n = row_text(v, y, buf), i;
	if (n + 1 > v->sb_cap)
		return;
	while (v->sb_cap - v->sb_len < n + 1) { /* drop the oldest line */
		size_t k = 0;
		while (k < v->sb_len && v->sb[(v->sb_head + k) % v->sb_cap] != '\n')
			k++;
		if (k < v->sb_len)
			k++;
		v->sb_head = (v->sb_head + k) % v->sb_cap;
		v->sb_len -= k;
	}
	buf[n++] = '\n';
	for (i = 0; i < n; i++)
		v->sb[(v->sb_head + v->sb_len++) % v->sb_cap] = (uint8_t)buf[i];
}

/* Scroll rows [t,b] up by n (content moves up, blank rows enter at the bottom). */
static void scroll_up(mt_vt *v, unsigned t, unsigned b, unsigned n, int hist)
{
	unsigned i, h = b - t + 1, c = v->cols, x;
	uint32_t bl;
	if (n > h)
		n = h;
	if (n == 0)
		return;
	if (hist && !v->alt && t == 0 && b == (unsigned)v->rows - 1)
		for (i = 0; i < n; i++)
			sb_push(v, i);
	memmove(v->cells + (size_t)t * c, v->cells + (size_t)(t + n) * c, (size_t)(h - n) * c * sizeof(uint32_t));
	bl = blank(v);
	for (i = b + 1 - n; i <= b; i++)
		for (x = 0; x < c; x++)
			v->cells[(size_t)i * c + x] = bl;
	v->dirty = 1;
}

static void scroll_down(mt_vt *v, unsigned t, unsigned b, unsigned n)
{
	unsigned i, h = b - t + 1, c = v->cols, x;
	uint32_t bl;
	if (n > h)
		n = h;
	if (n == 0)
		return;
	memmove(v->cells + (size_t)(t + n) * c, v->cells + (size_t)t * c, (size_t)(h - n) * c * sizeof(uint32_t));
	bl = blank(v);
	for (i = t; i < t + n; i++)
		for (x = 0; x < c; x++)
			v->cells[(size_t)i * c + x] = bl;
	v->dirty = 1;
}

static void index_down(mt_vt *v)
{
	if (v->cy == v->bot)
		scroll_up(v, v->top, v->bot, 1, 1);
	else if (v->cy + 1 < v->rows)
		v->cy++;
}

static void index_up(mt_vt *v)
{
	if (v->cy == v->top)
		scroll_down(v, v->top, v->bot, 1);
	else if (v->cy > 0)
		v->cy--;
}

static int wide(uint32_t c) /* 0 zero width, 1, 2 */
{
	static const uint32_t w2[][2] = {
		{0x1100, 0x115f}, {0x2e80, 0x303e}, {0x3041, 0x33ff}, {0x3400, 0x4dbf}, {0x4e00, 0x9fff},
		{0xa000, 0xa4cf}, {0xa960, 0xa97f}, {0xac00, 0xd7a3}, {0xf900, 0xfaff}, {0xfe10, 0xfe19},
		{0xfe30, 0xfe6f}, {0xff00, 0xff60}, {0xffe0, 0xffe6}, {0x1f300, 0x1f64f}, {0x1f680, 0x1f6ff},
		{0x1f900, 0x1f9ff}, {0x1fa70, 0x1faff}, {0x20000, 0x3fffd}};
	static const uint32_t w0[][2] = {
		{0x300, 0x36f}, {0x483, 0x489}, {0x591, 0x5bd}, {0x610, 0x61a}, {0x64b, 0x65f}, {0x1160, 0x11ff},
		{0x1ab0, 0x1aff}, {0x1dc0, 0x1dff}, {0x200b, 0x200f}, {0x20d0, 0x20ff}, {0xfe00, 0xfe0f},
		{0xfe20, 0xfe2f}, {0xfeff, 0xfeff}, {0xe0100, 0xe01ef}};
	size_t i;
	if (c < 0x300)
		return c == 0xad ? 0 : 1;
	for (i = 0; i < sizeof w0 / sizeof w0[0]; i++)
		if (c >= w0[i][0] && c <= w0[i][1])
			return 0;
	for (i = 0; i < sizeof w2 / sizeof w2[0]; i++)
		if (c >= w2[i][0] && c <= w2[i][1])
			return 2;
	return 1;
}

static void put(mt_vt *v, uint32_t cp)
{
	int w;
	unsigned si, y;
	if ((cp >= 0x80 && cp <= 0x9f) || (cp >= 0x202a && cp <= 0x202e) || (cp >= 0x2066 && cp <= 0x2069) ||
	    (cp >= 0xe0000 && cp <= 0xe007f))
		return; /* the Go sanitiser drops these as well */
	w = wide(cp);
	if (w == 0)
		return; /* combining marks are not stored */
	if (v->wrap) {
		v->wrap = 0;
		if (v->autowrap) {
			v->cx = 0;
			index_down(v);
		}
	}
	if (w == 2 && v->cx + 1 >= v->cols) {
		if (v->cols < 2 || !v->autowrap)
			return;
		setc(v, v->cx, v->cy, CELL(' ', 0, 0));
		v->cx = 0;
		index_down(v);
	}
	y = v->cy;
	si = cur_style(v);
	setc(v, v->cx, y, CELL(cp, w == 2 ? F_WIDE : 0, si));
	if (w == 2)
		setc(v, v->cx + 1u, y, CELL(0, F_CONT, si));
	fix_row(v, y);
	v->cx = (uint16_t)(v->cx + w);
	if (v->cx >= v->cols) {
		v->cx = (uint16_t)(v->cols - 1);
		v->wrap = 1;
	}
}

static void clear_all(mt_vt *v)
{
	unsigned y;
	for (y = 0; y < v->rows; y++)
		erase(v, y, 0, v->cols);
}

static void soft_reset(mt_vt *v)
{
	v->alloc = 1;
	memset(&v->styles[0], 0, sizeof v->styles[0]);
	v->fg = v->bg = 0;
	v->attr = 0;
	v->cur_si = NO_SI;
	v->cx = v->cy = 0;
	v->top = 0;
	v->bot = (uint16_t)(v->rows - 1);
	v->wrap = 0;
	v->autowrap = v->vis = 1;
	v->alt = 0;
	v->st = ST_GROUND;
	v->u_need = 0;
	v->sfg = v->sbg = 0;
	v->sattr = v->swrap = 0;
	v->scx = v->scy = 0;
	clear_all(v);
}

static void save_cur(mt_vt *v)
{
	v->scx = v->cx; v->scy = v->cy; v->sfg = v->fg; v->sbg = v->bg; v->sattr = v->attr; v->swrap = v->wrap;
}

static void restore_cur(mt_vt *v)
{
	v->cx = v->scx < v->cols ? v->scx : (uint16_t)(v->cols - 1);
	v->cy = v->scy < v->rows ? v->scy : (uint16_t)(v->rows - 1);
	v->fg = v->sfg; v->bg = v->sbg; v->attr = v->sattr; v->wrap = v->swrap;
	v->cur_si = NO_SI;
}

/* ------------------------------------------------------------ escape sequences */

static unsigned arg(const mt_vt *v, unsigned i, unsigned def) { return i < v->np && v->p[i] > 0 ? v->p[i] : def; }
static unsigned clampu(unsigned x, unsigned lo, unsigned hi) { return x < lo ? lo : x > hi ? hi : x; }

static void sgr_color(mt_vt *v, unsigned *i, int is_fg)
{
	unsigned k = *i;
	uint32_t col = 0;
	if (k + 1 < v->np && v->p[k + 1] == 5) {
		if (k + 2 < v->np && v->p[k + 2] <= 255)
			col = C_PAL | v->p[k + 2];
		*i = k + 2;
	} else if (k + 1 < v->np && v->p[k + 1] == 2) {
		if (k + 4 < v->np && v->p[k + 2] <= 255 && v->p[k + 3] <= 255 && v->p[k + 4] <= 255)
			col = C_RGB | (uint32_t)v->p[k + 2] << 16 | (uint32_t)v->p[k + 3] << 8 | v->p[k + 4];
		*i = k + 4;
	} else {
		*i = v->np; /* malformed: skip the rest */
	}
	if (col) {
		if (is_fg) v->fg = col; else v->bg = col;
	}
}

static void do_sgr(mt_vt *v)
{
	unsigned i, c;
	if (v->np == 0) { v->fg = v->bg = 0; v->attr = 0; }
	for (i = 0; i < v->np; i++) {
		c = v->p[i];
		if (c == 0) { v->fg = v->bg = 0; v->attr = 0; }
		else if (c == 1) v->attr |= A_BOLD;
		else if (c == 2) v->attr |= A_DIM;
		else if (c == 3) v->attr |= A_ITAL;
		else if (c == 4) v->attr |= A_UL;
		else if (c == 7) v->attr |= A_INV;
		else if (c == 9) v->attr |= A_STRIKE;
		else if (c == 22) v->attr &= (uint8_t)~(A_BOLD | A_DIM);
		else if (c == 23) v->attr &= (uint8_t)~A_ITAL;
		else if (c == 24) v->attr &= (uint8_t)~A_UL;
		else if (c == 27) v->attr &= (uint8_t)~A_INV;
		else if (c == 29) v->attr &= (uint8_t)~A_STRIKE;
		else if (c >= 30 && c <= 37) v->fg = C_PAL | (c - 30);
		else if (c >= 40 && c <= 47) v->bg = C_PAL | (c - 40);
		else if (c >= 90 && c <= 97) v->fg = C_PAL | (c - 90 + 8);
		else if (c >= 100 && c <= 107) v->bg = C_PAL | (c - 100 + 8);
		else if (c == 39) v->fg = 0;
		else if (c == 49) v->bg = 0;
		else if (c == 38 || c == 48) sgr_color(v, &i, c == 38);
	}
	v->cur_si = NO_SI;
}

static void set_mode(mt_vt *v, unsigned mode, int on)
{
	switch (mode) {
	case 25: v->vis = (uint8_t)on; break;
	case 7: v->autowrap = (uint8_t)on; break;
	case 1048: if (on) save_cur(v); else restore_cur(v); break;
	case 47: case 1047: case 1049:
		if (on == (int)v->alt)
			break;
		if (mode == 1049 && on) save_cur(v);
		v->alt = (uint8_t)on;
		v->fg = v->bg = 0; v->attr = 0; v->cur_si = NO_SI;
		clear_all(v); /* the primary screen is not kept: the alternate screen starts and ends cleared */
		v->cx = v->cy = 0; v->wrap = 0;
		if (mode == 1049 && !on) restore_cur(v);
		break;
	}
}

static void erase_display(mt_vt *v, unsigned m)
{
	unsigned y;
	if (m == 3) { v->sb_len = v->sb_head = 0; return; }
	if (m == 0) {
		erase(v, v->cy, v->cx, v->cols);
		for (y = v->cy + 1u; y < v->rows; y++) erase(v, y, 0, v->cols);
	} else if (m == 1) {
		for (y = 0; y < v->cy; y++) erase(v, y, 0, v->cols);
		erase(v, v->cy, 0, v->cx + 1u);
	} else if (m == 2) {
		clear_all(v);
	}
}

static void csi_dispatch(mt_vt *v, uint8_t f)
{
	unsigned n = arg(v, 0, 1), c = v->cols, y = v->cy, x = v->cx, k;
	if (v->bad || v->inter)
		return;
	if (v->priv) {
		if (v->priv == '?' && (f == 'h' || f == 'l'))
			for (k = 0; k < v->np; k++) set_mode(v, v->p[k], f == 'h');
		return;
	}
	/* only sequences that move the cursor or edit the screen end a pending wrap; SGR, queries (n, c) and
	 * unknown finals leave it alone */
	if (strchr("ABCDEFGHJKLMPSTX@dfr", f) != NULL)
		v->wrap = 0;
	switch (f) {
	case 'A': v->cy = (uint16_t)(y >= v->top ? (y - v->top < n ? v->top : y - n) : (y < n ? 0 : y - n)); break;
	case 'B': v->cy = (uint16_t)(y <= v->bot ? (v->bot - y < n ? v->bot : y + n) : clampu(y + n, 0, v->rows - 1u)); break;
	case 'C': v->cx = (uint16_t)clampu(x + n, 0, c - 1u); break;
	case 'D': v->cx = (uint16_t)(x < n ? 0 : x - n); break;
	case 'E': v->cy = (uint16_t)clampu(y + n, 0, v->rows - 1u); v->cx = 0; break;
	case 'F': v->cy = (uint16_t)(y < n ? 0 : y - n); v->cx = 0; break;
	case 'G': v->cx = (uint16_t)clampu(n - 1, 0, c - 1u); break;
	case 'd': v->cy = (uint16_t)clampu(n - 1, 0, v->rows - 1u); break;
	case 'H': case 'f':
		v->cy = (uint16_t)clampu(n - 1, 0, v->rows - 1u);
		v->cx = (uint16_t)clampu(arg(v, 1, 1) - 1, 0, c - 1u);
		break;
	case 'J': erase_display(v, v->np ? v->p[0] : 0); break;
	case 'K': {
		unsigned m = v->np ? v->p[0] : 0;
		if (m == 0) erase(v, y, x, c); else if (m == 1) erase(v, y, 0, x + 1u); else if (m == 2) erase(v, y, 0, c);
		break;
	}
	case 'L': if (y >= v->top && y <= v->bot) scroll_down(v, y, v->bot, n); break;
	case 'M': if (y >= v->top && y <= v->bot) scroll_up(v, y, v->bot, n, 0); break;
	case 'P': case '@': {
		uint32_t *r = v->cells + (size_t)y * c, bl = blank(v);
		unsigned i;
		if (n > c - x) n = c - x;
		if (f == 'P') {
			memmove(r + x, r + x + n, (size_t)(c - x - n) * sizeof *r);
			for (i = c - n; i < c; i++) r[i] = bl;
		} else {
			memmove(r + x + n, r + x, (size_t)(c - x - n) * sizeof *r);
			for (i = x; i < x + n; i++) r[i] = bl;
		}
		fix_row(v, y);
		v->dirty = 1;
		break;
	}
	case 'X': erase(v, y, x, x + n); break;
	case 'S': scroll_up(v, v->top, v->bot, n, 1); break;
	case 'T': scroll_down(v, v->top, v->bot, n); break;
	case 'r': {
		unsigned t = arg(v, 0, 1), b = arg(v, 1, v->rows);
		if (b > v->rows) b = v->rows;
		if (t < b) {
			v->top = (uint16_t)(t - 1); v->bot = (uint16_t)(b - 1);
			v->cx = 0; v->cy = 0;
		}
		break;
	}
	case 'm': do_sgr(v); break;
	case 's': save_cur(v); break;
	case 'u': restore_cur(v); break;
	default: break;
	}
}

static void control(mt_vt *v, uint8_t c)
{
	switch (c) {
	case 8: if (v->cx > 0) v->cx--; v->wrap = 0; break;
	case 9: v->cx = (uint16_t)clampu((v->cx & ~7u) + 8u, 0, v->cols - 1u); v->wrap = 0; break;
	case 10: case 11: case 12: v->wrap = 0; index_down(v); break;
	case 13: v->cx = 0; v->wrap = 0; break;
	default: break; /* BEL, SO, SI and the rest are ignored */
	}
}

static void esc_dispatch(mt_vt *v, uint8_t c)
{
	switch (c) {
	case '[': v->st = ST_CSI; v->np = 0; v->cur = -1; v->priv = v->inter = v->bad = 0; return;
	case ']': case 'P': case 'X': case '^': case '_': v->st = ST_STR; return;
	case '(': case ')': case '*': case '+': case '-': case '.': case '/': case '#': case '%': case ' ':
		v->st = ST_ESCI; return;
	case '7': save_cur(v); break;
	case '8': restore_cur(v); break;
	case 'M': v->wrap = 0; index_up(v); break;
	case 'D': v->wrap = 0; index_down(v); break;
	case 'E': v->wrap = 0; v->cx = 0; index_down(v); break;
	case 'c': soft_reset(v); break;
	default: break;
	}
	v->st = ST_GROUND;
}

static void csi_byte(mt_vt *v, uint8_t c)
{
	if (c >= '0' && c <= '9') {
		v->cur = (v->cur < 0 ? 0 : v->cur) * 10 + (c - '0');
		if (v->cur > 65535) v->cur = 65535;
	} else if (c == ';' || c == ':') {
		if (v->np < MT_VT_MAX_PARAMS) v->p[v->np++] = (uint16_t)(v->cur < 0 ? 0 : v->cur);
		else v->bad = 1;
		v->cur = -1;
	} else if (c >= 0x3c && c <= 0x3f) {
		if (v->np == 0 && v->cur < 0 && !v->priv) v->priv = c; else v->bad = 1;
	} else if (c >= 0x20 && c <= 0x2f) {
		v->inter = 1;
	} else if (c >= 0x40 && c <= 0x7e) {
		if (v->cur >= 0 || v->np > 0) {
			if (v->np < MT_VT_MAX_PARAMS) v->p[v->np++] = (uint16_t)(v->cur < 0 ? 0 : v->cur);
			else v->bad = 1;
		}
		csi_dispatch(v, c);
		v->st = ST_GROUND;
	}
}

static void feed_byte(mt_vt *v, uint8_t c)
{
again:
	switch (v->st) {
	case ST_GROUND:
		if (v->u_need) {
			if ((c & 0xC0) == 0x80) {
				v->u_cp = (v->u_cp << 6) | (c & 0x3f);
				if (--v->u_need == 0) {
					if (v->u_cp < v->u_min || v->u_cp > 0x10FFFF || (v->u_cp >= 0xD800 && v->u_cp <= 0xDFFF))
						v->u_cp = 0xFFFD;
					put(v, v->u_cp);
				}
				return;
			}
			v->u_need = 0;
			put(v, 0xFFFD);
			goto again;
		}
		if (c == 0x1b) v->st = ST_ESC;
		else if (c < 0x20) control(v, c);
		else if (c < 0x7f) put(v, c);
		else if (c == 0x7f) return;
		else if (c >= 0xC2 && c <= 0xDF) { v->u_need = 1; v->u_cp = c & 0x1f; v->u_min = 0x80; }
		else if (c >= 0xE0 && c <= 0xEF) { v->u_need = 2; v->u_cp = c & 0x0f; v->u_min = 0x800; }
		else if (c >= 0xF0 && c <= 0xF4) { v->u_need = 3; v->u_cp = c & 0x07; v->u_min = 0x10000; }
		else put(v, 0xFFFD);
		return;
	case ST_ESC:
		if (c == 0x1b) return;
		if (c < 0x20) { control(v, c); return; }
		if (c >= 0x7f) { v->st = ST_GROUND; goto again; }
		esc_dispatch(v, c);
		return;
	case ST_ESCI:
		if (c == 0x1b) v->st = ST_ESC;
		else if (c >= 0x30 && c <= 0x7e) v->st = ST_GROUND; /* final byte of a charset etc.: ignored */
		else if (c >= 0x80) { v->st = ST_GROUND; goto again; }
		return;
	case ST_CSI:
		if (c == 0x1b) v->st = ST_ESC;
		else if (c == 0x18 || c == 0x1a) v->st = ST_GROUND;
		else if (c < 0x20) control(v, c);
		else if (c >= 0x7f) { v->st = ST_GROUND; if (c > 0x7f) goto again; }
		else csi_byte(v, c);
		return;
	case ST_STR:
		if (c == 7 || c == 0x18 || c == 0x1a) v->st = ST_GROUND;
		else if (c == 0x1b) v->st = ST_STRE;
		return;
	default: /* ST_STRE */
		if (c == '\\') { v->st = ST_GROUND; return; }
		v->st = ST_ESC;
		goto again;
	}
}

/* ------------------------------------------------------------ public API */

size_t mt_vt_mem_size(uint16_t cols, uint16_t rows, size_t sb)
{
	if (cols < 1 || cols > MT_VT_MAX_COLS || rows < 1 || rows > MT_VT_MAX_ROWS || sb > ((size_t)1 << 24))
		return 0;
	return (size_t)cols * rows * sizeof(uint32_t) + sb + 4;
}

void mt_vt_reset(mt_vt *v)
{
	v->sb_head = v->sb_len = 0;
	soft_reset(v);
	v->gen++;
}

int mt_vt_init(mt_vt *v, uint16_t cols, uint16_t rows, size_t sb, void *mem, size_t mem_len)
{
	size_t need = mt_vt_mem_size(cols, rows, sb), pad;
	uint8_t *m = (uint8_t *)mem;
	if (!v || !mem || need == 0 || mem_len < need)
		return -1;
	memset(v, 0, sizeof *v);
	pad = (size_t)(-(uintptr_t)m) & 3u;
	memset(m, 0, need);
	v->cells = (uint32_t *)(void *)(m + pad);
	v->sb = m + pad + (size_t)cols * rows * sizeof(uint32_t);
	v->sb_cap = sb;
	v->cols = cols;
	v->rows = rows;
	soft_reset(v);
	v->gen = 1;
	return 0;
}

void mt_vt_feed(mt_vt *v, const uint8_t *d, size_t n)
{
	uint16_t ox = v->cx, oy = v->cy;
	uint8_t ov = v->vis;
	size_t i;
	v->dirty = 0;
	for (i = 0; i < n; i++)
		feed_byte(v, d[i]);
	if (v->dirty || ox != v->cx || oy != v->cy || ov != v->vis)
		v->gen++;
}

uint32_t mt_vt_generation(const mt_vt *v) { return v->gen; }

static void color_str(uint32_t c, char *o)
{
	static const char hx[] = "0123456789abcdef";
	if (c & C_RGB) {
		int i;
		o[0] = '#';
		for (i = 0; i < 6; i++) o[1 + i] = hx[(c >> (20 - 4 * i)) & 15];
		o[7] = 0;
	} else {
		unsigned p = c & 255, n = 0;
		o[n++] = 'p';
		if (p >= 100) o[n++] = (char)('0' + p / 100);
		if (p >= 10) o[n++] = (char)('0' + p / 10 % 10);
		o[n++] = (char)('0' + p % 10);
		o[n] = 0;
	}
}

static void emit_span(mt_jw *w, const mt_vt_style *s, unsigned line, unsigned a, unsigned b)
{
	char col[8];
	mt_jw_obj(w);
	mt_jw_kint(w, "line", line);
	mt_jw_kint(w, "start", a);
	mt_jw_kint(w, "end", b);
	if (s->fg) { color_str(s->fg, col); mt_jw_kstr(w, "fg", col); }
	if (s->bg) { color_str(s->bg, col); mt_jw_kstr(w, "bg", col); }
	if (s->attr & A_BOLD) mt_jw_kbool(w, "bold", 1);
	if (s->attr & A_DIM) mt_jw_kbool(w, "dim", 1);
	if (s->attr & A_ITAL) mt_jw_kbool(w, "italic", 1);
	if (s->attr & A_UL) mt_jw_kbool(w, "underline", 1);
	if (s->attr & A_INV) mt_jw_kbool(w, "inverse", 1);
	if (s->attr & A_STRIKE) mt_jw_kbool(w, "strike", 1);
	mt_jw_obj_end(w);
}

static void row_spans(const mt_vt *v, unsigned y, unsigned line, mt_jw *w)
{
	const uint32_t *r = v->cells + (size_t)y * v->cols;
	unsigned x, e = row_end(v, y), runes = 0, start = 0, cur = 0;
	for (x = 0; x < e; x++) {
		if (r[x] & F_CONT)
			continue;
		if (SI(r[x]) != cur) {
			if (cur && runes > start) emit_span(w, &v->styles[cur], line, start, runes);
			cur = SI(r[x]);
			start = runes;
		}
		runes++;
	}
	if (cur && runes > start)
		emit_span(w, &v->styles[cur], line, start, runes);
}

static void head(const mt_vt *v, mt_jw *w, int height)
{
	mt_jw_obj(w);
	mt_jw_kint(w, "width", v->cols);
	mt_jw_kint(w, "height", height);
	mt_jw_key(w, "lines");
	mt_jw_arr(w);
}

int mt_vt_screen_json(const mt_vt *v, mt_jw *w)
{
	char buf[ROWBUF];
	unsigned y;
	head(v, w, v->rows);
	for (y = 0; y < v->rows; y++)
		mt_jw_strn(w, buf, row_text(v, y, buf));
	mt_jw_arr_end(w);
	mt_jw_key(w, "spans");
	mt_jw_arr(w);
	for (y = 0; y < v->rows; y++)
		row_spans(v, y, y, w);
	mt_jw_arr_end(w);
	mt_jw_key(w, "cursor");
	if (v->vis) {
		mt_jw_obj(w);
		mt_jw_kint(w, "row", v->cy);
		mt_jw_kint(w, "col", cursor_rune_col(v));
		mt_jw_obj_end(w);
	} else {
		mt_jw_null(w);
	}
	mt_jw_obj_end(w);
	return w->err ? -1 : 0;
}

static uint8_t sbat(const mt_vt *v, size_t i) { return v->sb[(v->sb_head + i) % v->sb_cap]; }

int mt_vt_capture_json(const mt_vt *v, int lines, mt_jw *w)
{
	char buf[ROWBUF];
	size_t off = 0, end = v->sb_len, i, nsb = 0, e, start, skip, k;
	int last = -1, y;
	unsigned vs, first;
	if (lines < 1 || lines > MT_VT_MAX_CAPTURE)
		return -1;
	for (y = v->rows - 1; y >= 0; y--)
		if (row_end(v, (unsigned)y)) { last = y; break; }
	if (last < 0) /* nothing visible: also drop trailing blank scrollback lines */
		while (end > 0 && (end == 1 || sbat(v, end - 2) == '\n'))
			end--;
	for (i = 0; i < end; i++)
		nsb += sbat(v, i) == '\n';
	e = nsb + (size_t)(last + 1);
	start = e > (size_t)lines ? e - (size_t)lines : 0;
	head(v, w, 0);
	for (skip = start; skip > 0 && skip <= nsb && off < end; skip--) { /* skip scrollback lines before start */
		while (sbat(v, off) != '\n') off++;
		off++;
	}
	for (i = start; i < nsb; i++) {
		for (k = 0; off < end && sbat(v, off) != '\n'; off++)
			if (k < sizeof buf) buf[k++] = (char)sbat(v, off);
		off++;
		mt_jw_strn(w, buf, k);
	}
	vs = start > nsb ? (unsigned)(start - nsb) : 0;
	for (y = (int)vs; y <= last; y++)
		mt_jw_strn(w, buf, row_text(v, (unsigned)y, buf));
	mt_jw_arr_end(w);
	mt_jw_key(w, "spans");
	mt_jw_arr(w);
	first = nsb > start ? (unsigned)(nsb - start) : 0;
	for (y = (int)vs; y <= last; y++)
		row_spans(v, (unsigned)y, first + (unsigned)y - vs, w);
	mt_jw_arr_end(w);
	mt_jw_key(w, "cursor");
	mt_jw_null(w);
	mt_jw_obj_end(w);
	return w->err ? -1 : 0;
}

void mt_vt_hash(const mt_vt *v, char out[65])
{
	static const char hx[] = "0123456789abcdef";
	mt_sha256_ctx ctx;
	uint8_t d[32], b[16];
	unsigned y, x, e, i;
	mt_sha256_init(&ctx);
	b[0] = (uint8_t)v->cols; b[1] = (uint8_t)v->rows; b[2] = v->vis;
	b[3] = (uint8_t)(v->vis ? v->cx : 0); b[4] = (uint8_t)(v->vis ? v->cy : 0);
	mt_sha256_update(&ctx, b, 5);
	for (y = 0; y < v->rows; y++) {
		const uint32_t *r = v->cells + (size_t)y * v->cols;
		e = row_end(v, y);
		for (x = 0; x < e; x++) {
			const mt_vt_style *s = &v->styles[SI(r[x])];
			uint32_t c = r[x] & (CP_MASK | F_WIDE | F_CONT);
			for (i = 0; i < 4; i++) {
				b[i] = (uint8_t)(c >> (8 * i));
				b[4 + i] = (uint8_t)(s->fg >> (8 * i));
				b[8 + i] = (uint8_t)(s->bg >> (8 * i));
			}
			b[12] = s->attr;
			mt_sha256_update(&ctx, b, 13);
		}
		b[0] = 0xff; b[1] = (uint8_t)x;
		mt_sha256_update(&ctx, b, 2);
	}
	mt_sha256_final(&ctx, d);
	for (i = 0; i < 32; i++) {
		out[2 * i] = hx[d[i] >> 4];
		out[2 * i + 1] = hx[d[i] & 15];
	}
	out[64] = 0;
}
