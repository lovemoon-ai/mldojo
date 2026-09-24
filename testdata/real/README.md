# Real artifacts

Files produced by real jobs on real machines, kept verbatim as test fixtures.

**The rule: a new parser is tested against this directory before it ships.**

Seventeen Go packages were green and 190 end-to-end assertions passed, and
then the first real task found a dozen bugs — because the synthetic fixtures
wrote two metric keys where the real job writes 335, and put the manifest
beside the videos where the real harness puts it two directories away.
A synthetic sample can only confirm the shapes we already thought of.

| File | Where it came from | What it is there to catch |
|---|---|---|
| `events.out.tfevents` | h20, LingBot-VLA-v2-6B training, first 64 KB of a 3.8 MB file | 46 real tag names across 23 steps, and a file cut mid-record — which is what a scanner tailing a live run always sees |
| `manifest.csv` | h20, RoboTwin `place_empty_cup` randomized evaluation, 20 trials | `video_result` and `original_result` disagree on five rows, and the seeds skip 100016 and 100018: episode index is not seed, and neither is a row number |
| `tfevents-keys.txt` | derived from the file above | the same key set in a form a package that cannot import the agent's parser can read; a test checks it still matches |
| `nvidia-smi-query-gpu.csv` | h20 | the card the training ran on |
| `nvidia-smi-compute-apps.csv` | h20 | `[Not Found]` — the agent runs in a container and cannot resolve the host pids nvidia-smi reports, so our own training process arrives anonymous |

`datahome/<node>.txt` is what the data-home probe printed on each real node,
run against an empty home so it saw what a first deploy sees: a container
with s3fs claiming 16 EB free and a disk bind-mounted as a file, a root disk
at 97% beside two data disks, and nodes whose home is already the big disk.

Keep additions small and keep them honest: truncate, do not edit. Anything
that would name a colleague or a private path does not belong here.
