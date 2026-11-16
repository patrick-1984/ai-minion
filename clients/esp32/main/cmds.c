/* The command set of the device, shared by the local serial console (local=1) and the Hub "console" session
 * (local=0). Administrative verbs (enroll, config set, wifi set, uart set|off, factory-reset) need physical
 * presence: they are refused from the Hub with "ERR local console only".
 *
 * Pin safety: GPIO/ADC/PWM/I2C/UART act only on pins in gpio_allow (default: none). Flash/PSRAM, console UART
 * and USB pins are always refused (table below + the IDF reservation map); strapping pins only when listed
 * as "<pin>!" in gpio_allow. Allowed pins start as inputs (high-Z) at boot; no output state is restored. */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/time.h>
#include <time.h>

#include "driver/gpio.h"
#include "driver/i2c_master.h"
#include "driver/ledc.h"
#include "esp_adc/adc_cali.h"
#include "esp_adc/adc_cali_scheme.h"
#include "esp_adc/adc_oneshot.h"
#include "esp_chip_info.h"
#include "esp_console.h"
#include "esp_heap_caps.h"
#include "esp_idf_version.h"
#include "esp_log.h"
#include "esp_private/esp_gpio_reserve.h"
#include "esp_system.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/semphr.h"
#include "freertos/task.h"
#include "linenoise/linenoise.h"
#include "soc/soc_caps.h"
#include "soc/spi_pins.h"
#include "soc/uart_pins.h"
#if CONFIG_IDF_TARGET_ESP32S3
#include "soc/usb_pins.h"
#endif

#include "app.h"
#include "mt_enc.h"
#include "mt_ws.h"

#define B(n) (1ULL << (n))
#define NPINS SOC_GPIO_PIN_COUNT

/* ---------------------------------------------------------------- per-chip pin tables
 * FORBID: always refused. Flash pins come from soc/spi_pins.h (MSPI_IOMUX_PIN_NUM_*). STRAP: strapping pins. */
#define MSPI_PINS                                                                                                   \
	(B(MSPI_IOMUX_PIN_NUM_CLK) | B(MSPI_IOMUX_PIN_NUM_MISO) | B(MSPI_IOMUX_PIN_NUM_MOSI) |                         \
	 B(MSPI_IOMUX_PIN_NUM_HD) | B(MSPI_IOMUX_PIN_NUM_WP) | B(MSPI_IOMUX_PIN_NUM_CS0))
#if CONFIG_IDF_TARGET_ESP32
/* 6-11 flash; 16-17 PSRAM (WROVER) or in-package flash (PICO-D4): refused on every ESP32 */
#define FORBID (MSPI_PINS | B(16) | B(17))
#define STRAP (B(0) | B(2) | B(5) | B(12) | B(15))
#elif CONFIG_IDF_TARGET_ESP32S3
/* 26-32 flash/PSRAM, 33-37 octal flash/PSRAM (refused even without octal parts), 19/20 USB D-/D+ */
#define FORBID                                                                                                      \
	(MSPI_PINS | B(MSPI_IOMUX_PIN_NUM_CS1) | B(MSPI_IOMUX_PIN_NUM_D4) | B(MSPI_IOMUX_PIN_NUM_D5) |                 \
	 B(MSPI_IOMUX_PIN_NUM_D6) | B(MSPI_IOMUX_PIN_NUM_D7) | B(MSPI_IOMUX_PIN_NUM_DQS) | B(USBPHY_DM_NUM) |           \
	 B(USBPHY_DP_NUM))
#define STRAP (B(0) | B(3) | B(45) | B(46))
#elif CONFIG_IDF_TARGET_ESP32C3
/* 12-17 flash, 18/19 USB D-/D+ */
#define FORBID (MSPI_PINS | B(18) | B(19))
#define STRAP (B(2) | B(8) | B(9))
#elif CONFIG_IDF_TARGET_ESP32C6
/* 24-30 flash (27 is the flash supply pin on in-package flash), 12/13 USB D-/D+ */
#define FORBID (MSPI_PINS | B(27) | B(12) | B(13))
#define STRAP (B(4) | B(5) | B(8) | B(9) | B(15))
#else
#error "unsupported target: add its flash/PSRAM/USB/strapping pins to cmds.c"
#endif
#define CONSOLE_PINS (B(U0TXD_GPIO_NUM) | B(U0RXD_GPIO_NUM))

enum { M_IN, M_IN_PU, M_IN_PD, M_OUT, M_OD, M_PWM, M_ADC };
static const char *const mode_name[] = {"in", "in_pullup", "in_pulldown", "out", "od", "pwm", "adc"};

typedef struct {
	uint8_t mode, level, watch, stable, cand;
	int8_t pwm_ch, pwm_t;
	volatile uint8_t pulsing;
	int64_t cand_ms;
	esp_timer_handle_t pulse;
} pin_t;

static pin_t P[NPINS];
static uint64_t g_allow, g_ack, g_reserved;
static SemaphoreHandle_t g_lock;

void cmd_lock(void) { xSemaphoreTakeRecursive(g_lock, portMAX_DELAY); }
void cmd_unlock(void) { xSemaphoreGiveRecursive(g_lock); }

void cmd_printf(int local, const char *fmt, ...)
{
	va_list ap;
	va_start(ap, fmt);
	if (local) {
		vprintf(fmt, ap);
	} else {
		char b[160], *m;
		va_list ap2;
		int n;
		va_copy(ap2, ap);
		n = vsnprintf(b, sizeof b, fmt, ap);
		if (n >= (int)sizeof b && (m = malloc((size_t)n + 1)) != NULL) { /* long text (help) */
			vsnprintf(m, (size_t)n + 1, fmt, ap2);
			console_out(m, (size_t)n);
			free(m);
		} else if (n > 0) {
			console_out(b, n < (int)sizeof b ? (size_t)n : sizeof b - 1);
		}
		va_end(ap2);
	}
	va_end(ap);
}

#define OUT(...) cmd_printf(local, __VA_ARGS__)
#define ERR(...) (cmd_printf(local, "ERR " __VA_ARGS__), 1)

static void restart_cb(void *arg) { esp_restart(); }

/* Restart after ms: the worker task first sends the result of the send_text that carried the command. */
static void schedule_restart(int ms)
{
	static esp_timer_handle_t t;
	esp_timer_create_args_t ta = {.callback = restart_cb, .name = "restart"};
	if (t || esp_timer_create(&ta, &t) == ESP_OK)
		esp_timer_start_once(t, (uint64_t)ms * 1000);
}

/* ---------------------------------------------------------------- pins */

static int pin_forbidden(int pin)
{
	if (pin < 0 || pin >= NPINS || !GPIO_IS_VALID_GPIO(pin))
		return 1;
	return (FORBID & B(pin)) || (CONSOLE_PINS & B(pin)) || (g_reserved & B(pin));
}

int pin_check(int pin, int output, char *err, size_t cap)
{
	if (pin < 0 || pin >= NPINS || !GPIO_IS_VALID_GPIO(pin))
		snprintf(err, cap, "GPIO%d does not exist on this chip", pin);
	else if (pin_forbidden(pin))
		snprintf(err, cap, "GPIO%d is a flash/PSRAM/console/USB pin and always refused", pin);
	else if (!(g_allow & B(pin)))
		snprintf(err, cap, "GPIO%d is not in gpio_allow", pin);
	else if ((STRAP & B(pin)) && !(g_ack & B(pin)))
		snprintf(err, cap, "GPIO%d is a strapping pin; allow it explicitly as %d! in gpio_allow", pin, pin);
	else if (output && !GPIO_IS_VALID_OUTPUT_GPIO(pin))
		snprintf(err, cap, "GPIO%d is input-only", pin);
	else
		return 0;
	return -1;
}

/* Parse "4,5,12-14,2!" into masks. Returns 0, or -1 with a message. */
static int parse_allow(const char *s, uint64_t *allow, uint64_t *ack, char *err, size_t cap)
{
	*allow = *ack = 0;
	while (s && *s) {
		char *end;
		long a, b;
		int bang = 0;
		while (*s == ',' || *s == ' ')
			s++;
		if (!*s)
			break;
		a = strtol(s, &end, 10);
		if (end == s)
			goto bad;
		b = a;
		if (*end == '-') {
			s = end + 1;
			b = strtol(s, &end, 10);
			if (end == s || b < a)
				goto bad;
		}
		if (*end == '!')
			bang = 1, end++;
		if (*end && *end != ',' && *end != ' ')
			goto bad;
		for (; a <= b; a++) {
			if (a < 0 || a >= NPINS || !GPIO_IS_VALID_GPIO(a)) {
				snprintf(err, cap, "GPIO%ld does not exist on this chip", a);
				return -1;
			}
			if (pin_forbidden((int)a)) {
				snprintf(err, cap, "GPIO%ld is a flash/PSRAM/console/USB pin and cannot be allowed", a);
				return -1;
			}
			if ((STRAP & B(a)) && !bang) {
				snprintf(err, cap, "GPIO%ld is a strapping pin: write it as %ld! to allow it anyway", a, a);
				return -1;
			}
			*allow |= B(a);
			if (bang)
				*ack |= B(a);
		}
		s = end;
	}
	return 0;
bad:
	snprintf(err, cap, "bad pin list (example: 4,5,12-14,2!)");
	return -1;
}

static void pin_input(int pin)
{
	gpio_config_t gc = {.pin_bit_mask = B(pin), .mode = GPIO_MODE_INPUT};
	gpio_config(&gc);
	P[pin].mode = M_IN;
	P[pin].level = 0;
}

/* PWM: one LEDC channel per pin, timers shared by frequency (LEDC_TIMER_MAX different frequencies). */
static struct {
	uint32_t freq;
	uint8_t refs, bits;
} T[LEDC_TIMER_MAX];
static int8_t ch_pin[SOC_LEDC_CHANNEL_NUM];

static void pwm_stop(int pin)
{
	pin_t *p = &P[pin];
	if (p->pwm_ch >= 0) {
		ledc_stop(LEDC_LOW_SPEED_MODE, (ledc_channel_t)p->pwm_ch, 0);
		ch_pin[p->pwm_ch] = -1;
	}
	if (p->pwm_t >= 0 && T[p->pwm_t].refs)
		T[p->pwm_t].refs--;
	p->pwm_ch = p->pwm_t = -1;
}

static void pin_release(int pin) /* back to a plain input: stop PWM, end watch */
{
	if (P[pin].mode == M_PWM)
		pwm_stop(pin);
	P[pin].watch = 0;
	pin_input(pin);
}

void cmd_init(void)
{
	char b[128], err[80];
	int i;
	g_lock = xSemaphoreCreateRecursiveMutex();
	/* Pins ESP-IDF reserved at startup (flash, PSRAM). Taken once, before this app configures anything:
	 * later reservations are our own peripherals (LEDC, UART) and must not lock the pin out. */
	for (i = 0; i < NPINS; i++)
		if (GPIO_IS_VALID_GPIO(i) && esp_gpio_is_reserved(B(i)))
			g_reserved |= B(i);
	for (i = 0; i < NPINS; i++)
		P[i].pwm_ch = P[i].pwm_t = -1;
	for (i = 0; i < SOC_LEDC_CHANNEL_NUM; i++)
		ch_pin[i] = -1;
	if (cfg_get_str("gpio_allow", b, sizeof b) == 0 && parse_allow(b, &g_allow, &g_ack, err, sizeof err) != 0) {
		ESP_LOGW("gpio", "gpio_allow ignored: %s", err);
		g_allow = g_ack = 0;
	}
	for (i = 0; i < NPINS; i++)
		if (g_allow & B(i))
			pin_input(i); /* safe boot state: input, no pull, nothing driven */
}

static int arg_int(const char *s, long lo, long hi, long *v)
{
	char *end;
	if (!s || !*s)
		return -1;
	*v = strtol(s, &end, 10);
	return *end || *v < lo || *v > hi ? -1 : 0;
}

/* A pin argument that passes the rules and is not busy with the uart bridge. */
static int use_pin(int local, const char *arg, int output, int *pin)
{
	char err[96];
	long v;
	int tx, rx;
	if (arg_int(arg, 0, NPINS - 1, &v) != 0)
		return ERR("bad pin '%s'\n", arg ? arg : "");
	if (pin_check((int)v, output, err, sizeof err) != 0)
		return ERR("%s\n", err);
	if (sess_uart_pins(&tx, &rx) && (v == tx || v == rx))
		return ERR("GPIO%ld is used by the uart bridge\n", v);
	*pin = (int)v;
	return 0;
}

static void pulse_end(void *arg)
{
	int pin = (int)(intptr_t)arg;
	gpio_set_level(pin, P[pin].level);
	P[pin].pulsing = 0;
}

/* ---------------------------------------------------------------- gpio */

static int cmd_gpio(int local, int argc, char **argv)
{
	const char *sub = argc > 1 ? argv[1] : "";
	int pin, i;
	long v;
	if (!strcmp(sub, "list")) {
		int any = 0;
		for (i = 0; i < NPINS; i++) {
			if (!(g_allow & B(i)))
				continue;
			any = 1;
			OUT("GPIO%-2d  %-11s level %d%s%s%s\n", i, mode_name[P[i].mode], gpio_get_level(i),
			    P[i].watch ? "  watch" : "", (STRAP & B(i)) ? "  strapping" : "",
			    GPIO_IS_VALID_OUTPUT_GPIO(i) ? "" : "  input-only");
		}
		if (!any)
			OUT("no pins allowed (local console: config set gpio_allow 4,5)\n");
		return 0;
	}
	if (argc < 3)
		return ERR("usage: gpio list | mode <pin> in|out|in_pullup|in_pulldown|od | read <pin> | write <pin> 0|1 | "
		           "toggle <pin> | pulse <pin> <ms> | watch <pin> on|off\n");
	if (!strcmp(sub, "mode")) {
		int m;
		gpio_config_t gc = {0};
		for (m = M_IN; m <= M_OD; m++)
			if (argc > 3 && !strcmp(argv[3], mode_name[m]))
				break;
		if (m > M_OD)
			return ERR("mode must be in, out, in_pullup, in_pulldown or od\n");
		if (use_pin(local, argv[2], m >= M_OUT || m == M_IN_PU || m == M_IN_PD, &pin))
			return 1;
		if (P[pin].pulsing)
			return ERR("GPIO%d is pulsing\n", pin);
		if (P[pin].mode == M_PWM)
			pwm_stop(pin);
		gpio_set_level(pin, 0); /* outputs start low */
		gc.pin_bit_mask = B(pin);
		gc.mode = m == M_OUT ? GPIO_MODE_INPUT_OUTPUT : m == M_OD ? GPIO_MODE_INPUT_OUTPUT_OD : GPIO_MODE_INPUT;
		gc.pull_up_en = m == M_IN_PU;
		gc.pull_down_en = m == M_IN_PD;
		if (gpio_config(&gc) != ESP_OK)
			return ERR("GPIO%d: cannot set mode %s\n", pin, mode_name[m]);
		P[pin].mode = (uint8_t)m;
		P[pin].level = 0;
		OUT("GPIO%d mode %s\n", pin, mode_name[m]);
		return 0;
	}
	if (!strcmp(sub, "read")) {
		if (use_pin(local, argv[2], 0, &pin))
			return 1;
		OUT("GPIO%d = %d\n", pin, gpio_get_level(pin));
		return 0;
	}
	if (!strcmp(sub, "write") || !strcmp(sub, "toggle") || !strcmp(sub, "pulse")) {
		int lvl;
		if (use_pin(local, argv[2], 1, &pin))
			return 1;
		if (P[pin].mode != M_OUT && P[pin].mode != M_OD)
			return ERR("GPIO%d is not an output (gpio mode %d out)\n", pin, pin);
		if (P[pin].pulsing)
			return ERR("GPIO%d is pulsing\n", pin);
		if (!strcmp(sub, "write")) {
			if (argc < 4 || arg_int(argv[3], 0, 1, &v))
				return ERR("usage: gpio write <pin> 0|1\n");
			P[pin].level = (uint8_t)v;
			gpio_set_level(pin, P[pin].level);
			OUT("GPIO%d = %d\n", pin, P[pin].level);
			return 0;
		}
		if (!strcmp(sub, "toggle")) {
			P[pin].level ^= 1;
			gpio_set_level(pin, P[pin].level);
			OUT("GPIO%d = %d\n", pin, P[pin].level);
			return 0;
		}
		if (argc < 4 || arg_int(argv[3], 1, 10000, &v))
			return ERR("usage: gpio pulse <pin> <ms 1..10000>\n");
		if (!P[pin].pulse) {
			esp_timer_create_args_t ta = {.callback = pulse_end, .arg = (void *)(intptr_t)pin, .name = "pulse"};
			if (esp_timer_create(&ta, &P[pin].pulse) != ESP_OK)
				return ERR("no timer\n");
		}
		lvl = !P[pin].level;
		P[pin].pulsing = 1;
		gpio_set_level(pin, lvl);
		esp_timer_start_once(P[pin].pulse, (uint64_t)v * 1000); /* non-blocking: the timer restores the level */
		OUT("GPIO%d = %d for %ld ms, then %d\n", pin, lvl, v, P[pin].level);
		return 0;
	}
	if (!strcmp(sub, "watch")) {
		int on;
		if (argc < 4 || (strcmp(argv[3], "on") && strcmp(argv[3], "off")))
			return ERR("usage: gpio watch <pin> on|off\n");
		if (use_pin(local, argv[2], 0, &pin))
			return 1;
		if (P[pin].mode == M_PWM || P[pin].mode == M_ADC)
			return ERR("GPIO%d is in %s mode\n", pin, mode_name[P[pin].mode]);
		on = !strcmp(argv[3], "on");
		P[pin].stable = P[pin].cand = (uint8_t)gpio_get_level(pin);
		P[pin].watch = (uint8_t)on;
		OUT("GPIO%d watch %s (level %d; changes are printed to the Hub console session)\n", pin, on ? "on" : "off",
		    P[pin].stable);
		return 0;
	}
	return ERR("unknown gpio command '%s'\n", sub);
}

/* Debounced level changes (30 ms stable), from the worker task. Skips a turn while a command runs. */
void cmd_poll_watch(void)
{
	int i;
	int64_t now = esp_timer_get_time() / 1000;
	if (!g_lock || xSemaphoreTakeRecursive(g_lock, 0) != pdTRUE)
		return;
	for (i = 0; i < NPINS; i++) {
		pin_t *p = &P[i];
		uint8_t l;
		if (!p->watch)
			continue;
		l = (uint8_t)gpio_get_level(i);
		if (l == p->stable) {
			p->cand = l;
			continue;
		}
		if (l != p->cand) {
			p->cand = l;
			p->cand_ms = now;
		} else if (now - p->cand_ms >= 30) {
			char b[48];
			p->stable = l;
			snprintf(b, sizeof b, "[watch] GPIO%d = %d", i, l);
			console_notice(b);
		}
	}
	cmd_unlock();
}

/* ---------------------------------------------------------------- adc */

static adc_oneshot_unit_handle_t g_adc1;

static int cmd_adc(int local, int argc, char **argv)
{
	adc_unit_t unit;
	adc_channel_t ch;
	adc_oneshot_chan_cfg_t cc = {.atten = ADC_ATTEN_DB_12, .bitwidth = ADC_BITWIDTH_DEFAULT};
	adc_cali_handle_t cal = NULL;
	int pin, raw = 0, sum = 0, i, mv = 0, calibrated = 0;
	if (argc < 3 || strcmp(argv[1], "read"))
		return ERR("usage: adc read <pin>\n");
	if (use_pin(local, argv[2], 0, &pin))
		return 1;
	if (adc_oneshot_io_to_channel(pin, &unit, &ch) != ESP_OK)
		return ERR("GPIO%d has no ADC channel\n", pin);
	if (unit != ADC_UNIT_1)
		return ERR("GPIO%d is on ADC2, which is not usable while Wi-Fi runs\n", pin);
	if (P[pin].mode == M_PWM || P[pin].pulsing)
		return ERR("GPIO%d is busy (%s)\n", pin, mode_name[P[pin].mode]);
	if (!g_adc1) {
		adc_oneshot_unit_init_cfg_t uc = {.unit_id = ADC_UNIT_1};
		if (adc_oneshot_new_unit(&uc, &g_adc1) != ESP_OK)
			return ERR("ADC1 unavailable\n");
	}
	if (adc_oneshot_config_channel(g_adc1, ch, &cc) != ESP_OK)
		return ERR("cannot configure ADC1 channel %d\n", (int)ch);
	P[pin].mode = M_ADC;
	P[pin].watch = 0;
	for (i = 0; i < 8; i++) {
		if (adc_oneshot_read(g_adc1, ch, &raw) != ESP_OK)
			return ERR("ADC read failed\n");
		sum += raw;
	}
	raw = sum / 8;
#if ADC_CALI_SCHEME_CURVE_FITTING_SUPPORTED
	{
		adc_cali_curve_fitting_config_t k = {
		    .unit_id = ADC_UNIT_1, .chan = ch, .atten = ADC_ATTEN_DB_12, .bitwidth = ADC_BITWIDTH_DEFAULT};
		if (adc_cali_create_scheme_curve_fitting(&k, &cal) == ESP_OK) {
			calibrated = adc_cali_raw_to_voltage(cal, raw, &mv) == ESP_OK;
			adc_cali_delete_scheme_curve_fitting(cal);
		}
	}
#elif ADC_CALI_SCHEME_LINE_FITTING_SUPPORTED
	{
		adc_cali_line_fitting_config_t k = {
		    .unit_id = ADC_UNIT_1, .atten = ADC_ATTEN_DB_12, .bitwidth = ADC_BITWIDTH_DEFAULT};
		if (adc_cali_create_scheme_line_fitting(&k, &cal) == ESP_OK) {
			calibrated = adc_cali_raw_to_voltage(cal, raw, &mv) == ESP_OK;
			adc_cali_delete_scheme_line_fitting(cal);
		}
	}
#endif
	if (!calibrated)
		mv = raw * 3100 / ((1 << SOC_ADC_RTC_MAX_BITWIDTH) - 1);
	OUT("GPIO%d (ADC1 ch %d): %d mV%s, raw %d\n", pin, (int)ch, mv, calibrated ? "" : " (uncalibrated)", raw);
	return 0;
}

/* ---------------------------------------------------------------- pwm */

static int cmd_pwm(int local, int argc, char **argv)
{
	int pin, t, ch;
	long f, duty;
	if (argc >= 3 && !strcmp(argv[1], "off")) {
		if (use_pin(local, argv[2], 1, &pin))
			return 1;
		if (P[pin].mode != M_PWM)
			return ERR("GPIO%d has no PWM\n", pin);
		pin_release(pin);
		OUT("GPIO%d PWM off (input)\n", pin);
		return 0;
	}
	if (argc < 5 || strcmp(argv[1], "set") || arg_int(argv[3], 1, 100000, &f) || arg_int(argv[4], 0, 100, &duty))
		return ERR("usage: pwm set <pin> <freq_hz 1..100000> <duty_pct 0..100> | pwm off <pin>\n");
	if (use_pin(local, argv[2], 1, &pin))
		return 1;
	if (P[pin].pulsing)
		return ERR("GPIO%d is pulsing\n", pin);
	if (P[pin].mode == M_PWM)
		pwm_stop(pin);
	for (t = 0; t < LEDC_TIMER_MAX; t++) /* a timer with this frequency, else a free one */
		if (T[t].refs && T[t].freq == (uint32_t)f)
			break;
	if (t == LEDC_TIMER_MAX) {
		for (t = 0; t < LEDC_TIMER_MAX && T[t].refs; t++)
			;
		if (t == LEDC_TIMER_MAX) {
			pin_input(pin);
			return ERR("all %d PWM timers are in use with other frequencies\n", LEDC_TIMER_MAX);
		}
		{
			uint32_t bits = ledc_find_suitable_duty_resolution(80000000, (uint32_t)f);
			ledc_timer_config_t tc = {.speed_mode = LEDC_LOW_SPEED_MODE,
			                          .timer_num = (ledc_timer_t)t,
			                          .freq_hz = (uint32_t)f,
			                          .clk_cfg = LEDC_AUTO_CLK};
			if (bits > SOC_LEDC_TIMER_BIT_WIDTH)
				bits = SOC_LEDC_TIMER_BIT_WIDTH;
			if (bits < 1)
				bits = 1;
			tc.duty_resolution = (ledc_timer_bit_t)bits;
			if (ledc_timer_config(&tc) != ESP_OK) {
				pin_input(pin);
				return ERR("%ld Hz is not reachable on this chip\n", f);
			}
			T[t].freq = (uint32_t)f;
			T[t].bits = (uint8_t)bits;
		}
	}
	for (ch = 0; ch < SOC_LEDC_CHANNEL_NUM && ch_pin[ch] >= 0; ch++)
		;
	if (ch == SOC_LEDC_CHANNEL_NUM) {
		pin_input(pin);
		return ERR("all %d PWM channels are in use\n", SOC_LEDC_CHANNEL_NUM);
	}
	{
		ledc_channel_config_t cc = {.gpio_num = pin,
		                            .speed_mode = LEDC_LOW_SPEED_MODE,
		                            .channel = (ledc_channel_t)ch,
		                            .timer_sel = (ledc_timer_t)t,
		                            .duty = (uint32_t)(((1ULL << T[t].bits) * (uint64_t)duty) / 100),
		                            .hpoint = 0};
		if (ledc_channel_config(&cc) != ESP_OK) {
			pin_input(pin);
			return ERR("cannot start PWM on GPIO%d\n", pin);
		}
	}
	T[t].refs++;
	ch_pin[ch] = (int8_t)pin;
	P[pin].pwm_ch = (int8_t)ch;
	P[pin].pwm_t = (int8_t)t;
	P[pin].mode = M_PWM;
	P[pin].watch = 0;
	OUT("GPIO%d PWM %ld Hz, %ld%% (%u-bit)\n", pin, f, duty, (unsigned)T[t].bits);
	return 0;
}

/* ---------------------------------------------------------------- i2c */

static int cmd_i2c(int local, int argc, char **argv)
{
	i2c_master_bus_config_t bc = {.i2c_port = -1, .clk_source = I2C_CLK_SRC_DEFAULT, .glitch_ignore_cnt = 7};
	i2c_master_bus_handle_t bus;
	char a[8], b[8];
	int sda, scl, addr, found = 0;
	if (argc < 2 || strcmp(argv[1], "scan"))
		return ERR("usage: i2c scan (pins from config i2c_sda / i2c_scl)\n");
	snprintf(a, sizeof a, "%d", cfg_get_int("i2c_sda", -1));
	snprintf(b, sizeof b, "%d", cfg_get_int("i2c_scl", -1));
	if (!strcmp(a, "-1") || !strcmp(b, "-1"))
		return ERR("no I2C pins (local console: config set i2c_sda <pin>; config set i2c_scl <pin>)\n");
	if (use_pin(local, a, 1, &sda) || use_pin(local, b, 1, &scl))
		return 1;
	if (sda == scl || P[sda].mode == M_PWM || P[scl].mode == M_PWM || P[sda].pulsing || P[scl].pulsing)
		return ERR("I2C pins are busy or identical\n");
	bc.sda_io_num = sda;
	bc.scl_io_num = scl;
	bc.flags.enable_internal_pullup = 1;
	if (i2c_new_master_bus(&bc, &bus) != ESP_OK)
		return ERR("cannot open the I2C bus on SDA %d SCL %d\n", sda, scl);
	OUT("I2C scan on SDA %d SCL %d:", sda, scl);
	for (addr = 0x08; addr <= 0x77; addr++)
		if (i2c_master_probe(bus, (uint16_t)addr, 20) == ESP_OK) {
			OUT(" 0x%02x", addr);
			found++;
		}
	OUT(found ? "\n%d device(s)\n" : " none\n%d devices\n", found);
	i2c_del_master_bus(bus);
	pin_release(sda);
	pin_release(scl);
	return 0;
}

/* ---------------------------------------------------------------- status, time, config */

static void fmt_time(char *out, size_t cap)
{
	struct timeval tv;
	struct tm tm;
	gettimeofday(&tv, NULL);
	gmtime_r(&tv.tv_sec, &tm);
	strftime(out, cap, "%Y-%m-%dT%H:%M:%SZ", &tm);
}

static const char *reset_reason(void)
{
	switch (esp_reset_reason()) {
	case ESP_RST_POWERON: return "power-on";
	case ESP_RST_EXT: return "external pin";
	case ESP_RST_SW: return "software";
	case ESP_RST_PANIC: return "panic";
	case ESP_RST_INT_WDT: return "interrupt watchdog";
	case ESP_RST_TASK_WDT: return "task watchdog";
	case ESP_RST_WDT: return "watchdog";
	case ESP_RST_DEEPSLEEP: return "deep sleep";
	case ESP_RST_BROWNOUT: return "brownout";
	case ESP_RST_SDIO: return "SDIO";
	default: return "unknown";
	}
}

static void minion_state(int local)
{
	mt_status st;
	int have;
	char hub[128], name[64];
	uint8_t one;
	if (cfg_get_str("hub", hub, sizeof hub) != 0 || !hub[0]) {
		OUT("minion: no Hub URL (local console: config set hub https://<hub>)\n");
		return;
	}
	if (cfg_get_str("name", name, sizeof name) != 0 || !name[0])
		cfg_default_name(name, sizeof name);
	OUT("minion: Hub %s, Worker name \"%s\"\n", hub, name);
	sess_status(&st, &have);
	if (!have) {
		OUT("minion: starting\n");
		return;
	}
	if (st.enrolled) {
		OUT("minion: enrolled as %s, Hub fingerprint %s\n", st.worker_id[0] ? st.worker_id : "?", st.hub_fp);
		OUT("minion: %s%s, max serial %llu\n", st.connected ? "connected" : "disconnected",
		    st.pending ? ", PENDING approval in the Hub" : "", (unsigned long long)st.max_serial);
	} else if (store_get(NULL, MT_KEY_INVITE, &one, 1) > 0) {
		OUT("minion: not enrolled; invitation key stored, enrolling%s\n", st.connected ? " (connected)" : "");
	} else {
		OUT("minion: %snot enrolled (local console: enroll)\n", st.revoked ? "REVOKED by the Hub; " : "");
	}
	if (st.last_error[0])
		OUT("minion: last error: %s\n", st.last_error);
}

static int cmd_status(int local)
{
	esp_chip_info_t ci;
	int64_t up = esp_timer_get_time() / 1000000;
	char t[32];
	esp_chip_info(&ci);
	OUT("minion-thin %s on %s rev %d.%d, %d core(s), IDF %s\n", MT_VERSION, CONFIG_IDF_TARGET, ci.revision / 100,
	    ci.revision % 100, ci.cores, esp_get_idf_version());
	OUT("uptime %lldd %02lld:%02lld:%02lld, reset reason: %s\n", up / 86400, up / 3600 % 24, up / 60 % 60, up % 60,
	    reset_reason());
	OUT("heap (8-bit) free %u, min free %u, largest block %u\n", (unsigned)heap_caps_get_free_size(MALLOC_CAP_8BIT),
	    (unsigned)heap_caps_get_minimum_free_size(MALLOC_CAP_8BIT),
	    (unsigned)heap_caps_get_largest_free_block(MALLOC_CAP_8BIT));
	if (g_worker_task)
		OUT("worker task stack: %u bytes never used\n", (unsigned)uxTaskGetStackHighWaterMark(g_worker_task));
	net_status(local);
	fmt_time(t, sizeof t);
	OUT("time: %s, %s\n", t, net_time_state());
	minion_state(local);
	OUT("sessions: console, log%s\n", sess_uart_active() ? ", uart" : "");
	return 0;
}

static int cmd_time(int local)
{
	char t[32];
	int64_t floor_s = 0;
	fmt_time(t, sizeof t);
	OUT("time: %s (UTC), %s\n", t, net_time_state());
	if (cfg_get_i64("time_floor", &floor_s) == 0) {
		time_t f = (time_t)floor_s;
		struct tm tm;
		gmtime_r(&f, &tm);
		strftime(t, sizeof t, "%Y-%m-%dT%H:%M:%SZ", &tm);
		OUT("time floor: %s\n", t);
	}
	return 0;
}

static const char *const cfg_keys[] = {"hub",    "name",      "gpio_allow", "uart_tx", "uart_rx",
                                       "uart_baud", "i2c_sda", "i2c_scl",    "wifi_ssid"};

static int is_cfg_key(const char *k)
{
	size_t i;
	for (i = 0; i < sizeof cfg_keys / sizeof cfg_keys[0]; i++)
		if (!strcmp(k, cfg_keys[i]))
			return 1;
	return 0;
}

static int cmd_config(int local, int argc, char **argv)
{
	char v[128], err[96];
	size_t i;
	if (argc >= 2 && !strcmp(argv[1], "get")) {
		if (argc >= 3 && (!strcmp(argv[2], "wifi_pass") || !strcmp(argv[2], "invite")))
			return ERR("%s is a secret and is never shown\n", argv[2]);
		if (argc >= 3 && !is_cfg_key(argv[2]))
			return ERR("unknown key '%s'\n", argv[2]);
		for (i = 0; i < sizeof cfg_keys / sizeof cfg_keys[0]; i++) {
			if (argc >= 3 && strcmp(argv[2], cfg_keys[i]))
				continue;
			if (cfg_get_str(cfg_keys[i], v, sizeof v) != 0)
				snprintf(v, sizeof v, "(not set)");
			OUT("%s = %s\n", cfg_keys[i], v);
		}
		return 0;
	}
	if (argc < 2 || strcmp(argv[1], "set"))
		return ERR("usage: config get [key] | config set <key> <value|->\n");
	if (!local)
		return ERR("local console only\n");
	if (argc < 4 || !is_cfg_key(argv[2]) || !strcmp(argv[2], "wifi_ssid"))
		return ERR("usage: config set hub|name|gpio_allow|uart_tx|uart_rx|uart_baud|i2c_sda|i2c_scl <value>  "
		           "('-' unsets)\n");
	{
		const char *k = argv[2], *val = strcmp(argv[3], "-") ? argv[3] : "";
		long n;
		if (strlen(val) > 120)
			return ERR("value longer than 120 bytes\n");
		if (!strcmp(k, "hub") && *val) {
			mt_url u;
			if (mt_url_parse(&u, val) != 0)
				return ERR("bad Hub URL (https://host[:port][/prefix])\n");
		} else if (!strcmp(k, "name") && *val) {
			if (strlen(val) > 56)
				return ERR("name is longer than 56 bytes\n");
		} else if (!strcmp(k, "gpio_allow")) {
			uint64_t allow, ack;
			if (parse_allow(val, &allow, &ack, err, sizeof err) != 0)
				return ERR("%s\n", err);
			for (i = 0; i < NPINS; i++) /* removed pins go back to a safe input, new pins start as inputs */
				if (((g_allow & ~allow) | (allow & ~g_allow)) & B(i))
					pin_release((int)i);
			g_allow = allow;
			g_ack = ack;
		} else if (!strcmp(k, "uart_baud") && *val) {
			if (arg_int(val, 300, 5000000, &n))
				return ERR("baud must be 300..5000000\n");
		} else if (*val && arg_int(val, 0, NPINS - 1, &n)) {
			return ERR("%s must be a GPIO number\n", k);
		}
		if (cfg_set_str(k, val) != 0)
			return ERR("cannot write NVS\n");
		OUT("%s = %s%s\n", k, *val ? val : "(not set)",
		    !strcmp(k, "gpio_allow") || !strncmp(k, "i2c", 3) ? ""
		    : !strcmp(k, "hub") && !sess_worker()       ? " (the Worker starts now)"
		                                                : " (reboot to apply)");
	}
	return 0;
}

/* ---------------------------------------------------------------- wifi, uart, enroll, reset */

static int cmd_wifi(int local, int argc, char **argv)
{
	const char *sub = argc > 1 ? argv[1] : "status";
	if (!strcmp(sub, "status")) {
		net_status(local);
		return 0;
	}
	if (!strcmp(sub, "scan"))
		return net_scan(local);
	if (strcmp(sub, "set"))
		return ERR("usage: wifi status | wifi scan | wifi set <ssid> [password]\n");
	if (!local)
		return ERR("local console only\n");
	if (argc < 3 || strlen(argv[2]) > 32)
		return ERR("usage: wifi set <ssid> [password] (ssid <= 32 bytes)\n");
	{
		const char *pw = argc > 3 ? argv[3] : "";
		size_t n = strlen(pw);
		if (n && (n < 8 || n > 63))
			return ERR("the password must be 8..63 characters (or none for an open network)\n");
		if (cfg_set_str("wifi_ssid", argv[2]) != 0 || cfg_set_str("wifi_pass", pw) != 0)
			return ERR("cannot write NVS\n");
		OUT("wifi: stored \"%s\", password %s; reconnecting\n", argv[2], n ? "***" : "(none)");
		net_wifi_apply();
	}
	return 0;
}

static int cmd_uart(int local, int argc, char **argv)
{
	char err[96];
	long baud, tx, rx;
	int ctx, crx;
	if (argc < 2 || !strcmp(argv[1], "status")) {
		if (sess_uart_pins(&ctx, &crx))
			OUT("uart: bridge on UART1, tx %d rx %d, %d baud\n", ctx, crx, cfg_get_int("uart_baud", 115200));
		else
			OUT("uart: off (local console: uart set <baud> <tx> <rx>, then reboot)\n");
		return 0;
	}
	if (!local)
		return ERR("local console only\n");
	if (!strcmp(argv[1], "off")) {
		cfg_set_str("uart_tx", NULL);
		cfg_set_str("uart_rx", NULL);
		OUT("uart: bridge disabled (reboot to remove the session)\n");
		return 0;
	}
	if (strcmp(argv[1], "set") || argc < 3 || arg_int(argv[2], 300, 5000000, &baud) ||
	    (argc != 3 && argc != 5))
		return ERR("usage: uart set <baud> [<tx> <rx>] | uart off | uart status\n");
	if (argc == 5) {
		if (arg_int(argv[3], 0, NPINS - 1, &tx) || arg_int(argv[4], 0, NPINS - 1, &rx) || tx == rx)
			return ERR("bad pins\n");
		if (pin_check((int)tx, 1, err, sizeof err) || pin_check((int)rx, 0, err, sizeof err))
			return ERR("%s\n", err);
		if (P[tx].mode == M_PWM || P[rx].mode == M_PWM || P[tx].pulsing || P[rx].pulsing)
			return ERR("pin busy\n");
	} else {
		tx = cfg_get_int("uart_tx", -1);
		rx = cfg_get_int("uart_rx", -1);
		if (tx < 0 || rx < 0)
			return ERR("give the pins: uart set <baud> <tx> <rx>\n");
	}
	{
		char b[16];
		snprintf(b, sizeof b, "%ld", baud);
		cfg_set_str("uart_baud", b);
		snprintf(b, sizeof b, "%ld", tx);
		cfg_set_str("uart_tx", b);
		snprintf(b, sizeof b, "%ld", rx);
		cfg_set_str("uart_rx", b);
	}
	if (sess_uart_active()) {
		int otx, orx;
		sess_uart_pins(&otx, &orx);
		P[tx].watch = P[rx].watch = 0;
		if (sess_uart_apply((int)baud, (int)tx, (int)rx) != 0)
			return ERR("cannot reconfigure UART1\n");
		if (otx != tx && otx != rx) /* the old pins stop carrying UART signals */
			pin_input(otx);
		if (orx != tx && orx != rx)
			pin_input(orx);
		OUT("uart: tx %ld rx %ld %ld baud (applied)\n", tx, rx, baud);
	} else {
		OUT("uart: tx %ld rx %ld %ld baud stored; reboot to start the uart session\n", tx, rx, baud);
	}
	return 0;
}

static int valid_invite(const char *k)
{
	uint8_t b[40];
	const char *dot = strchr(k, '.');
	return !strncmp(k, "mnk_", 4) && dot && dot - k == 4 + 52 && strlen(dot + 1) == 52 &&
	       mt_b32_decode(b, sizeof b, k + 4, 52) == 32 && mt_b32_decode(b, sizeof b, dot + 1, 52) == 32;
}

static int cmd_enroll(int local, int argc, char **argv)
{
	mt_status st;
	int have;
	if (!local)
		return ERR("local console only\n");
	if (argc < 2)
		return ERR("usage: enroll (then paste the mnk_ key; it is not shown)\n");
	sess_status(&st, &have);
	if (have && st.enrolled)
		return ERR("already enrolled as %s; run factory-reset first to enroll again\n", st.worker_id);
	if (!valid_invite(argv[1]))
		return ERR("not an invitation key (expected mnk_<52>.<52>)\n");
	if (store_set(NULL, MT_KEY_INVITE, (const uint8_t *)argv[1], strlen(argv[1])) != 0)
		return ERR("cannot write NVS\n");
	OUT("invitation key stored (mnk_***). The Worker enrolls once Wi-Fi, the clock and the Hub URL are ready;\n"
	    "then approve it in the Hub. The key is erased after a successful enrollment.\n");
	{
		char hub[128];
		if (cfg_get_str("hub", hub, sizeof hub) != 0 || !hub[0])
			OUT("No Hub URL yet: config set hub https://<hub>, then reboot.\n");
	}
	return 0;
}

static int cmd_factory_reset(int local, int argc, char **argv)
{
	int all = argc > 2 && !strcmp(argv[2], "all");
	if (!local)
		return ERR("local console only\n");
	if (argc < 2 || strcmp(argv[1], "ERASE")) {
		OUT("factory-reset ERASE       erases the Minion identity, enrollment, invitation key and share rule\n"
		    "factory-reset ERASE all   also erases the config (Wi-Fi, Hub URL, pins)\n"
		    "The Hub then sees a new Worker after a new enrollment. Type the word ERASE to confirm.\n");
		return 1;
	}
	/* stop the Worker first, so it cannot write its state back after the erase */
	if (sess_worker())
		mt_worker_stop(sess_worker());
	vTaskDelay(pdMS_TO_TICKS(500));
	if (store_erase_all() != 0 || (all && cfg_erase_all() != 0)) {
		schedule_restart(500);
		return ERR("cannot erase NVS (rebooting anyway)\n");
	}
	OUT("erased%s; rebooting\n", all ? " (including config)" : "");
	fflush(stdout);
	esp_restart();
	return 0;
}

static int cmd_help(int local)
{
	OUT("help                      this list\n"
	    "status                    chip, uptime, heap, Wi-Fi, time, Minion state\n"
	    "gpio list | mode <pin> in|out|in_pullup|in_pulldown|od | read <pin> | write <pin> 0|1\n"
	    "     toggle <pin> | pulse <pin> <ms> | watch <pin> on|off\n"
	    "adc read <pin>            millivolts (ADC1 pins)\n"
	    "pwm set <pin> <hz> <duty%%> | pwm off <pin>\n"
	    "i2c scan                  on the configured i2c_sda/i2c_scl pins\n"
	    "wifi status | wifi scan   time   uart status   config get [key]   reboot\n");
	OUT("%s", local ? "local console only:\n"
	                  "enroll [mnk_...]          store the invitation key (prompts without echo when omitted)\n"
	                  "wifi set <ssid> [password] (prompts without echo when omitted; '-' for an open network)\n"
	                  "config set <key> <value>  hub name gpio_allow uart_tx uart_rx uart_baud i2c_sda i2c_scl\n"
	                  "uart set <baud> [tx rx] | uart off\n"
	                  "factory-reset ERASE [all]\n"
	                : "(enroll, config set, wifi set, uart set and factory-reset need the local serial console)\n");
	OUT("Pins must be in gpio_allow; flash/PSRAM/console/USB pins are always refused.\n");
	return 0;
}

int cmd_run(int local, int argc, char **argv)
{
	const char *c = argv[0];
	int r;
	cmd_lock();
	if (!strcmp(c, "help"))
		r = cmd_help(local);
	else if (!strcmp(c, "status"))
		r = cmd_status(local);
	else if (!strcmp(c, "gpio"))
		r = cmd_gpio(local, argc, argv);
	else if (!strcmp(c, "adc"))
		r = cmd_adc(local, argc, argv);
	else if (!strcmp(c, "pwm"))
		r = cmd_pwm(local, argc, argv);
	else if (!strcmp(c, "i2c"))
		r = cmd_i2c(local, argc, argv);
	else if (!strcmp(c, "wifi"))
		r = cmd_wifi(local, argc, argv);
	else if (!strcmp(c, "time"))
		r = cmd_time(local);
	else if (!strcmp(c, "config"))
		r = cmd_config(local, argc, argv);
	else if (!strcmp(c, "uart"))
		r = cmd_uart(local, argc, argv);
	else if (!strcmp(c, "enroll"))
		r = cmd_enroll(local, argc, argv);
	else if (!strcmp(c, "factory-reset"))
		r = cmd_factory_reset(local, argc, argv);
	else if (!strcmp(c, "reboot")) {
		OUT("rebooting in 1 s\n");
		schedule_restart(1000);
		r = 0;
	} else
		r = ERR("unknown command '%s' (help)\n", c);
	cmd_unlock();
	return r;
}

/* While a line is typed into the Hub console: is the next character part of a secret? (enroll <key>,
 * wifi set <ssid> <password>, any token that starts with mnk_). Those are refused from the Hub anyway; the echo
 * must still not put them on a screen other people can see. */
int cmd_secret_pos(const char *line, size_t len)
{
	char t0[8] = "", t1[8] = "";
	size_t i = 0, tok = 0, start = 0;
	int in = 0;
	for (i = 0; i < len; i++) {
		if (line[i] == ' ' || line[i] == '\t') {
			if (in) {
				size_t n = i - start;
				if (tok == 0 && n < sizeof t0)
					memcpy(t0, line + start, n), t0[n] = 0;
				if (tok == 1 && n < sizeof t1)
					memcpy(t1, line + start, n), t1[n] = 0;
				tok++;
			}
			in = 0;
		} else if (!in) {
			in = 1;
			start = i;
		}
	}
	if (in && len - start >= 4 && !strncmp(line + start, "mnk_", 4))
		return 1;
	if (!in && tok == 0)
		return 0;
	{
		size_t cur = tok; /* index of the token the next char belongs to (or starts) */
		if (!strcmp(t0, "enroll") && cur >= 1)
			return 1;
		if (!strcmp(t0, "wifi") && !strcmp(t1, "set") && cur >= 3)
			return 1;
	}
	return 0;
}

/* ---------------------------------------------------------------- local serial console (esp_console REPL) */

/* Read a secret from the local console without echo (one '*' per character). */
static int read_secret(const char *prompt, char *out, size_t cap)
{
	size_t n = 0;
	printf("%s", prompt);
	fflush(stdout);
	for (;;) {
		int c = fgetc(stdin);
		if (c == EOF) {
			vTaskDelay(pdMS_TO_TICKS(10));
			continue;
		}
		if (c == '\r' || c == '\n')
			break;
		if (c == 3) {
			printf("^C\n");
			memset(out, 0, cap);
			return -1;
		}
		if ((c == 8 || c == 0x7f) && n) {
			n--;
			printf("\b \b");
		} else if (c >= 0x20 && c < 0x7f && n + 1 < cap) {
			out[n++] = (char)c;
			putchar('*');
		}
		fflush(stdout);
	}
	out[n] = 0;
	printf("\n");
	return (int)n;
}

static int local_cmd(int argc, char **argv)
{
	char secret[128], *av[8];
	int i, r, has_secret = 0;
	if (argc > 8)
		argc = 8;
	for (i = 0; i < argc; i++)
		av[i] = argv[i];
	/* the secret is read BEFORE the command lock is taken: typing must not stall the worker */
	if (!strcmp(argv[0], "enroll")) {
		has_secret = 1;
		if (argc == 1) {
			if (read_secret("invitation key (not shown): ", secret, sizeof secret) <= 0)
				return 0;
			av[1] = secret;
			argc = 2;
		}
	} else if (!strcmp(argv[0], "wifi") && argc >= 3 && !strcmp(argv[1], "set")) {
		has_secret = 1;
		if (argc == 3) {
			if (read_secret("password (not shown, empty for an open network): ", secret, sizeof secret) < 0)
				return 0;
			av[3] = secret;
			argc = 4;
		} else if (!strcmp(argv[3], "-")) {
			av[3] = "";
		}
	}
	r = cmd_run(1, argc, av);
	if (has_secret) {
		/* The REPL already put the line into the history: keep only a blank entry, then restore the length.
		 * (linenoiseHistoryFree() leaves a stale count behind and crashes the next add.) */
		linenoiseHistoryAdd(" ");
		linenoiseHistorySetMaxLen(1);
		linenoiseHistorySetMaxLen(16);
		for (i = 1; i < argc; i++)
			memset(av[i], 0, strlen(av[i]));
	}
	memset(secret, 0, sizeof secret);
	(void)r; /* errors were printed as "ERR ..."; a non-zero code would only add esp_console's own line */
	return 0;
}

void cmd_register_local(void)
{
	static const char *const verbs[][2] = {
	    {"help", "list the commands"},
	    {"status", "device and Minion status"},
	    {"gpio", "list | mode | read | write | toggle | pulse | watch"},
	    {"adc", "read <pin>"},
	    {"pwm", "set <pin> <hz> <duty%> | off <pin>"},
	    {"i2c", "scan"},
	    {"wifi", "status | scan | set <ssid> [password]"},
	    {"time", "wall time and SNTP state"},
	    {"config", "get [key] | set <key> <value>"},
	    {"uart", "status | set <baud> [tx rx] | off"},
	    {"enroll", "store the invitation key (enroll, then paste it)"},
	    {"factory-reset", "erase the Minion identity (factory-reset ERASE [all])"},
	    {"reboot", "restart the device"},
	};
	size_t i;
	for (i = 0; i < sizeof verbs / sizeof verbs[0]; i++) {
		esp_console_cmd_t c = {.command = verbs[i][0], .help = verbs[i][1], .func = local_cmd};
		esp_console_cmd_register(&c);
	}
}

void banner(void)
{
	char hub[128], name[64];
	uint8_t one;
	int enrolled = store_get(NULL, MT_KEY_STATE, &one, 1) > 0, invite = store_get(NULL, MT_KEY_INVITE, &one, 1) > 0;
	if (cfg_get_str("name", name, sizeof name) != 0 || !name[0])
		cfg_default_name(name, sizeof name);
	if (cfg_get_str("hub", hub, sizeof hub) != 0)
		hub[0] = 0;
	printf("\n=== Minion thin Worker %s (%s, IDF %s) ===\n", MT_VERSION, CONFIG_IDF_TARGET, esp_get_idf_version());
	printf("Worker \"%s\", Hub %s\n", name, hub[0] ? hub : "(not set)");
	printf("State: %s\n", enrolled ? "enrolled (identity stored); 'status' shows the connection"
	                      : invite ? "invitation key stored; enrolling when online"
	                               : "NOT ENROLLED");
	if (!enrolled)
		printf("To enroll: wifi set <ssid>   (password prompt)\n"
		       "           config set hub https://<your-hub>\n"
		       "           enroll        (paste the mnk_ invitation key from the Hub; not shown)\n"
		       "           then approve the Worker in the Hub.\n");
	printf("Type 'help' for the commands.\n\n");
}
