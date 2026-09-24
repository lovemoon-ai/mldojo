"""MLDojo Python SDK.

    import mldojo
    mldojo.init(project="demo", config={"lr": 1e-3})   # optional
    mldojo.log({"loss": 0.31}, step=1200)

Inside an MLDojo run the agent sets MLDOJO_METRICS_FILE and the SDK appends
JSON lines to it (the agent ships them). Elsewhere (e.g. queue-plugin jobs that can
reach the API) it POSTs to $MLDOJO_API_URL with $MLDOJO_RUN_TOKEN.

init() covers the third case: a script nobody submitted. It creates a run
over the API and logging works from there, so the same file runs both under
`mldojo run submit` and by hand. Without it a run could only exist if the
platform created it, which made "just start logging" impossible.

Without a run and without init(), every call is a cheap no-op. Pure stdlib.
"""
import json
import math
import os
import threading
import time
import urllib.request

__all__ = ["init", "log", "log_episode", "flush", "finish", "run_id", "run_url", "enabled", "config", "summary"]
__version__ = "0.2.0"

_lock = threading.Lock()
_buffer = []
_step = 0
_last_flush = 0.0
_started_here = False  # this process created the run, so it should finish it

#: Hyperparameters. Assign before/at init(), or pass config= to init().
config = {}
#: Final values worth putting on a leaderboard; uploaded at finish().
summary = {}


def run_id():
    return os.environ.get("MLDOJO_RUN_ID")


def run_url():
    """Web URL of the current run, if the API base is known."""
    api, rid = os.environ.get("MLDOJO_API_URL"), run_id()
    return f"{api.rstrip('/')}/run?id={rid}" if api and rid else None


def enabled():
    return bool(os.environ.get("MLDOJO_METRICS_FILE") or (os.environ.get("MLDOJO_API_URL") and run_id()))


def _server():
    """API base URL: the environment first, then ~/.mldojo/config.yaml."""
    if os.environ.get("MLDOJO_API_URL"):
        return os.environ["MLDOJO_API_URL"].rstrip("/")
    return _config_file().get("server", "").rstrip("/")


def _api_token():
    if os.environ.get("MLDOJO_TOKEN"):
        return os.environ["MLDOJO_TOKEN"]
    return _config_file().get("token", "")


def _config_file():
    """Minimal reader for the two top-level scalars the SDK needs.

    Deliberately not a YAML parser: the SDK is pure stdlib so that importing
    it can never fail on a missing dependency in someone's training image.
    """
    path = os.path.join(os.environ.get("MLDOJO_HOME") or os.path.expanduser("~/.mldojo"), "config.yaml")
    out = {}
    try:
        with open(path) as f:
            for line in f:
                if line[:1].isspace() or ":" not in line:
                    continue  # nested keys belong to a section, not to us
                k, _, v = line.partition(":")
                out[k.strip()] = v.strip().strip("\'\"")
    except OSError:
        pass
    return out


def _request(method, path, body=None, token=None, timeout=15):
    url = f"{_server()}{path}"
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        raw = resp.read()
    return json.loads(raw) if raw else {}


def init(project=None, experiment=None, name=None, config=None, notes=None):
    """Start reporting, creating a run if this process is not inside one.

    Inside an MLDojo run this only uploads the config, so the same script
    works submitted or run by hand. Returns the run id, or None when there is
    no server to talk to (the no-op case).
    """
    global _started_here
    cfg = dict(globals()["config"])
    cfg.update(config or {})
    globals()["config"] = cfg

    if run_id():  # already inside a run: just attach the config
        _put_config(cfg)
        return run_id()
    if not _server():
        return None  # no server configured: stay a no-op rather than failing
    try:
        res = _request(
            "POST",
            "/api/v1/runs/external",
            {
                "project": project or os.environ.get("MLDOJO_PROJECT") or "external",
                "experiment": experiment or os.environ.get("MLDOJO_EXPERIMENT") or "default",
                "name": name or "",
                "host": _hostname(),
                "config": cfg,
                "notes": notes or "",
            },
            token=_api_token(),
        )
    except Exception as e:  # never break training
        print(f"[mldojo] init failed, continuing without tracking: {e}", flush=True)
        return None
    run = res.get("run") or {}
    os.environ["MLDOJO_RUN_ID"] = run.get("id", "")
    os.environ["MLDOJO_RUN_TOKEN"] = res.get("token", "")
    os.environ.setdefault("MLDOJO_API_URL", _server())
    _started_here = True
    url = run_url()
    print(f"[mldojo] tracking run {run.get('id', '')[:8]}" + (f" at {url}" if url else ""), flush=True)
    return run.get("id")


def _hostname():
    import re
    import socket

    return re.sub(r"[^A-Za-z0-9._-]", "-", socket.gethostname() or "unknown")[:64] or "unknown"


def _put_config(cfg):
    if not cfg or not _server() or not run_id():
        return
    try:
        _request("POST", f"/api/v1/runs/{run_id()}/config", cfg,
                 token=os.environ.get("MLDOJO_RUN_TOKEN") or _api_token())
    except Exception as e:
        print(f"[mldojo] config upload failed: {e}", flush=True)


def finish(status="succeeded", exit_code=None):
    """Flush and close a run created by init(). Called automatically at exit."""
    global _started_here
    flush()
    # Upload config again so anything assigned after init() is recorded, plus
    # the summary values a leaderboard sorts on.
    final = dict(config)
    final.update({f"summary/{k}": v for k, v in summary.items()})
    if final:
        _put_config(final)
    if not _started_here or not run_id():
        return
    _started_here = False
    try:
        _request("POST", f"/api/v1/runs/{run_id()}/finish",
                 {"status": status, "exit_code": exit_code},
                 token=os.environ.get("MLDOJO_RUN_TOKEN") or _api_token())
    except Exception as e:
        print(f"[mldojo] finish failed: {e}", flush=True)


def _num(v):
    try:
        f = float(v)
    except (TypeError, ValueError):
        # torch/numpy scalars
        try:
            f = float(v.item())
        except Exception:
            return None
    if math.isnan(f):
        return "NaN"
    if math.isinf(f):
        return "Infinity" if f > 0 else "-Infinity"
    return f


def _flatten(prefix, d, out):
    for k, v in d.items():
        key = f"{prefix}/{k}" if prefix else str(k)
        if isinstance(v, dict):
            _flatten(key, v, out)
        else:
            n = _num(v)
            if n is not None:
                out[key] = n


def log(data, step=None, commit=True):
    """Record numeric values. Non-numeric values are ignored."""
    global _step
    if not enabled():
        return
    flat = {}
    _flatten("", data, flat)
    if not flat:
        return
    with _lock:
        if step is None:
            step = _step
            if commit:
                _step += 1
        else:
            _step = max(_step, int(step) + 1)
        rec = {"step": int(step), "_timestamp": time.time()}
        rec.update(flat)
        path = os.environ.get("MLDOJO_METRICS_FILE")
        if path:
            with open(path, "a") as f:
                f.write(json.dumps(rec) + "\n")
            return
        _buffer.append(rec)
    if time.time() - _last_flush > 5:
        flush()


_episode = 0


def log_episode(success, index=None, seed=None, steps=None, duration_s=None, video=None, **extra):
    """Record one evaluation trial.

    A policy's result is not a curve, it is a rate over trials, and the rate
    alone is not enough: re-running the same checkpoint on the same seeds can
    score 6/20 and then 5/20, and the useful question is which trials
    flipped. So each trial is recorded with the seed that produced it and,
    where there is one, the video to watch when it fails.
    """
    global _episode
    if not enabled():
        return
    with _lock:
        if index is None:
            index = _episode
        _episode = max(_episode, int(index) + 1)
        rec = {"episode": int(index), "success": bool(success)}
        if seed is not None:
            rec["seed"] = int(seed)
        if steps is not None:
            rec["steps"] = int(steps)
        if duration_s is not None:
            rec["duration_s"] = float(duration_s)
        if video is not None:
            rec["video"] = str(video)
        for k, v in extra.items():
            rec.setdefault(k, v)
        path = os.environ.get("MLDOJO_EPISODES_FILE")
        if not path:
            return
        with open(path, "a") as f:
            f.write(json.dumps(rec) + "\n")


def flush():
    """Send buffered points to the API (HTTP mode only)."""
    global _last_flush
    with _lock:
        recs = list(_buffer)
        _buffer.clear()
        _last_flush = time.time()
    if not recs:
        return
    api, rid, tok = os.environ.get("MLDOJO_API_URL"), run_id(), os.environ.get("MLDOJO_RUN_TOKEN", "")
    pts = []
    for r in recs:
        ts = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(r.pop("_timestamp")))
        step = r.pop("step")
        pts += [{"step": step, "key": k, "value": v, "ts": ts} for k, v in r.items()]
    req = urllib.request.Request(
        f"{api.rstrip('/')}/api/v1/runs/{rid}/metrics",
        data=json.dumps({"points": pts}).encode(),
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {tok}"},
        method="POST",
    )
    try:
        urllib.request.urlopen(req, timeout=10).read()
    except Exception as e:  # never break training
        print(f"[mldojo] metrics upload failed: {e}", flush=True)


import atexit  # noqa: E402


def _at_exit():
    # An unhandled exception leaves sys.exc_info set, so a crashed script is
    # recorded as failed rather than silently succeeding.
    import sys

    failed = sys.exc_info()[0] is not None
    finish("failed" if failed else "succeeded", 1 if failed else 0)


atexit.register(_at_exit)
