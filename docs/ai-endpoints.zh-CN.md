[English](ai-endpoints.md) | 简体中文

# AI 友好层

设计思路：与其让 AI 调五个通用接口再自己拼上下文，不如由 server 把常见问题预先拼好，返回一个短
JSON。字段名稳定、简短；只有请求头带 `X-Include-Summary: true`（或 `?summary=1`）时，才额外返回
自然语言 `summary`。CLI 总是带上这个请求头。

| CLI | API | 用途 |
|---|---|---|
| `mldojo ai brief <run>` | `GET /api/v1/ai/runs/{id}/brief` | 状态、进度/ETA（参数里有 `max_steps`/`steps`… 时）、最新指标、异常、best_ckpt |
| `mldojo ai free-nodes` | `GET /api/v1/ai/nodes/free` | 在线节点中空闲 GPU（没有 mldojo run、利用率 <10%、显存占用 <1GiB） |
| `mldojo ai submit '<json>'` | `POST /api/v1/ai/runs` | 宽松提交：`{project, exp?, target, cmd, image?, gpus?, seeds?, params?, env?, repo?, ref?}` |
| `mldojo ai summarize-exp p/e` | `GET /api/v1/ai/experiments/{p}/{e}/summary` | 按主指标排名，给出最佳 run 和下一步建议 |
| `mldojo ai anomaly-check <run>` | `POST /api/v1/ai/anomaly-check/{run}` | 扫描一次异常，并记为 run event |

## 主指标

依次选 `val/loss`、`val_loss`、`eval/loss`、`loss`、`train/loss`；都没有时，取名字匹配
loss/err/mse/ppl 的 key（越小越好）或 reward/acc/success/return（越大越好）。

## 异常

| 标记 | 规则 |
|---|---|
| `nan_<key>@step_N` | 出现 NaN 或 Inf |
| `spike_<key>@step_N` | loss 类指标的最新值 > 前 50 个点中位数的 3 倍 |
| `diverging_<key>` | loss 类指标后半段均值 > 前半段均值的 1.5 倍 |
| `stalled_metrics_<m>m` | run 仍在运行，但超过 30 分钟没有新指标 |
| `oom_cuda` `oom_host` `import_error` `file_not_found` `nccl_error` `python_exception` | 失败 run 的 stderr 末尾命中对应模式 |

## 示例

```bash
$ mldojo ai brief 61dda6ff
Run succeeded, loss 0.0595 at step 30, 100% done, on node:gpu-a for 6s.
{ "id": "...", "status": "succeeded", "progress": 1, "latest_metrics": {"loss": 0.0595, ...},
  "anomalies": [], "best_ckpt": {"step": 30, "metric": 0.0595, "uri": "node://gpu-a/.../model_step30.ckpt"}, ... }

$ mldojo ai submit '{"project":"x","target":"node:gpu-a","cmd":"python train.py --seed $SEED","seeds":[0,1,2]}'
```

所有命令都支持 `--json`，exit code 固定为：0 成功、2 用户错误、3 不可达、4 后端错误、5 冲突。
