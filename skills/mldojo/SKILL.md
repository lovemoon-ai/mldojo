---
name: mldojo
description: Manage GPU compute and ML experiments with the MLDojo CLI (`mldojo`) - find free GPUs, write recipes, submit training/eval jobs (including parameter matrices), watch logs/metrics/GPUs, inspect per-episode eval success rates, compare runs, register and reference model versions, and clean up node disks. Use this skill whenever a task involves running training or evaluation on GPU nodes or queue plugins, or the user mentions mldojo, recipes, run ids, "submit an experiment", "which GPU is free", "how far is training", "eval success rate", "compare two runs", "register a checkpoint" - even if the user never says "mldojo", use it whenever the job is "put an experiment on the cluster and keep an eye on it".
---

English | [简体中文](SKILL.zh-CN.md)

# MLDojo CLI

MLDojo manages two things: **compute** (which nodes, which GPUs, who is using them) and **experiments** (each run's code, parameters, logs, metrics, artifacts, eval results and model lineage). `mldojo` is the command line shared by humans and AI agents; the web UI shows the same data.

One design decision runs through everything, so remember it first: **observation is pull-based**. MLDojo does not require changes to training code; it reads the files the job already writes (tensorboard events, jsonl, eval manifests, videos) and collects artifacts by glob. So bringing an existing project under MLDojo is almost entirely about **getting the paths in the recipe right**, especially "one output directory per run" in §2.3.

---

## 0. Connection and conventions

**First-time setup on a new machine** (server address `{{MLDOJO_SERVER}}`; nodes that cannot reach this address should replace it below with the intranet address `{{MLDOJO_INTRANET_SERVER}}`):

```bash
# 1. Install the CLI into ~/.local/bin (no login needed; linux/macOS, amd64/arm64)
curl -fsSL {{MLDOJO_SERVER}}/install.sh | sh
# 2. Log in: prints a link and a verification code; log in in the browser, check the code, click "Approve", and the CLI finishes on its own
mldojo login --server {{MLDOJO_SERVER}}   # nodes that cannot reach this address: mldojo login --server {{MLDOJO_INTRANET_SERVER}}
mldojo health                             # check connectivity; exit 3 = cannot connect
# 3. (optional) install this skill
mkdir -p ~/.claude/skills/mldojo && curl -fsS -H "Authorization: Bearer $(awk '/^token:/{print $2}' ~/.mldojo/config.yaml)" \
  {{MLDOJO_SERVER}}/SKILL.md -o ~/.claude/skills/mldojo/SKILL.md
```

- Login gives you a token issued in your name (valid for 90 days; after it expires or is revoked, just run `mldojo login` again), stored in `~/.mldojo/config.yaml` (0600) and never shown in the terminal. The link can be opened in a browser on any computer, not necessarily this machine.
- The approval page shows the hostname and IP that started the login: **if it is not a `mldojo login` you just ran yourself, click "Deny"** - approving hands your permissions to that machine.
- Scripts/CI skip the browser: `mldojo login --server URL --token <token>`, or set the environment variables `MLDOJO_SERVER` / `MLDOJO_TOKEN`.
- On the API server itself use `http://127.0.0.1:8765`; the CLI is at `~/.local/bin/mldojo`.
- **Never print tokens or any secret in output, logs or commits.** Store secrets with `mldojo secret set <ns>/<name> --stdin`, and only write `secret://ns/name` references in recipes and node configs.
- **Agents always add `--json`**: every command supports it and the output is stable and parseable; table output is for humans and its columns may change. Common fields are in §10.
- A run id can be abbreviated to a unique prefix (e.g. `d5900061`, or even shorter).
- Projects and experiments are created automatically on first submit; no need to `project create` first. Names may only contain letters, digits, `.`, `_`, `-`, and must start with a letter or digit.
- Exit codes are meaningful; branch on them in scripts:

| Code | Meaning | What to do |
|---|---|---|
| 0 | Success | |
| 2 | User error (bad recipe, target does not exist, resources can never be satisfied, model not registered...) | Fix the input; do not retry |
| 3 | Cannot reach the server or node | Check network/node; retry is fine |
| 4 | Backend error; with `--wait`, some run did not succeed | Check `run logs --stream stderr` |
| 5 | Conflict (already exists, still running...) | |

---

## 1. Standard workflow

### 1.1 Check compute first, then decide where to run

```bash
mldojo ai free-nodes --min-free-gb 40 --label 5090 --gpus 2   # flags can be combined, or all omitted
mldojo node ls                                 # nodes, online status, per-GPU utilization
mldojo node gpu gpu-b                          # per-GPU memory and processes on one machine; -w to keep refreshing
mldojo node history bastion-gpu-1 --since 24h  # what this machine has been doing over a period (not right now)
```

`gpus_free` counts GPUs that have **no MLDojo run and enough free memory** (threshold set by `--min-free-gb`). It does not matter who is using a GPU: the cluster is shared, GPUs often have colleagues' processes on them, and `holders` lists them.

### 1.2 Write a recipe -> dry-run -> submit

```bash
mldojo run submit -f recipe.yaml --target node:gpu-b --dry-run --matrix seed   # lists the substituted cmd/workdir of every matrix point; creates no runs
mldojo run submit -f recipe.yaml --target node:gpu-b                # submit; returns the run id
mldojo run submit -f recipe.yaml --target node:gpu-b --follow       # single run only: stream logs while it runs
mldojo run submit -f eval.yaml --target node:gpu-b --matrix setting,chunk --wait   # matrix; wait for all to finish
mldojo run submit -f recipe.yaml --target pool:5090 --param lr=3e-4 --param max_steps=2000
```

- **Dry-run first.** All of these exit 2 right at submit time: a bad recipe, a target node that does not exist, `gpus`/`gpu_type`/`min_mem_gb` that can **never** be satisfied on that machine, a referenced model that is not registered. Far cheaper than finding out halfway through. The dry-run output shows the substituted `cmd` and `workdir` for every matrix point - check each one before submitting; outputs paths are not in the output, so work them out from the parameters yourself.
- `--matrix a,b` takes the Cartesian product of list parameters, one run per point, with run names like `chunk=50,setting=clean`; `--matrix all` expands all list parameters; list parameters not in the matrix use only their first value.
- `--param k=v` overrides a parameter; the value is parsed as YAML (e.g. `--param seeds=[0,1]`).
- `--project` / `--exp` override `metadata.project` / `metadata.name` in the recipe. **With only `--exp`, the project is still the one in the recipe**, so the run may land in a project you did not expect - pass both.
- `--gpus N` overrides `resources.gpus`. `--notes "..."` attaches a one-line note to the run, useful when looking back later.

| target | Meaning |
|---|---|
| `node:<id>` | A specific machine (most common) |
| `pool:<label>[,<label>]` | Any online node with these labels, picked by the scheduler, e.g. `pool:5090`, `pool:5090,8gpu` |
| `queue:<plugin>/<queue>` | Submit to a queue of a queue plugin (logs are polled, near-realtime) |
| `external:<host>` | A run not started by the platform, created by the SDK's `mldojo.init()`; see §6 |

### 1.3 Keep an eye on it

```bash
mldojo run status <run>                # one line: status, exit code, message
mldojo ai brief <run>                  # everything at once: progress/ETA, latest metrics, anomalies, best checkpoint, eval results
mldojo run logs <run> -f               # follow stdout; --stream stderr|system|all; --tail 20000 for only the last N bytes
mldojo run metrics <run> --json        # .keys lists the metrics this run actually has
mldojo run metrics <run> --key training/loss,training/lr --since step:1000
mldojo ai anomaly-check <run>          # NaN, spikes, plateaus, OOM
mldojo run events <run>                # state transitions, which machine it went to, retries, which model got registered...
mldojo run gpu <run>                   # the GPUs this run holds: current state plus the whole lifetime
```

- **`queued` does not necessarily mean something is wrong.** First look at `message` in `mldojo run show <run>`; it says what it is waiting for, e.g. `waiting: node gpu-b has 0 of the 1 cards with 40 GB free that it needs`, or `node X is at its limit of 1 concurrent runs`. Once resources free up, the scheduler (checks every 15 seconds) starts it automatically - no need to resubmit; resubmitting just queues another one.
- There is no dedicated "wait until done" command for an already-submitted run: poll `run status <run> --json` and watch for `.status` to become `succeeded` / `failed` / `cancelled`. For long jobs, stretch the poll interval to a few minutes.
- `ai brief` computes progress and ETA from the total step count in the parameters; the parameter must be named one of `max_steps` / `total_steps` / `steps` / `num_steps` / `train_steps` / `iterations` / `iters`.
- When troubleshooting, check the `system` stream first: it holds what the platform itself recorded - submission, dispatch, state changes, why it did not retry, what was registered.

### 1.4 Look at results, compare

```bash
mldojo ai summarize-exp lingbot/place-empty-cup-eval   # rank all runs in the experiment by the primary metric + next-step suggestions
mldojo exp show lingbot/place-empty-cup-eval
mldojo run ls --project lingbot --exp place-empty-cup-eval --status succeeded
mldojo run ls --search chunk --sort duration --limit 20     # --search matches a substring of the run or experiment name
mldojo compare <runA> <runB>              # two runs: code diff, environment differences, parameters, metrics
mldojo compare <r1> <r2> <r3> <r4>        # several runs: parameters and final metrics side by side
mldojo run artifacts ls <run>             # asks the node to rescan first
mldojo run artifacts get <run> <uri> --dest ./out
```

The "primary metric" is chosen automatically: if there is an eval success rate `eval/success_rate`, it is used; otherwise it looks for `val/loss`, `eval/loss`, `loss`, `train/loss`, `training/loss` in that order; if none exist it picks the plainest-named `*_loss`. So **whenever you run an eval, always configure `outputs.episodes`**. Loss often says little on real tasks: in one eval, loss stayed at 0.06-0.08 from start to finish while the success rate went from 40% to 75%.

The **Matrix** tab on the experiment page in the web UI pivots runs into a table by the parameters that vary: rows = setting, columns = chunk, cells = success rate. After a matrix finishes, send people this page - it is much clearer than a list of runs.

### 1.5 Wrap up

```bash
mldojo run cancel <run>...                       # SIGTERM the whole process group first, SIGKILL after 15 seconds
mldojo run rerun <run>                           # run again with the same code, environment and parameters
mldojo run rerun <run> --target node:gpu-a --param lr=1e-4
mldojo run rm <run>... --purge-node              # also delete the working directory on the node (including checkpoints)
mldojo node disk gpu-b                           # how much disk MLDojo uses on this machine, per run
```

`--purge-node` deletes checkpoints; **before deleting, make sure they are no longer needed, or have been registered and copied elsewhere**. Artifacts of in-place runs (§2.1) live in the project's own directory, and MLDojo does not delete them.

---

## 2. Recipe

A recipe describes four things: what to run, in what environment, how many resources, and where the artifacts are. Parsing is strict: **a misspelled field name is an error**, not silently ignored. For editor validation, add a line at the top of the recipe file: `# yaml-language-server: $schema=<repo>/recipes/schema/recipe.schema.json`.

### 2.1 In-place runs: code and weights are already on the node (most common)

Most real projects **cannot be packaged and uploaded**: right next to the code are tens of GB of weights, an installed venv and simulator assets. Writing `run.workdir` as an absolute path means running in place, uploading nothing.

```yaml
apiVersion: mldojo/v1
kind: Experiment
metadata:
  project: lingbot
  name: place-empty-cup
  tags: [vla, robotwin]
code:
  source: none                         # an absolute workdir defaults to none; writing it out is clearer
env:
  default: {type: none}                # use the node's existing environment; see §2.4
run:
  workdir: /home/alice/lingbot     # must already exist; MLDojo will not create it
  cmd: |
    OUTPUT_DIR="$PWD/outputs/${out}" \
    MAX_STEPS=${max_steps} MICRO_BATCH_SIZE=${batch} \
    bash scripts/train_one_step.sh
  params:
    out: b16_2k                        # this run's own output directory name; see §2.3
    batch: 16
    max_steps: 2000                    # use this name so ai brief can compute progress and ETA
resources:
  default: {gpus: 1, gpu_type: h20, min_mem_gb: 80}
outputs:
  model: place-empty-cup               # on success, automatically registered as a new version of lingbot/place-empty-cup; see §5
  checkpoints: outputs/${out}/checkpoints/**/*.safetensors
  metrics:
    - type: tensorboard                # entries containing ${...} must not use the {a: b} flow style; see §2.5
      path: outputs/${out}/runs
```

Relative paths in outputs are relative to `run.workdir` (for in-place runs, that is the project directory itself); absolute paths also work. When artifacts land **outside** the workdir (e.g. on a shared disk), you must use absolute paths; an absolute glob with wildcards is scanned starting from the deepest directory that contains no wildcard.

### 2.2 Package and upload: small projects, locally modified code

```yaml
code:
  source: local        # package code.path (relative to the recipe file, default .), filtered by .gitignore / .mldojoignore
  path: .
# source: git          # when the recipe is in a git checkout, package the whole repo and record the HEAD commit and uncommitted diff
# source: git + repo/ref, when the recipe is not in a checkout: the node clones repo@ref itself
# source: inline-patch # clone repo@ref first, then git apply code.patch
env:
  default: {type: venv, spec: requirements.txt}   # cached by file hash; can also be conda (environment.yaml or an existing env name) / docker (image) / none
run:
  cmd: python train.py --lr ${lr} --seed ${seed}
  params: {lr: 3e-4, seed: [0, 1, 2]}             # list parameter, use with --matrix seed
  setup: pip install -e .                         # optional, runs inside the env before cmd
  env: {WANDB_MODE: offline}
outputs:
  checkpoints: outputs/**/*.ckpt                  # each uploaded run has its own separate directory, so nothing mixes
  metrics:
    - type: jsonl
      path: outputs/metrics.jsonl
```

What `source: git` buys you is reproducibility: the run records the commit and uncommitted diff, and `mldojo compare` can show the code difference between two runs directly.

### 2.3 Every run must have its own output directory (the easiest mistake with in-place runs)

**outputs collects every file the glob matches**: it does not filter by modification time and cannot tell whether this run wrote the file. With in-place runs, the project directory is shared by all runs - results from earlier runs and from other points of the same matrix all get collected; `outputs.model` may even register another run's checkpoint as its own. So:

- **Every run (every matrix point) writes to a directory that belongs only to it, with the directory name built from parameters**: `outputs/seed${seed}/...`, `eval_results/mldojo_${setting}_c${chunk}/...`.
- If the script accepts an output directory argument, pass it directly (preferred).
- If the script creates its own timestamped directory, `mv` the new directory at the end of cmd to a fixed, parameter-named location and point outputs there. This is the approach that actually worked in a real eval; full example in §3. Artifacts are scanned roughly every 30 seconds while running, and **scanned fully once more after the process exits**, so an `mv` at the very end of cmd is still collected.
- This `mv` approach **only suits evals**: for training you want to watch metrics while it runs, and MLDojo sees nothing until the directory is moved. If a training script cannot even take an output directory, first check whether its directory name is determined by parameters (e.g. `outputs/pusht_s${seed}_*`); if so, match it with a parameter-based glob. If not, add an output directory argument or environment variable to the script - this is where breaking the "don't change training code" principle is worth it.
- Use `mv`, **not symlinks**: the scan does not follow symlinked directories.
- **Do not `rm -rf` the directory holding a registered model's checkpoints.** Artifacts are the raw files on the node (§9), a model version is just a path pointing at them, and versions have no delete API. The `rm -rf "$DEST"` in §3 only clears the previous result of the same eval point; putting it in a training recipe deletes models.
- **Parameters cannot reference each other**: `out: seed${seed}` is not expanded. Write `${seed}` directly in the path.
- Only parameters, `${project}` and `${experiment}` are substituted in outputs; environment variables like `$MLDOJO_RUN_ID` are **not** expanded.
- When two similar runs run concurrently on the same machine, "take the newest directory" races. On single-GPU nodes GPU admission queues them so they do not collide; on multi-GPU nodes, either pass the directory to the script as a parameter, or serialize with `mldojo node limit <id> --max-runs 1`. If the script writes shared files in the working directory (caches, `latest` symlinks, lock files), be careful with concurrency too.

### 2.4 The environment a job runs in

- cmd is executed by a **non-login bash** as the user the agent was deployed as (the user in `node add --ssh`), inheriting the agent process's environment variables. `env: {type: none}` uses exactly this PATH and does **not** source `.bashrc` or activate conda automatically. If you need a venv/conda, write `source .venv/bin/activate` explicitly in cmd or `run.setup`, or use the interpreter's absolute path.
- When unsure about a node's environment, spend a few seconds probing first:

  ```bash
  mldojo ai submit '{"project":"scratch","target":"node:bastion-gpu-1","workdir":"/home/alice/proj","cmd":"whoami; which python; python train.py --help","gpus":0}' --json
  mldojo run logs <run>
  ```

  An absolute `workdir` in `ai submit` means running in place; `gpus: 0` means no GPU is taken.

- For in-place runs, this user must have write access to the project directory. If the project is in someone else's home, add `test -w .` to the probe command.
- `gpus: N` means exclusive use of N GPUs: once a GPU has an MLDojo run, it is not given to another run, even if `min_mem_gb` is far below the GPU's memory.

Environment variables available to the job: `MLDOJO_RUN_ID`, `MLDOJO_PROJECT`, `MLDOJO_EXPERIMENT`, `MLDOJO_TARGET`, `MLDOJO_WORKDIR`, `MLDOJO_GPUS` (assigned GPU indices), `CUDA_VISIBLE_DEVICES`, `MLDOJO_PARAM_<NAME>` (one per parameter), `MLDOJO_DATASET_<NAME>` (dataset path), `MLDOJO_RESUME_FROM` (only on retries), `MLDOJO_METRICS_FILE` / `MLDOJO_EPISODES_FILE` (the SDK writes here), `MLDOJO_API_URL` / `MLDOJO_RUN_TOKEN`, plus the node's configured `http(s)_proxy`.

### 2.5 Other fields

```yaml
datasets:                          # must be registered first with mldojo dataset register
  - {name: pusht, version: v1, mount: /data/pusht}
models:                            # reference registry models by name; see §5
  - {name: place-empty-cup, version: latest, as: ckpt}
run:
  wandb: shim                      # logs written via import wandb in your code go into MLDojo
resources:
  default:
    gpus: 1
    gpu_type: "4090|5090|h20"      # | separates alternatives, matched as a substring of the GPU model; pure numbers must be quoted: "5090"
    min_mem_gb: 40                 # admission at dispatch time checks live free GPU memory; queues if not enough
    wall_time_min: 240             # timeout counts as failure
  overrides:                       # override defaults per target (shallow merge)
    - when: {backend_kind: queue, backend: mock}
      use: {workers: 1, gpu_per_worker: 4, cpu_per_worker: 4, cpu_mem_ratio: 4}
    - when: {labels: [h20]}
      use: {min_mem_gb: 80}
retry:                             # no retries without this section
  max: 2                           # 0-10
  on: infra                        # default: only retry runs the platform lost (node offline, agent restart, dispatch failure); any: also retry when the process itself exits non-zero
  resume_from: "*.ckpt"            # pick the highest-step checkpoint file collected from the failed attempt and pass it to the next as MLDOJO_RESUME_FROM
outputs:
  logs: outputs/**/*.log
  videos: /abs/path/**/*.mp4
  images: outputs/**/*.png
hooks:
  pre_submit: hooks/tweak.py       # runs at CLI submit time: reads recipe JSON from stdin, writes modified JSON to stdout
```

- `${name}` is replaced with the parameter value; `${project}` and `${experiment}` are also available, in `cmd`, `workdir`, `setup`, `run.env` and outputs paths; any other `${VAR}` is left as-is for the shell.
- **`gpu_type: 5090` is rejected** (YAML reads it as an integer; error `gpu_type must be a string`); write `gpu_type: "5090"`.
- **Values containing `${...}` cannot go in YAML flow style `{...}`.** For example in `- {type: tensorboard, path: out/${run}/tb}`, the `}` in `${run}` is taken as the end of the mapping and the whole recipe fails to parse (error `did not find expected ',' or '}'`). Use multi-line block style instead, or quote the value: `path: "out/${run}/tb"`.
- `retry.on` defaults to retrying only platform problems: a bug in the job is still the same bug on retry. When it does not retry, the system log says why.
- `resume_from` matches **files** (artifacts collected by `outputs.checkpoints`), anchored only at the end of the path: `*.ckpt` and `**/*.safetensors` both match; a pattern ending in a directory, like `checkpoints/**/global_step_*`, matches nothing. Also, it only works if **the training script itself reads `MLDOJO_RESUME_FROM`**; otherwise a retry starts from scratch.
- A tensorboard `path` can be a single event file or a directory (searched recursively for files whose names contain `tfevents`). jsonl has one JSON object per line: the step is the first present of `step`, `_step`, `global_step`, `iteration`, `iter`, `epoch`, falling back to the line number; all other numeric fields are recorded as metrics, with nested objects joined into keys with `/`.

---

## 3. Evaluation: the result is "k of N episodes succeeded", not a curve

`outputs.episodes` points at the files the eval harness **already writes**; the harness itself needs no changes. Below is a recipe that actually worked in a real eval: the harness creates a new timestamped directory each time, and the directory name does not include chunk, so at the end of cmd it is moved into a parameter-named directory (§2.3).

```yaml
apiVersion: mldojo/v1
kind: Experiment
metadata: {project: lingbot, name: place-empty-cup-eval}
code: {source: none}
env: {default: {type: none}}
models:
  - {name: place-empty-cup, version: latest, as: ckpt}   # reference by name, don't paste paths (§5)
run:
  workdir: /home/alice/lingbot
  cmd: |
    set -euo pipefail
    DEST="$PWD/eval_results/mldojo_${experiment}_${setting}_c${chunk}"
    rm -rf "$DEST"
    EVAL_VIDEO_LOG=True bash scripts/eval_single_task.sh "${ckpt}" ${setting} ${episodes} ${chunk}   # EVAL_VIDEO_LOG is this harness's switch for recording videos
    latest=$(ls -td "$PWD"/eval_results/place_empty_cup_${setting}_* | head -1)
    mv "$latest" "$DEST"
  params:
    setting: [clean, randomized]
    chunk: [16, 50]
    episodes: 20
resources:
  default: {gpus: 1, min_mem_gb: 40}     # the inference server loads in fp32
outputs:
  videos: eval_results/mldojo_${experiment}_${setting}_c${chunk}/**/*.mp4
  episodes:
    - type: csv                          # sources listed first decide success/failure; here the manifest also provides seed
      path: eval_results/mldojo_${experiment}_${setting}_c${chunk}/manifest.csv
    - type: filenames                    # later sources only fill empty fields: here, videos
      path: eval_results/mldojo_${experiment}_${setting}_c${chunk}/episodes
```

Submit: `mldojo run submit -f eval.yaml --target node:gpu-b --matrix setting,chunk --exp place-empty-cup-eval-v3`, then look at the Matrix tab when done.

- **One eval experiment per model version** (`--exp <name>-v<N>`; look up the version number with `model show` first). That way each cell on the Matrix tab corresponds to exactly one version; and because the output directory includes `${experiment}`, evaluating a new version does not overwrite the old version's videos - the old runs' episode records still point at those files.
- Taking "the newest directory" with `ls -td ... | head -1` is safe here only because **gpu-b has a single GPU**: GPU admission runs the 4 points one after another, so they never create directories at the same time. Before copying this to a multi-GPU node, make the harness accept an output directory; if that is not possible, serialize with `mldojo node limit` (§2.3).

- **csv / jsonl** are recognized by column name, and common spellings all work: episode index `episode|idx|ep|index`, `seed`, result `success|result|video_result|status|outcome|passed` (values `success/succeeded/ok/pass/passed/true/yes/1` count as success, anything else as failure), steps `steps|frames|length`, duration `duration_seconds|duration_s|duration_ms`, video `video|filename|video_path|file`.
- **filenames** recursively scans files like `episode12_success.mp4`, `episode3_failure.mp4`; the result is read from the filename, so the video is matched up too (mp4/webm/mov/gif supported).
- Multiple sources are **merged into one row per episode index**: the source listed first decides success/failure, later ones only fill empty fields (seed, video, steps). So if the glob matches several eval directories, the same episode index from different directories is merged into one row too - another reason for each run to own its directory. A `filename` in the manifest is looked up in the manifest's directory and its subdirectories.
- The aggregate is written as the metric `eval/success_rate`, so ranking, `summarize-exp` and the Matrix tab need no eval-specific handling.

```bash
mldojo run episodes <run>                        # every episode: seed, success/failure, steps, video
mldojo run episodes <run> --result failure --json   # .episodes[].video_uri are the videos of failed episodes
mldojo run artifacts get <run> <video_uri> --dest ./videos   # download the videos to watch
mldojo compare episodes <runA> <runB> --flipped  # episode-by-episode comparison of two evals, listing only episodes whose result flipped
```

**Look at the flipped episodes, not just the success rate.** The same checkpoint with the same seeds run twice may give 6/20 once and 5/20 the next time; the real information is in which episodes flipped. `compare episodes` aligns by **seed**, falling back to episode index when there is no seed. It is meant for comparing two evals over **the same seeds**: two checkpoints under the same setting, or the same checkpoint run twice. If two settings use different seeds, most episodes appear on only one side - episodes on only one side do not count as flipped, and `a` or `b` is null in the result. The output has no video paths: once you have the episode index or seed, look up the video URI with `run episodes --json`.

---

## 4. Hyperparameter search (sweep)

```yaml
apiVersion: mldojo/v1
kind: Sweep
metadata: {project: smoke, name: lr-search}
recipe: recipe.yaml          # relative to this file
target: pool:5090
method: random               # random | grid (grid requires values on every axis)
metric: {name: loss, goal: min}
budget: {max_runs: 12, max_parallel: 3}
space:
  lr:    {log_uniform: [1e-5, 1e-2]}
  seed:  {int_uniform: [0, 1000]}
  warm:  {uniform: [0.0, 0.1]}
  steps: {values: [1000, 2000]}
```

```bash
mldojo sweep create -f sweep.yaml
mldojo sweep show smoke/lr-search      # leaderboard
mldojo sweep ls --status running
mldojo sweep stop smoke/lr-search      # start no new runs; runs already going finish
```

When one training run takes hours and checkpoints are tens of GB, a sweep is rarely worth it; a small, dense controlled matrix (`--matrix`) is more common.

---

## 5. Model lineage: register when done, reference by name

**Registering.** Write `outputs.model: <name>` (or `<project>/<name>`) in the training recipe, and a new version is registered **the moment the run succeeds**. The system log shows `registered <p>/<n>@<v> -> <uri>`, and `run events` gets a `model_registered` entry. The rules:

- Take the files collected by `outputs.checkpoints`, group them by the step number in the path; the group with the **highest step** is this version (not the one with the best metric). The step is the number following the **first** `step|iter|epoch|ckpt|checkpoint` in the full path (at most one `_` or `-` in between; `global_step_2000` and `epoch_12.pt` both work; in `ckpt/epoch_12.pt` what follows `ckpt/` is not a number, so it does not mis-match). So do not put combinations like `max_step_20000` in directory names above the checkpoints, or every checkpoint will be read as the same step.
- If a group has several shard files, their common directory is registered (e.g. `.../global_step_2000/hf_ckpt`); with a single file, that file is registered. The run's latest metrics at that moment are snapshotted along with it.
- `outputs.model` must be written together with `outputs.checkpoints`. If the run succeeds but the glob matches no files, the run still counts as succeeded but nothing is registered; the system log says `outputs.model <name>: no checkpoints were collected, nothing to register`.
- **When several runs register to the same model** (e.g. 3 seeds), each successful run gets its own version, numbered in the order they succeeded, and `latest` is the last one to succeed. `model show` lists which run each version came from and its metrics at the time. Training loss often cannot tell good from bad, so usually evaluate all versions first, then `promote` the chosen one to production; later evals write `version: production` so the meaning is unambiguous.

**Referencing.** In eval recipes, do not paste checkpoint paths; reference by name:

```yaml
models:
  - name: place-empty-cup        # without / it belongs to this recipe's project
    version: latest              # version number | latest (default) | production | staging
    as: ckpt                     # fills ${ckpt}; default parameter name is model; must not clash with keys in run.params
```

`${ckpt}` resolves to a plain path on the node: a sharded checkpoint is a directory, a single file is the file itself, so the harness must accept the corresponding form. You can see the resolved path in the dry-run cmd; after actual submission, `.metadata.models[].version` in `run show` records the resolved version **number**, not the literal `latest`. A referenced model that is not registered, or a version that does not exist, **exits 2 at submit time**; if the artifact is on another node and the target is `node:<this one>`, it is also rejected outright (this machine cannot read that path). To evaluate another version, change `version:`; `--param ckpt=...` is rejected, since otherwise the run would claim to read one version while actually reading another path.

```bash
mldojo model ls [--project lingbot]
mldojo model show lingbot/place-empty-cup         # all versions, plus USED BY: which runs evaluated each version
mldojo run show <run>                             # model: which version this run read; registers: what it will register when done
mldojo model promote lingbot/place-empty-cup 3 --stage production   # only one each of production and staging; the old one becomes archived
mldojo model register lingbot/place-empty-cup --run <run> --uri node://gpu-b/<path>   # manually register after the fact
```

This answers both directions: "which checkpoint produced this 25%" (`run show`), and "how many times has this checkpoint been evaluated, and with what results" (`model show`). `run rerun` replays references and pins the version number from the time, so a rerun uses the same weights. **Versions currently have no delete API**, so think before a manual `model register`.

---

## 6. Reporting from code (optional)

When the no-code-change approach of §2.5, where the platform reads files (tensorboard / jsonl scanning), is enough, leave the code alone. When you really need it:

```python
import mldojo                                   # inside a run, the agent already puts the SDK on PYTHONPATH; elsewhere pip install <repo>/sdk/python
mldojo.init(project="demo", config={"lr": 3e-4})   # optional; when run manually outside MLDojo, creates an external: run
mldojo.log({"loss": 0.31, "train": {"acc": 0.9}}, step=1200)   # nested dicts are joined into train/acc
mldojo.log_episode(success=True, seed=100003, steps=160, video="ep3.mp4")
mldojo.summary["best_val"] = 0.12
mldojo.finish()
```

With no server to connect to, every call is a no-op, so the same script runs both inside and outside MLDojo. With `run.wandb: shim`, `wandb.init/log/finish` write into MLDojo.

---

## 7. Compute accounting and limits

```bash
mldojo usage --by project --since 168h     # GPU hours by project | run | submitter | node
mldojo gpu idle --since 24h                # GPUs held by runs but barely used (--min-hours 0.5)
mldojo node limit gpu-b --max-runs 1       # max concurrent runs on one machine (0 = unlimited)
mldojo project limit lingbot --max-runs 4  # max concurrent runs in one project
```

Time windows can be durations like `24h`, `90m`, or RFC3339 timestamps. Day units (`7d`, also the default for `usage`) require a server that includes commit `25133ad`; older servers reject it, and even `mldojo usage` without arguments errors out. If unsure, use hours (`168h`), which every version accepts.

Admission has two steps: at submit time, against the machine's static specs, rejecting anything that can **never** be satisfied; at dispatch time, under the node lock, against **live free GPU memory**, queuing with a stated reason if not enough. Runs that are admitted but whose process has not started yet also count as usage, so nothing is oversold.

---

## 8. Admin commands (usually admins only)

```bash
mldojo node add --id gpu-a --ssh alice@192.0.2.11 --labels 5090 --dry-run   # only test connectivity and probe the node; do not deploy
mldojo node add --id bastion-gpu-1 --ssh "user@host" --port 2222 --identity secret://ssh_keys/id_ed25519 \
    --via gpu-a --via local --labels 5090,8gpu,bastion
mldojo node add --id x --ssh user@host --reverse-tunnel     # when the node cannot reach the server, connect back through an SSH reverse tunnel
mldojo node test <id>                     # SSH reachability + agent health
# On node add, if the node has no ~/.mldojo yet, it is created in the user's directory on the data disk with the most free space (<disk>/<user>/mldojo),
# leaving only a symlink in home; --dry-run shows where it plans to put it. Nodes that already have ~/.mldojo are left alone; to keep it in home, mkdir ~/.mldojo before deploying.
mldojo node upgrade <id>...               # redeploy the agent; running jobs are re-adopted
mldojo node show <id>
mldojo node rm <id> --force               # --keep-agent keeps the agent process on the node
mldojo dataset register --name pusht --version v1 --mount /data/pusht \
    --location node:gpu-a:/data/pusht --authoritative --location bucket:<provider>/<bucket>/<path>
mldojo dataset push pusht@v1 --to node:bastion-gpu-1 # pre-warm a node's dataset cache
mldojo dataset ls
mldojo dataset rm pusht@v1                           # only unregister; files are not deleted
mldojo project ls
mldojo project rm <p> --force
mldojo exp ls <project>
mldojo exp rm <p>/<e> --force
mldojo queue add --id <plugin>/<queue> --backend <plugin> --credentials secret://<plugin>/default
mldojo secret set ssh_keys/foo --from-file ~/.ssh/id_rsa
mldojo secret ls
mldojo secret unlock
```

---

## 9. Pitfalls (all of these really happened)

- **With in-place runs, outputs of several runs get mixed together**: see §2.3. This is the most common mistake and the one with the most hidden consequences - metrics, eval results and registered models quietly become someone else's.
- **No `${...}` in flow-style YAML**: see §2.5.
- **An absolute workdir must already exist**, and implies `code.source: none`; combining it with `local`/`git` is an error.
- **`env: none` does not activate an environment for you**: see §2.4. The first time you run on a node, probe first (and `test -w .` for write access while you are at it).
- **`gpu_type: 5090` needs quotes**: see §2.5.
- **Always set `min_mem_gb`**: GPUs on a shared cluster often have someone else's processes using tens of GB. With it, the run queues for a GPU; without it, the run is dispatched onto a nearly full GPU and then OOMs.
- **Run stuck in `queued`**: read the message in `run show`; don't resubmit.
- **Nodes in containers cannot see process owners**: nvidia-smi reports host PIDs, and the GPU table shows `runid ?`, meaning "inferred to belong to this run". This is normal.
- **Artifacts are not copied away**: an artifact URI is `node://<node>/<absolute path>`, i.e. the raw file on the node; `run artifacts get` fetches it on demand via the agent. If the node is cleaned up, the artifacts are gone.
- **With `--wait`, exit 4 if any run did not succeed**: one failure in a matrix makes the whole command fail.
- **Resuming only half done**: only when the training script itself reads `MLDOJO_RESUME_FROM` does a retry continue from the checkpoint.
- **`--exp` does not change the project**: when submitting someone else's recipe, add `--project <your project>`.

---

## 10. Recommended flow and JSON fields for agents

1. `mldojo health --json` -> `mldojo ai free-nodes --min-free-gb <memory needed> --json`, pick a node with enough `gpus_free`, and use its `target` field.
2. Write the recipe (run in place when you can, one output directory per run); the first time you use a node, probe the environment with `ai submit`; then `--dry-run --json` to check each point's `cmd` and `workdir`.
3. `run submit ... --json`, and record each run from `.runs[].id` / `.runs[].name`.
4. Poll `mldojo ai brief <run> --json`: one call gives `.status`, `.progress`, `.eta`, `.latest_metrics`, `.anomalies`, `.eval.success_rate`, `.best_ckpt.uri`; don't call logs and metrics separately each time.
5. On failure: `mldojo run logs <run> --stream stderr --tail 20000`, then look at `--stream system`.
6. When done: `mldojo ai summarize-exp <p>/<e> --json`, taking the ranking from `.ranking[] {id,name,status,value,params}` and `.metric`; for evals use `compare episodes --flipped --json` (`.flipped`, `.episodes[] {seed,index,a,b}`); if `outputs.model` was set, use `model show <p>/<n> --json` to confirm `.versions[0]` was registered by this run.
7. When reporting to people, include the run id, experiment name and web link: `{{MLDOJO_SERVER}}/experiment?project=<p>&name=<e>` (matrix results are in the Matrix tab).

Other common fields: `--json` of both `run submit` and `ai submit` is `{experiment, runs: [Run]}`; `ai free-nodes --json` -> array `[{node_id, target, gpus_free, gpus_total, gpus_held, gpu_model, max_free_gb, labels, holders}]`; `.progress` in `ai brief` is a fraction 0-1; `model show --json` -> `{model, versions: [{version, stage, run_id, uri, sha256, size_bytes, metrics, notes, created_at}], used_by: [{version, run_id, project, experiment, name, status}]}`, with `versions` ordered newest first; `compare episodes --json` -> `{a, b, flipped, episodes: [{seed, index, a, b}]}`, where `a`/`b` are booleans (when an episode appears on only one side, the other is null) and `seed` is a number (-1 when there is no seed); `run status --json` -> `{id, status, exit_code, message}`; `run show --json` -> the Run object (status is top-level `.status`), where `.metadata.models[] {name,version,uri,as}` is which model version it read, `.metadata.outputs.model` is what it will register, and `.metadata.message` is what it is waiting for; `run ls --json` -> array of Run; `run episodes --json` -> `{summary: {total, successes, success_rate}, episodes: [{index, seed, success, steps, duration_ms, video_uri}]}`.
