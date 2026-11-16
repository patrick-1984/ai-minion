/* Minion thin Worker for ESP32-family chips: boot order and the worker task.
 *
 *   1. NVS (the "minion" partition: identity + config), pins to their safe input state, session screens
 *   2. Wi-Fi + SNTP; only after esp_wifi_start() (RF on, true RNG) may the identity seed be generated
 *   3. worker task: mt_worker_new() (loads or creates the seed) and mt_worker_run() forever
 *   4. local serial console (esp_console REPL) for the administrative commands
 *   5. app_main stays as the 1 s housekeeping loop (time floor, SNTP fallback) */
#include <string.h>

#include "esp_console.h"
#include "esp_heap_caps.h"
#include "esp_log.h"
#include "esp_system.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"

#include "app.h"

#define WORKER_STACK 10240 /* measured peak 6.7 KB (TLS 1.3 handshake, enrollment, operations) */

static const char *TAG = "app";
static mt_platform g_pf;
TaskHandle_t g_worker_task;
static char g_hub[128], g_name[64];

static void worker_task(void *arg)
{
	mt_session *ss[3];
	mt_config cfg;
	mt_worker *w;
	size_t n = 0;
	uint8_t one;

	/* Nothing to connect to until the Hub URL is set (local console: config set hub ..., then reboot). */
	while (cfg_get_str("hub", g_hub, sizeof g_hub) != 0 || !g_hub[0]) {
		static int told;
		if (!told++)
			ESP_LOGW(TAG, "no Hub URL: set it on the serial console (config set hub https://...) and reboot");
		sess_poll();
		vTaskDelay(pdMS_TO_TICKS(50));
	}
	if (cfg_get_str("name", g_name, sizeof g_name) != 0 || !g_name[0])
		cfg_default_name(g_name, sizeof g_name);

	ss[n++] = &g_console;
	ss[n++] = &g_log;
	if (sess_uart_active())
		ss[n++] = &g_uart;
	memset(&cfg, 0, sizeof cfg);
	cfg.hub_url = g_hub;
	cfg.name = g_name;
	cfg.sessions = ss;
	cfg.nsessions = n;
	cfg.rx_cap = 16404 + 64; /* one full record */
	cfg.frame_cap = 2048;    /* larger op requests (long send_text) are answered message_too_large */
	cfg.out_cap = 8 * 1024;  /* snapshot / capture of an 80x24 screen with 2-3 KB scrollback */
	cfg.max_streams = 4;     /* ev + 3 concurrent operations */
	cfg.results_ring = 16;   /* remembered write results (about 200 bytes each) */
	cfg.tx_rec_cap = 4096;   /* outgoing records of 4 KB: saves about 24 KB of heap */
	w = mt_worker_new(&g_pf, &cfg);
	if (!w) {
		ESP_LOGE(TAG, "cannot start the Worker (bad Hub URL '%s' or out of memory)", g_hub);
		for (;;) {
			sess_poll();
			vTaskDelay(pdMS_TO_TICKS(50));
		}
	}
	sess_set_worker(w);
	ESP_LOGI(TAG, "Worker \"%s\" for %s; 8-bit heap free %u, worker stack free %u", g_name, g_hub,
	         (unsigned)heap_caps_get_free_size(MALLOC_CAP_8BIT), (unsigned)uxTaskGetStackHighWaterMark(NULL));
	for (;;) {
		int r = mt_worker_run(w);
		if (r == -2) {
			/* A signed revoke erased the enrollment and the key. A new invitation key needs a NEW identity: the
			 * old key is tombstoned in the Hub. Restart, so the next boot generates a fresh seed. */
			ESP_LOGW(TAG, "revoked by the Hub; give a new invitation key with 'enroll' on the serial console");
			while (store_get(NULL, MT_KEY_INVITE, &one, 1) <= 0) {
				sess_poll();
				vTaskDelay(pdMS_TO_TICKS(200));
			}
			ESP_LOGI(TAG, "new invitation key: restarting with a new identity");
			vTaskDelay(pdMS_TO_TICKS(500));
			esp_restart();
		}
		sess_poll();
		vTaskDelay(pdMS_TO_TICKS(1000));
	}
}

static void start_repl(void)
{
	esp_console_repl_t *repl = NULL;
	esp_console_repl_config_t rc = ESP_CONSOLE_REPL_CONFIG_DEFAULT();
	esp_err_t e;
	rc.prompt = "esp> ";
	rc.max_history_len = 16;
	rc.task_stack_size = 5120;
	rc.max_cmdline_length = 200;
#if defined(CONFIG_ESP_CONSOLE_UART_DEFAULT) || defined(CONFIG_ESP_CONSOLE_UART_CUSTOM)
	esp_console_dev_uart_config_t hc = ESP_CONSOLE_DEV_UART_CONFIG_DEFAULT();
	e = esp_console_new_repl_uart(&hc, &rc, &repl);
#elif defined(CONFIG_ESP_CONSOLE_USB_CDC)
	esp_console_dev_usb_cdc_config_t hc = ESP_CONSOLE_DEV_CDC_CONFIG_DEFAULT();
	e = esp_console_new_repl_usb_cdc(&hc, &rc, &repl);
#elif defined(CONFIG_ESP_CONSOLE_USB_SERIAL_JTAG)
	esp_console_dev_usb_serial_jtag_config_t hc = ESP_CONSOLE_DEV_USB_SERIAL_JTAG_CONFIG_DEFAULT();
	e = esp_console_new_repl_usb_serial_jtag(&hc, &rc, &repl);
#else
	e = ESP_ERR_NOT_SUPPORTED;
#endif
	if (e != ESP_OK) {
		ESP_LOGE(TAG, "no local console: %s", esp_err_to_name(e));
		return;
	}
	cmd_register_local();
	esp_console_start_repl(repl);
}

void app_main(void)
{
	int ok = cfg_init() == 0;
	cmd_init();
	if (sess_init() != 0)
		ESP_LOGE(TAG, "out of memory for the session screens");
	ESP_LOGI(TAG, "8-bit heap free %u before the network", (unsigned)heap_caps_get_free_size(MALLOC_CAP_8BIT));
	net_start(); /* from here on the RF is on: the RNG is a true RNG */
	ESP_LOGI(TAG, "8-bit heap free %u with the network up", (unsigned)heap_caps_get_free_size(MALLOC_CAP_8BIT));
	platform_init(&g_pf);
	banner();
	if (!ok)
		ESP_LOGE(TAG, "the 'minion' NVS partition is missing or broken: flash the image with its partition table");
	else
		xTaskCreate(worker_task, "minion", WORKER_STACK, NULL, 5, &g_worker_task);
	start_repl();
	for (;;) {
		vTaskDelay(pdMS_TO_TICKS(1000));
		net_tick();
	}
}
