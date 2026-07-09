#!/usr/bin/env python3
"""Loopback stand-in for openbox-core's /api/v1/governance/evaluate.

Accepts the AIP-signed POST the shift-left client sends, records the headers +
JSON body (one line per event) to a log file, and returns 200. It does NOT
verify the signature or accept-list the event_type — it is a capture sink so we
can OBSERVE exactly what shift-left emits, standing in for core (which is EXT and
would 400 the developer event types today).
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

LOG = sys.argv[1] if len(sys.argv) > 1 else "events.log"
PORT = int(sys.argv[2]) if len(sys.argv) > 2 else 8787


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):  # silence default stderr access log
        pass

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(n)
        rec = {
            "path": self.path,
            "sig_headers": {
                k: self.headers.get(k)
                for k in (
                    "Authorization", "X-OpenBox-Agent-DID", "X-OpenBox-Agent-Signature",
                    "X-OpenBox-Agent-Timestamp", "X-OpenBox-Body-SHA256", "X-OpenBox-SDK-Version",
                )
            },
        }
        try:
            rec["event"] = json.loads(body)
        except Exception:
            rec["event_raw"] = body.decode("utf-8", "replace")
        with open(LOG, "a") as f:
            f.write(json.dumps(rec) + "\n")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(b'{"decision":"allow","verdict":"observe"}')


if __name__ == "__main__":
    open(LOG, "w").close()  # truncate
    HTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
