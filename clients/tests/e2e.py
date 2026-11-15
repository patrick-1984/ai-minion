#!/usr/bin/env python3
"""Interoperability test: the thin Worker against the real Go Hub of this repository.

  e2e.py --hub-bin MINION --thin-bin MINION_THIN [--tls] [--work DIR]

Flow (each step must pass):
  1. start a Hub (plain HTTP, or TLS with its self-signed certificate when --tls), set its passphrase
  2. enroll the thin Worker with an invitation key; the key file is consumed; the Worker is pending
  3. approve it; it reconnects and is online; its sessions appear in the Hub's live session list
  4. send_text (submit) runs a command in the device shell; session_snapshot shows the output
  5. send_keys and session_capture; a repeated request id is answered from the cache, not re-run
  6. share_set none hides the sessions, share_set all shows them again
  7. the Hub re-issues its certificate: the Worker reconnects by itself and keeps working
  8. the Hub rotates its master (root) key: the Worker follows the signed chain and keeps working
  9. the Hub restarts: the Worker reconnects by itself
 10. the Hub removes the Worker: a signed live revoke wipes the device state and the Worker exits (3)
"""

import argparse
import json
import os
import pty
import shutil
import signal
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import hub_harness as hh  # noqa: E402

PASS2 = "harness-passphrase-rotated-0123"


def log(msg):
    print(f"[e2e {time.strftime('%H:%M:%S')}] {msg}", flush=True)


def fail(msg):
    print(f"FAIL: {msg}", flush=True)
    sys.exit(1)


def until(what, fn, timeout=30.0, every=0.3):
    end = time.time() + timeout
    last = None
    while time.time() < end:
        try:
            last = fn()
            if last:
                return last
        except (OSError, ValueError, KeyError) as e:
            last = e
        time.sleep(every)
    fail(f"timed out waiting for: {what} (last: {last!r})")


def drive(binary, args, env, answers, timeout=60):
    """Run a CLI command on a pseudo-terminal and answer its prompts in order: [(prompt substring, answer)]."""
    pid, fd = pty.fork()
    if pid == 0:
        os.execve(binary, [binary] + args, env)
    buf, i, end = b"", 0, time.time() + timeout
    while time.time() < end:
        try:
            data = os.read(fd, 4096)
        except OSError:
            break
        if not data:
            break
        buf += data
        while i < len(answers) and answers[i][0].encode() in buf:
            buf = buf.split(answers[i][0].encode(), 1)[1]
            time.sleep(0.2)
            os.write(fd, answers[i][1].encode() + b"\r")
            i += 1
    _, status = os.waitpid(pid, 0)
    if os.waitstatus_to_exitcode(status) != 0:
        fail(f"{' '.join(args)} failed: {buf.decode(errors='replace')[-300:]}")


class Hub:
    def __init__(self, binary, work, tls, port):
        self.bin, self.dir, self.tls, self.port = binary, os.path.join(work, "hub"), tls, port

    def start(self, first=True):
        a = argparse.Namespace(bin=self.bin, dir=self.dir, port=self.port, plain=not self.tls)
        if first:
            hh.up(a)
        else:  # restart on the same data directory: no passphrase prompt (the root is sealed but loaded? no:
            # a restarted Hub needs no passphrase to serve; it keeps the operational key)
            st = hh._state(self.dir)
            env = hh._env(self.dir, self.port, not self.tls)
            logf = open(os.path.join(self.dir, "hub.log"), "ab")
            p = subprocess.Popen([self.bin, "hub", "serve"], env=env, stdout=logf, stderr=subprocess.STDOUT,
                                 start_new_session=True)
            st["pid"] = p.pid
            hh._save(self.dir, st)
            until("hub healthz", lambda: hh._req(st, "GET", "/healthz")[0] == 200)
            code, body, headers = hh._req({**st, "cookie": None, "csrf": None}, "POST", "/auth/login",
                                          {"username": "admin", "password": hh.PASSWORD})
            if code != 200:
                fail(f"login after restart: {code} {body}")
            cookies = [v.split(";", 1)[0] for k, v in headers.items() if k.lower() == "set-cookie"]
            st.update({"cookie": "; ".join(cookies), "csrf": body["csrf_token"]})
            hh._save(self.dir, st)

    def stop(self):
        try:
            os.killpg(hh._state(self.dir)["pid"], signal.SIGTERM)
        except OSError:
            pass
        time.sleep(1.0)

    @property
    def url(self):
        return f"{'https' if self.tls else 'http'}://127.0.0.1:{self.port}"

    def op(self, name, body=None, write=False):
        st = hh._state(self.dir)
        body = dict(body or {})
        if write:
            body.setdefault("request_id", hh._uuid7())
            body.setdefault("force", False)
        code, res, _ = hh._req(st, "POST", "/api/v1/ops/" + name, body)
        return code, res

    def get(self, path):
        st = hh._state(self.dir)
        code, res, _ = hh._req(st, "GET", path)
        return code, res

    def env(self):
        return hh._env(self.dir, self.port, not self.tls)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--hub-bin", required=True)
    ap.add_argument("--thin-bin", required=True)
    ap.add_argument("--tls", action="store_true")
    ap.add_argument("--work", default="/tmp/minion-thin-e2e")
    ap.add_argument("--port", type=int, default=18555)
    a = ap.parse_args()
    shutil.rmtree(a.work, ignore_errors=True)
    os.makedirs(a.work)
    hub = Hub(os.path.abspath(a.hub_bin), a.work, a.tls, a.port)
    thin_log = open(os.path.join(a.work, "thin.log"), "ab")
    state = os.path.join(a.work, "thin-state")
    thin = None
    try:
        log("1. Hub up")
        hub.start()

        log("2. enroll with an invitation key")
        code, res = hub.op("invitation_create", {"mode": "one_time", "ttl": "1h", "kind": "private", "name": "e2e"})
        if code != 200:
            fail(f"invitation_create {code} {res}")
        inv = os.path.join(a.work, "invite.txt")
        with open(inv, "w") as f:
            f.write(res["key"])
        thin = subprocess.Popen([os.path.abspath(a.thin_bin), "--hub", hub.url, "--state-dir", state, "--name", "e2e-thin",
                                 "--invite-file", inv, "--session", "shell=exec /bin/sh", "-v"],
                                stdout=thin_log, stderr=subprocess.STDOUT, start_new_session=True)
        w = until("pending Worker", lambda: next((x for x in hub.op("workers_list", {"all": True})[1]["workers"]
                                                    if x["name"] == "e2e-thin" and x.get("pending")), None))
        wid = w["id"]
        if os.path.exists(inv):
            fail("the invitation key file was not consumed")
        until("invite erased from the state", lambda: not os.path.exists(os.path.join(state, "invite")))

        log("3. approve; online; live session list")
        code, res = hub.op("worker_approve", {"worker_id": wid}, write=True)
        if code != 200 or res.get("outcome") != "delivered":
            fail(f"approve {code} {res}")
        until("online", lambda: next(x for x in hub.op("workers_list", {"all": True})[1]["workers"]
                                    if x["id"] == wid).get("online"))
        until("session in the live list", lambda: any(s["worker_id"] == wid and s["session"] == "shell"
                                                      for s in hub.get("/api/v1/sessions")[1]["sessions"]))
        target = {"worker_id": wid, "session_name": "shell"}

        def screen_text(op="session_snapshot", **extra):
            code, res = hub.op(op, {"target": target, **extra})
            if code != 200:
                raise ValueError(f"{op} {code} {res}")
            return "\n".join(res["screen"]["lines"])

        log("4. send_text + snapshot")
        code, res = hub.op("send_text", {"target": target, "text": "echo e2e-$((6*7))", "submit": True}, write=True)
        if code != 200 or res.get("outcome") != "delivered":
            fail(f"send_text {code} {res}")
        until("command output on screen", lambda: "e2e-42" in screen_text())

        log("5. send_keys, capture, request id cache")
        rid = hh._uuid7()
        body = {"target": target, "keys": ["e", "c", "h", "o", "Space", "K", "1", "Enter"], "request_id": rid,
                "force": False}
        c1, r1 = hub.op("send_keys", body)
        c2, r2 = hub.op("send_keys", body)
        if c1 != 200 or r1 != r2:
            fail(f"send_keys {c1} {r1} / repeat {c2} {r2}")
        until("K1 on screen", lambda: screen_text("session_capture", lines=50).count("K1") == 2)  # echoed + output once

        log("6. share")
        code, res = hub.op("share_set", {"worker_id": wid, "mode": "none"})
        if code != 200 or res["rule"]["mode"] != "none":
            fail(f"share_set none {code} {res}")
        until("session hidden", lambda: not any(s["worker_id"] == wid for s in hub.get("/api/v1/sessions")[1]["sessions"]))
        code, res = hub.op("share_set", {"worker_id": wid, "mode": "all"})
        until("session shown", lambda: any(s["worker_id"] == wid for s in hub.get("/api/v1/sessions")[1]["sessions"]))

        log("7. certificate re-issue: the Worker reconnects by itself")
        drive(hub.bin, ["hub", "identity", "reissue"], hub.env(), [("Passphrase: ", hh.PASSPHRASE)])
        time.sleep(2)
        until("works after re-issue", lambda: "e2e-42" in screen_text(), timeout=90)

        log("8. master key rotation: the Worker follows the signed chain")
        drive(hub.bin, ["hub", "identity", "rotate-master"], hub.env(),
              [("Current passphrase: ", hh.PASSPHRASE), ("New passphrase: ", PASS2),
               ("Repeat the new passphrase: ", PASS2), ("Type ROTATE to confirm: ", "ROTATE")])
        time.sleep(2)
        until("works after rotation", lambda: "e2e-42" in screen_text(), timeout=90)

        log("9. Hub restart: the Worker reconnects by itself")
        hub.stop()
        hub.start(first=False)
        until("online after restart", lambda: next(x for x in hub.op("workers_list", {"all": True})[1]["workers"]
                                                  if x["id"] == wid).get("online"), timeout=120)

        log("10. remove: signed live revoke wipes the device")
        code, res = hub.op("worker_remove", {"worker_id": wid}, write=True)
        if code != 200:
            fail(f"worker_remove {code} {res}")
        try:
            rc = thin.wait(timeout=60)
        except subprocess.TimeoutExpired:
            fail("the thin Worker did not exit after the revoke")
        if rc != 3:
            fail(f"thin Worker exit code {rc}, want 3")
        if os.path.exists(os.path.join(state, "state")) or os.path.exists(os.path.join(state, "seed")):
            fail("device state not wiped after the revoke")
        thin = None
        log("PASS")
    finally:
        if thin:
            os.killpg(thin.pid, signal.SIGTERM)
        hub.stop()
        thin_log.close()
        print("--- thin Worker log (tail) ---")
        with open(os.path.join(a.work, "thin.log"), errors="replace") as f:
            print("".join(f.readlines()[-25:]))


if __name__ == "__main__":
    main()
