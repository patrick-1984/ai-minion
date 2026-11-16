#include <stdio.h>
#include <string.h>

#include "mt_keys.h"

static int fails;

static void expect(const char *name, const char *want, size_t wn)
{
	uint8_t out[MT_KEY_MAX];
	int n = mt_key_bytes(name, strlen(name), out, sizeof out);
	if (want == NULL) {
		if (n != -1) { printf("FAIL %s: expected -1, got %d\n", name, n); fails++; }
		return;
	}
	if (n != (int)wn || memcmp(out, want, wn) != 0) {
		int i;
		printf("FAIL %s: got %d bytes:", name, n);
		for (i = 0; i < n; i++) printf(" %02x", out[i]);
		printf("\n");
		fails++;
	}
}

#define OK(name, lit) expect(name, lit, sizeof(lit) - 1)
#define BAD(name) expect(name, NULL, 0)

int main(void)
{
	uint8_t small[2];
	OK("Enter", "\r"); OK("Tab", "\t"); OK("BTab", "\x1b[Z"); OK("Escape", "\x1b"); OK("Space", " ");
	OK("BSpace", "\x7f"); OK("DC", "\x1b[3~"); OK("IC", "\x1b[2~"); OK("Home", "\x1b[1~"); OK("End", "\x1b[4~");
	OK("PPage", "\x1b[5~"); OK("PageUp", "\x1b[5~"); OK("NPage", "\x1b[6~"); OK("PageDown", "\x1b[6~");
	OK("Up", "\x1b[A"); OK("Down", "\x1b[B"); OK("Right", "\x1b[C"); OK("Left", "\x1b[D");
	OK("F1", "\x1bOP"); OK("F4", "\x1bOS"); OK("F5", "\x1b[15~"); OK("F10", "\x1b[21~"); OK("F12", "\x1b[24~");
	OK("C-c", "\x03"); OK("C-a", "\x01"); OK("C-z", "\x1a"); OK("C-A", "\x01"); OK("C-@", "\x00");
	OK("C-[", "\x1b"); OK("C-\\", "\x1c"); OK("C-]", "\x1d"); OK("C-^", "\x1e"); OK("C-_", "\x1f");
	OK("C-?", "\x7f"); OK("C-Space", "\x00");
	OK("M-x", "\x1bx"); OK("M-Enter", "\x1b\r"); OK("M-BSpace", "\x1b\x7f"); OK("M-C-a", "\x1b\x01");
	OK("M-Up", "\x1b[1;3A"); OK("S-Up", "\x1b[1;2A"); OK("C-Left", "\x1b[1;5D"); OK("C-S-Right", "\x1b[1;6C");
	OK("S-F1", "\x1b[1;2P"); OK("S-F5", "\x1b[15;2~"); OK("C-F12", "\x1b[24;5~"); OK("C-M-Home", "\x1b[1;7~");
	OK("C-PageDown", "\x1b[6;5~"); OK("S-Tab", "\x1b[Z"); OK("M-BTab", "\x1b\x1b[Z");
	OK("C-Enter", "\x1b[27;5;13~"); OK("S-Enter", "\x1b[27;2;13~");
	OK("S-a", "A"); OK("M-S-a", "\x1b" "A");
	OK("a", "a"); OK("Z", "Z"); OK("-", "-"); OK(";", ";"); OK("/", "/"); OK("\xc3\xa9", "\xc3\xa9");
	OK("\xe2\x82\xac", "\xe2\x82\xac"); OK("\xf0\x9f\x98\x80", "\xf0\x9f\x98\x80"); OK("M-\xc3\xa9", "\x1b\xc3\xa9");
	OK("hello", "hello"); OK("ls -l", "ls -l"); OK("a-b", "a-b"); OK("KP5", "5"); OK("KPEnter", "\r");
	OK("PgUp", "\x1b[5~"); OK("PgDn", "\x1b[6~");
	BAD(""); BAD("C-"); BAD("M-"); BAD("S-"); BAD("C-ab");
	/* as tmux: C-2..C-8 are control bytes, other modified characters are sent unchanged */
	expect("C-2", "\x00", 1); expect("C-3", "\x1b", 1); expect("C-4", "\x1c", 1); expect("C-5", "\x1d", 1);
	expect("C-6", "\x1e", 1); expect("C-7", "\x1f", 1); expect("C-8", "\x7f", 1);
	expect("C-1", "1", 1); expect("C-\xc3\xa9", "\xc3\xa9", 2); expect("S-1", "1", 1); expect("S-a", "A", 1);
	BAD("M-foo"); BAD("C-BTab"); BAD("\xff"); BAD("\xc3"); BAD("\xc0\x80"); BAD("a\nb"); BAD("a\x1b" "b");
	BAD("\x01"); BAD("\x7f"); BAD("\xc2\x85"); BAD("\xed\xa0\x80");
	BAD("0123456789012345678901234567890123"); /* 34 bytes */
	if (mt_key_bytes("F5", 2, small, sizeof small) != -1) { printf("FAIL: small buffer accepted\n"); fails++; }
	if (mt_key_bytes("Enter", 5, small, 0) != -1) { printf("FAIL: zero cap accepted\n"); fails++; }
	if (mt_key_bytes(NULL, 0, small, sizeof small) != -1) { printf("FAIL: NULL accepted\n"); fails++; }
	if (fails) { printf("test_keys: %d failure(s)\n", fails); return 1; }
	printf("test_keys: PASS\n");
	return 0;
}
