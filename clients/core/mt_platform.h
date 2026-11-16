/* Minion thin Worker: the platform interface. The core is single-threaded; one task/thread runs
 * mt_worker_run() and every hook below is called from it. */
#ifndef MT_PLATFORM_H
#define MT_PLATFORM_H

#include <stddef.h>
#include <stdint.h>

enum { MT_LOG_ERROR = 0, MT_LOG_WARN = 1, MT_LOG_INFO = 2, MT_LOG_DEBUG = 3 };

typedef struct mt_platform {
	void *ctx;
	/* Network: one connection at a time. tls != 0 asks for TLS WITHOUT certificate verification (trust comes
	 * from the pinned Hub root inside the protocol), with SNI = host. */
	int (*net_connect)(void *ctx, const char *host, uint16_t port, int tls, int timeout_ms);
	/* >0 bytes read, 0 on timeout, <0 closed or failed. */
	int (*net_read)(void *ctx, uint8_t *buf, size_t cap, int timeout_ms);
	/* Writes all n bytes or fails (<0). */
	int (*net_write)(void *ctx, const uint8_t *buf, size_t n);
	void (*net_close)(void *ctx);
	/* Wall clock in Unix milliseconds; return 0 while the clock is not set (before SNTP). */
	int64_t (*now_ms)(void *ctx);
	/* Monotonic milliseconds. */
	int64_t (*mono_ms)(void *ctx);
	/* Cryptographic random bytes (getrandom, esp_fill_random with RF on). */
	void (*random)(void *ctx, uint8_t *buf, size_t n);
	/* Persistent store, one atomic blob per key. get returns the length or -1; set with n == 0 deletes. */
	int (*store_get)(void *ctx, const char *key, uint8_t *buf, size_t cap);
	int (*store_set)(void *ctx, const char *key, const uint8_t *buf, size_t n);
	void (*log)(void *ctx, int level, const char *msg);
	/* Pump session I/O (PTY, UART, console) without blocking; called on every loop turn. */
	void (*poll)(void *ctx);
	void (*sleep_ms)(void *ctx, int ms);
} mt_platform;

#endif
