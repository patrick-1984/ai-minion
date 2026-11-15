#include "mt_keys.h"

#include <string.h>

#define MOD_S 1
#define MOD_A 2
#define MOD_C 4

enum { K_CSI, K_TILDE, K_SS3, K_BYTE, K_BTAB };

typedef struct {
	const char *name;
	uint8_t kind, num;
	char fin;
} keydef;

static const keydef keys[] = {
	{"Up", K_CSI, 1, 'A'}, {"Down", K_CSI, 1, 'B'}, {"Right", K_CSI, 1, 'C'}, {"Left", K_CSI, 1, 'D'},
	{"Home", K_TILDE, 1, 0}, {"End", K_TILDE, 4, 0}, {"IC", K_TILDE, 2, 0}, {"DC", K_TILDE, 3, 0},
	{"PPage", K_TILDE, 5, 0}, {"PageUp", K_TILDE, 5, 0}, {"PgUp", K_TILDE, 5, 0},
	{"NPage", K_TILDE, 6, 0}, {"PageDown", K_TILDE, 6, 0}, {"PgDn", K_TILDE, 6, 0},
	{"F1", K_SS3, 1, 'P'}, {"F2", K_SS3, 1, 'Q'}, {"F3", K_SS3, 1, 'R'}, {"F4", K_SS3, 1, 'S'},
	{"F5", K_TILDE, 15, 0}, {"F6", K_TILDE, 17, 0}, {"F7", K_TILDE, 18, 0}, {"F8", K_TILDE, 19, 0},
	{"F9", K_TILDE, 20, 0}, {"F10", K_TILDE, 21, 0}, {"F11", K_TILDE, 23, 0}, {"F12", K_TILDE, 24, 0},
	{"Enter", K_BYTE, 13, 0}, {"Tab", K_BYTE, 9, 0}, {"Escape", K_BYTE, 27, 0}, {"BSpace", K_BYTE, 127, 0},
	{"Space", K_BYTE, ' ', 0}, {"BTab", K_BTAB, 0, 0}, {"KPEnter", K_BYTE, 13, 0},
	{"KP0", K_BYTE, '0', 0}, {"KP1", K_BYTE, '1', 0}, {"KP2", K_BYTE, '2', 0}, {"KP3", K_BYTE, '3', 0},
	{"KP4", K_BYTE, '4', 0}, {"KP5", K_BYTE, '5', 0}, {"KP6", K_BYTE, '6', 0}, {"KP7", K_BYTE, '7', 0},
	{"KP8", K_BYTE, '8', 0}, {"KP9", K_BYTE, '9', 0}, {"KP/", K_BYTE, '/', 0}, {"KP*", K_BYTE, '*', 0},
	{"KP-", K_BYTE, '-', 0}, {"KP+", K_BYTE, '+', 0}, {"KP.", K_BYTE, '.', 0},
};

/* Strict UTF-8 decode of one character: returns its length (1..4) or 0 when invalid. */
static int dec(const uint8_t *s, size_t n, uint32_t *cp)
{
	uint32_t c = s[0], min;
	int need, i;
	if (c < 0x80) { *cp = c; return 1; }
	if (c >= 0xC2 && c <= 0xDF) { need = 1; c &= 0x1f; min = 0x80; }
	else if (c >= 0xE0 && c <= 0xEF) { need = 2; c &= 0x0f; min = 0x800; }
	else if (c >= 0xF0 && c <= 0xF4) { need = 3; c &= 0x07; min = 0x10000; }
	else return 0;
	if ((size_t)need >= n) return 0;
	for (i = 1; i <= need; i++) {
		if ((s[i] & 0xC0) != 0x80) return 0;
		c = (c << 6) | (s[i] & 0x3f);
	}
	if (c < min || c > 0x10FFFF || (c >= 0xD800 && c <= 0xDFFF)) return 0;
	*cp = c;
	return need + 1;
}

static int is_ctl(uint32_t c) { return c < 0x20 || (c >= 0x7f && c <= 0x9f); }

static size_t num(uint8_t *t, unsigned v)
{
	char d[5];
	size_t n = 0, i = 0;
	do { d[n++] = (char)('0' + v % 10); v /= 10; } while (v);
	while (n) t[i++] = (uint8_t)d[--n];
	return i;
}

/* ESC [ num ; mod fin   (num omitted without mod for K_CSI) */
static size_t csi(uint8_t *t, int kind, unsigned nm, char fin, unsigned mod)
{
	size_t n = 0;
	t[n++] = 27;
	if (kind == K_SS3 && mod == 1) { t[n++] = 'O'; t[n++] = (uint8_t)fin; return n; }
	t[n++] = '[';
	if (mod > 1) {
		n += num(t + n, nm);
		t[n++] = ';';
		n += num(t + n, mod);
	} else if (kind == K_TILDE) {
		n += num(t + n, nm);
	}
	t[n++] = (uint8_t)(kind == K_TILDE ? '~' : fin);
	return n;
}

int mt_key_bytes(const char *name, size_t len, uint8_t *out, size_t cap)
{
	uint8_t t[MT_KEY_MAX + 16];
	const uint8_t *s = (const uint8_t *)name;
	size_t n = 0, i;
	unsigned mods = 0, mod;
	uint32_t cp = 0;
	int cl;
	const keydef *k = NULL;

	if (!name || !out || len == 0 || len > 32) return -1;
	if (len == 2 && s[1] == '-' && (s[0] == 'C' || s[0] == 'M' || s[0] == 'S')) return -1;
	while (len > 2 && s[1] == '-' && (s[0] == 'C' || s[0] == 'M' || s[0] == 'S')) {
		mods |= s[0] == 'C' ? MOD_C : s[0] == 'M' ? MOD_A : MOD_S;
		s += 2;
		len -= 2;
	}
	for (i = 0; i < sizeof keys / sizeof keys[0]; i++)
		if (strlen(keys[i].name) == len && memcmp(keys[i].name, s, len) == 0) { k = &keys[i]; break; }
	mod = 1u + mods;

	if (k && k->kind != K_BYTE && k->kind != K_BTAB) {
		n = csi(t, k->kind, k->num, k->fin ? k->fin : '~', mod);
	} else if (k && k->kind == K_BTAB) {
		if (mods & MOD_C) return -1;
		if (mods & MOD_A) t[n++] = 27;
		t[n++] = 27; t[n++] = '['; t[n++] = 'Z';
	} else if (k) {
		uint8_t b = k->num;
		if (b == 9 && (mods & MOD_S) && !(mods & MOD_C)) { /* S-Tab == BTab */
			if (mods & MOD_A) t[n++] = 27;
			t[n++] = 27; t[n++] = '['; t[n++] = 'Z';
		} else if (!(mods & (MOD_S | MOD_C))) {
			if (mods & MOD_A) t[n++] = 27;
			t[n++] = b;
		} else if (b == ' ' && (mods & MOD_C) && !(mods & MOD_S)) {
			if (mods & MOD_A) t[n++] = 27;
			t[n++] = 0;
		} else { /* xterm modifyOtherKeys: ESC [ 27 ; mod ; code ~ */
			t[n++] = 27; t[n++] = '['; n += num(t + n, 27); t[n++] = ';';
			n += num(t + n, mod); t[n++] = ';'; n += num(t + n, b); t[n++] = '~';
		}
	} else {
		cl = dec(s, len, &cp);
		if (cl == 0 || is_ctl(cp)) return -1;
		if ((size_t)cl == len) { /* one character with modifiers */
			if (mods & MOD_S) {
				if (cp >= 'a' && cp <= 'z') cp -= 32;
				else if (!(cp >= 'A' && cp <= 'Z')) return -1;
			}
			if (mods & MOD_C) {
				if (cp >= 'a' && cp <= 'z') cp -= 96;
				else if (cp >= 'A' && cp <= 'Z') cp -= 64;
				else if (cp >= '@' && cp <= '_') cp -= 64; /* @ [ \ ] ^ _ */
				else if (cp == '?') cp = 127;
				else return -1;
			}
			if (mods & MOD_A) t[n++] = 27;
			if (cp < 0x80) t[n++] = (uint8_t)cp;
			else { memcpy(t + n, s, len); n += len; }
		} else {
			/* multi-character literal: only without modifiers; every character must be valid text */
			size_t p = 0;
			if (mods) return -1;
			while (p < len) {
				cl = dec(s + p, len - p, &cp);
				if (cl == 0 || is_ctl(cp)) return -1;
				p += (size_t)cl;
			}
			memcpy(t, s, len);
			n = len;
		}
	}
	if (n == 0 || n > cap) return -1;
	memcpy(out, t, n);
	return (int)n;
}
