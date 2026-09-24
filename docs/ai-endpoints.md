English | [简体中文](ai-endpoints.zh-CN.md)

# AI-friendly layer

The idea: instead of having an AI call five generic endpoints and assemble the context itself, the server
pre-assembles the answers to common questions and returns a short JSON. Field names are stable and short;
a natural-language `summary` is added only when the request carries the header `X-Include-Summary: true`
(or `?summary=1`). The CLI always sends this header.

| CLI | API | Purpose |
|---|---|---|
| `mldojo ai brief <run>` | `GET /api/v1/ai/runs/{id}/brief` | Status, progress/ETA (when params include `max_steps`/`steps`…), latest metrics, anomalies, best_ckpt |
| `mldojo ai free-nodes` | `GET /api/v1/ai/nodes/free` | Idle GPUs on online nodes (no mldojo run, utilization <10%, memory used <1GiB) |
| `mldojo ai submit '<json>'` | `POST /api/v1/ai/runs` | Loose submit: `{project, exp?, target, cmd, image?, gpus?, seeds?, params?, env?, repo?, ref?}` |
| `mldojo ai summarize-exp p/e` | `GET /api/v1/ai/experiments/{p}/{e}/summary` | Ranks runs by the primary metric, gives the best run and suggested next steps |
| `mldojo ai anomaly-check <run>` | `POST /api/v1/ai/anomaly-check/{run}` | Scans for anomalies once and records the result as a run event |

## Primary metric

Tried in order: `val/loss`, `val_loss`, `eval/loss`, `loss`, `train/loss`. If none exists, the first key whose
name matches loss/err/mse/ppl (lower is better) or reward/acc/success/return (higher is better).

## Anomalies

| Flag | Rule |
|---|---|
| `nan_<key>@step_N` | A NaN or Inf appeared |
| `spike_<key>@step_N` | The latest value of a loss-like metric > 3x the median of the previous 50 points |
| `diverging_<key>` | Mean of the second half of a loss-like metric > 1.5x the mean of the first half |
| `stalled_metrics_<m>m` | The run is still running but has had no new metrics for more than 30 minutes |
| `oom_cuda` `oom_host` `import_error` `file_not_found` `nccl_error` `python_exception` | The tail of a failed run's stderr matches the corresponding pattern |

## Example

```bash
$ mldojo ai brief 61dda6ff
Run succeeded, loss 0.0595 at step 30, 100% done, on node:gpu-a for 6s.
{ "id": "...", "status": "succeeded", "progress": 1, "latest_metrics": {"loss": 0.0595, ...},
  "anomalies": [], "best_ckpt": {"step": 30, "metric": 0.0595, "uri": "node://gpu-a/.../model_step30.ckpt"}, ... }

$ mldojo ai submit '{"project":"x","target":"node:gpu-a","cmd":"python train.py --seed $SEED","seeds":[0,1,2]}'
```

Every command supports `--json`, and exit codes are fixed: 0 success, 2 user error, 3 unreachable,
4 backend error, 5 conflict.
