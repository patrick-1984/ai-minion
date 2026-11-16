/* mt_platform hooks for ESP-IDF: lwIP BSD sockets, mbedTLS without certificate verification (trust comes from the
 * pinned Hub root inside the Worker protocol; SNI = host), esp_timer, esp_fill_random, NVS, ESP_LOG. */
#include <errno.h>
#include <fcntl.h>
#include <stdlib.h>
#include <string.h>
#include <sys/time.h>

#include "esp_log.h"
#include "esp_random.h"
#include "esp_timer.h"
#include "freertos/FreeRTOS.h"
#include "freertos/task.h"
#include "lwip/netdb.h"
#include "lwip/sockets.h"
#include "mbedtls/ctr_drbg.h"
#include "mbedtls/entropy.h"
#include "mbedtls/net_sockets.h"
#include "mbedtls/ssl.h"
#if CONFIG_MBEDTLS_SSL_PROTO_TLS1_3
#include "psa/crypto.h"
#endif

#include "app.h"

static const char *TAG = "minion";

static struct {
	int fd, tls, tls_ready;
	mbedtls_ssl_context ssl;
	mbedtls_ssl_config conf;
	mbedtls_ctr_drbg_context drbg;
	mbedtls_entropy_context entropy;
} N = {.fd = -1};

/* ---------------------------------------------------------------- time, random, log, sleep */

static int64_t now_ms(void *c)
{
	struct timeval tv;
	gettimeofday(&tv, NULL);
	if (tv.tv_sec < TIME_VALID_S)
		return 0; /* not set yet: the core waits */
	return (int64_t)tv.tv_sec * 1000 + tv.tv_usec / 1000;
}

static int64_t mono_ms(void *c) { return esp_timer_get_time() / 1000; }

static void rnd(void *c, uint8_t *b, size_t n)
{
	/* Only a true RNG with the RF on (Wi-Fi started). The worker is created after net_start(), so this never
	 * fires; it guards against a future change that would make a weak, permanent identity key. */
	if (!net_rf_on())
		abort();
	esp_fill_random(b, n);
}

static void lg(void *c, int level, const char *msg)
{
	switch (level) {
	case MT_LOG_ERROR: ESP_LOGE(TAG, "%s", msg); break;
	case MT_LOG_WARN: ESP_LOGW(TAG, "%s", msg); break;
	case MT_LOG_INFO: ESP_LOGI(TAG, "%s", msg); break;
	default: ESP_LOGD(TAG, "%s", msg); break;
	}
}

static void poll_(void *c) { sess_poll(); }

/* Sleeps in short slices that keep the sessions alive; returns early when the network or the clock became
 * usable, so the first connect does not wait out a long backoff. */
static void sleep_ms(void *c, int ms)
{
	uint32_t gen = net_generation();
	int64_t end = mono_ms(NULL) + ms;
	while (mono_ms(NULL) < end) {
		sess_poll();
		if (net_generation() != gen)
			return;
		vTaskDelay(pdMS_TO_TICKS(20));
	}
}

/* ---------------------------------------------------------------- network */

static int tcp_connect(const char *host, uint16_t port, int timeout_ms)
{
	struct addrinfo hints, *res = NULL, *ai;
	char ps[8];
	int fd = -1;
	memset(&hints, 0, sizeof hints);
	hints.ai_socktype = SOCK_STREAM;
	hints.ai_family = AF_UNSPEC;
	snprintf(ps, sizeof ps, "%u", (unsigned)port);
	if (getaddrinfo(host, ps, &hints, &res) != 0 || !res)
		return -1;
	for (ai = res; ai; ai = ai->ai_next) {
		fd_set ws;
		struct timeval tv = {timeout_ms / 1000, (timeout_ms % 1000) * 1000};
		int err = 0, one = 1;
		socklen_t el = sizeof err;
		fd = socket(ai->ai_family, ai->ai_socktype, ai->ai_protocol);
		if (fd < 0)
			continue;
		fcntl(fd, F_SETFL, fcntl(fd, F_GETFL, 0) | O_NONBLOCK);
		if (connect(fd, ai->ai_addr, ai->ai_addrlen) == 0)
			goto ok;
		if (errno == EINPROGRESS) {
			FD_ZERO(&ws);
			FD_SET(fd, &ws);
			if (select(fd + 1, NULL, &ws, NULL, &tv) == 1 && getsockopt(fd, SOL_SOCKET, SO_ERROR, &err, &el) == 0 &&
			    !err)
				goto ok;
		}
		close(fd);
		fd = -1;
		continue;
	ok:
		/* the socket stays non-blocking: every read and write below has its own deadline, so a peer that
		 * trickles bytes cannot hold the worker task (UART, GPIO watch and keepalives keep running) */
		setsockopt(fd, IPPROTO_TCP, TCP_NODELAY, &one, sizeof one);
		break;
	}
	freeaddrinfo(res);
	return fd;
}

static int bio_send(void *ctx, const unsigned char *b, size_t n)
{
	int r = send(*(int *)ctx, b, n, 0);
	if (r < 0)
		return errno == EINTR || errno == EAGAIN || errno == EWOULDBLOCK ? MBEDTLS_ERR_SSL_WANT_WRITE
		                                                                  : MBEDTLS_ERR_NET_SEND_FAILED;
	return r;
}

/* Wait until the socket is readable or writable, at most until the deadline (and at most 1 s per call). */
static int wait_io(int want_read, int64_t deadline)
{
	fd_set fs;
	int64_t left = deadline - mono_ms(NULL);
	struct timeval tv;
	if (left <= 0 || N.fd < 0)
		return -1;
	if (left > 1000)
		left = 1000;
	tv.tv_sec = 0;
	tv.tv_usec = (long)left * 1000;
	FD_ZERO(&fs);
	FD_SET(N.fd, &fs);
	return select(N.fd + 1, want_read ? &fs : NULL, want_read ? NULL : &fs, NULL, &tv) < 0 && errno != EINTR ? -1 : 0;
}

static int bio_recv(void *ctx, unsigned char *b, size_t n)
{
	int r = recv(*(int *)ctx, b, n, 0);
	if (r < 0)
		return errno == EINTR || errno == EAGAIN || errno == EWOULDBLOCK ? MBEDTLS_ERR_SSL_WANT_READ
		                                                                  : MBEDTLS_ERR_NET_RECV_FAILED;
	return r == 0 ? MBEDTLS_ERR_NET_CONN_RESET : r;
}

static void net_close(void *c)
{
	if (N.tls_ready) {
		if (N.fd >= 0)
			mbedtls_ssl_close_notify(&N.ssl);
		mbedtls_ssl_free(&N.ssl);
		mbedtls_ssl_config_free(&N.conf);
		mbedtls_ctr_drbg_free(&N.drbg);
		mbedtls_entropy_free(&N.entropy);
		N.tls_ready = 0;
	}
	if (N.fd >= 0)
		close(N.fd);
	N.fd = -1;
}

static int net_connect(void *c, const char *host, uint16_t port, int tls, int timeout_ms)
{
	int r;
	net_close(NULL);
	N.fd = tcp_connect(host, port, timeout_ms);
	N.tls = tls;
	if (N.fd < 0)
		return -1;
	if (!tls)
		return 0;
#if CONFIG_MBEDTLS_SSL_PROTO_TLS1_3
	if (psa_crypto_init() != PSA_SUCCESS) { /* TLS 1.3 in mbedTLS 3.6 runs on PSA */
		net_close(NULL);
		return -1;
	}
#endif
	mbedtls_ssl_init(&N.ssl);
	mbedtls_ssl_config_init(&N.conf);
	mbedtls_ctr_drbg_init(&N.drbg);
	mbedtls_entropy_init(&N.entropy);
	N.tls_ready = 1;
	if (mbedtls_ctr_drbg_seed(&N.drbg, mbedtls_entropy_func, &N.entropy, (const unsigned char *)"minion-thin", 11) ||
	    mbedtls_ssl_config_defaults(&N.conf, MBEDTLS_SSL_IS_CLIENT, MBEDTLS_SSL_TRANSPORT_STREAM,
	                                MBEDTLS_SSL_PRESET_DEFAULT))
		goto fail;
	/* Trust comes from the pinned Hub root inside the Worker protocol, not from TLS (see the design). */
	mbedtls_ssl_conf_authmode(&N.conf, MBEDTLS_SSL_VERIFY_NONE);
	mbedtls_ssl_conf_rng(&N.conf, mbedtls_ctr_drbg_random, &N.drbg);
	if (mbedtls_ssl_setup(&N.ssl, &N.conf) || mbedtls_ssl_set_hostname(&N.ssl, host))
		goto fail;
	mbedtls_ssl_set_bio(&N.ssl, &N.fd, bio_send, bio_recv, NULL);
	{
		int64_t end = mono_ms(NULL) + timeout_ms;
		while ((r = mbedtls_ssl_handshake(&N.ssl)) != 0) {
			if ((r != MBEDTLS_ERR_SSL_WANT_READ && r != MBEDTLS_ERR_SSL_WANT_WRITE) ||
			    wait_io(r == MBEDTLS_ERR_SSL_WANT_READ, end) != 0) {
				ESP_LOGW(TAG, "TLS handshake with %s failed (-0x%04x)", host, (unsigned)-r);
				goto fail;
			}
		}
	}
	return 0;
fail:
	net_close(NULL);
	return -1;
}

static int net_read(void *c, uint8_t *b, size_t cap, int timeout_ms)
{
	int r;
	if (N.fd < 0)
		return -1;
	if (!N.tls || mbedtls_ssl_get_bytes_avail(&N.ssl) == 0) {
		fd_set rs;
		struct timeval tv = {timeout_ms / 1000, (timeout_ms % 1000) * 1000};
		FD_ZERO(&rs);
		FD_SET(N.fd, &rs);
		r = select(N.fd + 1, &rs, NULL, NULL, &tv);
		if (r == 0)
			return 0;
		if (r < 0)
			return errno == EINTR ? 0 : -1;
	}
	if (N.tls) {
		r = mbedtls_ssl_read(&N.ssl, b, cap);
		if (r == MBEDTLS_ERR_SSL_WANT_READ || r == MBEDTLS_ERR_SSL_WANT_WRITE ||
		    r == MBEDTLS_ERR_SSL_RECEIVED_NEW_SESSION_TICKET) /* TLS 1.3 tickets: not data */
			return 0;
		return r > 0 ? r : -1;
	}
	r = recv(N.fd, b, cap, 0);
	if (r < 0 && (errno == EAGAIN || errno == EWOULDBLOCK))
		return 0;
	return r > 0 ? r : -1;
}

static int net_write(void *c, const uint8_t *b, size_t n)
{
	int64_t deadline = mono_ms(NULL) + 30000; /* a peer that stops reading drops the connection after 30 s */
	while (n) {
		int r;
		if (N.fd < 0)
			return -1;
		if (N.tls) {
			r = mbedtls_ssl_write(&N.ssl, b, n);
			if (r == MBEDTLS_ERR_SSL_WANT_READ || r == MBEDTLS_ERR_SSL_WANT_WRITE ||
			    r == MBEDTLS_ERR_SSL_RECEIVED_NEW_SESSION_TICKET) {
				if (wait_io(r == MBEDTLS_ERR_SSL_WANT_READ, deadline) != 0)
					return -1;
				continue;
			}
		} else {
			r = send(N.fd, b, n, 0);
			if (r < 0 && (errno == EINTR || errno == EAGAIN || errno == EWOULDBLOCK)) {
				if (wait_io(0, deadline) != 0)
					return -1;
				continue;
			}
		}
		if (r <= 0)
			return -1;
		b += r;
		n -= (size_t)r;
	}
	return 0;
}

void platform_init(mt_platform *p)
{
	memset(p, 0, sizeof *p);
	p->net_connect = net_connect;
	p->net_read = net_read;
	p->net_write = net_write;
	p->net_close = net_close;
	p->now_ms = now_ms;
	p->mono_ms = mono_ms;
	p->random = rnd;
	p->store_get = store_get;
	p->store_set = store_set;
	p->log = lg;
	p->poll = poll_;
	p->sleep_ms = sleep_ms;
}
