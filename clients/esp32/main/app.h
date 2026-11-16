/* Minion thin Worker for ESP32: shared declarations of the platform modules.
 *
 * Threads: the worker task runs mt_worker_run() and is the ONLY task that feeds the session screens (mt_vt)
 * and runs commands typed into the Hub "console" session. The local REPL task runs local commands; both take
 * the command lock (cmd_lock). Log lines from any task go through a small ring drained by the worker task. */
#ifndef MT_ESP_APP_H
#define MT_ESP_APP_H

#include <stdarg.h>
#include <stddef.h>
#include <stdint.h>

#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "mt_worker.h"

#define APP_PART "minion"   /* NVS partition at a fixed offset (partitions.csv) */
#define APP_NS_STORE "minion" /* core store: seed, state, invite, share */
#define APP_NS_CFG "mcfg"     /* device config: hub, name, Wi-Fi, pins, time floor */
#define TIME_VALID_S 1735689600LL /* 2025-01-01T00:00:00Z */

/* ---- cfg.c: NVS config and the core store */
int cfg_init(void);
int cfg_get_str(const char *key, char *out, size_t cap); /* 0 ok, -1 missing */
int cfg_set_str(const char *key, const char *val);       /* NULL or "" erases */
int cfg_get_int(const char *key, int def);
int cfg_get_i64(const char *key, int64_t *v);
int cfg_set_i64(const char *key, int64_t v);
int cfg_erase_all(void);
int store_get(void *ctx, const char *key, uint8_t *buf, size_t cap);
int store_set(void *ctx, const char *key, const uint8_t *buf, size_t n);
int store_erase_all(void);
void cfg_default_name(char *out, size_t cap);

/* ---- net.c: Wi-Fi station, SNTP and the persisted time floor */
void net_start(void);           /* returns after esp_wifi_start(): the RF is on, the RNG is a true RNG */
void net_tick(void);            /* once per second from app_main */
void net_wifi_apply(void);      /* re-read SSID/password from NVS and reconnect */
int net_rf_on(void);
uint32_t net_generation(void);  /* changes when the network or the clock became usable */
const char *net_time_state(void);
void net_status(int local);     /* Wi-Fi lines for "status" and "wifi status" */
int net_scan(int local);

/* ---- sessions.c: console, uart, log sessions */
extern mt_session g_console, g_log, g_uart;
int sess_init(void);            /* allocates the screens, hooks the log */
int sess_uart_active(void);
int sess_uart_pins(int *tx, int *rx);
int sess_uart_apply(int baud, int tx, int rx); /* reconfigure the running bridge */
void sess_poll(void);           /* worker task: drain log, UART RX, run queued console lines, GPIO watch */
void console_out(const char *s, size_t n); /* worker task: text into the console screen (\n -> \r\n) */
void console_notice(const char *msg);      /* worker task: a line above the prompt */
void sess_set_worker(mt_worker *w);
mt_worker *sess_worker(void);
void sess_status(mt_status *st, int *have); /* snapshot for other tasks */

/* ---- cmds.c: the command set, shared by the local serial console (local=1) and the Hub console (local=0) */
void cmd_init(void);            /* pins in gpio_allow start as inputs */
void cmd_lock(void);
void cmd_unlock(void);
int cmd_run(int local, int argc, char **argv); /* 0 ok, 1 error; takes the command lock */
void cmd_printf(int local, const char *fmt, ...) __attribute__((format(printf, 2, 3)));
void cmd_poll_watch(void);      /* worker task: debounced GPIO watch */
int pin_check(int pin, int output, char *err, size_t cap); /* 0 when the pin may be used */
int cmd_secret_pos(const char *line, size_t len); /* 1 when the next typed char is part of a secret */
void cmd_register_local(void);  /* esp_console commands */
void banner(void);

/* ---- app_main.c */
extern TaskHandle_t g_worker_task; /* NULL until the worker task runs */

/* ---- platform.c */
void platform_init(mt_platform *p);

#endif
