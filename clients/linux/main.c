/* minion-thin for Linux and OpenWrt: PTY sessions, a file store, plain sockets or mbedTLS.
 *
 *   minion-thin --hub URL --state-dir DIR [--name NAME] [--invite-file FILE]
 *               [--session NAME=COMMAND]... [--cols 80 --rows 24 --scrollback 16384] [-v]
 *
 * The invitation key file is read once, stored in the state directory and deleted by the core after a
 * successful enrollment (the file given here is deleted right after it was copied). */
#ifndef _GNU_SOURCE
#define _GNU_SOURCE
#endif
#include <errno.h>
#include <fcntl.h>
#include <netdb.h>
#include <poll.h>
#include <signal.h>
#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/random.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <termios.h>
#include <time.h>
#include <unistd.h>
#if defined(__GLIBC__)
#include <pty.h>
#else
#include <pty.h> /* musl provides forkpty in libc */
#endif

#include "mt_worker.h"

#ifdef MT_WITH_MBEDTLS
#include <mbedtls/ctr_drbg.h>
#include <mbedtls/entropy.h>
#include <mbedtls/net_sockets.h>
#include <mbedtls/ssl.h>
#endif

#define MAX_SESSIONS 8

typedef struct {
	mt_session s;
	mt_vt vt;
	void *mem;
	char cmd[256];
	int fd;
	pid_t pid;
	int64_t respawn_at;
	uint16_t cols, rows;
} pty_session;

static struct {
	const char *state_dir;
	int verbose;
	int fd;
	int tls;
#ifdef MT_WITH_MBEDTLS
	mbedtls_ssl_context ssl;
	mbedtls_ssl_config conf;
	mbedtls_ctr_drbg_context drbg;
	mbedtls_entropy_context entropy;
	int tls_ready;
#endif
	pty_session ps[MAX_SESSIONS];
	int nps;
} G = {.fd = -1};

static volatile sig_atomic_t g_stop;
static mt_worker *g_w;

/* ---------------------------------------------------------------- time, random, log */

static int64_t now_ms(void *c)
{
	struct timespec ts;
	(void)c;
	clock_gettime(CLOCK_REALTIME, &ts);
	return (int64_t)ts.tv_sec * 1000 + ts.tv_nsec / 1000000;
}

static int64_t mono_ms(void *c)
{
	struct timespec ts;
	(void)c;
	clock_gettime(CLOCK_MONOTONIC, &ts);
	return (int64_t)ts.tv_sec * 1000 + ts.tv_nsec / 1000000;
}

static void rnd(void *c, uint8_t *b, size_t n)
{
	(void)c;
	while (n) {
		ssize_t r = getrandom(b, n, 0);
		if (r < 0) {
			if (errno == EINTR)
				continue;
			abort(); /* no randomness: never continue with weak keys */
		}
		b += r;
		n -= (size_t)r;
	}
}

static void lg(void *c, int level, const char *msg)
{
	static const char *names[] = {"ERROR", "WARN", "INFO", "DEBUG"};
	(void)c;
	if (level > (G.verbose ? MT_LOG_DEBUG : MT_LOG_INFO))
		return;
	fprintf(stderr, "minion-thin %s: %s\n", names[level & 3], msg);
}

static void sleep_ms(void *c, int ms)
{
	struct timespec ts = {ms / 1000, (long)(ms % 1000) * 1000000L};
	(void)c;
	while (nanosleep(&ts, &ts) != 0 && errno == EINTR && !g_stop)
		;
}

/* ---------------------------------------------------------------- store: one file per key, atomic replace */

static int store_path(char *out, size_t cap, const char *key)
{
	int n = snprintf(out, cap, "%s/%s", G.state_dir, key);
	return n > 0 && (size_t)n < cap ? 0 : -1;
}

static int store_get(void *c, const char *key, uint8_t *buf, size_t cap)
{
	char p[512];
	int fd, n;
	(void)c;
	if (store_path(p, sizeof p, key))
		return -1;
	fd = open(p, O_RDONLY | O_CLOEXEC);
	if (fd < 0)
		return -1;
	n = (int)read(fd, buf, cap);
	close(fd);
	return n;
}

static int store_set(void *c, const char *key, const uint8_t *buf, size_t n)
{
	char p[512], tmp[520];
	int fd;
	(void)c;
	if (store_path(p, sizeof p, key))
		return -1;
	if (n == 0) { /* a deletion (invitation key after enrollment, state after a revoke) must be durable too */
		if (unlink(p) != 0 && errno != ENOENT)
			return -1;
		fd = open(G.state_dir, O_RDONLY | O_DIRECTORY | O_CLOEXEC);
		if (fd >= 0) {
			fsync(fd);
			close(fd);
		}
		return 0;
	}
	snprintf(tmp, sizeof tmp, "%s.tmp", p);
	fd = open(tmp, O_WRONLY | O_CREAT | O_TRUNC | O_CLOEXEC, 0600);
	if (fd < 0)
		return -1;
	if (write(fd, buf, n) != (ssize_t)n || fsync(fd) != 0) {
		close(fd);
		unlink(tmp);
		return -1;
	}
	close(fd);
	if (rename(tmp, p) != 0)
		return -1;
	fd = open(G.state_dir, O_RDONLY | O_DIRECTORY | O_CLOEXEC); /* make the rename durable */
	if (fd >= 0) {
		fsync(fd);
		close(fd);
	}
	return 0;
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
	if (getaddrinfo(host, ps, &hints, &res) != 0)
		return -1;
	for (ai = res; ai; ai = ai->ai_next) {
		struct pollfd pf;
		int err = 0;
		socklen_t el = sizeof err;
		fd = socket(ai->ai_family, ai->ai_socktype | SOCK_CLOEXEC, ai->ai_protocol);
		if (fd < 0)
			continue;
		fcntl(fd, F_SETFL, fcntl(fd, F_GETFL) | O_NONBLOCK);
		if (connect(fd, ai->ai_addr, ai->ai_addrlen) == 0)
			break;
		if (errno == EINPROGRESS) {
			pf.fd = fd;
			pf.events = POLLOUT;
			if (poll(&pf, 1, timeout_ms) == 1 && getsockopt(fd, SOL_SOCKET, SO_ERROR, &err, &el) == 0 && !err)
				break;
		}
		close(fd);
		fd = -1;
	}
	freeaddrinfo(res);
	if (fd >= 0)
		fcntl(fd, F_SETFL, fcntl(fd, F_GETFL) & ~O_NONBLOCK);
	return fd;
}

#ifdef MT_WITH_MBEDTLS
static int bio_send(void *ctx, const unsigned char *b, size_t n)
{
	ssize_t r = send(*(int *)ctx, b, n, MSG_NOSIGNAL);
	if (r < 0)
		return errno == EINTR ? MBEDTLS_ERR_SSL_WANT_WRITE : MBEDTLS_ERR_NET_SEND_FAILED;
	return (int)r;
}

static int bio_recv(void *ctx, unsigned char *b, size_t n)
{
	ssize_t r = recv(*(int *)ctx, b, n, 0);
	if (r < 0)
		return errno == EINTR ? MBEDTLS_ERR_SSL_WANT_READ : MBEDTLS_ERR_NET_RECV_FAILED; /* EAGAIN: timed out */
	return (int)r;
}
#endif

static void net_close(void *c)
{
	(void)c;
#ifdef MT_WITH_MBEDTLS
	if (G.tls && G.tls_ready) {
		mbedtls_ssl_close_notify(&G.ssl);
		mbedtls_ssl_free(&G.ssl);
		mbedtls_ssl_config_free(&G.conf);
		mbedtls_ctr_drbg_free(&G.drbg);
		mbedtls_entropy_free(&G.entropy);
		G.tls_ready = 0;
	}
#endif
	if (G.fd >= 0)
		close(G.fd);
	G.fd = -1;
}

static int net_connect(void *c, const char *host, uint16_t port, int tls, int timeout_ms)
{
	(void)c;
	G.fd = tcp_connect(host, port, timeout_ms);
	G.tls = tls;
	if (G.fd < 0)
		return -1;
	if (!tls)
		return 0;
#ifdef MT_WITH_MBEDTLS
	{
		int r;
		mbedtls_ssl_init(&G.ssl);
		mbedtls_ssl_config_init(&G.conf);
		mbedtls_ctr_drbg_init(&G.drbg);
		mbedtls_entropy_init(&G.entropy);
		G.tls_ready = 1;
		if (mbedtls_ctr_drbg_seed(&G.drbg, mbedtls_entropy_func, &G.entropy, (const unsigned char *)"minion-thin",
		                          11) ||
		    mbedtls_ssl_config_defaults(&G.conf, MBEDTLS_SSL_IS_CLIENT, MBEDTLS_SSL_TRANSPORT_STREAM,
		                                MBEDTLS_SSL_PRESET_DEFAULT))
			goto fail;
		/* Trust comes from the pinned Hub root inside the Worker protocol, not from TLS (see the design). */
		mbedtls_ssl_conf_authmode(&G.conf, MBEDTLS_SSL_VERIFY_NONE);
		mbedtls_ssl_conf_rng(&G.conf, mbedtls_ctr_drbg_random, &G.drbg);
		if (mbedtls_ssl_setup(&G.ssl, &G.conf) || mbedtls_ssl_set_hostname(&G.ssl, host))
			goto fail;
		mbedtls_ssl_set_bio(&G.ssl, &G.fd, bio_send, bio_recv, NULL);
		{ /* the TLS handshake shares the connect deadline: a peer that stalls it must not freeze the Worker */
			struct timeval tv = {timeout_ms / 1000, (timeout_ms % 1000) * 1000}, zero = {0, 0};
			setsockopt(G.fd, SOL_SOCKET, SO_RCVTIMEO, &tv, sizeof tv);
			setsockopt(G.fd, SOL_SOCKET, SO_SNDTIMEO, &tv, sizeof tv);
			while ((r = mbedtls_ssl_handshake(&G.ssl)) != 0)
				if (r != MBEDTLS_ERR_SSL_WANT_READ && r != MBEDTLS_ERR_SSL_WANT_WRITE)
					goto fail;
			setsockopt(G.fd, SOL_SOCKET, SO_RCVTIMEO, &zero, sizeof zero);
			setsockopt(G.fd, SOL_SOCKET, SO_SNDTIMEO, &zero, sizeof zero);
		}
		return 0;
	}
fail:
	net_close(NULL);
	return -1;
#else
	lg(NULL, MT_LOG_ERROR, "this build has no TLS; use an http:// or ws:// Hub URL");
	net_close(NULL);
	return -1;
#endif
}

static int net_read(void *c, uint8_t *b, size_t cap, int timeout_ms)
{
	struct pollfd pf;
	(void)c;
	if (G.fd < 0)
		return -1;
#ifdef MT_WITH_MBEDTLS
	if (!G.tls || mbedtls_ssl_get_bytes_avail(&G.ssl) == 0) {
#else
	if (1) {
#endif
		int r;
		pf.fd = G.fd;
		pf.events = POLLIN;
		r = poll(&pf, 1, timeout_ms);
		if (r == 0)
			return 0;
		if (r < 0)
			return errno == EINTR ? 0 : -1;
	}
#ifdef MT_WITH_MBEDTLS
	if (G.tls) {
		int r = mbedtls_ssl_read(&G.ssl, b, cap);
		if (r == MBEDTLS_ERR_SSL_WANT_READ || r == MBEDTLS_ERR_SSL_WANT_WRITE)
			return 0;
		return r > 0 ? r : -1;
	}
#endif
	{
		ssize_t r = recv(G.fd, b, cap, 0);
		return r > 0 ? (int)r : -1;
	}
}

static int net_write(void *c, const uint8_t *b, size_t n)
{
	(void)c;
	while (n) {
		int r;
#ifdef MT_WITH_MBEDTLS
		if (G.tls) {
			r = mbedtls_ssl_write(&G.ssl, b, n);
			if (r == MBEDTLS_ERR_SSL_WANT_READ || r == MBEDTLS_ERR_SSL_WANT_WRITE)
				continue;
		} else
#endif
			r = (int)send(G.fd, b, n, MSG_NOSIGNAL);
		if (r <= 0) {
			if (r < 0 && errno == EINTR)
				continue;
			return -1;
		}
		b += r;
		n -= (size_t)r;
	}
	return 0;
}

/* ---------------------------------------------------------------- PTY sessions */

/* Input for a session. A program that stops reading must not stall the Worker: after 2 s without progress the
 * write fails (the operation reports it) and the loop goes on serving keepalives, other sessions and revokes. */
static int pty_write(mt_session *s, const uint8_t *d, size_t n)
{
	pty_session *p = s->ctx;
	int64_t give_up = mono_ms(NULL) + 2000;
	size_t written = 0;
	while (n) {
		ssize_t r;
		if (p->fd < 0)
			return written ? -2 : -1;
		r = write(p->fd, d, n);
		if (r < 0) {
			if (errno == EINTR)
				continue;
			if (errno == EAGAIN) {
				struct pollfd pf = {p->fd, POLLOUT, 0};
				int64_t left = give_up - mono_ms(NULL);
				if (left <= 0)
					return written ? -2 : -1;
				poll(&pf, 1, (int)(left > 200 ? 200 : left));
				continue;
			}
			return written ? -2 : -1;
		}
		d += r;
		n -= (size_t)r;
		written += (size_t)r;
	}
	return 0;
}

static void pty_spawn(pty_session *p)
{
	struct winsize ws;
	pid_t pid;
	memset(&ws, 0, sizeof ws);
	ws.ws_col = p->cols;
	ws.ws_row = p->rows;
	pid = forkpty(&p->fd, NULL, NULL, &ws);
	if (pid < 0) {
		p->fd = -1;
		p->respawn_at = mono_ms(NULL) + 5000;
		return;
	}
	if (pid == 0) {
		setenv("TERM", "xterm-256color", 1);
		execl("/bin/sh", "sh", "-c", p->cmd, (char *)NULL);
		_exit(127);
	}
	p->pid = pid;
	fcntl(p->fd, F_SETFL, fcntl(p->fd, F_GETFL) | O_NONBLOCK);
	fcntl(p->fd, F_SETFD, fcntl(p->fd, F_GETFD) | FD_CLOEXEC); /* other sessions must not inherit it */
	p->s.created_ms = now_ms(NULL);
}

static void poll_sessions(void *c)
{
	int i;
	(void)c;
	for (i = 0; i < G.nps; i++) {
		pty_session *p = &G.ps[i];
		uint8_t buf[2048];
		if (p->fd < 0) {
			if (mono_ms(NULL) >= p->respawn_at)
				pty_spawn(p);
			continue;
		}
		for (;;) {
			ssize_t r = read(p->fd, buf, sizeof buf);
			if (r > 0) {
				mt_vt_feed(&p->vt, buf, (size_t)r);
				continue;
			}
			if (r < 0 && (errno == EAGAIN || errno == EINTR))
				break;
			/* EOF or EIO: the program ended; restart it shortly */
			close(p->fd);
			p->fd = -1;
			if (p->pid > 0)
				waitpid(p->pid, NULL, WNOHANG);
			p->pid = 0;
			p->respawn_at = mono_ms(NULL) + 1000;
			break;
		}
	}
	while (waitpid(-1, NULL, WNOHANG) > 0)
		;
}

static int add_session(const char *spec, uint16_t cols, uint16_t rows, size_t sb)
{
	const char *eq = strchr(spec, '=');
	pty_session *p;
	size_t nl, mem;
	if (!eq || G.nps >= MAX_SESSIONS || (nl = (size_t)(eq - spec)) == 0 || nl > 32)
		return -1;
	p = &G.ps[G.nps];
	memset(p, 0, sizeof *p);
	memcpy(p->s.name, spec, nl);
	for (size_t k = 0; k < nl; k++) { /* names travel in JSON and in targets: plain ASCII only */
		char ch = p->s.name[k];
		if (!((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' ||
		      ch == '_'))
			return -1;
	}
	snprintf(p->cmd, sizeof p->cmd, "%s", eq + 1);
	p->cols = cols;
	p->rows = rows;
	mem = mt_vt_mem_size(cols, rows, sb);
	p->mem = mem ? malloc(mem) : NULL;
	if (!p->mem || mt_vt_init(&p->vt, cols, rows, sb, p->mem, mem) != 0)
		return -1;
	p->s.kind = "shell";
	p->s.command = "sh";
	p->s.vt = &p->vt;
	p->s.write = pty_write;
	p->s.ctx = p;
	p->fd = -1;
	G.nps++;
	return 0;
}

static void on_signal(int sig)
{
	(void)sig;
	g_stop = 1;
	if (g_w)
		mt_worker_stop(g_w);
}

static void usage(void)
{
	fprintf(stderr, "usage: minion-thin --hub URL --state-dir DIR [--name NAME] [--invite-file FILE]\n"
	                "                   [--session NAME=COMMAND]... [--cols N] [--rows N] [--scrollback BYTES] [-v]\n"
	                "       minion-thin --version\n");
}

int main(int argc, char **argv)
{
	const char *hub = NULL, *name = NULL, *invite_file = NULL, *specs[MAX_SESSIONS];
	int nspecs = 0, i;
	unsigned long cols = 80, rows = 24, sb = 16384;
	char host[64];
	mt_platform pf;
	mt_config cfg;
	mt_session *ss[MAX_SESSIONS];
	mt_worker *w;
	for (i = 1; i < argc; i++) {
		const char *a = argv[i], *v = i + 1 < argc ? argv[i + 1] : NULL;
		if (!strcmp(a, "--version")) {
			printf("minion-thin %s\n", MT_VERSION);
			return 0;
		} else if (!strcmp(a, "-v") || !strcmp(a, "--verbose")) {
			G.verbose = 1;
		} else if (v && !strcmp(a, "--hub")) {
			hub = v, i++;
		} else if (v && !strcmp(a, "--state-dir")) {
			G.state_dir = v, i++;
		} else if (v && !strcmp(a, "--name")) {
			name = v, i++;
		} else if (v && !strcmp(a, "--invite-file")) {
			invite_file = v, i++;
		} else if (v && !strcmp(a, "--session") && nspecs < MAX_SESSIONS) {
			specs[nspecs++] = v, i++;
		} else if (v && !strcmp(a, "--cols")) {
			cols = strtoul(v, NULL, 10), i++;
		} else if (v && !strcmp(a, "--rows")) {
			rows = strtoul(v, NULL, 10), i++;
		} else if (v && !strcmp(a, "--scrollback")) {
			sb = strtoul(v, NULL, 10), i++;
		} else {
			usage();
			return 2;
		}
	}
	if (!hub || !G.state_dir || cols < 20 || cols > 256 || rows < 5 || rows > 255) {
		usage();
		return 2;
	}
	if (mkdir(G.state_dir, 0700) != 0 && errno != EEXIST) {
		perror("state dir");
		return 1;
	}
	chmod(G.state_dir, 0700);
	if (!name) {
		if (gethostname(host, sizeof host) != 0)
			snprintf(host, sizeof host, "minion-thin");
		host[sizeof host - 1] = 0;
		name = host;
	}
	if (nspecs == 0)
		specs[nspecs++] = "shell=exec /bin/sh -l";
	for (i = 0; i < nspecs; i++)
		if (add_session(specs[i], (uint16_t)cols, (uint16_t)rows, sb) != 0) {
			fprintf(stderr, "bad --session %s (NAME=COMMAND, NAME: 1..32 of A-Z a-z 0-9 - _)\n", specs[i]);
			return 2;
		}
	if (invite_file) { /* copy into the store, then remove the original: the key is a credential */
		uint8_t key[200];
		int fd = open(invite_file, O_RDONLY | O_CLOEXEC);
		ssize_t n = fd >= 0 ? read(fd, key, sizeof key) : -1;
		if (fd >= 0)
			close(fd);
		if (n > 0 && store_set(NULL, MT_KEY_INVITE, key, (size_t)n) == 0)
			unlink(invite_file);
		memset(key, 0, sizeof key);
	}
	signal(SIGPIPE, SIG_IGN);
	signal(SIGTERM, on_signal);
	signal(SIGINT, on_signal);
	memset(&pf, 0, sizeof pf);
	pf.net_connect = net_connect;
	pf.net_read = net_read;
	pf.net_write = net_write;
	pf.net_close = net_close;
	pf.now_ms = now_ms;
	pf.mono_ms = mono_ms;
	pf.random = rnd;
	pf.store_get = store_get;
	pf.store_set = store_set;
	pf.log = lg;
	pf.poll = poll_sessions;
	pf.sleep_ms = sleep_ms;
	for (i = 0; i < G.nps; i++)
		ss[i] = &G.ps[i].s;
	memset(&cfg, 0, sizeof cfg);
	cfg.hub_url = hub;
	cfg.name = name;
	cfg.sessions = ss;
	cfg.nsessions = (size_t)G.nps;
	cfg.rx_cap = 72 * 1024; /* a Hub hello with a 128-link transition chain is about 32 KB */
	cfg.frame_cap = 64 * 1024;
	cfg.out_cap = 128 * 1024;
	cfg.max_streams = 16;
	cfg.results_ring = 256;
	w = mt_worker_new(&pf, &cfg);
	if (!w) {
		fprintf(stderr, "cannot start (bad --hub URL or out of memory)\n");
		return 1;
	}
	g_w = w;
	{
		mt_status st;
		mt_worker_status(w, &st);
		if (st.enrolled)
			fprintf(stderr, "minion-thin %s: enrolled as %s with Hub %s\n", MT_VERSION, st.worker_id, st.hub_fp);
		else
			fprintf(stderr, "minion-thin %s: not enrolled; waiting for an invitation key\n", MT_VERSION);
	}
	for (i = 0; i < G.nps; i++)
		pty_spawn(&G.ps[i]);
	while (!g_stop) {
		int r;
		r = mt_worker_run(w);
		if (r == -2) {
			fprintf(stderr, "minion-thin: revoked by the Hub and no new invitation key; exiting\n");
			return 3;
		}
		if (g_stop)
			break;
	}
	mt_worker_stop(w);
	for (i = 0; i < G.nps; i++)
		if (G.ps[i].pid > 0)
			kill(G.ps[i].pid, SIGHUP);
	return 0;
}
