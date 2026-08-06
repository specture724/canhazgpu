# Guard: Enforcing Reservations

canhazgpu is cooperative by default: nothing stops somebody from running `python train.py` on a GPU without reserving it. The `guard` command closes that gap. It watches the GPUs continuously, warns people who bypass the reservation system **in the terminal where their job is running**, and can terminate the process if the warnings are ignored.

```bash
# Warn only (default)
canhazgpu guard

# Warn, then terminate offenders that ignore the warnings
canhazgpu guard --enforce
```

## What counts as a violation

| Kind | Meaning | Where it shows up |
|------|---------|-------------------|
| `unreserved` | A process on a GPU nobody reserved | `status` shows `⚠ UNRESERVED` |
| `foreign` | A process belonging to somebody other than the GPU's reservation holder | `status` shows `⚠ FOREIGN` |

The `foreign` case covers somebody starting a job on a GPU you reserved, or a job that kept running after a [scheduled booking](usage-schedule.md) preempted its reservation:

```text
❯ canhazgpu status
 GPU │ STATUS    │ USER  │ DURATION │ TYPE   │ DETAILS                                                    │ MEMORY               │ NOTE │ UTIL
─────┼───────────┼───────┼──────────┼────────┼────────────────────────────────────────────────────────────┼──────────────────────┼──────┼──────
 0   │ ⚠ FOREIGN │ alice │ 1h 0m    │ MANUAL │ expires in 59m, idle 10m, used by bob, processes: PID 123 (5m)│ 8452MB, 1 processes  │ -    │ 0%
```

The USER column still names who holds the reservation; DETAILS names who is actually on the GPU. In `--json` output the status stays `IN_USE` for compatibility, with the detail in `foreign_users`, `foreign_processes` and `foreign_memory_mb`.

Process owners are compared against the reservation's OS account, so `--user "Alice Smith"` display names do not confuse it — and warnings to the holder are addressed to their account rather than the display name.

Because [idle detection](features-idle-timeout.md) attributes usage to its owner, a squatted reservation also goes idle and is reclaimed on schedule; the squatter then becomes a plain `unreserved` violation.

## How a violation plays out

```
scan ──► confirmed (2 scans) ──► grace period (60s) ──► warning 1 ──► warning 2 ──► warning 3 ──► SIGINT ──► SIGTERM ──► SIGKILL
                                                          └── 5 min apart ──┘         └── only with --enforce, 30s apart ──┘
```

Every stage is configurable, and the process stops escalating the moment the GPU is released or a reservation is made.

A warning looks like this in the offender's terminal:

```text
[canhazgpu] GPU 2 is being used without a reservation (PID 12345, 8452MB).
Please reserve GPUs before using them:
  canhazgpu run --gpus 1 -- <your command>  # reserve and run
  canhazgpu reserve --gpus 1 --duration 2h  # reserve for later
Warning 2 of 3. After the last warning the process is terminated (5m later).
```

For `foreign` usage the reservation holder is told as well (`--notify-holder`, on by default):

```text
[canhazgpu] GPU 2 that you reserved is being used by bob (PID 12345, 8452MB).
Check the details with: canhazgpu status
```

## Warning channels

`--channels` selects how warnings are delivered; the default is `process,tty,log`.

| Channel | What it does | Needs root |
|---------|--------------|------------|
| `process` | Writes into the offending process's standard error, so the warning appears in the terminal running the job | Yes (for other users) |
| `tty` | Writes to every terminal that user has open | Yes (for other users) |
| `log` | Writes to the guard's own output (journald under systemd) and to `--log-file` | No |
| `wall` | Broadcasts to all logged-in users | Yes |

The `process` channel only writes to terminals. If the job's stderr is a log file, a pipe or `/dev/null`, the warning is skipped rather than injected — the guard never corrupts a job's own output.

!!! warning "Run the guard as root"
    Warning and terminating *other users'* processes requires root, because `/proc/<pid>/fd` is only readable by its owner. A guard started as a normal user still detects and records everything, but can only reach its own processes; it says so at startup.

## Termination

Enforcement is **off by default** — terminating other people's work is a policy decision, not a default:

```bash
canhazgpu guard --enforce --dry-run   # show what would be killed, send nothing
canhazgpu guard --enforce             # for real
```

Safety rails:

- **Escalation ladder**: SIGINT → SIGTERM → SIGKILL, `--kill-grace` apart, so a job gets the chance to shut down cleanly
- **Circuit breaker**: at most `--max-kills-per-hour` terminations (default 3); beyond that the guard only warns, since a storm of kills is more likely a bug than a room full of offenders
- **Unknown owners are never terminated**: if the process owner cannot be determined it might be a system process
- **Allow lists**: `--exclude-users` (default `root`) and `--exclude-commands` (default `Xorg,nvidia-smi,amd-smi,dcgm-exporter,nvidia-persistenced`)
- **Dry run**: `--dry-run` records what would have happened, including in `canhazgpu violations`

## Avoiding false positives

| Mechanism | Default | Why |
|-----------|---------|-----|
| `--grace` | 60s | A job that just started may be seconds away from getting its reservation |
| `--confirmations` | 2 | A single scan can catch a process that is already exiting |
| `--min-memory` | 0 (off) | Ignore small allocations if your site has noisy tooling |
| memory threshold | 100 MB | A GPU below `--memory-threshold` is not considered in use at all |
| allow lists | `root`, display/monitoring tools | System daemons legitimately touch GPUs |

!!! warning "root is excluded by default"
    `--exclude-users` defaults to `root`, so guard ignores **all** root processes — including a root job running on somebody else's reserved GPU. `status` will still show `⚠ FOREIGN`. To make guard react to root users as well, start it with `--exclude-users ''` (or configure `guard.exclude-users: []`); display/monitoring tools remain covered by `--exclude-commands`.

## Housekeeping

The guard is the only long-running part of canhazgpu, so it also performs the maintenance that other commands trigger on demand:

- [Scheduled bookings](usage-schedule.md) are activated as soon as their window starts
- Expired, stale and [idle](features-idle-timeout.md) reservations are released promptly

That means installing the guard also fixes the "nothing happens until somebody runs a command" gap. Use `--no-maintenance` to turn this off.

## Inspecting violations

```bash
canhazgpu violations                    # what is going on right now
canhazgpu violations --json             # for scripts
canhazgpu violations --history --days 7 # what happened this week
```

```text
 GPU │ KIND       │ USER                  │ PID   │ MEMORY  │ DURATION │ COMMAND                  │ ACTION
─────┼────────────┼───────────────────────┼───────┼─────────┼──────────┼──────────────────────────┼──────────────────────
 2   │ UNRESERVED │ bob                   │ 12345 │ 8452MB  │ 15m      │ python train.py --mod... │ 3 warning(s), SIGINT
 3   │ FOREIGN    │ carol (holder: alice) │ 23456 │ 40960MB │ 5m       │ vllm serve               │ 1 warning(s)
```

Resolved violations are kept for 90 days, so `--history` answers "who keeps bypassing the tool?".

## Deployment

```ini
# /etc/systemd/system/canhazgpu-guard.service
[Unit]
Description=canhazgpu GPU reservation guard
After=network.target redis.service

[Service]
ExecStart=/usr/local/bin/canhazgpu guard --interval 15s
Restart=always
User=root

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now canhazgpu-guard
journalctl -u canhazgpu-guard -f
```

Only one guard runs at a time: the role is held with a Redis lock, and a second instance exits with an error naming the host and PID that holds it.

For hosts where a daemon is unwelcome, run single scans from cron:

```bash
*/2 * * * * /usr/local/bin/canhazgpu guard --once >> /var/log/canhazgpu-guard.log 2>&1
```

Note that warnings are throttled by wall-clock time, so cron scans escalate on exactly the same schedule as a daemon — just with coarser granularity.

## Configuration file

```yaml
# ~/.canhazgpu.yaml (or /etc/canhazgpu.yaml for a system-wide guard)
guard:
  interval: "15s"
  grace: "60s"
  confirmations: 2
  warn-interval: "5m"
  max-warnings: 3
  enforce: false
  kill-grace: "30s"
  max-kills-per-hour: 3
  channels: ["process", "tty", "log"]
  log-file: "/var/log/canhazgpu-violations.log"
  exclude-users: ["root", "jenkins"]
  exclude-commands: ["Xorg", "dcgm-exporter"]
  notify-holder: true
```

## Custom reactions

Everything the guard records is available as JSON, so site-specific actions (mail, Slack, ticketing) can be layered on top:

```bash
# Notify offenders by mail once per hour
canhazgpu violations --json | jq -r '.[] | select(.warn_count >= 2) | .user' | sort -u | \
  while read -r user; do
      mail -s "GPU usage policy" "$user@example.com" <<< "Please reserve GPUs with canhazgpu."
  done
```
