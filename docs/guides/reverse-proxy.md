# Put the Hub behind a reverse proxy

Back to the [README](../../README.md).

A reverse proxy gives the Hub a certificate you control (for example from Let's Encrypt), a standard port and a name such as `hub.example.com`. The Hub then runs without TLS on the loopback interface and trusts the proxy for the client address and the scheme.

**Trust:** a proxy that ends TLS sees everything the Hub sees: session screens, typed text and the web UI traffic. Run it yourself, on a machine you control. See [security](../security.md). If you cannot accept that, give the Hub a certificate of its own (`MINION_TLS_CERT_FILE`, `MINION_TLS_KEY_FILE`) and let the proxy pass the TCP stream through without ending TLS (last section).

## The Hub side

```sh
MINION_TLS=off
MINION_LISTEN=127.0.0.1:8443
MINION_PUBLIC_URL=https://hub.example.com
MINION_TRUSTED_PROXIES=127.0.0.1/32
```

| Variable | Why |
| --- | --- |
| `MINION_TLS=off` | The Hub speaks plain HTTP. Never expose it like that on a network interface. |
| `MINION_LISTEN` | Keep it on the loopback interface (or on a private network only the proxy can reach). |
| `MINION_PUBLIC_URL` | **Required** with `MINION_TLS=off`. It is the address users and Workers use; it decides the redirect address of [OIDC](oidc.md) sign-in and whether cookies are marked secure. |
| `MINION_TRUSTED_PROXIES` | Comma-separated addresses or CIDRs of the proxies whose `X-Forwarded-For` and `X-Forwarded-Proto` headers the Hub honours. Name the **exact** address or the smallest network your proxy has. Never `0.0.0.0/0` or `::/0`: any client could then claim any address and scheme. |

The Hub accepts a passphrase (the [Hub identity](hub-identity.md) operations) only on a request that arrived over HTTPS by these rules: with `MINION_TLS=off` the scheme comes from `X-Forwarded-Proto` of a trusted proxy, so `MINION_TRUSTED_PROXIES` must name your proxy and the proxy must set the header from its own connection. A loopback address is not by itself trusted.

For the Docker Hub ([`deploy/compose/hub.yaml`](../../deploy/compose/hub.yaml)) uncomment the three variables in the file and keep the port published on `127.0.0.1`. When the proxy runs as a container on the same Compose network, its address is the address of that container on that network; give that address (or the network's subnet) as the trusted proxy.

## What the proxy must do

- **Pass WebSocket upgrades.** Workers keep one long-lived connection to the Hub.
- **Not close idle connections early.** The idle timeout must be above 30 seconds; an hour is a sensible value.
- **Not buffer the event stream.** The web UI follows live changes over server-sent events; a proxy that buffers them delivers nothing until the buffer fills.
- **Set the forwarded headers** from the real client address, overwriting what the client sent (not appending to it).
- Send the `Host` header of the public name.

### nginx

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

server {
    listen 443 ssl;
    server_name hub.example.com;
    ssl_certificate     /etc/ssl/hub.example.com/fullchain.pem;
    ssl_certificate_key /etc/ssl/hub.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8443;
        proxy_http_version 1.1;
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-For   $remote_addr;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade           $http_upgrade;
        proxy_set_header Connection        $connection_upgrade;
        proxy_buffering    off;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }
}
```

### Caddy

```caddyfile
hub.example.com {
    reverse_proxy 127.0.0.1:8443 {
        flush_interval -1
    }
}
```

Caddy upgrades WebSocket connections by itself, gets and renews the certificate, and sets the forwarded headers.

## The Worker side

Point Workers at the public name:

```sh
minion worker pair --server https://hub.example.com
```

A Worker does not use the proxy's certificate to trust the Hub: pairing and enrollment pin the Hub's identity key, so a change of the proxy certificate, or of the proxy, does not break paired Workers. Give the Hub a host name of its own. A Worker keeps a path in its Hub URL when it connects, but serving the Hub under a path prefix is not a setup this guide documents.

## Passing TLS through instead

To keep the proxy out of the content, give the Hub a real certificate and let the proxy forward the encrypted stream:

```sh
MINION_TLS_CERT_FILE=/etc/minion/hub.example.com.crt
MINION_TLS_KEY_FILE=/etc/minion/hub.example.com.key
```

and, in nginx, a `stream` block instead of the `server` block above:

```nginx
stream {
    server {
        listen 443;
        proxy_pass 127.0.0.1:8443;
    }
}
```

The Hub then reads the certificate files (send it `SIGHUP` after a renewal to reload them), and sees the proxy's address as the client address, because no forwarded headers exist at this level. Leave `MINION_TRUSTED_PROXIES` empty.
