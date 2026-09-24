[English](recipes.md) | 简体中文

# Recipe（mldojo/v1）

Recipe 用 YAML 声明一次实验，完整示例见 `recipes/examples/dp-pusht.yaml`。解析器位于
`recipes/`，JSON Schema 位于 `recipes/schema/recipe.schema.json`（在文件开头加
`# yaml-language-server: $schema=...` 即可在编辑器里校验）。未知字段会直接报错。

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

## 代码

| source | 行为 |
|---|---|
| `local` | CLI 打包 `code.path`（相对 recipe 文件，默认 `.`）。在 git 仓库内时按 `.gitignore` 过滤；否则跳过 `.git`、`node_modules`、`outputs` 等目录，并遵守 `.mldojoignore` |
| `git` | recipe 位于某个 checkout 内时，打包整个仓库，同时记录 HEAD commit 和脏工作区 patch（含 untracked 文件）；不在 checkout 内时，由 agent 去 clone `repo@ref` |
| `inline-patch` | agent clone `repo@ref`，再 `git apply` 上 `code.patch` |

`run.workdir` 相对于代码根目录（`git` 模式下是仓库根目录）。

## overrides

`when` 中所有非空字段都必须匹配：`backend_kind`（node|queue）、`backend`（队列插件名，如 `mock`，
或节点 id；见 [queue-plugins.zh-CN.md](queue-plugins.zh-CN.md)）、`backend_id`、`target`、`labels`（节点或队列要包含全部这些 label）。`use` 在 default
基础上浅合并；env 的 `type` 改变时整体替换。

## 参数与矩阵

- `${name}` 替换为参数值；也可以用 `${project}`、`${experiment}`。其他 `${VAR}` 原样保留，交给 shell。
- `mldojo run submit --matrix seed[,lr]` 对列出的列表参数做笛卡尔积，`--matrix all` 表示展开所有列表参数。
  没有列入矩阵的列表参数取第一个值。
- `--param k=v` 覆盖参数，值按 YAML 解析，例如 `--param seed=[0,1]`。
- 每个参数也会以环境变量 `MLDOJO_PARAM_<NAME>` 的形式传给进程。

## 模型：登记与引用

血缘的两个方向都不该需要额外动作。

- **`outputs.model: <name>` 或 `<project>/<name>`**：run 成功时自动登记一个版本。
  取 `outputs.checkpoints` 采集到的 artifact，按路径里的 step 分组，step 最大的那组就是这个版本；
  一组里多个文件（分片 checkpoint）登记它们共同的目录，单个文件就登记这个文件。
  run 的 latest metrics 会一并快照下来，即使 run 被删掉版本也还站得住。
  需要同时写 `outputs.checkpoints`。

- **`models: [{name, version, as}]`**：按名字引用，解析成路径填进参数，`cmd` 里照常写 `${ckpt}`。
  `name` 不带 `/` 时属于本 recipe 的 project。`version` 可以是版本号，或 `latest`（默认）、
  `production`、`staging`。`as` 默认是 `model`，不能和 `run.params` 里已有的键重名。
  引用的模型没注册、或版本不存在，在 submit 时就报错（exit 2），不会跑到一半才发现。
  artifact 是 `node://<node>/<path>` 时会还原成节点上的路径；目标节点对不上会直接拒绝。

于是两个方向都问得出来：`mldojo run show` 显示这个 run 读了哪个版本、会登记成什么，
`mldojo model show` 显示每个版本被哪些 run 评过。`run rerun` 回放的是**引用**（钉死在当时那个版本），
不是当时解析出来的路径。

## 运行时环境变量

`MLDOJO_RUN_ID`、`MLDOJO_RUN_TOKEN`、`MLDOJO_PROJECT`、`MLDOJO_EXPERIMENT`、`MLDOJO_TARGET`、
`MLDOJO_API_URL`、`MLDOJO_METRICS_FILE`（SDK 写这里）、`MLDOJO_DATASET_<NAME>`、`MLDOJO_WORKDIR`、
`CUDA_VISIBLE_DEVICES`，以及节点 proxy 设置对应的 `http(s)_proxy`。

## 指标

- **文件扫描（默认，无需改代码）**：jsonl 每行一个对象。step 依次取 `step`、`_step`、`global_step`、
  `iteration`、`iter`、`epoch` 中第一个存在的，都没有则用行号；其余数值字段都算指标，嵌套对象用 `/`
  拼成 key。tensorboard 支持 `simple_value` 和标量 tensor（TF2）。
- **SDK**：`import mldojo; mldojo.log({"loss": x}, step=i)`（agent 会自动把 SDK 放进 PYTHONPATH；
  其他环境可用 `pip install sdk/python`）。
- **wandb**：设置 `run.wandb: shim` 后，`wandb.init/log/finish` 会写入 MLDojo。
