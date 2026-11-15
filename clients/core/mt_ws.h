/* Minion thin Worker: minimal RFC 6455 WebSocket client (binary messages, subprotocol minion.v1). */
#ifndef MT_WS_H
#define MT_WS_H

#include <stddef.h>
#include <stdint.h>

#include "mt_platform.h"

typedef struct {
	char scheme_tls;  /* 1 for https/wss */
	char host[128];
	uint16_t port;
	char path[160];   /* full path including the connect endpoint */
} mt_url;

/* Parse the Hub URL (https/http/wss/ws, optional port and path prefix) and append /api/v1/worker/connect. */
int mt_url_parse(mt_url *u, const char *url);

typedef struct {
	const mt_platform *p;
	uint8_t *buf;     /* receive buffer for one message */
	size_t cap, len;  /* len: bytes of the current message assembled so far */
	uint8_t raw[512]; /* small read-ahead buffer */
	size_t raw_len, raw_off;
	int http_status;  /* set when the upgrade is refused */
	char http_reason[64];
} mt_ws;

void mt_ws_init(mt_ws *ws, const mt_platform *p, uint8_t *buf, size_t cap);
/* TCP/TLS connect and HTTP upgrade. Returns 0, or <0 (ws->http_status > 0 when the server answered non-101). */
int mt_ws_connect(mt_ws *ws, const mt_url *u, int timeout_ms);
/* Send one binary message (masked, one frame). */
int mt_ws_send(mt_ws *ws, const uint8_t *data, size_t n);
/* Receive one binary message into ws->buf. Returns its length, 0 on timeout (nothing pending), <0 on close or
 * error. Pings are answered, pongs ignored, text messages refused. */
int mt_ws_recv(mt_ws *ws, int timeout_ms);
void mt_ws_close(mt_ws *ws);

#endif
