English | [简体中文](recipes.zh-CN.md)

# Recipes (mldojo/v1)

A recipe declares one experiment in YAML; see `recipes/examples/dp-pusht.yaml` for a full example. The parser lives in
`recipes/` and the JSON Schema in `recipes/schema/recipe.schema.json` (add
`# yaml-language-server: $schema=...` at the top of the file to get validation in your editor). Unknown fields are an error.

```yaml
apiVersion: mldojo/v1
kind: Experiment
metadata: {project: manip-diffusion, name: dp-pusht-baseline, tags: [diffusion]}
code:
  source: git            # git | local | inline-patch
  repo: git@github.com:me/xxx.git
  ref: main
env:
  default: {type: conda, spec: environment.yaml}
  overrides:
    - when: {backend_kind: queue, backend: mock}   # backend is the queue plugin name
      use: {type: docker, image: registry.example.com/my-team/train:cu130}
datasets: [{name: pusht, version: v1, mount: /data/pusht}]
models: [{name: policy, version: latest, as: ckpt}]   # optional: reference a registered model by name
run:
  cmd: python lerobot/train.py seed=${seed}
  workdir: .
  setup: pip install -e .           # optional: runs inside the env, before cmd
  params: {seed: [0, 1, 2], max_steps: 20000}
  env: {WANDB_MODE: offline}
  wandb: shim                       # optional: import wandb is redirected to MLDojo
resources:
  default: {gpus: 1, gpu_type: "3090|4090|5090|h20", min_mem_gb: 24}
  overrides:
    - when: {backend_kind: queue, backend: mock}
      use: {workers: 1, gpu_per_worker: 4, cpu_per_worker: 4, cpu_mem_ratio: 4, wall_time_min: 240}
outputs:
  logs: outputs/**/*.log
  checkpoints: outputs/**/*.ckpt
  videos: outputs/**/*.mp4
  model: policy                     # optional: on success, register the latest checkpoint as a version
  metrics:
    - {type: tensorboard, path: outputs/tb}
    - {type: jsonl, path: outputs/metrics.jsonl}
hooks:
  pre_submit: hooks/tweak.py        # Python escape hatch: reads recipe JSON on stdin, writes modified JSON to stdout
```

## Code

| source | Behavior |
|---|---|
| `local` | The CLI packs `code.path` (relative to the recipe file, default `.`). Inside a git repo it filters by `.gitignore`; otherwise it skips directories such as `.git`, `node_modules` and `outputs`, and honors `.mldojoignore` |
| `git` | If the recipe is inside a checkout, packs the whole repo and records the HEAD commit plus a patch of the dirty working tree (including untracked files); if not in a checkout, the agent clones `repo@ref` |
| `inline-patch` | The agent clones `repo@ref`, then `git apply`s `code.patch` |

`run.workdir` is relative to the code root (the repo root in `git` mode).

## overrides

Every non-empty field in `when` must match: `backend_kind` (node|queue), `backend` (queue plugin name such as `mock`,
or a node id; see [queue-plugins.md](queue-plugins.md)), `backend_id`, `target`, `labels` (the node or queue must have all of these labels). `use` is shallow-merged
onto the default; if the env `type` changes, the env is replaced entirely.

## Parameters and matrices

- `${name}` is replaced with the parameter value; `${project}` and `${experiment}` also work. Any other `${VAR}` is left as is for the shell.
- `mldojo run submit --matrix seed[,lr]` takes the Cartesian product of the listed list parameters; `--matrix all` expands every list parameter.
  List parameters not in the matrix take their first value.
- `--param k=v` overrides a parameter; the value is parsed as YAML, e.g. `--param seed=[0,1]`.
- Each parameter is also passed to the process as the environment variable `MLDOJO_PARAM_<NAME>`.

## Models: registering and referencing

Neither direction of lineage should need extra steps.

- **`outputs.model: <name>` or `<project>/<name>`**: registers a version automatically when the run succeeds.
  It takes the artifacts collected by `outputs.checkpoints`, groups them by the step in their paths, and the group with the highest step becomes the version;
  if a group has multiple files (a sharded checkpoint), their common directory is registered; a single file is registered as is.
  The run's latest metrics are snapshotted along with it, so the version stands even if the run is deleted.
  Requires `outputs.checkpoints` to be set.

- **`models: [{name, version, as}]`**: references a model by name, resolves it to a path and fills it into the parameters; use `${ckpt}` in `cmd` as usual.
  A `name` without `/` belongs to this recipe's project. `version` can be a version number, or `latest` (default),
  `production`, `staging`. `as` defaults to `model` and must not collide with an existing key in `run.params`.
  If the referenced model is not registered or the version does not exist, submit fails right away (exit 2) instead of failing halfway through the run.
  An artifact of the form `node://<node>/<path>` is resolved back to the path on that node; if the target node does not match, the submission is rejected.

So both questions can be answered: `mldojo run show` shows which version a run read and what it will register as,
and `mldojo model show` shows which runs evaluated each version. `run rerun` replays the **reference** (pinned to the version used at the time),
not the path it resolved to back then.

## Runtime environment variables

`MLDOJO_RUN_ID`, `MLDOJO_RUN_TOKEN`, `MLDOJO_PROJECT`, `MLDOJO_EXPERIMENT`, `MLDOJO_TARGET`,
`MLDOJO_API_URL`, `MLDOJO_METRICS_FILE` (where the SDK writes), `MLDOJO_DATASET_<NAME>`, `MLDOJO_WORKDIR`,
`CUDA_VISIBLE_DEVICES`, and `http(s)_proxy` from the node's proxy settings.

## Metrics

- **File scanning (default, no code changes)**: jsonl, one object per line. The step is the first present of `step`, `_step`, `global_step`,
  `iteration`, `iter`, `epoch`, falling back to the line number; every other numeric field is a metric, and nested objects are joined into keys with `/`.
  tensorboard supports `simple_value` and scalar tensors (TF2).
- **SDK**: `import mldojo; mldojo.log({"loss": x}, step=i)` (the agent puts the SDK on PYTHONPATH automatically;
  elsewhere use `pip install sdk/python`).
- **wandb**: with `run.wandb: shim`, `wandb.init/log/finish` write to MLDojo.
