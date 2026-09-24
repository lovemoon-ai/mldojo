#!/usr/bin/env python3
"""Self-contained mock-mode test for server.py (stdlib only).

    python3 test_server.py

Starts the sidecar with --mock on a free port, serves a tiny code bundle over
a local http.server (Bearer-token protected), and exercises submit / status /
log / metrics / cancel plus the error paths.
"""

import base64
import io
import json
import os
import shlex
import signal
import socket
import struct
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
import unittest
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
import server  # noqa: E402  (glob helpers)

BUNDLE_TOKEN = "bundle-secret-token"
PY = shlex.quote(sys.executable)
CKPT = bytes(range(256)) * 40
VIDEO = b"\x00\x00\x00\x18ftypmp42" + bytes(5000)

TRAIN_PY = """\
import json, os, sys, time
print("hello from mldojo mock job", flush=True)
print("GREETING=" + os.environ.get("GREETING", ""), flush=True)
with open("metrics.jsonl", "w") as f:
    for step in range(3):
        f.write(json.dumps({"step": step, "loss": 1.0 / (step + 1), "acc": step * 0.1, "note": "x"}) + "\\n")
        f.flush()
        print("step", step, flush=True)
        time.sleep(0.4)
os.makedirs("outputs", exist_ok=True)
open("outputs/model_step10.ckpt", "wb").write(bytes(range(256)) * 40)
open("outputs/video.mp4", "wb").write(b"\\x00\\x00\\x00\\x18ftypmp42" + bytes(5000))
print("to stderr", file=sys.stderr, flush=True)
print("done", flush=True)
"""


# -- minimal tfevents writer (protobuf by hand; the parser ignores CRCs)

def _varint(n):
    out = bytearray()
    while True:
        b, n = n & 0x7F, n >> 7
        out.append(b | (0x80 if n else 0))
        if not n:
            return bytes(out)


def _field(fn, wt, payload):
    key = _varint(fn << 3 | wt)
    if wt == 0:
        return key + _varint(payload)
    if wt == 2:
        return key + _varint(len(payload)) + payload
    return key + payload


def _record(data):
    return struct.pack("<Q", len(data)) + b"\0" * 4 + data + b"\0" * 4


def _event(step, wall, values=None, file_version=None):
    msg = _field(1, 1, struct.pack("<d", wall)) + _field(2, 0, step)
    if file_version:
        msg += _field(3, 2, file_version)
    if values:
        msg += _field(5, 2, b"".join(_field(1, 2, v) for v in values))
    return _record(msg)


def tfevents_bytes():
    wall = 1_700_000_000.0
    simple = _field(1, 2, b"train/loss") + _field(2, 5, struct.pack("<f", 0.5))
    scalar_tensor = _field(1, 2, b"lr") + _field(8, 2, _field(1, 0, 1) + _field(2, 2, b"")
                                                  + _field(4, 2, struct.pack("<f", 0.25)))
    vector_tensor = _field(1, 2, b"vec") + _field(8, 2, _field(1, 0, 1) + _field(2, 2, _field(2, 2, _field(1, 0, 2)))
                                                   + _field(4, 2, struct.pack("<ff", 1, 2)))
    string_tensor = _field(1, 2, b"text") + _field(8, 2, _field(1, 0, 7) + _field(8, 2, b"hello"))
    return (_event(0, wall, file_version=b"brain.Event:2")
            + _event(1, wall + 1, [simple])
            + _event(2, wall + 2, [scalar_tensor, vector_tensor, string_tensor])
            + b"\x10\x00")  # truncated trailing record must be ignored


def make_bundle():
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as tf:
        for name, data in (("proj/train.py", TRAIN_PY.encode()),
                           ("proj/tb/events.out.tfevents.1700000000.host", tfevents_bytes())):
            info = tarfile.TarInfo(name)
            info.size = len(data)
            info.mode = 0o644
            tf.addfile(info, io.BytesIO(data))
    return buf.getvalue()


def free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def start_sidecar(state_dir, extra_args=(), env=None):
    port = free_port()
    proc = subprocess.Popen([sys.executable, os.path.join(HERE, "server.py"), "--port", str(port),
                             "--state-dir", state_dir, *extra_args],
                            env={**os.environ, **(env or {})}, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    base = f"http://127.0.0.1:{port}"
    for _ in range(100):
        try:
            with urllib.request.urlopen(base + "/health", timeout=1):
                return proc, base
        except OSError:
            time.sleep(0.1)
    proc.kill()
    raise RuntimeError("sidecar did not start")


class MockSidecarTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.TemporaryDirectory(prefix="mldojo-sidecar-test-")
        bundle = make_bundle()

        class BundleHandler(BaseHTTPRequestHandler):
            def do_GET(self):
                if self.headers.get("Authorization") != f"Bearer {BUNDLE_TOKEN}":
                    self.send_response(401)
                    self.send_header("Content-Length", "0")
                    self.end_headers()
                    return
                self.send_response(200)
                self.send_header("Content-Type", "application/gzip")
                self.send_header("Content-Length", str(len(bundle)))
                self.end_headers()
                self.wfile.write(bundle)

            def log_message(self, *args):
                pass

        cls.bundle_srv = ThreadingHTTPServer(("127.0.0.1", 0), BundleHandler)
        threading.Thread(target=cls.bundle_srv.serve_forever, daemon=True).start()
        cls.bundle_url = f"http://127.0.0.1:{cls.bundle_srv.server_port}/bundle.tar.gz"
        cls.proc, cls.base = start_sidecar(os.path.join(cls.tmp.name, "state"), ["--mock"])

    @classmethod
    def tearDownClass(cls):
        cls.proc.send_signal(signal.SIGTERM)
        try:
            cls.proc.wait(10)
        except subprocess.TimeoutExpired:
            cls.proc.kill()
        cls.bundle_srv.shutdown()
        cls.bundle_srv.server_close()
        cls.tmp.cleanup()

    # -- helpers

    def call(self, method, path, body=None, headers=None, base=None):
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request((base or self.base) + path, data=data, method=method,
                                     headers={"Content-Type": "application/json", **(headers or {})})
        try:
            with urllib.request.urlopen(req, timeout=30) as r:
                return r.status, json.loads(r.read())
        except urllib.error.HTTPError as e:
            with e:
                return e.code, json.loads(e.read())

    def raw_get(self, path):
        try:
            with urllib.request.urlopen(self.base + path, timeout=30) as r:
                return r.status, r.headers, r.read()
        except urllib.error.HTTPError as e:
            with e:
                return e.code, e.headers, e.read()

    def submit(self, cmd, **kw):
        body = {"run_id": "run-1", "job_name": "test", "queue_name": "q", "project_id": "p",
                "docker_image": "img", "num_workers": 1, "gpu_per_worker": 0, "cmd": cmd, "workdir": "proj",
                "bundle_url": self.bundle_url, "bundle_token": BUNDLE_TOKEN, "env": {}, "mounts": [],
                "job_password": "pw", **kw}
        code, out = self.call("POST", "/jobs", body)
        self.assertEqual(code, 200, out)
        self.assertRegex(out["job_id"], r"^mock-[0-9a-f]{8}$")
        self.assertIsNone(out["dag_id"])
        self.workspace = out["workspace_folder"]
        return out["job_id"]

    def wait_phase(self, job_id, phases, timeout=30):
        seen = []
        deadline = time.time() + timeout
        while time.time() < deadline:
            code, st = self.call("GET", f"/jobs/{job_id}/status")
            self.assertEqual(code, 200, st)
            if not seen or seen[-1] != st["phase"]:
                seen.append(st["phase"])
            if st["phase"] in phases:
                return st, seen
            time.sleep(0.1)
        self.fail(f"job {job_id} never reached {phases}; seen {seen}")

    # -- tests

    def test_health(self):
        code, out = self.call("GET", "/health")
        self.assertEqual((code, out), (200, {"ok": True, "mock": True, "sdk": None}))

    def test_job_lifecycle_log_and_metrics(self):
        job_id = self.submit(f"{PY} train.py", env={"GREETING": "hi 'there' $HOME"})
        st, seen = self.wait_phase(job_id, ("succeeded", "failed", "cancelled"))
        self.assertEqual(seen[0], "queued")
        self.assertIn("running", seen)
        self.assertEqual((st["phase"], st["exit_code"]), ("succeeded", 0), st)
        for key in ("started_at", "finished_at"):
            self.assertIsNotNone(datetime.fromisoformat(st[key]).tzinfo)

        code, out = self.call("GET", f"/jobs/{job_id}/log")
        self.assertEqual(code, 200)
        for line in ("hello from mldojo mock job", "GREETING=hi 'there' $HOME", "step 2", "to stderr", "done"):
            self.assertIn(line, out["log"])

        code, out = self.call("POST", f"/jobs/{job_id}/metrics", {"bucket": "ignored", "tracking": False, "paths": [
            {"type": "jsonl", "path": "proj/metrics.jsonl"},
            {"type": "tensorboard", "path": "proj/tb"},
            {"type": "jsonl", "path": "proj/missing.jsonl"}]})
        self.assertEqual(code, 200, out)
        got = {(p["key"], p["step"]): p["value"] for p in out["points"]}
        self.assertEqual(len(out["points"]), 8, out["points"])
        self.assertAlmostEqual(got[("loss", 2)], 1 / 3)
        self.assertAlmostEqual(got[("acc", 1)], 0.1)
        self.assertEqual(got[("train/loss", 1)], 0.5)
        self.assertEqual(got[("lr", 2)], 0.25)
        self.assertNotIn(("note", 0), got)
        for p in out["points"]:
            self.assertIsNotNone(datetime.fromisoformat(p["ts"]).tzinfo)
        self.assertEqual(out["warnings"], ["proj/missing.jsonl: not found"])

    def test_files_and_download(self):
        job_id = self.submit(f"{PY} train.py")
        st, _ = self.wait_phase(job_id, ("succeeded", "failed", "cancelled"))
        self.assertEqual(st["phase"], "succeeded")
        globs = [{"kind": "ckpt", "glob": "proj/outputs/**/*.ckpt"},
                 {"kind": "video", "glob": "proj/outputs/**/*.{mp4,webm}"},
                 {"kind": "other", "glob": "proj/outputs/**"}]  # first match wins
        code, out = self.call("POST", f"/jobs/{job_id}/files", {"bucket": "", "globs": globs, "max_files": 100})
        self.assertEqual(code, 200, out)
        self.assertEqual({f["path"]: (f["kind"], f["size"]) for f in out["files"]},
                         {"proj/outputs/model_step10.ckpt": ("ckpt", len(CKPT)),
                          "proj/outputs/video.mp4": ("video", len(VIDEO))})
        self.assertNotIn("warnings", out)

        for path, data in (("proj/outputs/model_step10.ckpt", CKPT), ("proj/outputs/video.mp4", VIDEO)):
            status, headers, body = self.raw_get(f"/jobs/{job_id}/download?bucket=&path={urllib.parse.quote(path)}")
            self.assertEqual(status, 200)
            self.assertEqual(headers["Content-Type"], "application/octet-stream")
            self.assertEqual(int(headers["Content-Length"]), len(data))
            self.assertEqual(body, data)

        status, _, body = self.raw_get(f"/jobs/{job_id}/download?path=proj/outputs/missing.ckpt")
        self.assertEqual(status, 404)
        self.assertIn("error", json.loads(body))
        for bad in ("../x", "proj/../../x", "proj/outputs", ""):
            status, _, body = self.raw_get(f"/jobs/{job_id}/download?path={urllib.parse.quote(bad)}")
            self.assertEqual(status, 400, (bad, body))

        code, out = self.call("POST", f"/jobs/{job_id}/files", {"globs": globs, "max_files": 1})
        self.assertEqual((code, len(out["files"]), out["warnings"]), (200, 1, ["truncated at max_files=1"]))
        code, out = self.call("POST", f"/jobs/{job_id}/files", {"globs": [{"kind": "x", "glob": "nope/*.ckpt"}]})
        self.assertEqual((code, out), (200, {"files": [], "warnings": ["nope: not found"]}))
        for bad in ([], [{"kind": "x"}], [{"kind": "x", "glob": "../*"}]):
            self.assertEqual(self.call("POST", f"/jobs/{job_id}/files", {"globs": bad})[0], 400, bad)
        self.assertEqual(self.call("POST", "/jobs/mock-00000000/files", {"globs": globs})[0], 404)

    def test_failed_job(self):
        job_id = self.submit("echo boom; exit 3", workdir="")
        st, _ = self.wait_phase(job_id, ("succeeded", "failed"))
        self.assertEqual((st["phase"], st["exit_code"]), ("failed", 3))
        self.assertIn("boom", self.call("GET", f"/jobs/{job_id}/log")[1]["log"])

    def test_cancel_running(self):
        job_id = self.submit("echo $$ > pid.txt; sleep 60")  # $$ = job bash = process group leader
        self.wait_phase(job_id, ("running",))
        pid_file = os.path.join(self.workspace, "proj", "pid.txt")
        for _ in range(50):
            if os.path.exists(pid_file) and os.path.getsize(pid_file):
                break
            time.sleep(0.1)
        with open(pid_file) as f:
            pgid = int(f.read())
        code, out = self.call("POST", f"/jobs/{job_id}/cancel", {})
        self.assertEqual((code, out), (200, {"ok": True}))
        st, _ = self.wait_phase(job_id, ("cancelled",), timeout=5)
        self.assertEqual(st["message"], "cancelled by user")
        for _ in range(50):
            try:
                os.killpg(pgid, 0)
            except ProcessLookupError:
                break
            time.sleep(0.1)
        else:
            self.fail("job process group still alive after cancel")
        self.assertEqual(self.call("POST", f"/jobs/{job_id}/cancel", {})[0], 200)  # idempotent

    def test_cancel_queued(self):
        job_id = self.submit("sleep 60")
        self.assertEqual(self.call("POST", f"/jobs/{job_id}/cancel", {})[0], 200)
        time.sleep(1.5)
        code, st = self.call("GET", f"/jobs/{job_id}/status")
        self.assertEqual((st["phase"], st["started_at"]), ("cancelled", None))

    def test_credentials_header_and_errors(self):
        creds = {"X-Queue-Credentials": base64.b64encode(b"token: abcdef123\n").decode()}
        job_id = self.submit("true")
        self.assertEqual(self.call("GET", f"/jobs/{job_id}/status", headers=creds)[0], 200)
        code, out = self.call("GET", "/jobs/mock-00000000/status")
        self.assertEqual(code, 404)
        self.assertIn("error", out)
        self.assertEqual(self.call("POST", "/jobs", {"workdir": "."})[0], 400)
        self.assertEqual(self.call("POST", "/jobs", {"cmd": "true", "workdir": "../x"})[0], 400)
        self.assertEqual(self.call("POST", "/jobs", {"cmd": "true", "env": {"BAD-NAME": "1"}})[0], 400)
        code, out = self.call("POST", "/jobs", {"cmd": "true", "bundle_url": self.bundle_url, "bundle_token": "wrong"})
        self.assertEqual(code, 502, out)
        self.assertEqual(self.call("GET", f"/jobs/{job_id}/cancel")[0], 405)
        self.assertEqual(self.call("POST", f"/jobs/{job_id}/metrics", {"paths": [{"type": "csv", "path": "x"}]})[0], 400)
        self.assertEqual(self.call("GET", "/nope")[0], 404)


class GlobTest(unittest.TestCase):
    def check(self, glob, yes, no, roots):
        [(_, regex, got_roots)] = server.compile_globs([{"kind": "k", "glob": glob}])
        self.assertEqual(got_roots, roots)
        for p in yes:
            self.assertTrue(regex.fullmatch(p), (glob, p))
        for p in no:
            self.assertFalse(regex.fullmatch(p), (glob, p))

    def test_globs(self):
        self.check("outputs/**/*.ckpt", ["outputs/a.ckpt", "outputs/x/y/a.ckpt"],
                   ["outputsx/a.ckpt", "outputs/a.ckpt.tmp", "a/outputs/a.ckpt"], {"outputs"})
        self.check("{a,b/c}/*.{mp4,webm}", ["a/v.mp4", "b/c/v.webm"], ["c/v.mp4", "a/x/v.mp4"], {"a", "b/c"})
        self.check("run?/log.txt", ["run1/log.txt"], ["run10/log.txt", "run/log.txt"], {""})
        self.check("/out/**", ["out/a", "out/a/b"], ["outx/a"], {"out"})
        self.check("**", ["a", "a/b/c"], [], {""})
        self.check("x/model.ckpt", ["x/model.ckpt"], ["x/modelXckpt"], {"x"})


if __name__ == "__main__":
    unittest.main(verbosity=2)
