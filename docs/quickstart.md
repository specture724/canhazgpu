# Team Quick Start

A practical guide for everyone sharing a GPU host with canhazgpu. It covers the commands you will use most days: `status`, `run`, `reserve`, `schedule`, `queue`, `release`, plus the team habits that keep the box productive.

## What canhazgpu does

canhazgpu is a cooperative GPU reservation system for one shared host:

- Every GPU has a **reservation** in Redis (run-type, manual, or a future booking).
- `run` ties a reservation to a process and cleans it up automatically.
- `reserve` gives you a time-boxed reservation for interactive work.
- `schedule` shows future bookings so you can plan ahead.
- `guard` (usually run as a service) warns people who use GPUs without a reservation and can enforce policy.

## 0. First-time setup (admin only)

If the host was already set up, skip this section. One-time initialization:

```bash
# NVIDIA host
canhazgpu admin --gpus $(nvidia-smi -L | wc -l)

# AMD host
canhazgpu admin --gpus $(amd-smi list --json | jq 'length')

# Huawei Ascend host (example: eight logical NPUs)
canhazgpu admin --gpus 8 --provider ascend
```

Do not run this on a busy host without `--force`; it resets all reservations.

## 1. Look before you leap: status

```bash
canhazgpu status
```

```
GPU  STATUS      USER     DURATION    TYPE    DETAILS                  MEMORY          NOTE  UTIL
---  ------      ----     --------    ----    -------                  ----------      ----  ----
0    AVAILABLE   -        -           -       free for 1h 2m 30s       1MB used        -     0%
1    IN_USE      alice    0h 15m 30s  RUN     heartbeat 5s ago, processes: PID 123 (2h3m)  8452MB, 1 processes   -     87%
2    IN_USE      bob      0h 30m 0s   MANUAL  expires in 3h 30m 0s     no usage detected  -   5%
3    UNRESERVED  carol    -           -       used by PID 123 (2h3m)  2048MB, 1 processes  -  42%
```

- `AVAILABLE` — free to reserve.
- `IN_USE` with type `RUN` — somebody's job; ends when the process ends.
- `IN_USE` with type `MANUAL` — somebody reserved it for a window.
- `UNRESERVED` — someone is using a GPU without a reservation; this GPU is excluded from allocation and `guard` may act on it.
- `⚠ FOREIGN` — GPU is reserved, but the processes belong to someone else.

By default DETAILS only shows the PID and how long the process has been running. Add `-v` for process names (up to two) or `-vv` for all:

```bash
canhazgpu status -v
canhazgpu status -vv
```

Machine-readable form for scripts:

```bash
canhazgpu status --json
canhazgpu status --json | jq -r '.[] | select(.status == "AVAILABLE") | .gpu_id'
```

## 2. Run a job: `run`

`run` is the recommended way to start anything accelerator-heavy. It reserves devices, sets `CUDA_VISIBLE_DEVICES` for NVIDIA/AMD or `ASCEND_RT_VISIBLE_DEVICES` for Ascend, runs your command, and releases the devices when the command exits. Use the `--` separator so canhazgpu does not try to parse your command's flags:

```bash
canhazgpu run --gpus 1 -- python train.py
canhazgpu run --gpus 2 -- python -m torch.distributed.launch --nproc_per_node=2 train.py
canhazgpu run --gpu-ids 2,3 -- python train.py
canhazgpu run --gpus 1 -- python            # interactive REPL works too
```

Common options:

| Option | Meaning |
|---|---|
| `--gpus N` | Number of GPUs (default 1) |
| `--gpu-ids 1,3` | Specific GPUs; waits until exactly those are free |
| `--timeout 4h` | Kill the job (SIGINT, then SIGKILL) after 4h |
| `--wait 1h` | Wait up to 1h in the queue, then fail |
| `--nonblock` | Fail immediately if GPUs are not available |
| `--note "..."` | Label the reservation; shows up in `status` |

If GPUs are busy, `run` waits in a first-come-first-served queue by default and prints progress. When your turn comes after waiting, the terminal is notified (desktop toast via OSC 777/9 when supported, otherwise a bell). Immediate allocations do not notify. Ctrl+C while waiting removes your queue entry.

```bash
canhazgpu queue                # see who is waiting
canhazgpu run --wait 30m --gpus 2 -- python train.py
```

Best practice for a long job:

```bash
canhazgpu run --gpus 2 --timeout 12h --note "bert-finetune" -- \
  python train.py --epochs 100 --save-every 10
```

## 3. Reserve for interactive work: `reserve`

Use `reserve` when you need devices for a while without running a single command: notebooks, debugging, multi-step experiments. The reservation is **manual**: it has a duration, does not set the provider visibility variable for you, and stays until it expires, goes idle, or you release it.

```bash
# 1 GPU for 4 hours
canhazgpu reserve --gpus 1 --duration 4h

# Specific GPUs
canhazgpu reserve --gpu-ids 0,2 --duration 2h

# Reserve and set the environment in one step
export CUDA_VISIBLE_DEVICES=$(canhazgpu reserve --gpus 2 --duration 3h --short)

# Huawei Ascend
export ASCEND_RT_VISIBLE_DEVICES=$(canhazgpu reserve --gpus 2 --duration 3h --short)

jupyter notebook
```

Release it as soon as you are done:

```bash
canhazgpu release                 # release all your manual reservations
canhazgpu release --gpu-ids 0,2   # release specific GPUs (also works for run-type)
```

Notes:

- The default duration is 30m — always pass `--duration` explicitly.
- By default a manual reservation that shows **no GPU usage for 15 minutes** is released automatically (`--idle-timeout`, `0` disables). This is a feature: forgotten reservations return to the pool. If you are loading a big model before training, give yourself more room: `--idle-timeout 1h`.
- `release` without `--gpu-ids` releases **all** of your manual reservations, so scripts should use `--gpu-ids`.

## 4. Plan ahead: `schedule`

Book a future time window like a meeting room with `reserve --start`, then look at the day with `schedule`:

```bash
canhazgpu schedule                       # today's grid
canhazgpu schedule --date tomorrow       # tomorrow
canhazgpu schedule --days 5              # the rest of the week

# 2 GPUs from 14:00 to 16:00 today
canhazgpu reserve --start 14:00 --end 16:00 --gpus 2 --note "demo"

# 4 hours from tomorrow 09:30
canhazgpu reserve --start 'tomorrow 09:30' --duration 4h --gpus 8

# Cancel your booking
canhazgpu schedule --cancel 4f89853e
```

How bookings behave:

- GPUs are chosen at booking time and shown on the schedule.
- While a booking is pending, ad-hoc `run`/`reserve` requests **avoid** those GPUs (30 minutes ahead for `run` by default).
- When the window starts, the booking **releases whoever currently holds the GPU**. It releases the reservation, not the process — warn people before booking over their work.
- `--end` is relative to `--start`, so `--start 22:00 --end 02:00` works.
- Cancel early if plans change; cancelling an active booking releases its GPUs immediately.

## 5. Check history: `report`

```bash
canhazgpu report          # last 30 days
canhazgpu report --days 7
```

## 6. Know the watchdog: guard and violations

On a managed host, `canhazgpu guard` usually runs as a service. It:

- warns people who use GPUs without a reservation (writes into their terminal);
- with `--enforce`, terminates them after repeated warnings (SIGINT → SIGTERM → SIGKILL);
- keeps the system tidy by activating bookings and releasing stale reservations.

See what it has caught:

```bash
canhazgpu violations              # currently open violations
canhazgpu violations --history    # resolved ones, last 7 days
```

If your job shows up there, you forgot to reserve — do not argue with the process that is now telling you politely.

## 7. Watch on a screen: `web`

```bash
canhazgpu web --port 8080
# open http://localhost:8080
```

The dashboard shows live status, the queue, and reports, and it refreshes automatically.

## Decision table

| What you need | Use |
|---|---|
| Run one command on GPUs now | `canhazgpu run --gpus N -- ...` |
| Interactive session, notebooks, multi-step work | `canhazgpu reserve --duration ...` |
| A specific GPU | add `--gpu-ids 0,2` |
| GPUs at a future time | `canhazgpu reserve --start ... --end ...` |
| See who is waiting | `canhazgpu queue` |
| See who is using what now | `canhazgpu status` |
| See usage history | `canhazgpu report --days N` |
| See policy violations | `canhazgpu violations` |
| Release manual GPUs | `canhazgpu release [--gpu-ids ...]` |

## Best practices

### Batch jobs: prefer `run`

`run` handles reservation, heartbeat and cleanup for you, and it keeps working over SSH disconnects (the supervisor survives SIGHUP). There is no need to wrap the command in `nohup`:

```bash
# Good
canhazgpu run --gpus 2 --timeout 12h --note "finetune-7b" -- ./train.sh

# Unnecessary — nohup adds nothing here
canhazgpu run --gpus 2 -- nohup ./train.sh
```

Use `--timeout` instead of hoping the system kills a runaway job.

### Manual reservations: keep them short and precise

```bash
# BAD: default duration, released only when it expires or goes idle
canhazgpu reserve --gpus 1

# GOOD: explicit window, explicit idle grace
canhazgpu reserve --gpus 1 --duration 2h --idle-timeout 30m --note "debugging"
```

### Scripts: fail fast, clean up precisely

```bash
#!/usr/bin/env bash
set -euo pipefail

# For a single command, run does everything:
canhazgpu run --gpus 2 -- python train.py --epochs 10

# For a multi-step session, reserve explicitly and release by GPU ID:
export CUDA_VISIBLE_DEVICES=$(canhazgpu reserve --gpus 2 --duration 3h --short)
trap 'canhazgpu release --gpu-ids "$CUDA_VISIBLE_DEVICES"' EXIT

python preprocess.py
python train.py
python evaluate.py
```

### Automation: use JSON, not table parsing

```bash
# Wait until at least one GPU is available, then run
while ! canhazgpu status --json | jq -e 'any(.[]; .status == "AVAILABLE")' > /dev/null; do
  sleep 30
done
canhazgpu run --gpus 1 -- python train.py
```

### Team etiquette

1. Check `canhazgpu status` before starting anything.
2. Always go through `run` or `reserve` — unreserved use shows up as `UNRESERVED`, is excluded from allocation, and triggers guard warnings.
3. Reserve what you need, not more, and release early.
4. Tell the team when a booking will preempt someone's GPU.
5. Use `--note` so others can see what a reservation is for.

## Defaults you can set for yourself

Put shared preferences in `~/.canhazgpu.yaml` instead of typing flags every time:

```yaml
memory:
  threshold: 100        # MB above which a GPU counts as "in use"
run:
  timeout: "8h"          # default timeout for run jobs
reserve:
  duration: "2h"         # your default reservation length
  idle-timeout: "30m"
```

## Troubleshooting

| Symptom | Fix |
|---|---|
| `Not enough GPUs available` | `canhazgpu status`, then `canhazgpu queue`; wait or reduce the request |
| Command failed | GPUs are released automatically; check the command output |
| `failed to connect to Redis` | `redis-cli ping`; make sure Redis is up and config points at the right host |
| Booking over your GPU | Planned in advance; the booking releases the reservation when it starts |
| Guard warned your process | You used a GPU without a reservation; reserve next time |

## Next steps

- [Commands overview](commands.md)
- [Running jobs in detail](usage-run.md)
- [Manual reservations in detail](usage-reserve.md)
- [Scheduled bookings in detail](usage-schedule.md)
- [Guard and enforcement](features-guard.md)
- [Configuration](configuration.md)
