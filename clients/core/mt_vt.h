/* Minion thin Worker: VT100/xterm-subset screen model (no malloc, compact cells).
 *
 * Output bytes of a session are fed in; the screen is read back as the Hub's Screen JSON
 * (internal/ops/screen.go): width, height, lines, spans, cursor. Span offsets count runes of the
 * line, colours are "pN" (palette) or "#rrggbb", trailing blanks are trimmed, so the result always
 * passes Screen.Validate. Fields of mt_vt are private. */
#ifndef MT_VT_H
#define MT_VT_H

#include <stddef.h>
#include <stdint.h>

#include "mt_json.h"

#define MT_VT_MAX_COLS 256
#define MT_VT_MAX_ROWS 255
#define MT_VT_NSTYLE 64
#define MT_VT_MAX_PARAMS 16
#define MT_VT_MAX_CAPTURE 10000 /* ops.MaxCaptureLines */

typedef struct {
	uint32_t fg, bg; /* 0 default, 0x2000000|idx palette, 0x1000000|rgb true colour */
	uint8_t attr;
} mt_vt_style;

typedef struct mt_vt {
	uint32_t *cells; /* cols*rows: codepoint (21 bits) | wide | cont | style index (9 bits) */
	uint8_t *sb;     /* scrollback ring: plain UTF-8 lines, each ended by '\n' */
	size_t sb_cap, sb_head, sb_len;
	uint32_t gen;
	uint32_t fg, bg, u_cp, u_min, sfg, sbg;
	uint64_t alloc; /* bit i set: styles[i] in use */
	uint16_t cols, rows, cx, cy, top, bot, scx, scy, cur_si;
	uint16_t p[MT_VT_MAX_PARAMS];
	int cur;
	uint8_t np, st, priv, inter, bad, u_need, attr, sattr, swrap;
	uint8_t wrap, autowrap, vis, alt, dirty;
	mt_vt_style styles[MT_VT_NSTYLE];
} mt_vt;

/* Bytes of memory the caller must provide to mt_vt_init (0 when the size is not supported:
 * cols 1..MT_VT_MAX_COLS, rows 1..MT_VT_MAX_ROWS, scrollback up to 16 MiB). */
size_t mt_vt_mem_size(uint16_t cols, uint16_t rows, size_t scrollback_bytes);
/* 0 on success, -1 on bad size or too little memory. mem needs no alignment. */
int mt_vt_init(mt_vt *vt, uint16_t cols, uint16_t rows, size_t scrollback_bytes, void *mem, size_t mem_len);
void mt_vt_feed(mt_vt *vt, const uint8_t *data, size_t n);
void mt_vt_reset(mt_vt *vt); /* full reset including scrollback */
/* The Screen object of session_snapshot: height == rows lines, cursor null when hidden.
 * 0 on success, -1 when the JSON buffer overflowed. */
int mt_vt_screen_json(const mt_vt *vt, mt_jw *w);
/* The Screen object of session_capture: the last `lines` (1..MT_VT_MAX_CAPTURE) rows of
 * scrollback + visible grid, trailing blank rows dropped, height 0, cursor null. Spans only cover
 * rows that are still on the visible grid. -1 on bad `lines` or JSON overflow. */
int mt_vt_capture_json(const mt_vt *vt, int lines, mt_jw *w);
/* Hex SHA-256 of size, visible text, styles and cursor. */
void mt_vt_hash(const mt_vt *vt, char out[65]);
/* Incremented by mt_vt_feed/mt_vt_reset whenever the visible screen (text, style, cursor) changed. */
uint32_t mt_vt_generation(const mt_vt *vt);

#endif
