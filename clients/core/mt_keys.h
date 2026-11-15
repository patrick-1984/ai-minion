/* Minion thin Worker: tmux key name -> the bytes a terminal sends. */
#ifndef MT_KEYS_H
#define MT_KEYS_H

#include <stddef.h>
#include <stdint.h>

/* Output buffer size that always suffices (a key name is at most 32 bytes). */
#define MT_KEY_MAX 40

/* Translate one send_keys key name into terminal input bytes.
 * Supported: Enter Tab BTab Escape Space BSpace DC IC Home End PPage/PageUp/PgUp NPage/PageDown/PgDn
 * Up Down Left Right F1..F12 KP0..KP9 KP/ KP* KP- KP+ KP. KPEnter, with any mix of the prefixes
 * C- (control), M- (ESC prefix / xterm alt) and S- (shift), C-<char> (incl. C-@ C-[ C-\ C-] C-^ C-_
 * C-? C-Space), and a literal key: a single UTF-8 character, or a string that is not a key name
 * (sent as its bytes; no control characters, at most 32 bytes).
 * Returns the number of bytes written to out (>= 1), or -1 for an unknown/invalid name or when cap
 * is too small. Nothing is guessed. */
int mt_key_bytes(const char *name, size_t len, uint8_t *out, size_t cap);

#endif
