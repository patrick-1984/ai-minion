#!/usr/bin/env python3
"""Throw-away Minion Hub for thin-client tests (stdlib only).

  hub_harness.py up      --bin MINION --dir DIR [--port 18443]   start a Hub, set its passphrase, sign in
  hub_harness.py invite  --dir DIR [--ttl 24h] [--mode reusable]   create an invitation key, print it
  hub_harness.py workers --dir DIR                                  workers_list (JSON)
  hub_harness.py approve --dir DIR WORKER_ID                        worker_approve
  hub_harness.py op      --dir DIR NAME [JSON]                      any operation with the admin session
  hub_harness.py down    --dir DIR                                  stop the Hub

State (pid, port, cookie, csrf) lives in DIR/harness.json. The Hub listens on 127.0.0.1 with its self-signed
certificate; this harness never verifies it (test only).
"""

import argparse
import http.cookiejar
import json
import os
import pty
import secrets
import signal
import ssl
import subprocess
import sys
import time
import urllib.request

PASSWORD = "harness-admin-password-0123"
PASSPHRASE = "harness-passphrase-0123456789"


def _state(d):
    with open(os.path.join(d, "harness.json")) as f:
        return json.load(f)


def _save(d, st):
    with open(os.path.join(d, "harness.json"), "w") as f:
        json.dump(st, f)


def _ctx():
    c = ssl.create_default_context()
    c.check_hostname = False
    c.verify_mode = ssl.CERT_NONE
    return c


def _req(st, method, path, body=None, headers=None):
    h = {"Content-Type": "application/json"}
    if st.get("cookie"):
        h["Cookie"] = st["cookie"]
    if st.get("csrf"):
        h["X-Minion-CSRF"] = st["csrf"]
    h.update(headers or {})
    data = json.dumps(body).encode() if body is not None else None
    scheme = "http" if st.get("plain") else "https"
    r = urllib.request.Request(f"{scheme}://127.0.0.1:{st['port']}{path}", data=data, headers=h, method=method)
    try:
        with urllib.request.urlopen(r, context=_ctx(), timeout=30) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else None), resp.headers
    except urllib.error.HTTPError as e:
        raw = e.read()
        try:
            return e.code, json.loads(raw), e.headers
        except ValueError:
            return e.code, raw.decode(errors="replace"), e.headers


def _uuid7():
    b = bytearray(int(time.time() * 1000).to_bytes(6, "big") + os.urandom(10))
    b[6] = (b[6] & 0x0F) | 0x70
    b[8] = (b[8] & 0x3F) | 0x80
    h = b.hex()
    return f"{h[:8]}-{h[8:12]}-{h[12:16]}-{h[16:20]}-{h[20:]}"


def _env(d, port, plain=False):
    e = dict(os.environ)
    e.update({"MINION_DATA_DIR": os.path.join(d, "data"), "MINION_LISTEN": f"127.0.0.1:{port}",
              "MINION_ADMIN_PASSWORD_FILE": os.path.join(d, "admin_password.txt"), "MINION_LOG_LEVEL": "debug"})
    if plain:
        e.update({"MINION_TLS": "off", "MINION_PUBLIC_URL": f"http://127.0.0.1:{port}",
                  "MINION_TRUSTED_PROXIES": "127.0.0.1/32"})
    return e


def _set_passphrase(binary, env):
    pid, fd = pty.fork()
    if pid == 0:
        os.execve(binary, [binary, "hub", "identity", "set-passphrase"], env)
    buf, answered, deadline = b"", 0, time.time() + 60
    while time.time() < deadline:
        try:
            data = os.read(fd, 4096)
        except OSError:
            break
        if not data:
            break
        buf += data
        while buf.lower().count(b"passphrase: ") > answered:
            time.sleep(0.2)
            os.write(fd, PASSPHRASE.encode() + b"\r")
            answered += 1
    _, status = os.waitpid(pid, 0)
    if os.waitstatus_to_exitcode(status) != 0:
        sys.exit("set-passphrase failed: " + buf.decode(errors="replace")[-400:])


def up(a):
    d = os.path.abspath(a.dir)
    os.makedirs(os.path.join(d, "data"), exist_ok=True)
    with open(os.path.join(d, "admin_password.txt"), "w") as f:
        f.write(PASSWORD)
    os.chmod(os.path.join(d, "admin_password.txt"), 0o600)
    env = _env(d, a.port, a.plain)
    log = open(os.path.join(d, "hub.log"), "ab")
    p = subprocess.Popen([a.bin, "hub", "serve"], env=env, stdout=log, stderr=subprocess.STDOUT,
                         start_new_session=True)
    st = {"pid": p.pid, "port": a.port, "bin": os.path.abspath(a.bin), "plain": a.plain}
    _save(d, st)
    for _ in range(100):
        try:
            code, _, _ = _req(st, "GET", "/healthz")
            if code == 200:
                break
        except OSError:
            pass
        time.sleep(0.2)
    else:
        sys.exit("hub did not answer /healthz; see " + os.path.join(d, "hub.log"))
    _set_passphrase(st["bin"], env)
    code, body, headers = _req(st, "POST", "/auth/login", {"username": "admin", "password": PASSWORD})
    if code != 200:
        sys.exit(f"login failed: {code} {body}")
    cookies = [v.split(";", 1)[0] for k, v in headers.items() if k.lower() == "set-cookie"]
    st.update({"cookie": "; ".join(cookies), "csrf": body["csrf_token"]})
    _save(d, st)
    code, ident, _ = _req(st, "GET", "/api/v1/ops/hub_identity_status")
    print(json.dumps({"url": f"{'http' if a.plain else 'https'}://127.0.0.1:{a.port}", "identity": ident}))


def op(a):
    st = _state(os.path.abspath(a.dir))
    body = json.loads(a.json) if a.json else {}
    if a.write:
        body.setdefault("request_id", _uuid7())
        body.setdefault("force", False)
    code, res, _ = _req(st, "POST", "/api/v1/ops/" + a.name, body)
    print(json.dumps({"status": code, "result": res}))
    return 0 if code == 200 else 1


def invite(a):
    st = _state(os.path.abspath(a.dir))
    code, res, _ = _req(st, "POST", "/api/v1/ops/invitation_create",
                        {"mode": a.mode, "ttl": a.ttl, "kind": "private", "name": "thin-" + secrets.token_hex(3)})
    if code != 200:
        sys.exit(f"invitation_create: {code} {res}")
    print(res["key"])


def down(a):
    d = os.path.abspath(a.dir)
    try:
        st = _state(d)
        os.killpg(st["pid"], signal.SIGTERM)
    except (OSError, ValueError, KeyError) as e:
        print("down:", e)


def main():
    p = argparse.ArgumentParser()
    s = p.add_subparsers(dest="cmd", required=True)
    x = s.add_parser("up"); x.add_argument("--bin", required=True); x.add_argument("--dir", required=True)
    x.add_argument("--port", type=int, default=18443); x.add_argument("--plain", action="store_true")
    x.set_defaults(f=up)
    x = s.add_parser("invite"); x.add_argument("--dir", required=True); x.add_argument("--ttl", default="24h")
    x.add_argument("--mode", default="one_time"); x.set_defaults(f=invite)
    x = s.add_parser("op"); x.add_argument("--dir", required=True); x.add_argument("name")
    x.add_argument("json", nargs="?"); x.add_argument("--write", action="store_true"); x.set_defaults(f=op)
    x = s.add_parser("workers"); x.add_argument("--dir", required=True)
    x.set_defaults(f=lambda a: op(argparse.Namespace(dir=a.dir, name="workers_list", json='{"all":true}',
                                                     write=False)))
    x = s.add_parser("approve"); x.add_argument("--dir", required=True); x.add_argument("worker_id")
    x.set_defaults(f=lambda a: op(argparse.Namespace(dir=a.dir, name="worker_approve",
                                                     json=json.dumps({"worker_id": a.worker_id}), write=True)))
    x = s.add_parser("down"); x.add_argument("--dir", required=True); x.set_defaults(f=down)
    a = p.parse_args()
    sys.exit(a.f(a) or 0)


if __name__ == "__main__":
    main()
