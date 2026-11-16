/* The three sessions of a device without a shell:
 *   console  built-in command shell for the Hub (line editing, prompt "esp> ", secrets masked in the echo)
 *   uart     serial bridge to an attached device on UART1 (never the console/log UART); off until configured
 *   log      ESP-IDF log output, read-only (the serial port keeps printing it too)
 * Screens are fed only by the worker task (sess_poll and the write hooks run there). */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "driver/uart.h"
#include "esp_console.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "esp_system.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

#include "app.h"

#define COLS 80
#define ROWS 24
#define SB_CONSOLE 3072 /* 80x24 cells (7.5 KB) + scrollback per screen */
#define SB_LOG 2048
#define SB_UART 2048
#define CON_LINE_MAX 200
#define LINEQ 8 /* lines of one pasted multi-line text */
#define LOGRING 2048
#define BRIDGE_UART UART_NUM_1

static const char *TAG = "sess";

mt_session g_console, g_log, g_uart;
static mt_vt vt_console, vt_log, vt_uart;
static mt_worker *g_w;

static portMUX_TYPE g_mux = portMUX_INITIALIZER_UNLOCKED;
static mt_status g_st;
static int g_st_have;
static int64_t g_st_mono;

/* ---------------------------------------------------------------- screens */

static int vt_alloc(mt_vt *vt, size_t sb)
{
	size_t n = mt_vt_mem_size(COLS, ROWS, sb);
	void *m = n ? heap_caps_malloc(n, MALLOC_CAP_8BIT | MALLOC_CAP_INTERNAL) : NULL;
	if (!m || mt_vt_init(vt, COLS, ROWS, sb, m, n) != 0) {
		free(m);
		return -1;
	}
	return 0;
}

/* Feed text, turning a bare "\n" into "\r\n" (a CR split from its LF across calls only adds a harmless CR). */
static void vt_text(mt_vt *vt, const char *s, size_t n)
{
	size_t i, start = 0;
	for (i = 0; i < n; i++) {
		if (s[i] == '\n' && !(i && s[i - 1] == '\r')) {
			if (i > start)
				mt_vt_feed(vt, (const uint8_t *)s + start, i - start);
			mt_vt_feed(vt, (const uint8_t *)"\r\n", 2);
			start = i + 1;
		}
	}
	if (n > start)
		mt_vt_feed(vt, (const uint8_t *)s + start, n - start);
}

void console_out(const char *s, size_t n) { vt_text(&vt_console, s, n); }

/* ---------------------------------------------------------------- log session */

static char g_ring[LOGRING];
static size_t g_ring_head, g_ring_len;
static vprintf_like_t g_prev_vprintf;

static int log_vprintf(const char *fmt, va_list ap)
{
	char b[192];
	va_list ap2;
	int n, r;
	va_copy(ap2, ap);
	r = g_prev_vprintf ? g_prev_vprintf(fmt, ap) : vprintf(fmt, ap); /* keep the serial output */
	n = vsnprintf(b, sizeof b, fmt, ap2);
	va_end(ap2);
	if (n > (int)sizeof b - 1) {
		n = sizeof b - 1;
		b[n - 1] = '\n'; /* truncated line still ends the line */
	}
	if (n > 0) {
		size_t i;
		portENTER_CRITICAL_SAFE(&g_mux);
		for (i = 0; i < (size_t)n; i++) {
			g_ring[(g_ring_head + g_ring_len) % LOGRING] = b[i];
			if (g_ring_len < LOGRING)
				g_ring_len++;
			else
				g_ring_head = (g_ring_head + 1) % LOGRING; /* overwrite the oldest bytes */
		}
		portEXIT_CRITICAL_SAFE(&g_mux);
	}
	return r;
}

static void drain_log(void)
{
	char b[256];
	for (;;) {
		size_t n = 0;
		portENTER_CRITICAL(&g_mux);
		while (n < sizeof b && g_ring_len) {
			b[n++] = g_ring[g_ring_head];
			g_ring_head = (g_ring_head + 1) % LOGRING;
			g_ring_len--;
		}
		portEXIT_CRITICAL(&g_mux);
		if (!n)
			return;
		vt_text(&vt_log, b, n);
	}
}

/* ---------------------------------------------------------------- console session: line editing */

static struct {
	char line[CON_LINE_MAX + 1];
	size_t len;
	uint8_t esc;       /* 1 after ESC, 2 inside CSI/SS3 */
	uint8_t last_cr;
	char q[LINEQ][CON_LINE_MAX + 1];
	int qh, qn;
} C;

static void prompt(void) { console_out("esp> ", 5); }

static void submit(void)
{
	console_out("\r\n", 2);
	C.line[C.len] = 0;
	if (C.qn < LINEQ) {
		memcpy(C.q[(C.qh + C.qn) % LINEQ], C.line, C.len + 1);
		C.qn++;
	} else {
		console_out("ERR busy (too many queued lines)\r\n", 34);
	}
	memset(C.line, 0, sizeof C.line); /* may have held a secret */
	C.len = 0;
}

static int console_write(mt_session *s, const uint8_t *d, size_t n)
{
	size_t i;
	for (i = 0; i < n; i++) {
		uint8_t c = d[i];
		if (C.esc == 1) { /* ESC [ or ESC O start a sequence, anything else ends it */
			C.esc = (c == '[' || c == 'O') ? 2 : 0;
			continue;
		}
		if (C.esc == 2) { /* arrows and other keys: ignored until the final byte */
			if (c >= 0x40 && c <= 0x7e)
				C.esc = 0;
			continue;
		}
		if (c == '\n' && C.last_cr) {
			C.last_cr = 0;
			continue;
		}
		C.last_cr = c == '\r';
		if (c == '\r' || c == '\n') {
			submit();
		} else if (c == 0x1b) {
			C.esc = 1;
		} else if (c == 0x08 || c == 0x7f) {
			if (C.len) {
				while (C.len && ((uint8_t)C.line[C.len - 1] & 0xc0) == 0x80) /* whole UTF-8 character */
					C.len--;
				if (C.len)
					C.len--;
				console_out("\b \b", 3);
			}
		} else if (c == 0x03) { /* C-c */
			C.len = 0;
			console_out("^C\r\n", 4);
			prompt();
		} else if (c == 0x15) { /* C-u: clear the line */
			while (C.len) {
				C.len--;
				if (((uint8_t)C.line[C.len] & 0xc0) != 0x80)
					console_out("\b \b", 3);
			}
		} else if (c >= 0x20 && C.len < CON_LINE_MAX) {
			/* echo as typed (the screen decodes UTF-8 itself); a secret shows one '*' per character */
			int secret = cmd_secret_pos(C.line, C.len);
			C.line[C.len++] = (char)c;
			if (!secret)
				console_out((const char *)&c, 1);
			else if ((c & 0xc0) != 0x80)
				console_out("*", 1);
		}
		/* other control bytes (Tab, C-d, ...) are ignored */
	}
	return 0;
}

/* An asynchronous line (GPIO watch) above the prompt; the line being typed is shown again (secrets masked). */
void console_notice(const char *msg)
{
	size_t i;
	console_out("\r\x1b[K", 4);
	console_out(msg, strlen(msg));
	console_out("\r\n", 2);
	prompt();
	for (i = 0; i < C.len; i++) {
		if (!cmd_secret_pos(C.line, i))
			console_out(C.line + i, 1);
		else if (((uint8_t)C.line[i] & 0xc0) != 0x80)
			console_out("*", 1);
	}
}

static void run_queued(void)
{
	while (C.qn) {
		char line[CON_LINE_MAX + 1], *argv[16];
		size_t argc;
		memcpy(line, C.q[C.qh], sizeof line);
		memset(C.q[C.qh], 0, sizeof C.q[C.qh]);
		C.qh = (C.qh + 1) % LINEQ;
		C.qn--;
		argc = esp_console_split_argv(line, argv, 16);
		if (argc > 0)
			cmd_run(0, (int)argc, argv);
		memset(line, 0, sizeof line);
		prompt();
	}
}

/* ---------------------------------------------------------------- uart bridge */

static int g_uart_on, g_uart_tx = -1, g_uart_rx = -1;

static int uart_write_(mt_session *s, const uint8_t *d, size_t n)
{
	int r;
	if (!g_uart_on)
		return -1;
	r = uart_write_bytes(BRIDGE_UART, d, n); /* queues into the 512-byte TX ring, blocks while it is full */
	if (r == (int)n)
		return 0;
	return r > 0 ? -2 : -1; /* -2: some bytes may have left (the core reports "partial") */
}

int sess_uart_active(void) { return g_uart_on; }

int sess_uart_pins(int *tx, int *rx)
{
	*tx = g_uart_tx;
	*rx = g_uart_rx;
	return g_uart_on;
}

int sess_uart_apply(int baud, int tx, int rx)
{
	uart_config_t uc = {.baud_rate = baud,
	                    .data_bits = UART_DATA_8_BITS,
	                    .parity = UART_PARITY_DISABLE,
	                    .stop_bits = UART_STOP_BITS_1,
	                    .flow_ctrl = UART_HW_FLOWCTRL_DISABLE,
	                    .source_clk = UART_SCLK_DEFAULT};
	if (!uart_is_driver_installed(BRIDGE_UART) && uart_driver_install(BRIDGE_UART, 1024, 512, 0, NULL, 0) != ESP_OK)
		return -1;
	if (uart_param_config(BRIDGE_UART, &uc) != ESP_OK || uart_set_pin(BRIDGE_UART, tx, rx, -1, -1) != ESP_OK)
		return -1;
	g_uart_tx = tx;
	g_uart_rx = rx;
	g_uart_on = 1;
	return 0;
}

static void uart_pump(void)
{
	uint8_t b[256];
	int n;
	if (!g_uart_on)
		return;
	while ((n = uart_read_bytes(BRIDGE_UART, b, sizeof b, 0)) > 0)
		mt_vt_feed(&vt_uart, b, (size_t)n);
}

/* ---------------------------------------------------------------- setup and poll */

static void sess_def(mt_session *s, const char *name, const char *cmd, mt_vt *vt)
{
	memset(s, 0, sizeof *s);
	snprintf(s->name, sizeof s->name, "%s", name);
	s->kind = "other";
	s->command = cmd;
	s->vt = vt;
}

int sess_init(void)
{
	if (vt_alloc(&vt_console, SB_CONSOLE) || vt_alloc(&vt_log, SB_LOG))
		return -1;
	sess_def(&g_console, "console", "console", &vt_console);
	g_console.write = console_write;
	sess_def(&g_log, "log", "log", &vt_log);
	g_log.readonly = 1;
	g_prev_vprintf = esp_log_set_vprintf(log_vprintf);

	/* uart bridge: only when both pins are configured locally and pass the pin rules */
	{
		int tx = cfg_get_int("uart_tx", -1), rx = cfg_get_int("uart_rx", -1), baud = cfg_get_int("uart_baud", 115200);
		if (tx >= 0 && rx >= 0) {
			char err[80];
			if (pin_check(tx, 1, err, sizeof err) || pin_check(rx, 0, err, sizeof err)) {
				ESP_LOGW(TAG, "uart bridge disabled: %s", err);
			} else if (vt_alloc(&vt_uart, SB_UART) || sess_uart_apply(baud, tx, rx)) {
				ESP_LOGW(TAG, "uart bridge disabled: cannot start UART1");
			} else {
				sess_def(&g_uart, "uart", "uart", &vt_uart);
				g_uart.write = uart_write_;
				ESP_LOGI(TAG, "uart bridge on UART1 tx=%d rx=%d %d baud", tx, rx, baud);
			}
		}
	}
	console_out("Minion thin Worker console. Type 'help'.\n", 41);
	prompt();
	return 0;
}

void sess_set_worker(mt_worker *w) { g_w = w; }
mt_worker *sess_worker(void) { return g_w; }

void sess_status(mt_status *st, int *have)
{
	portENTER_CRITICAL(&g_mux);
	*st = g_st;
	*have = g_st_have;
	portEXIT_CRITICAL(&g_mux);
}

void sess_poll(void)
{
	int64_t now = esp_timer_get_time() / 1000;
	drain_log();
	uart_pump();
	if (C.qn) {
		cmd_lock();
		run_queued();
		cmd_unlock();
	}
	cmd_poll_watch();
	if (g_w && now - g_st_mono >= 500) { /* status snapshot for the local console task */
		mt_status st;
		static int was_connected;
		mt_worker_status(g_w, &st);
		portENTER_CRITICAL(&g_mux);
		g_st = st;
		g_st_have = 1;
		portEXIT_CRITICAL(&g_mux);
		g_st_mono = now;
		if (st.connected && !was_connected)
			ESP_LOGI(TAG, "connected: 8-bit heap free %u (min %u, largest block %u), worker stack free %u bytes",
			         (unsigned)heap_caps_get_free_size(MALLOC_CAP_8BIT),
			         (unsigned)heap_caps_get_minimum_free_size(MALLOC_CAP_8BIT),
			         (unsigned)heap_caps_get_largest_free_block(MALLOC_CAP_8BIT),
			         (unsigned)uxTaskGetStackHighWaterMark(NULL));
		was_connected = st.connected;
	}
}
