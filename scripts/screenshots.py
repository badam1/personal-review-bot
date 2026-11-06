#!/usr/bin/env python3
"""Retake the README's screenshots (docs/screenshots/) from the demo page.

Starts `go run -tags demo ./cmd/demo` (a made-up org, no GitHub, no Claude), opens it in headless Chrome over the
DevTools protocol, and saves each page at 1600px wide, rendered at 2x and scaled down so text stays sharp.

    python3 scripts/screenshots.py              # from the repo root; needs Go and Chrome or Chromium
    CHROME=/path/to/chrome python3 scripts/screenshots.py

Standard library only. The pages are captured from the top of the document rather than with Chrome's own
--screenshot, which can come out scrolled.
"""
import base64
import json
import os
import shutil
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import time
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OUT = os.path.join(ROOT, "docs", "screenshots")
ADDR = "127.0.0.1:12398"
WIDTH = 1600

# name, how to find the page, how tall a part of it to keep (CSS pixels)
SHOTS = [
    ("reviews", "/", 1000),
    ("review", "card:Retry card captures", 1560),  # the review waiting with a finding and a design concern
    ("settings", "/settings", 1180),
]


def chrome_path():
    if os.environ.get("CHROME"):
        return os.environ["CHROME"]
    mac = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
    if os.path.exists(mac):
        return mac
    for name in ("google-chrome", "google-chrome-stable", "chromium", "chromium-browser"):
        if shutil.which(name):
            return shutil.which(name)
    sys.exit("no Chrome or Chromium found: set CHROME to its path")


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def wait_for(url, what, seconds=120):
    deadline = time.time() + seconds
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=2) as r:
                return r.read()
        except OSError:
            time.sleep(0.5)
    sys.exit(f"{what} didn't come up at {url}")


class DevTools:
    """Just enough of a WebSocket client to send DevTools commands and read their answers."""

    def __init__(self, ws_url):
        host, path = ws_url[len("ws://"):].split("/", 1)
        name, port = host.rsplit(":", 1)
        self.sock = socket.create_connection((name, int(port)))
        key = base64.b64encode(os.urandom(16)).decode()
        self.sock.sendall((f"GET /{path} HTTP/1.1\r\nHost: {host}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
                           f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n").encode())
        buf = b""
        while b"\r\n\r\n" not in buf:
            buf += self.sock.recv(4096)
        self.buf = buf.split(b"\r\n\r\n", 1)[1]
        self.next_id = 0

    def _read(self, n):
        while len(self.buf) < n:
            chunk = self.sock.recv(1 << 20)
            if not chunk:
                raise EOFError("Chrome closed the connection")
            self.buf += chunk
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def _message(self):
        data = b""
        while True:
            head, size = self._read(2)
            size &= 0x7F
            if size == 126:
                size = struct.unpack(">H", self._read(2))[0]
            elif size == 127:
                size = struct.unpack(">Q", self._read(8))[0]
            data += self._read(size)
            if head & 0x80:
                return json.loads(data)

    def call(self, method, **params):
        self.next_id += 1
        payload = json.dumps({"id": self.next_id, "method": method, "params": params}).encode()
        mask = os.urandom(4)
        n = len(payload)
        head = bytes([0x81]) + (bytes([0x80 | n]) if n < 126 else
                                bytes([0x80 | 126]) + struct.pack(">H", n) if n < 65536 else
                                bytes([0x80 | 127]) + struct.pack(">Q", n))
        self.sock.sendall(head + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(payload)))
        while True:
            msg = self._message()
            if msg.get("id") == self.next_id:
                if "error" in msg:
                    raise RuntimeError(f"{method}: {msg['error']}")
                return msg.get("result", {})

    def eval(self, expression):
        return self.call("Runtime.evaluate", expression=expression, returnByValue=True).get("result", {}).get("value")


def main():
    os.makedirs(OUT, exist_ok=True)
    version = subprocess.run(["git", "describe", "--tags", "--abbrev=0"], cwd=ROOT, capture_output=True,
                             text=True).stdout.strip() or "dev"
    profile = tempfile.mkdtemp(prefix="prb-shots-")
    demo = subprocess.Popen(["go", "run", "-tags", "demo", "./cmd/demo", "-addr", ADDR, "-version", version],
                            cwd=ROOT, start_new_session=True)
    chrome = None
    try:
        wait_for(f"http://{ADDR}/", "the demo page")
        port = free_port()
        chrome = subprocess.Popen([chrome_path(), "--headless=new", "--disable-gpu", "--hide-scrollbars",
                                   "--force-prefers-reduced-motion", f"--remote-debugging-port={port}",
                                   f"--user-data-dir={profile}", "about:blank"],
                                  stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True)
        wait_for(f"http://127.0.0.1:{port}/json/version", "Chrome")
        tabs = json.loads(urllib.request.urlopen(f"http://127.0.0.1:{port}/json").read())
        tab = DevTools(next(t for t in tabs if t["type"] == "page")["webSocketDebuggerUrl"])
        tab.call("Page.enable")
        tab.call("Emulation.setDeviceMetricsOverride", width=WIDTH, height=1000, deviceScaleFactor=2, mobile=False)

        for name, where, height in SHOTS:
            path = where
            if where.startswith("card:"):
                tab.call("Page.navigate", url=f"http://{ADDR}/")
                time.sleep(1.5)
                title = json.dumps(where[len("card:"):])
                path = tab.eval(f'[...document.querySelectorAll(".card")].find(c => c.textContent.includes({title}))'
                                '?.querySelector(\'a[href^="/r/"]\')?.getAttribute("href")')
                if not path:
                    sys.exit(f"no card for {where} on the demo's Reviews page")
            tab.call("Page.navigate", url=f"http://{ADDR}{path}")
            time.sleep(1.5)
            tab.eval("window.scrollTo(0, 0)")
            time.sleep(0.5)
            shot = tab.call("Page.captureScreenshot", format="png", captureBeyondViewport=True,
                            clip={"x": 0, "y": 0, "width": WIDTH, "height": height, "scale": 0.5})
            with open(os.path.join(OUT, name + ".png"), "wb") as f:
                f.write(base64.b64decode(shot["data"]))
            print(f"docs/screenshots/{name}.png  ({path})")
    finally:
        for proc in (chrome, demo):
            if proc:
                os.killpg(proc.pid, signal.SIGTERM)
                proc.wait()
        shutil.rmtree(profile, ignore_errors=True)


if __name__ == "__main__":
    main()
