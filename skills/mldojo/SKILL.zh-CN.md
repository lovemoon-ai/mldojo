---
name: mldojo
description: 用 MLDojo 命令行（`mldojo`）管理 GPU 算力和 ML 实验：找空闲卡、写 recipe、提交训练/评测作业（含参数矩阵）、看日志/指标/GPU、按 episode 看评测成功率、对比 run、登记和引用模型版本、清理节点磁盘。只要任务涉及在 GPU 节点或队列插件上跑训练或评测，或者用户提到 mldojo、recipe、run id、「提交实验」「看看哪张卡空着」「训练跑到哪了」「评测成功率」「对比两次实验」「登记 checkpoint」，就应该用这个 skill——即使用户没说出 mldojo 这个名字，只要是「把一个实验放到集群上跑并盯住它」也用。
---

[English](SKILL.md) | 简体中文

# MLDojo CLI

MLDojo 管的是两件事：**算力**（哪些节点、哪张卡、谁占着）和**实验**（每次 run 的代码、参数、日志、指标、产物、评测结果、模型血缘）。`mldojo` 是人和 AI agent 共用的命令行；Web 界面展示的是同一份数据。

有一个设计决定贯穿所有用法，先记住它：**观测是「拉」的**。MLDojo 不要求改训练代码，而是去读作业本来就会写的文件（tensorboard event、jsonl、评测 manifest、视频），并按 glob 收集产物。所以用 MLDojo 管一个现成项目，工作量几乎全在**把 recipe 里的路径写对**上，尤其是 §2.3 的「每个 run 一个输出目录」。

---

## 0. 连接与约定

**新机器首次配置**（服务器地址 `{{MLDOJO_SERVER}}`；连不到这个地址的节点，把下面的地址换成内网 `{{MLDOJO_INTRANET_SERVER}}`）：

```bash
# 1. 装 CLI 到 ~/.local/bin（不用登录；linux/macOS，amd64/arm64）
curl -fsSL {{MLDOJO_SERVER}}/install.sh | sh
# 2. 登录：打印一个链接和校验码，在浏览器里登录、核对校验码后点「批准」，命令行自动完成
mldojo login --server {{MLDOJO_SERVER}}   # 连不到这个地址的节点：mldojo login --server {{MLDOJO_INTRANET_SERVER}}
mldojo health                             # 确认能连上；exit 3 = 连不上
# 3.（可选）装本 skill
mkdir -p ~/.claude/skills/mldojo && curl -fsS -H "Authorization: Bearer $(awk '/^token:/{print $2}' ~/.mldojo/config.yaml)" \
  {{MLDOJO_SERVER}}/SKILL.md -o ~/.claude/skills/mldojo/SKILL.md
```

- 登录拿到的是以你的名义签发的 token（90 天有效，过期或被吊销后重新 `mldojo login` 即可），存在 `~/.mldojo/config.yaml`（0600），不会显示在终端上。链接可以在任意一台电脑的浏览器里打开，不必是这台机器。
- 批准页上会显示发起登录的主机名和 IP：**不是你自己刚执行的 `mldojo login` 就点「拒绝」**，批准等于把你的权限交给那台机器。
- 脚本/CI 不走浏览器：`mldojo login --server URL --token <token>`，或者设环境变量 `MLDOJO_SERVER` / `MLDOJO_TOKEN`。
- 在 API 服务器本机上用 `http://127.0.0.1:8765`，CLI 在 `~/.local/bin/mldojo`。
- **不要在输出、日志、commit 里打印 token 或任何 secret。** 存密钥用 `mldojo secret set <ns>/<name> --stdin`，recipe 和节点配置里只写 `secret://ns/name` 引用。
- **agent 调用一律加 `--json`**：每条命令都支持，输出稳定、可解析；表格输出是给人看的，列可能会变。常用字段见 §10。
- run id 可以只写唯一前缀（比如 `d5900061`，甚至更短）。
- project 和 experiment 在第一次提交时自动创建，不用事先 `project create`。名字只能用字母、数字、`.`、`_`、`-`，并以字母或数字开头。
- 退出码有语义，脚本里据此分支：

| 码 | 含义 | 该怎么做 |
|---|---|---|
| 0 | 成功 | |
| 2 | 用户错误（recipe 写错、目标不存在、资源永远满足不了、模型未注册…） | 改输入，不要重试 |
| 3 | 连不上服务器或节点 | 查网络/节点，可以重试 |
| 4 | 后端错误；用 `--wait` 时表示有 run 没成功 | 看 `run logs --stream stderr` |
| 5 | 冲突（已存在、还在运行…） | |

---

## 1. 标准工作流

### 1.1 先看算力，再决定放哪

```bash
mldojo ai free-nodes --min-free-gb 40 --label 5090 --gpus 2   # 各 flag 可以组合，也可以都不写
mldojo node ls                                 # 节点、在线状态、每张卡的利用率
mldojo node gpu gpu-b                          # 某台机器每张卡的显存和进程；-w 持续刷新
mldojo node history bastion-gpu-1 --since 24h  # 这台机器过去一段时间在干什么（不是此刻）
```

`gpus_free` 数的是**没有 MLDojo run、且空闲显存足够**的卡（门槛由 `--min-free-gb` 决定）。卡被谁占着都一样算：集群是共享的，卡上常有同事的进程，`holders` 会把它们列出来。

### 1.2 写 recipe → dry-run → 提交

```bash
mldojo run submit -f recipe.yaml --target node:gpu-b --dry-run --matrix seed   # 每个矩阵点替换后的 cmd/workdir 都列出来，不建 run
mldojo run submit -f recipe.yaml --target node:gpu-b                # 提交，返回 run id
mldojo run submit -f recipe.yaml --target node:gpu-b --follow       # 只有一个 run 时：边跑边看日志
mldojo run submit -f eval.yaml --target node:gpu-b --matrix setting,chunk --wait   # 矩阵，等全部结束
mldojo run submit -f recipe.yaml --target pool:5090 --param lr=3e-4 --param max_steps=2000
```

- **先 dry-run。** 这些问题都会在提交时直接 exit 2：recipe 写错、目标节点不存在、`gpus`/`gpu_type`/`min_mem_gb` 在这台机器上**永远**满足不了、引用的模型没注册。比跑到一半才发现便宜得多。dry-run 的输出里能看到每个矩阵点替换后的 `cmd` 和 `workdir`，提交前逐一核对；outputs 路径不在输出里，自己按参数推一遍。
- `--matrix a,b` 对列表参数做笛卡尔积，一个点一个 run，run 名形如 `chunk=50,setting=clean`；`--matrix all` 展开所有列表参数；没进矩阵的列表参数只取第一个值。
- `--param k=v` 覆盖参数，值按 YAML 解析（比如 `--param seeds=[0,1]`）。
- `--project` / `--exp` 覆盖 recipe 里的 `metadata.project` / `metadata.name`。**只写 `--exp` 时，project 还是 recipe 里那个**，run 可能落进你没预料的项目——两个都写上。
- `--gpus N` 覆盖 `resources.gpus`。`--notes "..."` 给 run 附一句说明，以后回看有用。

| target | 含义 |
|---|---|
| `node:<id>` | 指定机器（最常用） |
| `pool:<label>[,<label>]` | 任意一台带这些 label 的在线节点，由调度器挑，如 `pool:5090`、`pool:5090,8gpu` |
| `queue:<plugin>/<queue>` | 提交到某个队列插件的队列（日志是轮询的，准实时） |
| `external:<host>` | 不是平台启动的 run，由 SDK 的 `mldojo.init()` 创建，见 §6 |

### 1.3 盯住它

```bash
mldojo run status <run>                # 一行：状态、退出码、message
mldojo ai brief <run>                  # 一次看全：进度/ETA、最新指标、异常、最好的 checkpoint、评测结果
mldojo run logs <run> -f               # 跟 stdout；--stream stderr|system|all；--tail 20000 只看最后 N 字节
mldojo run metrics <run> --json        # .keys 列出这个 run 实际有哪些指标
mldojo run metrics <run> --key training/loss,training/lr --since step:1000
mldojo ai anomaly-check <run>          # NaN、突刺、停滞、OOM
mldojo run events <run>                # 状态迁移、分到哪台机器、重试、登记了哪个模型…
mldojo run gpu <run>                   # 这个 run 占的卡，此刻的状态加上整个生命周期
```

- **`queued` 不一定是出错了。** 先看 `mldojo run show <run>` 里的 `message`，它会写明在等什么，比如 `waiting: node gpu-b has 0 of the 1 cards with 40 GB free that it needs`，或者 `node X is at its limit of 1 concurrent runs`。资源空出来后，调度器（每 15 秒检查一次）会自动启动它，不用重提——重提只会多排一个。
- 已经提交的 run 没有专门的「等它结束」命令：轮询 `run status <run> --json`，看 `.status` 什么时候变成 `succeeded` / `failed` / `cancelled`。长任务把轮询间隔拉长到几分钟。
- `ai brief` 的进度和 ETA 是从参数里的总步数算出来的，参数得叫 `max_steps` / `total_steps` / `steps` / `num_steps` / `train_steps` / `iterations` / `iters` 之一。
- 排查问题先看 `system` 流：里面是平台自己记的事——提交、派发、状态变化、为什么没重试、登记了什么。

### 1.4 看结果、做对比

```bash
mldojo ai summarize-exp lingbot/place-empty-cup-eval   # 实验内所有 run 按主指标排名 + 下一步建议
mldojo exp show lingbot/place-empty-cup-eval
mldojo run ls --project lingbot --exp place-empty-cup-eval --status succeeded
mldojo run ls --search chunk --sort duration --limit 20     # --search 匹配 run 名或实验名的子串
mldojo compare <runA> <runB>              # 两个 run：代码 diff、环境差异、参数、指标
mldojo compare <r1> <r2> <r3> <r4>        # 多个 run：参数和最终指标并排
mldojo run artifacts ls <run>             # 会先让节点重新扫一遍
mldojo run artifacts get <run> <uri> --dest ./out
```

「主指标」是自动选的：有评测成功率 `eval/success_rate` 就用它；没有的话，依次找 `val/loss`、`eval/loss`、`loss`、`train/loss`、`training/loss`；都没有就挑名字最朴素的 `*_loss`。所以**只要做了评测，就一定配上 `outputs.episodes`**。loss 在真实任务里常常说明不了什么：有一次评测，loss 从头到尾都在 0.06–0.08，成功率却从 40% 涨到了 75%。

Web 端 experiment 页的 **Matrix** 标签页会按变化的参数把 run 透视成一张表：行 = setting，列 = chunk，格子 = 成功率。跑完矩阵后把这一页发给人看，比列一串 run 清楚得多。

### 1.5 收尾

```bash
mldojo run cancel <run>...                       # 先 SIGTERM 整个进程组，15 秒后 SIGKILL
mldojo run rerun <run>                           # 同代码、同环境、同参数再跑一次
mldojo run rerun <run> --target node:gpu-a --param lr=1e-4
mldojo run rm <run>... --purge-node              # 连节点上的工作目录（含 checkpoint）一起删
mldojo node disk gpu-b                           # MLDojo 在这台机器上占了多少盘，按 run 列出
```

`--purge-node` 会删掉 checkpoint，**删之前确认它们已经没用了，或者已经登记、拷走了**。原地运行（§2.1）的产物在项目自己的目录里，MLDojo 不会去删它们。

---

## 2. Recipe

recipe 写的是四件事：跑什么、在什么环境里跑、要多少资源、产物在哪。解析是严格的：**写错字段名会直接报错**，不会被悄悄忽略。编辑器校验：在 recipe 文件顶部加一行 `# yaml-language-server: $schema=<仓库>/recipes/schema/recipe.schema.json`。

### 2.1 原地运行：代码和权重已经在节点上（最常见）

大多数真实项目**没法打包上传**：代码旁边就是几十 GB 的权重、装好的 venv 和仿真器资源。`run.workdir` 写成绝对路径，就是在原地跑，什么都不上传。

```yaml
apiVersion: mldojo/v1
kind: Experiment
metadata:
  project: lingbot
  name: place-empty-cup
  tags: [vla, robotwin]
code:
  source: none                         # 绝对路径的 workdir 默认就是 none，写出来更清楚
env:
  default: {type: none}                # 用节点上现成的环境，见 §2.4
run:
  workdir: /home/alice/lingbot     # 必须已经存在，MLDojo 不会创建它
  cmd: |
    OUTPUT_DIR="$PWD/outputs/${out}" \
    MAX_STEPS=${max_steps} MICRO_BATCH_SIZE=${batch} \
    bash scripts/train_one_step.sh
  params:
    out: b16_2k                        # 这个 run 自己的输出目录名，见 §2.3
    batch: 16
    max_steps: 2000                    # 起这个名字，ai brief 才能算出进度和 ETA
resources:
  default: {gpus: 1, gpu_type: h20, min_mem_gb: 80}
outputs:
  model: place-empty-cup               # 成功后自动登记为 lingbot/place-empty-cup 的新版本，见 §5
  checkpoints: outputs/${out}/checkpoints/**/*.safetensors
  metrics:
    - type: tensorboard                # 含 ${...} 的条目不要写成 {a: b} 的 flow 形式，见 §2.5
      path: outputs/${out}/runs
```

outputs 里的相对路径是相对 `run.workdir` 的（原地运行时就是项目目录本身），也可以写绝对路径。产物落在 workdir **之外**（比如一块共享盘）时，就必须写绝对路径；带通配的绝对 glob 从最深一层不含通配符的目录开始往下扫。

### 2.2 打包上传：小项目、本地改过的代码

```yaml
code:
  source: local        # 打包 code.path（相对 recipe 文件，默认 .），按 .gitignore / .mldojoignore 过滤
  path: .
# source: git          # recipe 在 git checkout 里时打包整个仓库，并记录 HEAD commit 和未提交的 diff
# source: git + repo/ref，recipe 不在 checkout 里时：由节点自己 clone repo@ref
# source: inline-patch # 先 clone repo@ref，再 git apply code.patch
env:
  default: {type: venv, spec: requirements.txt}   # 按文件哈希缓存；也可以是 conda（environment.yaml 或已有 env 名）/ docker（image）/ none
run:
  cmd: python train.py --lr ${lr} --seed ${seed}
  params: {lr: 3e-4, seed: [0, 1, 2]}             # 列表参数配合 --matrix seed
  setup: pip install -e .                         # 可选，在 env 里、cmd 之前执行
  env: {WANDB_MODE: offline}
outputs:
  checkpoints: outputs/**/*.ckpt                  # 每个打包上传的 run 有自己独立的目录，不会串
  metrics:
    - type: jsonl
      path: outputs/metrics.jsonl
```

用 `source: git` 换来的是可复现：run 会记下 commit 和未提交的 diff，`mldojo compare` 能直接显示两次 run 之间的代码差异。

### 2.3 每个 run 必须有自己的输出目录（原地运行时最容易出错）

**outputs 收集的是 glob 匹配到的所有文件**：不按修改时间过滤，也分不出是不是这个 run 写的。原地运行时，项目目录是所有 run 共用的——以前跑过的结果、同一个矩阵里其他点的结果，都会被收进来；`outputs.model` 甚至可能把别的 run 的 checkpoint 登记成自己的。所以：

- **每个 run（每个矩阵点）都写到只属于它的目录，目录名用参数拼出来**：`outputs/seed${seed}/...`、`eval_results/mldojo_${setting}_c${chunk}/...`。
- 脚本能接受输出目录参数时，直接把它传进去（首选）。
- 脚本自己按时间戳建目录时，在 cmd 最后把新建的目录 `mv` 到一个按参数命名的固定位置，outputs 指向那里。这是一次真实评测跑通的写法，完整示例见 §3。产物在运行中大约每 30 秒扫一次，**进程退出后还会再完整扫一次**，所以在 cmd 最后才 `mv` 也收得到。
- 这个 `mv` 的写法**只适合评测**：训练要在跑的过程中看指标，而目录挪过去之前 MLDojo 什么都看不到。训练脚本如果连输出目录都没法指定，先看它的目录名是不是由参数决定的（比如 `outputs/pusht_s${seed}_*`），是的话，glob 直接按参数去匹配；不是的话，就给脚本加一个输出目录参数或环境变量——这是「不改训练代码」这条原则值得破例的地方。
- 用 `mv`，**不要用软链接**：扫描不会跟进软链接指向的目录。
- **不要 `rm -rf` 已登记模型的 checkpoint 所在的目录。** 产物就是节点上的原始文件（§9），模型版本也只是指向它们的路径，而且版本没有删除接口。§3 里的 `rm -rf "$DEST"` 只清掉同一个评测点上一次的结果，放进训练 recipe 里用就是在删模型。
- **参数之间不能互相引用**：`out: seed${seed}` 不会被展开。路径里直接写 `${seed}`。
- outputs 里只能替换参数、`${project}`、`${experiment}`；`$MLDOJO_RUN_ID` 这类环境变量**不会**展开。
- 同一台机器上并发跑两个同类 run 时，「取最新的目录」会互相抢。单卡节点上 GPU 准入会让它们排队，不会撞；多卡节点上，要么把目录作为参数传给脚本，要么用 `mldojo node limit <id> --max-runs 1` 串行。脚本如果在工作目录里写共享文件（缓存、`latest` 软链接、锁文件），并发时也要小心。

### 2.4 作业在什么环境里跑

- cmd 由一个**非登录的 bash** 执行，用户是部署 agent 时的那个用户（`node add --ssh` 里的用户），环境变量继承 agent 进程。`env: {type: none}` 用的就是这个 PATH，**不会**自动 source `.bashrc` 或激活 conda。需要 venv/conda 时，在 cmd 或 `run.setup` 里显式写 `source .venv/bin/activate`，或者直接用解释器的绝对路径。
- 拿不准节点上的环境时，先花几秒探一下：

  ```bash
  mldojo ai submit '{"project":"scratch","target":"node:bastion-gpu-1","workdir":"/home/alice/proj","cmd":"whoami; which python; python train.py --help","gpus":0}' --json
  mldojo run logs <run>
  ```

  `ai submit` 里的 `workdir` 写绝对路径就是原地运行；`gpus: 0` 表示不占卡。

- 原地运行时，这个用户必须对项目目录有写权限。项目在别人的 home 下时，在探测命令里加一句 `test -w .`。
- `gpus: N` 表示独占 N 张卡：一张卡上有 MLDojo 的 run，就不会再分给别的 run，哪怕 `min_mem_gb` 远小于卡的显存。

作业能拿到的环境变量：`MLDOJO_RUN_ID`、`MLDOJO_PROJECT`、`MLDOJO_EXPERIMENT`、`MLDOJO_TARGET`、`MLDOJO_WORKDIR`、`MLDOJO_GPUS`（分到的卡号）、`CUDA_VISIBLE_DEVICES`、`MLDOJO_PARAM_<NAME>`（每个参数一个）、`MLDOJO_DATASET_<NAME>`（数据集路径）、`MLDOJO_RESUME_FROM`（重试时才有）、`MLDOJO_METRICS_FILE` / `MLDOJO_EPISODES_FILE`（SDK 往这里写）、`MLDOJO_API_URL` / `MLDOJO_RUN_TOKEN`，以及节点配置的 `http(s)_proxy`。

### 2.5 其余字段

```yaml
datasets:                          # 必须先 mldojo dataset register
  - {name: pusht, version: v1, mount: /data/pusht}
models:                            # 按名字引用注册表里的模型，见 §5
  - {name: place-empty-cup, version: latest, as: ckpt}
run:
  wandb: shim                      # 代码里 import wandb 写的日志会进 MLDojo
resources:
  default:
    gpus: 1
    gpu_type: "4090|5090|h20"      # 用 | 写多个备选，按 GPU 型号的子串匹配；纯数字必须加引号："5090"
    min_mem_gb: 40                 # 派发时按实时空闲显存准入，不够就排队
    wall_time_min: 240             # 超时算失败
  overrides:                       # 按 target 覆盖默认值（浅合并）
    - when: {backend_kind: queue, backend: mock}
      use: {workers: 1, gpu_per_worker: 4, cpu_per_worker: 4, cpu_mem_ratio: 4}
    - when: {labels: [h20]}
      use: {min_mem_gb: 80}
retry:                             # 不写这一节就不重试
  max: 2                           # 0–10
  on: infra                        # 默认：只重试平台弄丢的 run（节点掉线、agent 重启、派发失败）；any：进程自己非零退出也重试
  resume_from: "*.ckpt"            # 从失败那次收集到的 checkpoint 文件里挑步数最大的，作为 MLDOJO_RESUME_FROM 传给下一次
outputs:
  logs: outputs/**/*.log
  videos: /abs/path/**/*.mp4
  images: outputs/**/*.png
hooks:
  pre_submit: hooks/tweak.py       # CLI 提交时执行：从 stdin 读 recipe JSON，往 stdout 输出改过的 JSON
```

- `${name}` 替换成参数值，另外还能用 `${project}`、`${experiment}`，在 `cmd`、`workdir`、`setup`、`run.env`、outputs 的路径里都有效；其他 `${VAR}` 原样留给 shell 处理。
- **`gpu_type: 5090` 会被拒绝**（YAML 把它读成整数，报 `gpu_type must be a string`），要写成 `gpu_type: "5090"`。
- **含 `${...}` 的值不能写进 YAML 的 flow 形式 `{...}`。** 比如 `- {type: tensorboard, path: out/${run}/tb}`，`${run}` 里的 `}` 会被当成 mapping 的结束，整个 recipe 解析失败（报 `did not find expected ',' or '}'`）。改成多行的 block 写法，或者给值加引号：`path: "out/${run}/tb"`。
- `retry.on` 默认只重试平台的问题：作业自己的 bug，重试一遍还是同一个 bug。不重试时，system 日志里会写明原因。
- `resume_from` 匹配的是**文件**（`outputs.checkpoints` 收集到的产物），而且只锚定在路径末尾：`*.ckpt`、`**/*.safetensors` 都能匹配；像 `checkpoints/**/global_step_*` 这样以目录结尾的写法什么都匹配不到。另外，只有**训练脚本自己去读 `MLDOJO_RESUME_FROM`** 时它才起作用，否则重试会从头跑。
- tensorboard 的 `path` 可以是单个 event 文件，也可以是目录（会递归找文件名含 `tfevents` 的文件）。jsonl 每行一个 JSON 对象：step 依次取 `step`、`_step`、`global_step`、`iteration`、`iter`、`epoch` 中第一个存在的，都没有就用行号；其余数值字段都记成指标，嵌套对象用 `/` 连成 key。

---

## 3. 评测：结果是「N 局里成功几局」，不是一条曲线

`outputs.episodes` 指向评测 harness **本来就会写**的文件，harness 本身不用改。下面是一次真实评测跑通的 recipe：harness 每次都新建一个带时间戳的目录，而且目录名里没有 chunk，所以在 cmd 最后把它挪到一个按参数命名的目录（§2.3）。

```yaml
apiVersion: mldojo/v1
kind: Experiment
metadata: {project: lingbot, name: place-empty-cup-eval}
code: {source: none}
env: {default: {type: none}}
models:
  - {name: place-empty-cup, version: latest, as: ckpt}   # 按名字引用，不要粘路径（§5）
run:
  workdir: /home/alice/lingbot
  cmd: |
    set -euo pipefail
    DEST="$PWD/eval_results/mldojo_${experiment}_${setting}_c${chunk}"
    rm -rf "$DEST"
    EVAL_VIDEO_LOG=True bash scripts/eval_single_task.sh "${ckpt}" ${setting} ${episodes} ${chunk}   # EVAL_VIDEO_LOG 是这个 harness 录视频的开关
    latest=$(ls -td "$PWD"/eval_results/place_empty_cup_${setting}_* | head -1)
    mv "$latest" "$DEST"
  params:
    setting: [clean, randomized]
    chunk: [16, 50]
    episodes: 20
resources:
  default: {gpus: 1, min_mem_gb: 40}     # 推理服务按 fp32 加载
outputs:
  videos: eval_results/mldojo_${experiment}_${setting}_c${chunk}/**/*.mp4
  episodes:
    - type: csv                          # 列在前面的来源决定成功/失败；这里 manifest 还提供 seed
      path: eval_results/mldojo_${experiment}_${setting}_c${chunk}/manifest.csv
    - type: filenames                    # 后面的来源只补空着的字段：这里补视频
      path: eval_results/mldojo_${experiment}_${setting}_c${chunk}/episodes
```

提交：`mldojo run submit -f eval.yaml --target node:gpu-b --matrix setting,chunk --exp place-empty-cup-eval-v3`，跑完看 Matrix 页。

- **一个模型版本一个评测实验**（`--exp <名字>-v<N>`，版本号先用 `model show` 查）。这样 Matrix 页上的每一格只对应一个版本；输出目录里带着 `${experiment}`，评测新版本时也不会冲掉旧版本的视频——旧 run 的 episode 记录还指着那些文件。
- 这里用 `ls -td … | head -1` 取「最新的目录」之所以安全，是因为 **gpu-b 只有一张卡**：GPU 准入让 4 个点依次跑，不会同时建目录。在多卡节点上照搬之前，先让 harness 接受输出目录；做不到就用 `mldojo node limit` 让它串行（§2.3）。

- **csv / jsonl** 按列名识别，常见写法都认：局号 `episode|idx|ep|index`、`seed`、结果 `success|result|video_result|status|outcome|passed`（值为 `success/succeeded/ok/pass/passed/true/yes/1` 时算成功，其余一律算失败）、步数 `steps|frames|length`、时长 `duration_seconds|duration_s|duration_ms`、视频 `video|filename|video_path|file`。
- **filenames** 递归扫描 `episode12_success.mp4`、`episode3_failure.mp4` 这类文件，结果从文件名读出来，视频也就对应上了（支持 mp4/webm/mov/gif）。
- 多个来源**按局号合并成一行**：列在前面的来源决定成功/失败，后面的只补空着的字段（seed、视频、步数）。因此 glob 若匹配到多个评测目录，不同目录里同一个局号也会被并成一行——又一个每个 run 独占目录的理由。manifest 里的 `filename` 会到它所在的目录及子目录里去找。
- 汇总结果会写成指标 `eval/success_rate`，所以排名、`summarize-exp`、Matrix 页都不用专门为评测做适配。

```bash
mldojo run episodes <run>                        # 每一局：seed、成功/失败、步数、视频
mldojo run episodes <run> --result failure --json   # .episodes[].video_uri 就是失败局的视频
mldojo run artifacts get <run> <video_uri> --dest ./videos   # 把视频下载下来看
mldojo compare episodes <runA> <runB> --flipped  # 两次评测逐局对比，只列结果翻转的局
```

**看翻转的那几局，别只看成功率。** 同一个 checkpoint、同一批 seed 跑两次，可能一次 6/20、一次 5/20，真正有信息量的是哪几局翻了。`compare episodes` 按 **seed** 对齐；没有 seed 时退回按局号对齐。它适合比较**同一批 seed** 下的两次评测：两个 checkpoint 放在同一个 setting 下，或者同一个 checkpoint 跑两次。如果两个 setting 用的 seed 不一样，大部分局只会出现在一边——只出现在一边的局不算翻转，结果里 `a` 或 `b` 是 null。输出里没有视频路径：拿到局号或 seed 后，用 `run episodes --json` 查视频 URI。

---

## 4. 超参搜索（sweep）

```yaml
apiVersion: mldojo/v1
kind: Sweep
metadata: {project: smoke, name: lr-search}
recipe: recipe.yaml          # 相对这个文件
target: pool:5090
method: random               # random | grid（grid 要求每个轴都写 values）
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
mldojo sweep show smoke/lr-search      # 排行榜
mldojo sweep ls --status running
mldojo sweep stop smoke/lr-search      # 不再启动新的 run，已经在跑的继续跑完
```

一次训练动辄几小时、checkpoint 几十 GB 时，sweep 很少划算；更常用的是一个小而密的对照矩阵（`--matrix`）。

---

## 5. 模型血缘：跑完就登记，用名字引用

**登记。** 训练 recipe 里写 `outputs.model: <name>`（或 `<project>/<name>`），run **成功的那一刻**就会登记一个新版本。system 日志里会出现 `registered <p>/<n>@<v> -> <uri>`，`run events` 里会多一条 `model_registered`。具体规则：

- 取 `outputs.checkpoints` 收集到的文件，按路径里的步数分组，**步数最大**的那组就是这个版本（不是指标最好的那组）。步数取的是完整路径里**第一个** `step|iter|epoch|ckpt|checkpoint` 后面的数字（中间最多隔一个 `_` 或 `-`，`global_step_2000`、`epoch_12.pt` 都行；`ckpt/epoch_12.pt` 里的 `ckpt/` 后面不是数字，不会误匹配）。所以 checkpoint 上层的目录名里不要再出现 `max_step_20000` 这种组合，否则每个 checkpoint 都会被读成同一个步数。
- 一组里有多个分片文件时，登记的是它们共同的目录（比如 `.../global_step_2000/hf_ckpt`）；只有一个文件时就登记这个文件。run 当时的最新指标会一起快照下来。
- `outputs.model` 必须和 `outputs.checkpoints` 一起写。run 成功了但 glob 一个文件都没匹配到时，run 仍然算成功，只是不登记；system 日志会写 `outputs.model <name>: no checkpoints were collected, nothing to register`。
- **多个 run 登记到同一个模型时**（比如 3 个 seed），每个成功的 run 各得一个版本，版本号按成功的先后排，`latest` 就是最后成功的那个。`model show` 会列出每个版本来自哪个 run、当时的指标是多少。训练 loss 往往分不出好坏，通常先把几个版本都评一遍，再把挑中的 `promote` 成 production；之后的评测写 `version: production`，意思就不会含糊。

**引用。** 评测 recipe 里不要粘 checkpoint 路径，按名字引用：

```yaml
models:
  - name: place-empty-cup        # 不带 / 就属于本 recipe 的 project
    version: latest              # 版本号 | latest（默认）| production | staging
    as: ckpt                     # 填进 ${ckpt}；默认参数名是 model；不能和 run.params 里的键重名
```

`${ckpt}` 会解析成节点上的普通路径：分片的 checkpoint 是目录，单个文件就是文件本身，所以 harness 要能接受对应的形式。dry-run 的 cmd 里就能看到解析出来的路径；真正提交后，`run show` 的 `.metadata.models[].version` 记的是解析后的版本**号**，而不是 `latest` 这个字面值。引用的模型没注册、版本不存在都会**在提交时 exit 2**；产物在另一台节点上、而 target 是 `node:<这台>` 时也会直接拒绝（这台机器读不到那个路径）。想评测别的版本就改 `version:`；`--param ckpt=...` 会被拒绝，否则 run 会声称读了某个版本，实际读的却是另一个路径。

```bash
mldojo model ls [--project lingbot]
mldojo model show lingbot/place-empty-cup         # 所有版本，外加 USED BY：每个版本被哪些 run 评过
mldojo run show <run>                             # model：这个 run 读的哪个版本；registers：跑完会登记成什么
mldojo model promote lingbot/place-empty-cup 3 --stage production   # production 和 staging 各只有一个，旧的转为 archived
mldojo model register lingbot/place-empty-cup --run <run> --uri node://gpu-b/<path>   # 手动补登记
```

这样两个方向都能回答：「这个 25% 是哪个 checkpoint 跑出来的」（`run show`），以及「这个 checkpoint 被评过几次、分别是多少」（`model show`）。`run rerun` 会按引用重放、锁定当时的版本号，所以重跑用的是同一份权重。**版本目前没有删除接口**，手动 `model register` 之前想清楚。

---

## 6. 在代码里上报（可选）

§2.5 那种不改代码、由平台去读文件的方式（tensorboard / jsonl 扫描）够用时，就别动代码。确实需要时：

```python
import mldojo                                   # 在 run 里，agent 已经把 SDK 放进 PYTHONPATH；别处用 pip install <仓库>/sdk/python
mldojo.init(project="demo", config={"lr": 3e-4})   # 可选；在 MLDojo 之外手动跑时，会创建一个 external: run
mldojo.log({"loss": 0.31, "train": {"acc": 0.9}}, step=1200)   # 嵌套字典会拼成 train/acc
mldojo.log_episode(success=True, seed=100003, steps=160, video="ep3.mp4")
mldojo.summary["best_val"] = 0.12
mldojo.finish()
```

没有服务器可连时，每个调用都是空操作，所以同一个脚本在 MLDojo 里外都能跑。设置 `run.wandb: shim` 后，`wandb.init/log/finish` 会写进 MLDojo。

---

## 7. 算力账与限额

```bash
mldojo usage --by project --since 168h     # 按 project | run | submitter | node 统计 GPU 小时
mldojo gpu idle --since 24h                # 被 run 占着却没怎么用的卡（--min-hours 0.5）
mldojo node limit gpu-b --max-runs 1       # 一台机器同时最多跑几个 run（0 = 不限）
mldojo project limit lingbot --max-runs 4  # 一个项目同时最多跑几个 run
```

时间窗口可以写 `24h`、`90m` 这样的时长，也可以写 RFC3339 时间。按天写（`7d`，也是 `usage` 的默认值）要求服务端包含 commit `25133ad`；更早的服务端会拒绝，连不带参数的 `mldojo usage` 都会报错。拿不准就换成小时（`168h`），哪个版本都认。

准入分两步：提交时按机器的静态规格检查，**永远**满足不了的直接拒绝；派发时在节点锁里按**实时空闲显存**再查一次，不够就排队并写明原因。已经准入、但进程还没起来的 run 也计入占用，所以不会超卖。

---

## 8. 管理类命令（一般只有管理员用）

```bash
mldojo node add --id gpu-a --ssh alice@192.0.2.11 --labels 5090 --dry-run   # 只测连通性和探测节点，不部署
mldojo node add --id bastion-gpu-1 --ssh "user@host" --port 2222 --identity secret://ssh_keys/id_ed25519 \
    --via gpu-a --via local --labels 5090,8gpu,bastion
mldojo node add --id x --ssh user@host --reverse-tunnel     # 节点连不到服务器时，走 SSH 反向隧道回连
mldojo node test <id>                     # SSH 可达性 + agent 健康
# node add 时，如果节点上还没有 ~/.mldojo，会把它建在空闲最多的数据盘上属于该用户的目录里（<盘>/<user>/mldojo），
# home 下只留软链接；--dry-run 会先显示打算放在哪。已有 ~/.mldojo 的节点不动；想留在 home，部署前先 mkdir ~/.mldojo。
mldojo node upgrade <id>...               # 重新部署 agent，正在跑的作业会被重新接管
mldojo node show <id>
mldojo node rm <id> --force               # --keep-agent 保留节点上的 agent 进程
mldojo dataset register --name pusht --version v1 --mount /data/pusht \
    --location node:gpu-a:/data/pusht --authoritative --location bucket:<provider>/<bucket>/<path>
mldojo dataset push pusht@v1 --to node:bastion-gpu-1 # 预热某台节点的数据集缓存
mldojo dataset ls
mldojo dataset rm pusht@v1                           # 只取消登记，不删文件
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

## 9. 踩过的坑（都真实发生过）

- **原地运行时，多个 run 的输出串到一起**：见 §2.3。这是最常见、后果也最隐蔽的错误——指标、评测结果和登记的模型会悄悄变成别人的。
- **flow 写法的 YAML 里不能放 `${...}`**：见 §2.5。
- **绝对路径的 workdir 必须已经存在**，而且意味着 `code.source: none`；和 `local`/`git` 一起写会报错。
- **`env: none` 不会帮你激活环境**：见 §2.4。第一次在某台节点上跑，先探一下（顺便 `test -w .` 看有没有写权限）。
- **`gpu_type: 5090` 要加引号**：见 §2.5。
- **一定要写 `min_mem_gb`**：共享集群的卡上常有别人几十 GB 的进程。写了，run 会排队等卡；不写，run 会被派到快满的卡上，然后 OOM。
- **run 卡在 `queued`**：看 `run show` 的 message，别重提。
- **容器里的节点查不到进程属主**：nvidia-smi 报的是宿主机的 PID，GPU 表里显示 `runid ?`，意思是「推断属于这个 run」。这是正常的。
- **产物不会被拷走**：产物的 URI 是 `node://<节点>/<绝对路径>`，也就是节点上的原始文件；`run artifacts get` 是按需经 agent 传回来的。节点被清理，产物也就没了。
- **`--wait` 时只要有一个 run 没成功就 exit 4**：矩阵里挂一个，整条命令都算失败。
- **续跑只做了一半**：训练脚本自己读 `MLDOJO_RESUME_FROM`，重试才会从 checkpoint 接着跑。
- **`--exp` 不改 project**：拿别人的 recipe 提交时，加上 `--project <你的项目>`。

---

## 10. 给 agent 的推荐节奏与 JSON 字段

1. `mldojo health --json` → `mldojo ai free-nodes --min-free-gb <需要的显存> --json`，挑一个 `gpus_free` 够用的节点，用它的 `target` 字段。
2. 写 recipe（能原地运行就原地运行，每个 run 一个输出目录）；第一次用某台节点时先用 `ai submit` 探一下环境；然后 `--dry-run --json` 核对每个点的 `cmd` 和 `workdir`。
3. `run submit ... --json`，从 `.runs[].id` / `.runs[].name` 记下每个 run。
4. 轮询 `mldojo ai brief <run> --json`：一次就拿到 `.status`、`.progress`、`.eta`、`.latest_metrics`、`.anomalies`、`.eval.success_rate`、`.best_ckpt.uri`，不要每次分别去调 logs 和 metrics。
5. 失败时：`mldojo run logs <run> --stream stderr --tail 20000`，再看 `--stream system`。
6. 结束后：`mldojo ai summarize-exp <p>/<e> --json`，从 `.ranking[] {id,name,status,value,params}` 和 `.metric` 拿排名；评测用 `compare episodes --flipped --json`（`.flipped`，`.episodes[] {seed,index,a,b}`）；写了 `outputs.model` 的，用 `model show <p>/<n> --json` 确认 `.versions[0]` 是这个 run 登记的。
7. 向人汇报时带上 run id、实验名和 Web 链接：`{{MLDOJO_SERVER}}/experiment?project=<p>&name=<e>`（矩阵结果在 Matrix 标签页）。

其他常用字段：`run submit` 和 `ai submit` 的 `--json` 都是 `{experiment, runs: [Run]}`；`ai free-nodes --json` → 数组 `[{node_id, target, gpus_free, gpus_total, gpus_held, gpu_model, max_free_gb, labels, holders}]`；`ai brief` 的 `.progress` 是 0–1 的小数；`model show --json` → `{model, versions: [{version, stage, run_id, uri, sha256, size_bytes, metrics, notes, created_at}], used_by: [{version, run_id, project, experiment, name, status}]}`，`versions` 按新到旧排；`compare episodes --json` → `{a, b, flipped, episodes: [{seed, index, a, b}]}`，其中 `a`/`b` 是布尔值（这一局只出现在一边时，另一边是 null），`seed` 是数字（没有 seed 时为 -1）；`run status --json` → `{id, status, exit_code, message}`；`run show --json` → Run 对象（状态是顶层的 `.status`），其中 `.metadata.models[] {name,version,uri,as}` 是读了哪个模型版本，`.metadata.outputs.model` 是会登记成什么，`.metadata.message` 是在等什么；`run ls --json` → Run 数组；`run episodes --json` → `{summary: {total, successes, success_rate}, episodes: [{index, seed, success, steps, duration_ms, video_uri}]}`。
