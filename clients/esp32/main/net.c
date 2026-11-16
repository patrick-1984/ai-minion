/* Wi-Fi station (credentials from NVS, reconnect forever with backoff), SNTP (DHCP-offered server + pool.ntp.org)
 * and a persisted "time floor": the last known wall time, so the device can still check the Hub certificate
 * window when NTP is blocked. The floor is written on SNTP sync and at most once per hour (flash wear). */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/time.h>
#include <time.h>

#include "esp_eth.h"
#include "esp_event.h"
#include "esp_log.h"
#include "esp_netif.h"
#include "esp_netif_sntp.h"
#include "esp_timer.h"
#include "esp_wifi.h"
#include "freertos/FreeRTOS.h"

#include "app.h"

static const char *TAG = "net";
static esp_netif_t *g_netif;
static esp_timer_handle_t g_retry;
static volatile int g_rf_on, g_has_ip, g_have_creds, g_sntp_synced, g_floor_used;
static volatile uint32_t g_gen;
static int g_backoff_ms = 1000;
static int64_t g_ip_mono_ms, g_floor_written_mono_ms = -1;

int net_rf_on(void) { return g_rf_on; }
uint32_t net_generation(void) { return g_gen; }

static int64_t mono_ms(void) { return esp_timer_get_time() / 1000; }

static int clock_valid(void)
{
	struct timeval tv;
	gettimeofday(&tv, NULL);
	return tv.tv_sec >= TIME_VALID_S;
}

const char *net_time_state(void)
{
	if (g_sntp_synced)
		return "SNTP synced";
	if (g_floor_used)
		return "set from the stored time floor (SNTP not synced)";
	return clock_valid() ? "set (SNTP not synced)" : "NOT SET (waiting for SNTP)";
}

static void retry_cb(void *arg)
{
	if (g_have_creds)
		esp_wifi_connect();
}

static void on_event(void *arg, esp_event_base_t base, int32_t id, void *data)
{
	if (base == WIFI_EVENT && id == WIFI_EVENT_STA_START) {
		if (g_have_creds)
			esp_wifi_connect();
	} else if (base == WIFI_EVENT && id == WIFI_EVENT_STA_DISCONNECTED) {
		wifi_event_sta_disconnected_t *d = data;
		g_has_ip = 0;
		if (!g_have_creds)
			return;
		ESP_LOGW(TAG, "Wi-Fi disconnected (reason %d); retry in %d ms", d ? d->reason : 0, g_backoff_ms);
		esp_timer_stop(g_retry);
		esp_timer_start_once(g_retry, (uint64_t)g_backoff_ms * 1000);
		g_backoff_ms = g_backoff_ms < 30000 ? g_backoff_ms * 2 : 30000;
	} else if (base == IP_EVENT && (id == IP_EVENT_STA_GOT_IP || id == IP_EVENT_ETH_GOT_IP)) {
		ip_event_got_ip_t *e = data;
		ESP_LOGI(TAG, "got IP " IPSTR, IP2STR(&e->ip_info.ip));
		g_backoff_ms = 1000;
		g_has_ip = 1;
		g_ip_mono_ms = mono_ms();
		g_gen++;
	}
}

static void on_sntp(struct timeval *tv)
{
	g_sntp_synced = 1;
	g_gen++;
}

static void load_creds(wifi_config_t *wc)
{
	char ssid[33], pass[65];
	memset(wc, 0, sizeof *wc);
	cfg_get_str("wifi_ssid", ssid, sizeof ssid);
	cfg_get_str("wifi_pass", pass, sizeof pass);
	memcpy(wc->sta.ssid, ssid, strlen(ssid));
	memcpy(wc->sta.password, pass, strlen(pass));
	wc->sta.threshold.authmode = pass[0] ? WIFI_AUTH_WPA2_PSK : WIFI_AUTH_OPEN;
	wc->sta.sae_pwe_h2e = WPA3_SAE_PWE_BOTH;
	wc->sta.scan_method = WIFI_ALL_CHANNEL_SCAN;
	wc->sta.sort_method = WIFI_CONNECT_AP_BY_SIGNAL;
	g_have_creds = ssid[0] != 0;
	memset(pass, 0, sizeof pass);
}

#if CONFIG_MINION_QEMU_OPENETH
/* Emulator tests: QEMU's OpenCores Ethernet with DHCP replaces Wi-Fi. */
static void eth_start(void)
{
	esp_netif_config_t nc = ESP_NETIF_DEFAULT_ETH();
	eth_mac_config_t mc = ETH_MAC_DEFAULT_CONFIG();
	eth_phy_config_t pc = ETH_PHY_DEFAULT_CONFIG();
	esp_eth_handle_t h = NULL;
	esp_eth_config_t ec;
	esp_eth_mac_t *mac;
	esp_eth_phy_t *phy;
	pc.autonego_timeout_ms = 100;
	mac = esp_eth_mac_new_openeth(&mc);
	phy = esp_eth_phy_new_dp83848(&pc);
	ec = (esp_eth_config_t)ETH_DEFAULT_CONFIG(mac, phy);
	g_netif = esp_netif_new(&nc);
	ESP_ERROR_CHECK(esp_eth_driver_install(&ec, &h));
	esp_netif_attach(g_netif, esp_eth_new_netif_glue(h));
	esp_event_handler_register(IP_EVENT, IP_EVENT_ETH_GOT_IP, on_event, NULL);
	ESP_ERROR_CHECK(esp_eth_start(h));
}
#endif

void net_start(void)
{
	wifi_init_config_t ic = WIFI_INIT_CONFIG_DEFAULT();
	wifi_config_t wc;
	esp_timer_create_args_t ta = {.callback = retry_cb, .name = "wifi_retry"};
	char name[64], host[33];
	size_t i, j;

	ESP_ERROR_CHECK(esp_netif_init());
	ESP_ERROR_CHECK(esp_event_loop_create_default());
#if CONFIG_MINION_QEMU_OPENETH
	{
		esp_sntp_config_t sc = ESP_NETIF_SNTP_DEFAULT_CONFIG("pool.ntp.org");
		sc.start = false;
		sc.server_from_dhcp = true;
		sc.renew_servers_after_new_IP = true;
		sc.index_of_first_server = 1;
		sc.ip_event_to_renew = IP_EVENT_ETH_GOT_IP;
		sc.wait_for_sync = false;
		sc.sync_cb = on_sntp;
		esp_netif_sntp_init(&sc);
	}
	eth_start();
	g_rf_on = 1; /* emulator: no RF; QEMU's RNG comes from the host */
	esp_netif_sntp_start();
	(void)ic, (void)wc, (void)ta, (void)name, (void)host, (void)i, (void)j;
	return;
#endif
	g_netif = esp_netif_create_default_wifi_sta();
	ESP_ERROR_CHECK(esp_wifi_init(&ic));
	esp_wifi_set_storage(WIFI_STORAGE_RAM); /* credentials live in the "minion" partition only */
	esp_timer_create(&ta, &g_retry);
	esp_event_handler_register(WIFI_EVENT, ESP_EVENT_ANY_ID, on_event, NULL);
	esp_event_handler_register(IP_EVENT, IP_EVENT_STA_GOT_IP, on_event, NULL);

	/* DHCP hostname: the Worker name reduced to [a-z0-9-] */
	if (cfg_get_str("name", name, sizeof name) != 0 || !name[0])
		cfg_default_name(name, sizeof name);
	for (i = j = 0; name[i] && j < sizeof host - 1; i++) {
		char c = name[i];
		if ((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-')
			host[j++] = c;
		else if (c >= 'A' && c <= 'Z')
			host[j++] = (char)(c - 'A' + 'a');
	}
	host[j] = 0;
	if (j)
		esp_netif_set_hostname(g_netif, host);

	/* SNTP before DHCP runs, so the DHCP NTP option is accepted (index 0 = DHCP server, 1 = pool). */
	{
		esp_sntp_config_t sc = ESP_NETIF_SNTP_DEFAULT_CONFIG("pool.ntp.org");
		sc.start = false;
		sc.server_from_dhcp = true;
		sc.renew_servers_after_new_IP = true;
		sc.index_of_first_server = 1;
		sc.ip_event_to_renew = IP_EVENT_STA_GOT_IP;
		sc.wait_for_sync = false;
		sc.sync_cb = on_sntp;
		if (esp_netif_sntp_init(&sc) != ESP_OK)
			ESP_LOGE(TAG, "SNTP init failed");
	}

	load_creds(&wc);
	ESP_ERROR_CHECK(esp_wifi_set_mode(WIFI_MODE_STA));
	ESP_ERROR_CHECK(esp_wifi_set_config(WIFI_IF_STA, &wc));
	memset(&wc, 0, sizeof wc);
	ESP_ERROR_CHECK(esp_wifi_start());
	g_rf_on = 1; /* RF on: esp_fill_random() is a true RNG from here on */
	esp_netif_sntp_start();
	if (!g_have_creds)
		ESP_LOGW(TAG, "no Wi-Fi credentials: use 'wifi set <ssid>' on the serial console");
}

void net_wifi_apply(void)
{
	wifi_config_t wc;
#if CONFIG_MINION_QEMU_OPENETH
	return;
#endif
	load_creds(&wc);
	esp_timer_stop(g_retry);
	g_backoff_ms = 1000;
	esp_wifi_disconnect();
	esp_wifi_set_config(WIFI_IF_STA, &wc);
	memset(&wc, 0, sizeof wc);
	if (g_have_creds)
		esp_wifi_connect();
}

/* Once per second from app_main: time floor bookkeeping and the fallback when SNTP does not answer. */
void net_tick(void)
{
	static int64_t floor_s = -1;
	int64_t now_mono = mono_ms();
	struct timeval tv;
	if (floor_s < 0 && cfg_get_i64("time_floor", &floor_s) != 0)
		floor_s = 0;
	gettimeofday(&tv, NULL);
	if (tv.tv_sec >= TIME_VALID_S) {
		/* write on the first SNTP sync of this boot, then at most hourly (a clock that runs from the floor is
		 * still a lower bound of the real time, so it may advance the floor too, after one hour) */
		int due = g_floor_written_mono_ms < 0 ? (g_sntp_synced || now_mono >= 3600000)
		                                       : now_mono - g_floor_written_mono_ms >= 3600000;
		if (due && tv.tv_sec > floor_s) {
			if (cfg_set_i64("time_floor", (int64_t)tv.tv_sec) == 0) {
				g_floor_written_mono_ms = now_mono;
				floor_s = tv.tv_sec;
			}
		} else if (due) {
			g_floor_written_mono_ms = now_mono; /* clock behind the floor: keep the floor, look again in 1 h */
		}
		return;
	}
	if (g_has_ip && !g_sntp_synced && !g_floor_used && now_mono - g_ip_mono_ms >= 30000 && floor_s >= TIME_VALID_S) {
		struct timeval set = {.tv_sec = (time_t)floor_s, .tv_usec = 0};
		settimeofday(&set, NULL);
		g_floor_used = 1;
		g_gen++;
		ESP_LOGW(TAG, "SNTP has not answered for 30 s: clock set to the stored time floor (%lld); "
		              "the Hub certificate window is checked against this lower bound",
		         (long long)floor_s);
	}
}

void net_status(int local)
{
	wifi_ap_record_t ap;
	esp_netif_ip_info_t ip;
	char ssid[33];
	cfg_get_str("wifi_ssid", ssid, sizeof ssid);
#if CONFIG_MINION_QEMU_OPENETH
	ssid[0] = 0;
	if (g_has_ip && esp_netif_get_ip_info(g_netif, &ip) == ESP_OK)
		cmd_printf(local, "net: QEMU Ethernet, ip " IPSTR "\n", IP2STR(&ip.ip));
	return;
#endif
	if (!ssid[0]) {
		cmd_printf(local, "wifi: not configured (local console: wifi set <ssid>)\n");
		return;
	}
	if (esp_wifi_sta_get_ap_info(&ap) == ESP_OK) {
		cmd_printf(local, "wifi: connected to \"%s\" rssi %d dBm channel %d\n", (const char *)ap.ssid, ap.rssi,
		           ap.primary);
	} else {
		cmd_printf(local, "wifi: \"%s\" not connected (retrying)\n", ssid);
	}
	if (g_has_ip && esp_netif_get_ip_info(g_netif, &ip) == ESP_OK)
		cmd_printf(local, "ip: " IPSTR " gw " IPSTR "\n", IP2STR(&ip.ip), IP2STR(&ip.gw));
	else
		cmd_printf(local, "ip: none\n");
}

int net_scan(int local)
{
	uint16_t n = 0, i, max = 16;
	wifi_ap_record_t *r;
	esp_err_t e = esp_wifi_scan_start(NULL, true);
	if (e != ESP_OK) {
		cmd_printf(local, "ERR scan: %s (busy connecting? try again)\n", esp_err_to_name(e));
		return 1;
	}
	esp_wifi_scan_get_ap_num(&n);
	if (n > max)
		n = max;
	r = calloc(n ? n : 1, sizeof *r);
	if (!r) {
		esp_wifi_clear_ap_list();
		cmd_printf(local, "ERR out of memory\n");
		return 1;
	}
	esp_wifi_scan_get_ap_records(&n, r);
	cmd_printf(local, "%u networks:\n", (unsigned)n);
	for (i = 0; i < n; i++)
		cmd_printf(local, "  %4d dBm  ch %2d  %s%s\n", r[i].rssi, r[i].primary, (const char *)r[i].ssid,
		           r[i].authmode == WIFI_AUTH_OPEN ? "  (open)" : "");
	free(r);
	return 0;
}
