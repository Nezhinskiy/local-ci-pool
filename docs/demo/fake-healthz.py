"""Serves healthz.json at /healthz on a loopback port: a stand-in for a running pool's health endpoint.

usage: python3 -I fake-healthz.py PORT FILE
"""
import http.server
import sys

port, path = int(sys.argv[1]), sys.argv[2]
body = open(path, "rb").read()


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/healthz":
            self.send_error(404)
            return
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


http.server.HTTPServer(("127.0.0.1", port), Handler).serve_forever()
