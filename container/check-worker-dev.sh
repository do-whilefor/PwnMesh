#!/bin/sh
set -eu

test "$PWNMESH_WORKER_ENVIRONMENT" = debian-dev
for tool in bash curl rg git python python3 pip pwnmesh; do
    command -v "$tool" >/dev/null
done
pwnmesh worker --help >/dev/null 2>&1

# Exercise the installed tools together, including local HTTP, without an
# external service or a model credential. Temporary files are always removed.
python - <<'PY'
import http.server
import json
import os
from pathlib import Path
import sqlite3
import ssl
import subprocess
import sys
import tempfile
import threading

assert sys.prefix == "/opt/pwnmesh-venv", sys.prefix
assert ssl.create_default_context().cert_store_stats()["x509_ca"] > 0
assert not any(key.startswith("ANTHROPIC_") and value for key, value in os.environ.items())
subprocess.run([sys.executable, "-m", "pip", "--version"], check=True, stdout=subprocess.DEVNULL)

with tempfile.TemporaryDirectory(prefix="pwnmesh-dev-check-", dir="/tmp") as directory:
    root = Path(directory)
    proof = root / "proof.json"
    proof.write_text(json.dumps({"marker": "PWNMESH_DEV_WORKER_OK"}), encoding="utf-8")
    subprocess.run(["bash", "-c", 'test -s "$1"', "check", str(proof)], check=True)
    subprocess.run(["rg", "--quiet", "PWNMESH_DEV_WORKER_OK", str(proof)], check=True)
    subprocess.run(["git", "init", "--quiet", directory], check=True)
    subprocess.run(["git", "-C", directory, "add", "proof.json"], check=True)
    subprocess.run(["git", "-C", directory, "-c", "user.name=PwnMesh Test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "Synthetic worker proof"], check=True)
    assert json.loads(subprocess.check_output(["git", "-C", directory, "show", "HEAD:proof.json"]))["marker"] == "PWNMESH_DEV_WORKER_OK"
    with sqlite3.connect(root / "sample.db") as database:
        database.execute("CREATE TABLE proof(value TEXT)")
        database.execute("INSERT INTO proof VALUES (?)", ("verified",))
        assert database.execute("SELECT value FROM proof").fetchone() == ("verified",)

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            body = proof.read_bytes()
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *args):
            pass

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        response = subprocess.check_output(["curl", "--noproxy", "*", "--fail", "--silent", "--show-error", "--max-time", "5", f"http://127.0.0.1:{server.server_port}/proof"])
        assert json.loads(response)["marker"] == "PWNMESH_DEV_WORKER_OK"
    finally:
        server.shutdown()
        server.server_close()
        thread.join()

print("PwnMesh development worker: Python, TLS, Git, shell, search, SQLite and local HTTP verified")
PY
