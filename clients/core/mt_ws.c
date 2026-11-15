#include "mt_ws.h"

#include <stdio.h>
#include <string.h>

#include "mt_enc.h"

#define CONNECT_PATH "/api/v1/worker/connect"
#define FRAME_TIMEOUT_MS 30000 /* once a frame has started, the rest must arrive within this */

/* ---------------------------------------------------------------- URL */

int mt_url_parse(mt_url *u, const char *url)
{
	const char *p = url, *host, *hend, *path;
	size_t hl, pl;
	unsigned long port = 0;
	memset(u, 0, sizeof *u);
	if (!strncmp(p, "https://", 8) || !strncmp(p, "wss://", 6)) {
		u->scheme_tls = 1;
		p += p[0] == 'h' ? 8 : 6;
	} else if (!strncmp(p, "http://", 7) || !strncmp(p, "ws://", 5)) {
		p += p[0] == 'h' ? 7 : 5;
	} else {
		return -1;
	}
	host = p;
	if (strchr(host, '@') && strchr(host, '@') < (strchr(host, '/') ? strchr(host, '/') : host + strlen(host)))
		return -1; /* no user info */
	hend = host;
	while (*hend && *hend != ':' && *hend != '/' && *hend != '?' && *hend != '#')
		hend++;
	hl = (size_t)(hend - host);
	if (hl == 0 || hl >= sizeof u->host)
		return -1;
	memcpy(u->host, host, hl);
	p = hend;
	if (*p == ':') {
		char *end;
		p++;
		if (*p < '0' || *p > '9')
			return -1;
		for (end = (char *)p; *end >= '0' && *end <= '9'; end++)
			port = port * 10 + (unsigned long)(*end - '0');
		if (port == 0 || port > 65535)
			return -1;
		p = end;
	}
	u->port = (uint16_t)(port ? port : (u->scheme_tls ? 443 : 80));
	path = p;
	pl = 0;
	while (path[pl] && path[pl] != '?' && path[pl] != '#')
		pl++;
	while (pl > 0 && path[pl - 1] == '/')
		pl--;
	if (pl && path[0] != '/')
		return -1;
	if (pl + sizeof CONNECT_PATH >= sizeof u->path)
		return -1;
	memcpy(u->path, path, pl);
	memcpy(u->path + pl, CONNECT_PATH, sizeof CONNECT_PATH);
	return 0;
}

/* ---------------------------------------------------------------- SHA-1 (only for Sec-WebSocket-Accept) */

static void sha1(const uint8_t *msg, size_t n, uint8_t out[20])
{
	uint32_t h[5] = {0x67452301, 0xEFCDAB89, 0x98BADCFE, 0x10325476, 0xC3D2E1F0};
	uint8_t blk[64];
	size_t total = n + 9, nb = (total + 63) / 64, b, i;
	for (b = 0; b < nb; b++) {
		uint32_t w[80], a, bb, c, d, e, f, k, t;
		for (i = 0; i < 64; i++) {
			size_t pos = b * 64 + i;
			if (pos < n)
				blk[i] = msg[pos];
			else if (pos == n)
				blk[i] = 0x80;
			else if (pos >= nb * 64 - 8)
				blk[i] = (uint8_t)(((uint64_t)n * 8) >> (8 * (nb * 64 - 1 - pos)));
			else
				blk[i] = 0;
		}
		for (i = 0; i < 16; i++)
			w[i] = (uint32_t)blk[4 * i] << 24 | (uint32_t)blk[4 * i + 1] << 16 | (uint32_t)blk[4 * i + 2] << 8 |
			       blk[4 * i + 3];
		for (i = 16; i < 80; i++) {
			t = w[i - 3] ^ w[i - 8] ^ w[i - 14] ^ w[i - 16];
			w[i] = (t << 1) | (t >> 31);
		}
		a = h[0]; bb = h[1]; c = h[2]; d = h[3]; e = h[4];
		for (i = 0; i < 80; i++) {
			if (i < 20) { f = (bb & c) | (~bb & d); k = 0x5A827999; }
			else if (i < 40) { f = bb ^ c ^ d; k = 0x6ED9EBA1; }
			else if (i < 60) { f = (bb & c) | (bb & d) | (c & d); k = 0x8F1BBCDC; }
			else { f = bb ^ c ^ d; k = 0xCA62C1D6; }
			t = ((a << 5) | (a >> 27)) + f + e + k + w[i];
			e = d; d = c; c = (bb << 30) | (bb >> 2); bb = a; a = t;
		}
		h[0] += a; h[1] += bb; h[2] += c; h[3] += d; h[4] += e;
	}
	for (i = 0; i < 5; i++) {
		out[4 * i] = (uint8_t)(h[i] >> 24);
		out[4 * i + 1] = (uint8_t)(h[i] >> 16);
		out[4 * i + 2] = (uint8_t)(h[i] >> 8);
		out[4 * i + 3] = (uint8_t)h[i];
	}
}

/* standard (padded) base64 for the handshake headers */
static void b64std(char *out, const uint8_t *in, size_t n)
{
	static const char t[] = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
	size_t i, o = 0;
	for (i = 0; i < n; i += 3) {
		uint32_t v = (uint32_t)in[i] << 16 | (i + 1 < n ? (uint32_t)in[i + 1] << 8 : 0) | (i + 2 < n ? in[i + 2] : 0);
		out[o++] = t[v >> 18];
		out[o++] = t[(v >> 12) & 63];
		out[o++] = i + 1 < n ? t[(v >> 6) & 63] : '=';
		out[o++] = i + 2 < n ? t[v & 63] : '=';
	}
	out[o] = 0;
}

/* ---------------------------------------------------------------- byte reader */

void mt_ws_init(mt_ws *ws, const mt_platform *p, uint8_t *buf, size_t cap)
{
	memset(ws, 0, sizeof *ws);
	ws->p = p;
	ws->buf = buf;
	ws->cap = cap;
}

/* Read exactly n bytes; the first byte may wait first_timeout, the rest FRAME_TIMEOUT_MS. 0 = timeout before the
 * first byte, n = ok, <0 = error. */
static int read_exact(mt_ws *ws, uint8_t *dst, size_t n, int first_timeout)
{
	size_t got = 0;
	while (got < n) {
		if (ws->raw_off < ws->raw_len) {
			size_t take = ws->raw_len - ws->raw_off;
			if (take > n - got)
				take = n - got;
			memcpy(dst + got, ws->raw + ws->raw_off, take);
			ws->raw_off += take;
			got += take;
			continue;
		}
		{
			int r = ws->p->net_read(ws->p->ctx, ws->raw, sizeof ws->raw, got ? FRAME_TIMEOUT_MS : first_timeout);
			if (r == 0 && got == 0 && first_timeout >= 0)
				return 0;
			if (r <= 0)
				return -1;
			ws->raw_len = (size_t)r;
			ws->raw_off = 0;
		}
	}
	return (int)n;
}

static int read_line(mt_ws *ws, char *line, size_t cap)
{
	size_t n = 0;
	for (;;) {
		uint8_t c;
		if (read_exact(ws, &c, 1, FRAME_TIMEOUT_MS) != 1)
			return -1;
		if (c == '\n')
			break;
		if (c != '\r' && n + 1 < cap)
			line[n++] = (char)c;
	}
	line[n] = 0;
	return (int)n;
}

static int ieq_prefix(const char *s, const char *prefix)
{
	for (; *prefix; s++, prefix++) {
		char a = *s, b = *prefix;
		if (a >= 'A' && a <= 'Z') a = (char)(a - 'A' + 'a');
		if (b >= 'A' && b <= 'Z') b = (char)(b - 'A' + 'a');
		if (a != b)
			return 0;
	}
	return 1;
}

int mt_ws_connect(mt_ws *ws, const mt_url *u, int timeout_ms)
{
	uint8_t nonce[16], digest[20];
	char key[32], accept[32], req[512], line[256], hostport[140], keyguid[80];
	int n, status = 0, ok_accept = 0, ok_proto = 0, ok_upgrade = 0;
	ws->len = ws->raw_len = ws->raw_off = 0;
	ws->http_status = 0;
	ws->http_reason[0] = 0;
	if (ws->p->net_connect(ws->p->ctx, u->host, u->port, u->scheme_tls, timeout_ms) != 0)
		return -1;
	ws->p->random(ws->p->ctx, nonce, sizeof nonce);
	b64std(key, nonce, sizeof nonce);
	if ((u->scheme_tls && u->port == 443) || (!u->scheme_tls && u->port == 80))
		snprintf(hostport, sizeof hostport, "%s", u->host);
	else
		snprintf(hostport, sizeof hostport, "%s:%u", u->host, (unsigned)u->port);
	n = snprintf(req, sizeof req,
	             "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
	             "Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: minion.v1\r\n"
	             "User-Agent: minion-thin\r\n\r\n",
	             u->path, hostport, key);
	if (n <= 0 || (size_t)n >= sizeof req || ws->p->net_write(ws->p->ctx, (const uint8_t *)req, (size_t)n) != 0)
		goto fail;
	snprintf(keyguid, sizeof keyguid, "%s258EAFA5-E914-47DA-95CA-C5AB0DC85B11", key);
	sha1((const uint8_t *)keyguid, strlen(keyguid), digest);
	b64std(accept, digest, sizeof digest);
	if (read_line(ws, line, sizeof line) < 0)
		goto fail;
	if (strncmp(line, "HTTP/1.1 ", 9) && strncmp(line, "HTTP/1.0 ", 9))
		goto fail;
	status = (line[9] - '0') * 100 + (line[10] - '0') * 10 + (line[11] - '0');
	for (;;) {
		int l = read_line(ws, line, sizeof line);
		if (l < 0)
			goto fail;
		if (l == 0)
			break;
		if (ieq_prefix(line, "sec-websocket-accept:")) {
			const char *v = line + 21;
			while (*v == ' ')
				v++;
			ok_accept = !strcmp(v, accept);
		} else if (ieq_prefix(line, "sec-websocket-protocol:")) {
			const char *v = line + 23;
			while (*v == ' ')
				v++;
			ok_proto = !strcmp(v, "minion.v1");
		} else if (ieq_prefix(line, "upgrade:")) {
			const char *v = line + 8;
			while (*v == ' ')
				v++;
			ok_upgrade = ieq_prefix(v, "websocket");
		}
	}
	if (status != 101) {
		size_t k = 0;
		ws->http_status = status;
		/* keep the first bytes of the body (the Hub answers JSON errors such as hub_cert_expired) */
		while (k + 1 < sizeof ws->http_reason) {
			uint8_t c;
			int r = read_exact(ws, &c, 1, 2000);
			if (r != 1)
				break;
			ws->http_reason[k++] = (char)((c >= 0x20 && c < 0x7f) ? c : ' ');
		}
		ws->http_reason[k] = 0;
		goto fail;
	}
	if (!ok_accept || !ok_proto || !ok_upgrade)
		goto fail;
	return 0;
fail:
	ws->p->net_close(ws->p->ctx);
	return -1;
}

static int send_frame(mt_ws *ws, uint8_t opcode, const uint8_t *data, size_t n)
{
	uint8_t h[14], mask[4], chunk[256];
	size_t hl = 2, done = 0, i;
	h[0] = (uint8_t)(0x80 | opcode);
	if (n < 126) {
		h[1] = (uint8_t)(0x80 | n);
	} else if (n <= 0xFFFF) {
		h[1] = 0x80 | 126;
		h[2] = (uint8_t)(n >> 8);
		h[3] = (uint8_t)n;
		hl = 4;
	} else {
		h[1] = 0x80 | 127;
		for (i = 0; i < 8; i++)
			h[2 + i] = (uint8_t)((uint64_t)n >> (56 - 8 * i));
		hl = 10;
	}
	ws->p->random(ws->p->ctx, mask, 4);
	memcpy(h + hl, mask, 4);
	if (ws->p->net_write(ws->p->ctx, h, hl + 4) != 0)
		return -1;
	while (done < n) {
		size_t take = n - done > sizeof chunk ? sizeof chunk : n - done;
		for (i = 0; i < take; i++)
			chunk[i] = data[done + i] ^ mask[(done + i) & 3];
		if (ws->p->net_write(ws->p->ctx, chunk, take) != 0)
			return -1;
		done += take;
	}
	return 0;
}

int mt_ws_send(mt_ws *ws, const uint8_t *data, size_t n)
{
	return send_frame(ws, 2, data, n);
}

int mt_ws_recv(mt_ws *ws, int timeout_ms)
{
	ws->len = 0;
	for (;;) {
		uint8_t h[2], ext[8], ctl[125];
		uint64_t plen = 0;
		int r, fin, op, i;
		r = read_exact(ws, h, 2, ws->len ? FRAME_TIMEOUT_MS : timeout_ms);
		if (r == 0)
			return 0;
		if (r < 0)
			return -1;
		fin = h[0] & 0x80;
		op = h[0] & 0x0F;
		if (h[0] & 0x70 || h[1] & 0x80)
			return -1; /* no extensions; a server never masks */
		plen = h[1] & 0x7F;
		if (plen == 126) {
			if (read_exact(ws, ext, 2, FRAME_TIMEOUT_MS) != 2)
				return -1;
			plen = (uint64_t)ext[0] << 8 | ext[1];
		} else if (plen == 127) {
			if (read_exact(ws, ext, 8, FRAME_TIMEOUT_MS) != 8)
				return -1;
			plen = 0;
			for (i = 0; i < 8; i++)
				plen = plen << 8 | ext[i];
		}
		if (op >= 8) { /* control frame */
			if (!fin || plen > 125)
				return -1;
			if (plen && read_exact(ws, ctl, (size_t)plen, FRAME_TIMEOUT_MS) != (int)plen)
				return -1;
			if (op == 9) {
				if (send_frame(ws, 10, ctl, (size_t)plen) != 0)
					return -1;
				continue;
			}
			if (op == 10)
				continue;
			if (op == 8) {
				send_frame(ws, 8, ctl, plen >= 2 ? 2 : 0);
				return -2;
			}
			return -1;
		}
		if (op == 1)
			return -1; /* text messages are not part of the protocol */
		if ((op == 0) != (ws->len > 0) && !(op == 2 && ws->len == 0))
			return -1; /* continuation without a start, or a new message inside one */
		if (plen > ws->cap - ws->len)
			return -3; /* message larger than our buffer */
		if (plen && read_exact(ws, ws->buf + ws->len, (size_t)plen, FRAME_TIMEOUT_MS) != (int)plen)
			return -1;
		ws->len += (size_t)plen;
		if (fin)
			return (int)ws->len;
	}
}

void mt_ws_close(mt_ws *ws)
{
	uint8_t code[2] = {0x03, 0xE8}; /* 1000 normal closure */
	send_frame(ws, 8, code, 2);
	ws->p->net_close(ws->p->ctx);
}
