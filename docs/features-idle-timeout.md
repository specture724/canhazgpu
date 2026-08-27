# Idle Reservation Timeout

A reservation that nobody uses is worse than no reservation at all: the GPU sits idle while other people queue for it. canhazgpu therefore releases **manual reservations** that show no GPU activity for a configurable period — 15 minutes by default.

## How it works

```bash
# Default: released after 15 minutes without GPU usage
canhazgpu reserve --gpus 1 --duration 8h

# Longer grace period for a job with a slow startup
canhazgpu reserve --gpus 1 --duration 8h --idle-timeout 1h

# No idle detection at all
canhazgpu reserve --gpus 1 --duration 8h --idle-timeout 0
```

The reservation is released when *both* of these hold:

1. No GPU usage above the memory threshold has been observed **from the reservation holder** (the same `--memory-threshold` used by [GPU validation](features-validation.md), default 100 MB)
2. That has been true for longer than the idle timeout

The clock starts when the reservation is created, so you always get the full grace period to launch your work. Every time canhazgpu observes the holder's own usage on the GPU, the clock resets.

## Usage is attributed to its owner

Only the holder's own processes keep a reservation alive. If somebody else's job is burning memory on a GPU you reserved, the reservation is still considered idle:

```text
❯ canhazgpu status
 GPU │ STATUS      │ USER  │ DURATION │ TYPE   │ DETAILS
─────┼─────────────┼───────┼──────────┼────────┼──────────────────────────────────────────────────────
 0   │ ⚠ FOREIGN   │ alice │ 1h 0m 0s │ MANUAL │ expires in 59m, idle 10m, released in 5m, used by bob (8452MB)
```

Without this, a squatter would keep the reservation alive on the holder's behalf forever: the GPU looks busy, so the idle timer never fires, yet the person who reserved it cannot use it. Attributing usage means the reservation is reclaimed on schedule, after which the squatter's processes show up plainly as `UNRESERVED` and [the guard](features-guard.md) escalates against them.

Ownership is compared against the reservation's **OS account**, so `--user "Alice Smith"` display names do not confuse it. Where usage cannot be attributed — no process list from the driver, or a process whose owner cannot be determined — the reservation gets the benefit of the doubt and is left alone.

```text
❯ canhazgpu status
Released GPU 3 reserved by alice: no GPU usage detected for 16m (idle timeout 15m)
 GPU │ STATUS      │ USER  │ DURATION  │ TYPE   │ DETAILS
─────┼─────────────┼───────┼───────────┼────────┼──────────────────────────────────────────────
 0   │ ● IN_USE    │ alice │ 0h 32m 0s │ MANUAL │ expires in 7h 28m 0s, idle 4m, released in 11m
 3   │ ● AVAILABLE │ -     │ -         │ -      │ free for 0h 0m 0s
```

The `status` DETAILS column shows how long a reservation has been idle and when it will be released, so there is no surprise.

## Run reservations get the same protection

`run` reservations are no longer exempt. When you launch a command with `canhazgpu run`, the supervisor watches the reserved GPUs and releases the reservation when no GPU usage is detected for the idle timeout — 30 minutes by default:

```bash
# Default: released after 30 minutes without GPU usage
canhazgpu run --gpus 1 -- python train.py

# Longer grace period for a job with a slow startup
canhazgpu run --gpus 1 --idle-timeout 2h -- python train.py

# No idle detection (e.g. interactive sessions that pause for long stretches)
canhazgpu run --gpus 1 --idle-timeout 0 -- python train.py
```

This closes the gap where a wrapper shell outlived its GPU process — for example a vLLM process that crashed and became a zombie while `bash script.sh` kept running. Previously the supervisor kept sending heartbeats and the GPUs stayed reserved forever. Now the supervisor itself notices there is no holder usage and releases the GPUs, even on a quiet host where nobody runs `canhazgpu status` to trigger cleanup.

The same rules apply as for manual reservations:

- Only the holder's own processes reset the clock; somebody else's usage keeps counting as idle.
- The clock starts when the reservation is created, so the job always gets the full grace period to start touching the GPU.
- If usage cannot be checked or attributed, the reservation gets the benefit of the doubt and is never released on a guess.
- `status` shows the idle countdown on run reservations too.

## What is not affected

**Reservations without an idle timeout are exempt.** That includes reservations created before this feature existed, `canhazgpu run --idle-timeout 0`, and any reservation whose stored timeout is zero. They stay tied to the lifetime of their process.

**Reservations are only released when usage can actually be checked.** If `nvidia-smi`/`amd-smi` cannot be queried, idle detection is skipped for that pass rather than guessed at. The same applies to usage that cannot be attributed to an owner.

**Memory counts as usage.** A GPU holding a loaded model with no active kernel is "in use" as far as canhazgpu is concerned — this matches how the rest of the tool defines usage. Lower `--memory-threshold` if you want stricter accounting.

## When the check runs

Idle detection is part of the maintenance pass that every canhazgpu command performs before it looks at or hands out GPUs: `status`, `reserve`, `run`, `queue`, `schedule`, and the refresh loop of `canhazgpu web`. On a machine where people regularly run canhazgpu, idle reservations are cleaned up within seconds of crossing the timeout.

If a host can be quiet for long stretches, run `canhazgpu web` (its dashboard refresh drives maintenance) or a periodic `canhazgpu status` from cron so idle reservations are reclaimed promptly.

## Defaults and configuration

| Setting | Default | Where |
|---------|---------|-------|
| Idle timeout for new manual reservations | `15m` (max `3h`) | `--idle-timeout`, or `reserve.idle-timeout` in the config file |
| Idle timeout for new `run` reservations | `30m` (max `3h`) | `run --idle-timeout`, or `run.idle-timeout` in the config file |
| Memory threshold that counts as usage | `100` MB | `--memory-threshold`, or `memory.threshold` |

```yaml
# ~/.canhazgpu.yaml - make one hour the site-wide default
reserve:
  idle-timeout: 1h
```

The timeout is stored on the reservation when it is created, so changing the default does not affect reservations that already exist.

## Interaction with bookings

[Scheduled bookings](usage-schedule.md) carry the idle timeout they were created with, and it applies from the moment the booking activates. A booked window that nobody uses is released back to the pool instead of blocking the GPU until the window ends, and the booking is marked completed.
