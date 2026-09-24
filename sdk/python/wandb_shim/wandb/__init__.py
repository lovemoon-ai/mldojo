"""wandb compatibility layer for MLDojo.

Enabled per run with `run.wandb: shim` in the recipe: the agent puts this
package first on PYTHONPATH, so `import wandb` in unmodified training code
lands here and wandb.log(...) goes to MLDojo metrics. Covers the commonly
used surface; everything else is a harmless no-op.
"""
import os
import sys

import mldojo as _m

__version__ = "0.0.0-mldojo-shim"
_mldojo_shim = True


class _Config(dict):
    def __getattr__(self, k):
        try:
            return self[k]
        except KeyError:
            raise AttributeError(k)

    def __setattr__(self, k, v):
        self[k] = v

    def update(self, d=None, allow_val_change=False, **kw):
        super().update(d or {}, **kw)

    def setdefaults(self, d):
        for k, v in (d or {}).items():
            self.setdefault(k, v)


class _Summary(dict):
    def update(self, d=None, **kw):
        super().update(d or {}, **kw)


class Run:
    def __init__(self, project=None, name=None, config=None, **kw):
        # mldojo.init() attaches to the surrounding run when there is one and
        # creates one otherwise, so `wandb.init()` in a script nobody
        # submitted now produces a real run instead of a local no-op.
        _m.init(project=project, name=name, config=dict(config or {}))
        self.project = project or os.environ.get("MLDOJO_PROJECT")
        self.name = name or os.environ.get("MLDOJO_RUN_ID")
        self.id = os.environ.get("MLDOJO_RUN_ID", "local")
        self.config = _Config(config or {})
        self.summary = _Summary()
        self.dir = os.getcwd()
        self.url = _m.run_url() or ""
        self._step = 0

    def log(self, data, step=None, commit=None, sync=None):
        if step is None:
            step = self._step
            if commit is not False:
                self._step += 1
        else:
            self._step = max(self._step, int(step) + 1)
        _m.log(data, step=step)
        for k, v in data.items():
            if isinstance(v, (int, float)):
                self.summary[k] = v

    def finish(self, exit_code=None, quiet=None):
        # Carry config and summary across: they are what a leaderboard sorts
        # on, and before this they only ever lived in this process's memory.
        _m.config.update(self.config)
        _m.summary.update(self.summary)
        _m.finish("failed" if exit_code else "succeeded", exit_code)

    def watch(self, *a, **k):
        pass

    def save(self, *a, **k):
        pass

    def define_metric(self, *a, **k):
        pass

    def log_artifact(self, *a, **k):
        pass

    def use_artifact(self, *a, **k):
        return None

    def alert(self, *a, **k):
        pass

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.finish()


run = None
config = _Config()
summary = _Summary()


def init(project=None, name=None, config=None, reinit=None, **kw):
    global run
    run = Run(project=project, name=name, config=config)
    globals()["config"] = run.config
    globals()["summary"] = run.summary
    return run


def log(data, step=None, commit=None, sync=None):
    if run is None:
        init()
    run.log(data, step=step, commit=commit)


def finish(exit_code=None, quiet=None):
    if run is not None:
        run.finish()


def watch(*a, **k):
    pass


def save(*a, **k):
    pass


def login(*a, **k):
    return True


def define_metric(*a, **k):
    pass


def Image(*a, **k):  # noqa: N802 - wandb API name
    return None


def Video(*a, **k):  # noqa: N802
    return None


def Histogram(*a, **k):  # noqa: N802
    return None


def Table(*a, **k):  # noqa: N802
    return None


class Artifact:
    def __init__(self, *a, **k):
        pass

    def add_file(self, *a, **k):
        pass

    def add_dir(self, *a, **k):
        pass


class Settings:
    def __init__(self, *a, **k):
        pass


sys.modules.setdefault("wandb.sdk", sys.modules[__name__])
