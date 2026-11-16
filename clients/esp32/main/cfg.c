/* NVS: the core store (namespace "minion", one blob per key) and the device config (namespace "mcfg"), both on
 * the dedicated "minion" partition. NVS writes one key atomically and survives power loss mid-write. */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "esp_log.h"
#include "esp_mac.h"
#include "nvs.h"
#include "nvs_flash.h"
#include "sdkconfig.h"

#include "app.h"

static const char *TAG = "cfg";
static nvs_handle_t h_store, h_cfg;

int cfg_init(void)
{
	esp_err_t e = nvs_flash_init(); /* default "nvs": Wi-Fi/PHY data only */
	if (e == ESP_ERR_NVS_NO_FREE_PAGES || e == ESP_ERR_NVS_NEW_VERSION_FOUND) {
		nvs_flash_erase();
		e = nvs_flash_init();
	}
	if (e != ESP_OK)
		ESP_LOGE(TAG, "default nvs: %s", esp_err_to_name(e));
	/* The identity partition is never erased automatically: a format problem is reported, not "repaired". */
	e = nvs_flash_init_partition(APP_PART);
	if (e != ESP_OK) {
		ESP_LOGE(TAG, "partition '%s': %s (identity and config unavailable)", APP_PART, esp_err_to_name(e));
		return -1;
	}
	if (nvs_open_from_partition(APP_PART, APP_NS_STORE, NVS_READWRITE, &h_store) != ESP_OK ||
	    nvs_open_from_partition(APP_PART, APP_NS_CFG, NVS_READWRITE, &h_cfg) != ESP_OK) {
		ESP_LOGE(TAG, "cannot open the NVS namespaces");
		return -1;
	}
	return 0;
}

/* ---------------------------------------------------------------- core store hooks */

int store_get(void *ctx, const char *key, uint8_t *buf, size_t cap)
{
	size_t len = 0;
	(void)ctx;
	if (!h_store || nvs_get_blob(h_store, key, NULL, &len) != ESP_OK)
		return -1;
	if (len <= cap) {
		if (nvs_get_blob(h_store, key, buf, &len) != ESP_OK)
			return -1;
		return (int)len;
	}
	/* like read(2): a short buffer gets the first cap bytes (the core probes "invite" with cap 1) */
	{
		uint8_t *tmp = malloc(len);
		int r = -1;
		if (tmp && nvs_get_blob(h_store, key, tmp, &len) == ESP_OK) {
			memcpy(buf, tmp, cap);
			r = (int)cap;
		}
		if (tmp) {
			memset(tmp, 0, len);
			free(tmp);
		}
		return r;
	}
}

int store_set(void *ctx, const char *key, const uint8_t *buf, size_t n)
{
	esp_err_t e;
	(void)ctx;
	if (!h_store)
		return -1;
	if (n == 0) {
		e = nvs_erase_key(h_store, key);
		if (e != ESP_OK && e != ESP_ERR_NVS_NOT_FOUND)
			return -1;
	} else if (nvs_set_blob(h_store, key, buf, n) != ESP_OK) {
		return -1;
	}
	return nvs_commit(h_store) == ESP_OK ? 0 : -1;
}

int store_erase_all(void)
{
	if (!h_store || nvs_erase_all(h_store) != ESP_OK)
		return -1;
	return nvs_commit(h_store) == ESP_OK ? 0 : -1;
}

/* ---------------------------------------------------------------- config */

int cfg_get_str(const char *key, char *out, size_t cap)
{
	size_t len = cap;
	if (cap)
		out[0] = 0;
	if (!h_cfg || nvs_get_str(h_cfg, key, out, &len) != ESP_OK) {
		if (cap)
			out[0] = 0;
		return -1;
	}
	return 0;
}

int cfg_set_str(const char *key, const char *val)
{
	esp_err_t e;
	if (!h_cfg)
		return -1;
	if (!val || !val[0]) {
		e = nvs_erase_key(h_cfg, key);
		if (e != ESP_OK && e != ESP_ERR_NVS_NOT_FOUND)
			return -1;
	} else if (nvs_set_str(h_cfg, key, val) != ESP_OK) {
		return -1;
	}
	return nvs_commit(h_cfg) == ESP_OK ? 0 : -1;
}

int cfg_get_int(const char *key, int def)
{
	char b[16], *end;
	long v;
	if (cfg_get_str(key, b, sizeof b) != 0 || !b[0])
		return def;
	v = strtol(b, &end, 10);
	return *end ? def : (int)v;
}

int cfg_get_i64(const char *key, int64_t *v)
{
	return h_cfg && nvs_get_i64(h_cfg, key, v) == ESP_OK ? 0 : -1;
}

int cfg_set_i64(const char *key, int64_t v)
{
	if (!h_cfg || nvs_set_i64(h_cfg, key, v) != ESP_OK)
		return -1;
	return nvs_commit(h_cfg) == ESP_OK ? 0 : -1;
}

int cfg_erase_all(void)
{
	if (!h_cfg || nvs_erase_all(h_cfg) != ESP_OK)
		return -1;
	return nvs_commit(h_cfg) == ESP_OK ? 0 : -1;
}

/* "esp32-<chip>-<last 3 MAC bytes>", e.g. esp32-c3-a1b2c3 (plain ESP32: esp32-a1b2c3). */
void cfg_default_name(char *out, size_t cap)
{
	uint8_t mac[6] = {0};
	const char *chip = CONFIG_IDF_TARGET; /* "esp32", "esp32c3", ... */
	esp_read_mac(mac, ESP_MAC_WIFI_STA);
	chip += strncmp(chip, "esp32", 5) == 0 ? 5 : 0;
	snprintf(out, cap, "esp32%s%s-%02x%02x%02x", *chip ? "-" : "", chip, mac[3], mac[4], mac[5]);
}
