#!/usr/bin/env python3
"""Reference MLDojo queue plugin: a mock sidecar that runs jobs as local
subprocesses.

A queue plugin is a small HTTP server the API calls to submit and observe
jobs on some external scheduler (see docs/queue-plugins.md for the
protocol). This one needs only the Python stdlib and is what the tests and
scripts/e2e.sh use; copy it as the skeleton of a real plugin.

    python server.py --host 127.0.0.1 --port 8766 [--state-dir DIR]
"""

from __future__ import annotations

import argparse
import base64
import json
import logging
import math
import os
import re
import secrets
import shlex
import shutil
import signal
import struct
import subprocess
import tarfile
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

log = logging.getLogger("mldojo.queue_sidecar")

TERMINAL = ("succeeded", "failed", "cancelled")
MOCK_QUEUE_SECONDS = 1.0
RUN_SCRIPT = "mldojo_run.sh"
ENV_KEY_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
BUCKET_RE = re.compile(r"^[A-Za-z0-9_.-]+$")
JOB_ID_RE = re.compile(r"^[A-Za-z0-9_.-]+$")
JOB_ROUTE = re.compile(r"^/jobs/([^/]+)/(status|log|cancel|metrics|files|download)$")
ROUTE_METHOD = {"status": "GET", "log": "GET", "cancel": "POST", "metrics": "POST", "files": "POST", "download": "GET"}
STEP_KEYS = ("step", "_step")
TS_KEYS = ("ts", "timestamp", "_timestamp")
MAX_LIST_ENTRIES = 20000  # directory entries visited per /files call
MAX_LIST_DEPTH = 16  # directory levels below a glob's literal prefix
MAX_FILES_CAP = 10000


class ApiError(Exception):
    def __init__(self, status: int, message: str):
        super().__init__(message)
        self.status = status
        self.message = message


# --------------------------------------------------------------------------
# secret redaction (tokens/passwords must never reach logs or HTTP errors)

_secrets: set[str] = set()
_secrets_lock = threading.Lock()


def add_secret(value) -> None:
    if isinstance(value, str) and len(value) >= 6:
        with _secrets_lock:
            _secrets.add(value)


def redact(text) -> str:
    text = str(text)
    with _secrets_lock:
        items = sorted(_secrets, key=len, reverse=True)
    for s in items:
        text = text.replace(s, "***")
    return text


class RedactingFormatter(logging.Formatter):
    def format(self, record):
        return redact(super().format(record))


# --------------------------------------------------------------------------
# small helpers


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def to_iso(value) -> str | None:
    """Epoch seconds/ms or an ISO string -> RFC3339 string (None if invalid)."""
    if isinstance(value, bool):
        return None
    if isinstance(value, (int, float)) and math.isfinite(value) and value > 1e9:  # epoch, not a duration
        secs = value / 1000.0 if value > 1e11 else value
        try:
            return datetime.fromtimestamp(secs, timezone.utc).isoformat()
        except (OverflowError, OSError, ValueError):
            return None
    if isinstance(value, str):
        try:
            dt = datetime.fromisoformat(value.strip().replace("Z", "+00:00"))
        except ValueError:
            return None
        return (dt if dt.tzinfo else dt.replace(tzinfo=timezone.utc)).isoformat()
    return None


def safe_rel_path(path, field: str) -> str:
    if not isinstance(path, str):
        raise ApiError(400, f"{field} must be a string")
    rel = path.strip().lstrip("/")
    if not rel or ".." in rel.split("/"):
        raise ApiError(400, f"invalid {field}: {path!r}")
    return rel


def fetch_bundle(url: str, token: str, dest: str) -> None:
    """GET the code tar.gz (Bearer token) and extract it into dest."""
    os.makedirs(dest, exist_ok=True)
    if not url:
        return
    req = urllib.request.Request(url, headers={"Authorization": f"Bearer {token}"} if token else {})
    with tempfile.TemporaryFile() as tmp:
        try:
            with urllib.request.urlopen(req, timeout=300) as r:
                shutil.copyfileobj(r, tmp, 1 << 20)
        except urllib.error.HTTPError as e:
            raise ApiError(502, f"bundle download failed: HTTP {e.code}")
        except (urllib.error.URLError, OSError) as e:
            raise ApiError(502, f"bundle download failed: {e}")
        tmp.seek(0)
        try:
            with tarfile.open(fileobj=tmp, mode="r:*") as tf:
                if hasattr(tarfile, "data_filter"):
                    tf.extractall(dest, filter="data")
                else:
                    root = os.path.realpath(dest)
                    for m in tf.getmembers():
                        target = os.path.realpath(os.path.join(root, m.name))
                        outside = target != root and not target.startswith(root + os.sep)
                        bad_link = (m.issym() or m.islnk()) and (
                            os.path.isabs(m.linkname) or ".." in m.linkname.split("/"))
                        if outside or bad_link or m.isdev():
                            raise ApiError(400, f"unsafe path in bundle: {m.name}")
                    tf.extractall(root)
        except tarfile.TarError as e:
            raise ApiError(400, f"bad bundle archive: {e}")


def build_script(cmd: str, workdir: str, env: dict | None = None, prelude: list[str] | None = None) -> str:
    lines = list(prelude or [])
    lines += [f"export {k}={shlex.quote(v)}" for k, v in (env or {}).items()]
    lines.append(f"cd -- {shlex.quote(workdir)} || exit 1")
    lines.append(cmd)
    return "\n".join(lines) + "\n"


def split_buckets(value, field: str) -> list[str]:
    if not value:
        return []
    items = value.split(",") if isinstance(value, str) else value
    if not isinstance(items, list):
        raise ApiError(400, f"{field} must be a string or a list")
    out = []
    for b in items:
        b = str(b).strip()
        if not b:
            continue
        if not BUCKET_RE.match(b):
            raise ApiError(400, f"invalid bucket name in {field}: {b!r}")
        if b not in out:
            out.append(b)
    return out


def validate_submit(body) -> dict:
    if not isinstance(body, dict):
        raise ApiError(400, "body must be a JSON object")
    cmd = body.get("cmd")
    if not isinstance(cmd, str) or not cmd.strip():
        raise ApiError(400, "cmd is required")
    workdir = (body.get("workdir") or ".").strip() or "."
    if os.path.isabs(workdir) or ".." in workdir.split("/"):
        raise ApiError(400, "workdir must be relative to the code bundle")
    env = body.get("env") or {}
    if not isinstance(env, dict):
        raise ApiError(400, "env must be an object")
    for k in env:
        if not ENV_KEY_RE.match(str(k)):
            raise ApiError(400, f"invalid env var name: {k!r}")
    env = {str(k): "" if v is None else str(v) for k, v in env.items()}
    extra = body.get("extra") or {}
    if not isinstance(extra, dict):
        raise ApiError(400, "extra must be an object")
    mounts = []
    for m in body.get("mounts") or []:
        if not isinstance(m, dict) or not BUCKET_RE.match(str(m.get("bucket") or "")):
            raise ApiError(400, f"invalid mount: {m!r}")
        path = str(m.get("path") or "").strip("/")
        if ".." in path.split("/"):
            raise ApiError(400, f"invalid mount path: {path!r}")
        mounts.append({"bucket": m["bucket"], "path": path, "mount": str(m.get("mount") or "")})
    req = {
        "cmd": cmd,
        "workdir": workdir,
        "env": env,
        "extra": extra,
        "mounts": mounts,
        "run_id": str(body.get("run_id") or ""),
        "job_name": str(body.get("job_name") or ""),
        "bundle_url": body.get("bundle_url") or "",
        "bundle_token": body.get("bundle_token") or "",
        "job_password": body.get("job_password") or "",
        "input_buckets": split_buckets(body.get("input_bucket"), "input_bucket"),
        "output_buckets": split_buckets(body.get("output_bucket"), "output_bucket"),
        "dry_run": bool(body.get("dry_run")),
    }
    # zero means "unset" (Go zero values); defaults follow plan §5.3
    for key, default in (("num_workers", 1), ("gpu_per_worker", 0), ("cpu_per_worker", 4),
                         ("cpu_mem_ratio", 4), ("wall_time_min", 60)):
        try:
            req[key] = int(body.get(key) or default)
        except (TypeError, ValueError):
            raise ApiError(400, f"{key} must be an integer")
    for key in ("queue_name", "project_id", "docker_image"):
        req[key] = str(body.get(key) or "").strip()
    add_secret(req["bundle_token"])
    add_secret(req["job_password"])
    return req


# --------------------------------------------------------------------------
# metrics parsing (jsonl + tensorboard event files, stdlib only)


def parse_jsonl(text: str, default_ts: str) -> list[dict]:
    points = []
    for idx, line in enumerate(text.splitlines()):
        line = line.strip()
        if not line:
            continue
        try:
            obj = json.loads(line)
        except ValueError:
            continue  # e.g. a partially written last line
        if not isinstance(obj, dict):
            continue
        step = obj.get("step", obj.get("_step", idx))
        try:
            step = int(step)
        except (TypeError, ValueError):
            step = idx
        ts = next((t for t in (to_iso(obj[k]) for k in TS_KEYS if k in obj) if t), default_ts)
        for k, v in obj.items():
            if k in STEP_KEYS or k in TS_KEYS or isinstance(v, bool) or not isinstance(v, (int, float)):
                continue
            if math.isfinite(v):
                points.append({"step": step, "key": k, "value": float(v), "ts": ts})
    return points


def _varint(buf: bytes, i: int):
    shift = result = 0
    while True:
        b = buf[i]
        i += 1
        result |= (b & 0x7F) << shift
        if not b & 0x80:
            return result, i
        shift += 7


def _pb_fields(buf: bytes):
    """Yield (field_number, wire_type, value) of a protobuf message."""
    i, n = 0, len(buf)
    while i < n:
        key, i = _varint(buf, i)
        fn, wt = key >> 3, key & 7
        if wt == 0:
            v, i = _varint(buf, i)
        elif wt == 1:
            v, i = buf[i:i + 8], i + 8
        elif wt == 2:
            ln, i = _varint(buf, i)
            v, i = buf[i:i + ln], i + ln
        elif wt == 5:
            v, i = buf[i:i + 4], i + 4
        else:
            return
        yield fn, wt, v


def _tensor_scalar(buf: bytes):
    """Scalar value of a TensorProto of dtype DT_FLOAT/DT_DOUBLE, else None."""
    dtype, content, fvals, dvals = None, b"", b"", b""
    for fn, wt, v in _pb_fields(buf):
        if fn == 1 and wt == 0:
            dtype = v
        elif fn == 2 and wt == 2 and any(f == 2 for f, _, _ in _pb_fields(v)):
            return None  # tensor_shape has dims: not a scalar
        elif fn == 4 and wt == 2:
            content = v  # tensor_content
        elif fn == 5 and wt in (2, 5):
            fvals += v  # float_val, packed or not
        elif fn == 6 and wt in (1, 2):
            dvals += v  # double_val, packed or not
    fmt, raw = {1: ("<f", content or fvals), 2: ("<d", content or dvals)}.get(dtype, ("", b""))
    size = struct.calcsize(fmt) if fmt else 0
    return struct.unpack(fmt, raw[:size])[0] if size and len(raw) >= size else None


def parse_tfevents(path: str) -> list[dict]:
    points = []
    with open(path, "rb") as f:
        while True:
            header = f.read(12)  # uint64 length + uint32 masked crc
            if len(header) < 12:
                break
            (length,) = struct.unpack("<Q", header[:8])
            data = f.read(length)
            if len(data) < length or len(f.read(4)) < 4:
                break  # partial trailing record
            wall, step, summary = None, 0, None
            try:
                for fn, wt, v in _pb_fields(data):  # Event
                    if fn == 1 and wt == 1:
                        wall = struct.unpack("<d", v)[0]
                    elif fn == 2 and wt == 0:
                        step = v - (1 << 64) if v >= 1 << 63 else v
                    elif fn == 5 and wt == 2:
                        summary = v
                if summary is None:
                    continue
                ts = to_iso(wall) or now_iso()
                for fn, wt, v in _pb_fields(summary):  # Summary.value
                    if fn != 1 or wt != 2:
                        continue
                    tag, val = None, None
                    for vfn, vwt, vv in _pb_fields(v):  # Summary.Value
                        if vfn == 1 and vwt == 2:
                            tag = vv.decode("utf-8", "replace")
                        elif vfn == 2 and vwt == 5:
                            val = struct.unpack("<f", vv)[0]
                        elif vfn == 8 and vwt == 2:
                            val = _tensor_scalar(vv)
                    if tag and val is not None and math.isfinite(val):
                        points.append({"step": step, "key": tag, "value": float(val), "ts": ts})
            except (IndexError, struct.error):
                continue
    return points


def validate_metric_paths(paths) -> list[dict]:
    if not isinstance(paths, list):
        raise ApiError(400, "paths must be a list")
    out = []
    for p in paths:
        if not isinstance(p, dict) or p.get("type") not in ("jsonl", "tensorboard"):
            raise ApiError(400, f"invalid metrics path entry: {p!r}")
        out.append({"type": p["type"], "path": safe_rel_path(p.get("path"), "metrics path")})
    return out


def read_metrics_local(root: str, paths: list[dict]):
    """Parse metric files under root; returns (points, warnings)."""
    points, warnings = [], []
    root = os.path.realpath(root)
    for p in paths:
        full = os.path.realpath(os.path.join(root, p["path"]))
        if not full.startswith(root + os.sep) or not os.path.exists(full):
            warnings.append(f"{p['path']}: not found")
            continue
        try:
            if p["type"] == "jsonl":
                ts = datetime.fromtimestamp(os.path.getmtime(full), timezone.utc).isoformat()
                with open(full, encoding="utf-8", errors="replace") as f:
                    points += parse_jsonl(f.read(), ts)
            else:
                files = [full] if os.path.isfile(full) else sorted(
                    os.path.join(d, n) for d, _, names in os.walk(full) for n in names if "tfevents" in n)
                for fp in files:
                    points += parse_tfevents(fp)
        except OSError as e:
            warnings.append(f"{p['path']}: {e}")
    return points, warnings


# --------------------------------------------------------------------------
# artifacts: glob listing + download


def _brace_expand(pat: str) -> list[str]:
    """Expand {a,b} groups (nesting allowed) into plain glob patterns."""
    depth, start = 0, None
    for i, ch in enumerate(pat):
        if ch == "{":
            start = i if depth == 0 else start
            depth += 1
        elif ch == "}" and depth:
            depth -= 1
            if depth == 0:
                parts, cur, d = [], "", 0
                for c in pat[start + 1:i]:
                    if c == "," and d == 0:
                        parts.append(cur)
                        cur = ""
                        continue
                    d += (c == "{") - (c == "}")
                    cur += c
                parts.append(cur)
                out = [x for p in parts for x in _brace_expand(pat[:start] + p + pat[i + 1:])]
                if len(out) > 64:
                    raise ApiError(400, f"glob expands to too many patterns: {pat!r}")
                return out
    return [pat]


def _glob_regex(pat: str) -> str:
    """* and ? stay within one path segment; ** spans any number of directories."""
    out, i = "", 0
    while i < len(pat):
        if pat.startswith("**", i):
            slash = pat[i + 2:i + 3] == "/"
            out += "(?:.*/)?" if slash else ".*"
            i += 3 if slash else 2
            continue
        ch = pat[i]
        out += "[^/]*" if ch == "*" else "[^/]" if ch == "?" else re.escape(ch)
        i += 1
    return out


def _glob_root(pat: str) -> str:
    """Directory part of the literal prefix before the first wildcard."""
    m = re.search(r"[*?]", pat)
    head = pat[:m.start()] if m else pat
    return head.rsplit("/", 1)[0] if "/" in head else ""


def compile_globs(globs, strip_prefix: str = ""):
    """[{"kind", "glob"}] -> [(kind, compiled regex, {literal root dirs})]."""
    if not isinstance(globs, list) or not globs or len(globs) > 64:
        raise ApiError(400, "globs must be a non-empty list (at most 64 entries)")
    out = []
    for g in globs:
        kind = g.get("kind") if isinstance(g, dict) else None
        pat = g.get("glob") if isinstance(g, dict) else None
        if not isinstance(kind, str) or not kind.strip() or not isinstance(pat, str) or not pat.strip():
            raise ApiError(400, f"invalid glob entry: {g!r}")
        pat = pat.strip().lstrip("/")
        if strip_prefix and pat.startswith(strip_prefix):
            pat = pat[len(strip_prefix):]
        pats = _brace_expand(pat)
        if any(".." in p.split("/") for p in pats):
            raise ApiError(400, f"glob must not contain '..': {g['glob']!r}")
        regex = re.compile("|".join(f"(?:{_glob_regex(p)})" for p in pats))
        out.append((kind.strip(), regex, {_glob_root(p) for p in pats}))
    return out


def parse_max_files(value) -> int:
    try:
        n = int(2000 if value is None else value)
    except (TypeError, ValueError):
        raise ApiError(400, "max_files must be an integer")
    return max(1, min(n, MAX_FILES_CAP))


def walk_files(lister, compiled, max_files: int):
    """Bounded BFS from each glob's literal root. lister(rel_dir) -> [(name, is_dir, size)]
    and raises FileNotFoundError for a missing directory. A file goes to the first matching glob."""
    roots = sorted(set().union(*(r for _, _, r in compiled)))
    roots = [r for r in roots if not any(o != r and (o == "" or r.startswith(o + "/")) for o in roots)]
    files, warnings, entries, depth_warned = [], [], 0, False
    queue = [(r, 0) for r in roots]
    while queue:
        rel_dir, depth = queue.pop(0)
        try:
            children = lister(rel_dir)
        except FileNotFoundError:
            warnings.append(f"{rel_dir or '.'}: not found")
            continue
        for name, is_dir, size in children:
            entries += 1
            if entries > MAX_LIST_ENTRIES:
                warnings.append(f"listing stopped after {MAX_LIST_ENTRIES} entries")
                return files, warnings
            rel = f"{rel_dir}/{name}" if rel_dir else name
            if is_dir:
                if depth < MAX_LIST_DEPTH:
                    queue.append((rel, depth + 1))
                elif not depth_warned:
                    depth_warned = True
                    warnings.append(f"not descending below depth limit {MAX_LIST_DEPTH} (e.g. {rel})")
                continue
            kind = next((k for k, regex, _ in compiled if regex.fullmatch(rel)), None)
            if kind is not None:
                files.append({"kind": kind, "path": rel, "size": size})
                if len(files) >= max_files:
                    warnings.append(f"truncated at max_files={max_files}")
                    return files, warnings
    return files, warnings


def local_lister(root: str):
    root = os.path.realpath(root)

    def lister(rel_dir):
        full = os.path.join(root, rel_dir)
        if not os.path.isdir(full) or os.path.islink(full):
            raise FileNotFoundError(rel_dir)
        out = []
        with os.scandir(full) as it:
            for e in it:
                if e.is_symlink():  # only files inside root; never follow directory links
                    real = os.path.realpath(e.path)
                    if not real.startswith(root + os.sep) or not os.path.isfile(real):
                        continue
                if e.is_dir(follow_symlinks=False):
                    out.append((e.name, True, None))
                elif e.is_file():
                    out.append((e.name, False, e.stat().st_size))
        return sorted(out)

    return lister


def local_file(root: str, path) -> str:
    """Resolve a download path inside root (400 if it escapes, 404 if missing)."""
    rel = safe_rel_path(path, "path")
    root = os.path.realpath(root)
    full = os.path.realpath(os.path.join(root, rel))
    if not full.startswith(root + os.sep):
        raise ApiError(400, f"path escapes the job directory: {path!r}")
    if not os.path.exists(full):
        raise ApiError(404, f"file not found: {rel}")
    if not os.path.isfile(full):
        raise ApiError(400, f"not a file: {rel}")
    return full


class Download:
    """A readable body for /download; close() releases the stream and temp files."""

    def __init__(self, fileobj, size, cleanup=None):
        self.fileobj, self.size, self._cleanup = fileobj, size, cleanup

    def close(self):
        try:
            self.fileobj.close()
        finally:
            if self._cleanup:
                self._cleanup()


# --------------------------------------------------------------------------
# mock backend: jobs are local subprocesses


class MockJob:
    def __init__(self, job_id: str, job_dir: str):
        self.job_id = job_id
        self.code_dir = os.path.join(job_dir, "code")
        self.log_path = os.path.join(job_dir, "output.log")
        self.script = ""
        self.lock = threading.Lock()
        self.phase = "queued"
        self.message = ""
        self.proc = None
        self.started_at = None
        self.finished_at = None
        self.exit_code = None


def _killpg(pid: int, sig) -> None:
    try:
        os.killpg(pid, sig)
    except (ProcessLookupError, PermissionError):
        pass


class MockBackend:
    mock = True

    def __init__(self, state_dir: str):
        self.jobs_dir = os.path.join(state_dir, "jobs")
        self.jobs: dict[str, MockJob] = {}
        self.lock = threading.Lock()

    def health(self):
        return {"ok": True, "mock": True, "sdk": None}

    def _get(self, job_id: str) -> MockJob:
        with self.lock:
            job = self.jobs.get(job_id)
        if job is None:
            raise ApiError(404, f"job not found: {job_id}")
        return job

    def submit(self, body, creds):
        req = validate_submit(body)
        with self.lock:
            job_id = "mock-" + secrets.token_hex(4)
            while job_id in self.jobs:
                job_id = "mock-" + secrets.token_hex(4)
            job = MockJob(job_id, os.path.join(self.jobs_dir, job_id))
            self.jobs[job_id] = job
        try:
            fetch_bundle(req["bundle_url"], req["bundle_token"], job.code_dir)
        except Exception:
            with self.lock:
                self.jobs.pop(job_id, None)
            shutil.rmtree(os.path.dirname(job.code_dir), ignore_errors=True)
            raise
        open(job.log_path, "wb").close()
        job.script = build_script(req["cmd"], req["workdir"], env=req["env"])
        threading.Thread(target=self._run, args=(job,), daemon=True).start()
        log.info("mock job %s submitted (run_id=%s)", job_id, req["run_id"])
        return {"job_id": job_id, "workspace_folder": job.code_dir, "url": "", "dag_id": None,
                "raw": {"mock": True, "run_id": req["run_id"], "job_name": req["job_name"]}}

    def _run(self, job: MockJob):
        time.sleep(MOCK_QUEUE_SECONDS)
        with job.lock:
            if job.phase != "queued":
                return  # cancelled while queued
            with open(job.log_path, "ab") as logf:
                try:
                    job.proc = subprocess.Popen(
                        ["bash", "-c", job.script], cwd=job.code_dir, stdin=subprocess.DEVNULL,
                        stdout=logf, stderr=subprocess.STDOUT, start_new_session=True)
                except OSError as e:
                    job.phase, job.message, job.finished_at = "failed", f"failed to start: {e}", now_iso()
                    return
            job.phase, job.started_at = "running", now_iso()
        rc = job.proc.wait()
        with job.lock:
            job.exit_code = rc if rc >= 0 else 128 - rc  # shell convention for signals
            job.finished_at = now_iso()
            if job.phase == "running":
                job.phase = "succeeded" if rc == 0 else "failed"
                job.message = "" if rc == 0 else f"exit code {job.exit_code}"

    def status(self, job_id, creds):
        job = self._get(job_id)
        with job.lock:
            return {"job_id": job_id, "phase": job.phase, "raw_phase": job.phase, "message": job.message,
                    "started_at": job.started_at, "finished_at": job.finished_at, "exit_code": job.exit_code}

    def log(self, job_id, creds):
        job = self._get(job_id)
        with open(job.log_path, encoding="utf-8", errors="replace") as f:
            return {"job_id": job_id, "log": f.read()}

    def cancel(self, job_id, creds):
        job = self._get(job_id)
        with job.lock:
            if job.phase in TERMINAL:
                return {"ok": True}
            prev, proc = job.phase, job.proc
            job.phase, job.message = "cancelled", "cancelled by user"
            if prev == "queued":
                job.finished_at = now_iso()
                return {"ok": True}
        _killpg(proc.pid, signal.SIGTERM)
        t = threading.Timer(5, lambda: proc.poll() is None and _killpg(proc.pid, signal.SIGKILL))
        t.daemon = True
        t.start()
        return {"ok": True}

    def metrics(self, job_id, body, creds):
        job = self._get(job_id)
        points, warnings = read_metrics_local(job.code_dir, validate_metric_paths(body.get("paths") or []))
        out = {"points": points}
        if warnings:
            out["warnings"] = warnings
        return out

    def files(self, job_id, body, creds):
        job = self._get(job_id)
        files, warnings = walk_files(local_lister(job.code_dir), compile_globs(body.get("globs")),
                                     parse_max_files(body.get("max_files")))
        return {"files": files, **({"warnings": warnings} if warnings else {})}

    def download(self, job_id, bucket, path, creds):
        f = open(local_file(self._get(job_id).code_dir, path), "rb")
        return Download(f, os.fstat(f.fileno()).st_size)

    def shutdown(self):
        with self.lock:
            jobs = list(self.jobs.values())
        for job in jobs:
            if job.proc is not None and job.proc.poll() is None:
                _killpg(job.proc.pid, signal.SIGKILL)


class Handler(BaseHTTPRequestHandler):
    server_version = "mldojo-queue-sidecar/1"
    protocol_version = "HTTP/1.1"
    backend = None  # set in main()

    def do_GET(self):
        self._dispatch("GET")

    def do_POST(self):
        self._dispatch("POST")

    def log_message(self, fmt, *args):
        log.info("%s %s", self.address_string(), fmt % args)

    def _send(self, status: int, obj) -> None:
        data = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _stream(self, dl: Download) -> None:
        """Send a Download; errors after the headers can only drop the connection."""
        try:
            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            if dl.size is None:
                self.send_header("Connection", "close")
                self.close_connection = True
            else:
                self.send_header("Content-Length", str(dl.size))
            self.end_headers()
            shutil.copyfileobj(dl.fileobj, self.wfile, 1 << 20)
        except Exception as e:  # noqa: BLE001
            self.close_connection = True
            log.warning("download aborted: %s: %s", type(e).__name__, redact(e))
        finally:
            dl.close()

    def _body(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length > 0 else b""
        if not raw.strip():
            return {}
        try:
            body = json.loads(raw)
        except ValueError:
            raise ApiError(400, "body is not valid JSON")
        if not isinstance(body, dict):
            raise ApiError(400, "body must be a JSON object")
        return body

    def _credentials(self, body):
        creds = body.get("credentials")
        header = self.headers.get("X-Queue-Credentials")
        if not creds and header:
            try:
                creds = base64.b64decode(header.strip()).decode("utf-8")
            except (ValueError, UnicodeDecodeError):
                raise ApiError(400, "X-Queue-Credentials is not valid base64")
        add_secret(creds)
        return creds or None

    def _dispatch(self, method: str) -> None:
        try:
            path = urllib.parse.urlsplit(self.path).path.rstrip("/") or "/"
            body = self._body() if method == "POST" else {}
            b = self.backend
            if path == "/health" and method == "GET":
                out = b.health()
            elif path == "/jobs" and method == "POST":
                out = b.submit(body, self._credentials(body))
            else:
                m = JOB_ROUTE.match(path)
                if not m:
                    raise ApiError(404, f"no route: {method} {path}")
                job_id, action = urllib.parse.unquote(m.group(1)), m.group(2)
                if method != ROUTE_METHOD[action]:
                    raise ApiError(405, f"use {ROUTE_METHOD[action]} for /jobs/<id>/{action}")
                if not JOB_ID_RE.match(job_id):
                    raise ApiError(400, f"invalid job id: {job_id!r}")
                creds = self._credentials(body)
                if action in ("metrics", "files"):
                    out = getattr(b, action)(job_id, body, creds)
                elif action == "download":
                    q = urllib.parse.parse_qs(urllib.parse.urlsplit(self.path).query)
                    out = b.download(job_id, q.get("bucket", [""])[0], q.get("path", [""])[0], creds)
                else:
                    out = getattr(b, action)(job_id, creds)
            if isinstance(out, Download):
                self._stream(out)
            else:
                self._send(200, out)
        except ApiError as e:
            self._send(e.status, {"error": redact(e.message)})
        except Exception as e:  # noqa: BLE001
            log.exception("unhandled error on %s %s", method, self.path)
            self._send(500, {"error": redact(f"{type(e).__name__}: {e}")})


def main(argv=None):
    ap = argparse.ArgumentParser(description="MLDojo reference queue plugin (mock: jobs run as local subprocesses)")
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=8766)
    ap.add_argument("--mock", action="store_true", help="accepted for compatibility; this plugin is always a mock")
    ap.add_argument("--state-dir", default=os.path.join(tempfile.gettempdir(), "mldojo-queue-sidecar"))
    args = ap.parse_args(argv)

    handler = logging.StreamHandler()
    handler.setFormatter(RedactingFormatter("%(asctime)s %(levelname)s %(name)s: %(message)s"))
    logging.basicConfig(level=logging.INFO, handlers=[handler])

    state_dir = os.path.abspath(args.state_dir)
    os.makedirs(state_dir, exist_ok=True)
    backend = MockBackend(state_dir)
    Handler.backend = backend
    srv = ThreadingHTTPServer((args.host, args.port), Handler)
    srv.daemon_threads = True
    signal.signal(signal.SIGTERM, lambda *_: threading.Thread(target=srv.shutdown).start())
    log.info("listening on http://%s:%d state_dir=%s", args.host, srv.server_port, state_dir)
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        backend.shutdown()
        srv.server_close()


if __name__ == "__main__":
    main()
