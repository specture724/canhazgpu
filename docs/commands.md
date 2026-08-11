# Commands Overview

canhazgpu provides eleven main commands for GPU management:

```bash
❯ canhazgpu --help
Usage: canhazgpu [OPTIONS] COMMAND [ARGS]...

Commands:
  admin       Initialize GPU pool for this machine
  guard       Watch the GPUs and warn users who bypass the reservation system
  queue       Show the GPU reservation queue
  release     Release manually reserved GPUs held by the current user
  report      Generate GPU usage reports
  reserve     Reserve GPUs manually for a specified duration
  run         Reserve GPUs and run a command with CUDA_VISIBLE_DEVICES set
  schedule    Show the GPU booking schedule for a day
  status      Show current GPU allocation status
  violations  Show GPU usage that bypassed the reservation system
  web         Start a web server for GPU status monitoring
```

## Global Flags

All commands support these global configuration flags:

- `--config`: Path to configuration file (default: `$HOME/.canhazgpu.yaml`)
- `--redis-host`: Redis server hostname (default: localhost)
- `--redis-port`: Redis server port (default: 6379)
- `--redis-db`: Redis database number (default: 0)
- `--memory-threshold`: Memory threshold in MB to consider a GPU as "in use" (default: 100)
- `--booking-protection-window`: How far ahead reservations without a fixed end time (`run`) avoid GPUs needed by [scheduled bookings](usage-schedule.md) (default: 30m)

**Configuration Methods:**

1. **Command-line flags** (highest priority)
2. **Environment variables** with `CANHAZGPU_` prefix
3. **Configuration files** in YAML, JSON, or TOML format
4. **Built-in defaults** (lowest priority)

**Examples:**
```bash
# Use command-line flags
canhazgpu status --redis-host redis.example.com --redis-port 6380

# Use environment variables
export CANHAZGPU_MEMORY_THRESHOLD=512
export CANHAZGPU_REDIS_HOST=redis.example.com
canhazgpu status

# Use a configuration file
canhazgpu --config /path/to/config.yaml status
canhazgpu --config config.json run --gpus 2 -- python train.py
```

**Configuration File Examples:**

YAML format (`~/.canhazgpu.yaml`):
```yaml
redis:
  host: redis.example.com
  port: 6379
  db: 0
memory:
  threshold: 512
```

JSON format:
```json
{
  "redis": {
    "host": "redis.example.com",
    "port": 6379,
    "db": 0
  },
  "memory": {
    "threshold": 512
  }
}
```

TOML format:
```toml
[redis]
host = "redis.example.com"
port = 6379
db = 0

[memory]
threshold = 512
```

The `--memory-threshold` flag controls when a GPU is considered "in use without reservation". GPUs using more than this amount of memory will be excluded from allocation and flagged as unreserved usage.

## admin

Initialize and configure the GPU pool.

```bash
canhazgpu admin --gpus <count> [--force] [--provider <type>]
```

**Options:**
- `--gpus`: Number of GPUs available on this machine (required)
- `--force`: Force reinitialization even if already initialized
- `--provider`: GPU provider type (`nvidia`, `amd`, or `fake`). Auto-detected if not specified.

**Examples:**
```bash
# Initial setup (auto-detects provider)
canhazgpu admin --gpus 8

# Explicitly specify NVIDIA provider
canhazgpu admin --gpus 8 --provider nvidia

# Use AMD GPUs
canhazgpu admin --gpus 4 --provider amd

# Use fake provider for development/testing (no real GPUs required)
canhazgpu admin --gpus 4 --provider fake

# Change GPU count (requires --force)
canhazgpu admin --gpus 4 --force
```

!!! tip "Fake Provider for Development"
    Use `--provider fake` to develop and test canhazgpu on systems without actual GPUs.
    The fake provider simulates GPU behavior without requiring nvidia-smi or amd-smi.

!!! warning "Destructive Operation"
    Using `--force` will clear all existing reservations. Use with caution in production.

## status

Show current GPU allocation status with automatic validation.

```bash
# Table output (default)
canhazgpu status

# JSON output for programmatic use
canhazgpu status --json
canhazgpu status -j
```

**Options:**
- `-j, --json`: Output status as JSON array instead of table format
- `-v, --verbose`: More process detail in DETAILS. `-v` adds process names (max 2), `-vv` shows all
- `--no-schedule`: Do not print today's schedule after the status table

**[→ Detailed Status Guide](usage-status.md)**

**Examples:**
```bash
# Standard status check
canhazgpu status

# JSON output for scripts and APIs
canhazgpu status --json

# Use a lower threshold to detect lighter GPU usage
canhazgpu status --memory-threshold 512

# Use a higher threshold to ignore small allocations
canhazgpu status --memory-threshold 2048

# Combine JSON with memory threshold
canhazgpu status --json --memory-threshold 512
```

!!! note "Global Memory Threshold"
    The `--memory-threshold` flag is a global option that affects GPU usage detection across all commands. It can be set in your [configuration file](configuration.md) or used with any command that performs GPU validation.

**Table Output Example:**
```bash
GPU  STATUS      USER      DURATION     TYPE    MODEL                    DETAILS                   MEMORY          NOTE  UTIL
---  ------      ----      --------     ----    -----                    -------                   ----------      ----  ----
0    AVAILABLE   -         -            -       -                        free for 0h 30m 15s      45MB used        -     0%
1    IN_USE      alice     0h 15m 30s   RUN     meta-llama/Llama-2-7b-chat-hf  heartbeat 0h 0m 5s ago    8452MB, 1 processes   -     87%
2    UNRESERVED  user bob  -            -       codellama/CodeLlama-7b-Instruct-hf        used by PID 12345 (2h3m), PID 67890 (5m)  1024MB, 2 processes  -  42%
3    IN_USE      charlie   1h 2m 15s    MANUAL  -                        expires in 3h 15m 45s    no usage detected  -   5%
```

**JSON Output Example:**
```json
[
  {
    "gpu_id": 0,
    "status": "AVAILABLE",
    "details": "free for 0h 30m 15s",
    "validation": "45MB used",
    "utilization_percent": 0
  },
  {
    "gpu_id": 1,
    "status": "IN_USE",
    "user": "alice",
    "duration": "0h 15m 30s",
    "type": "RUN",
    "details": "heartbeat 0h 0m 5s ago",
    "validation": "8452MB, 1 processes",
    "model": {
      "provider": "meta-llama",
      "model": "meta-llama/Llama-2-7b-chat-hf"
    }
  },
  {
    "gpu_id": 2,
    "status": "UNRESERVED",
    "details": "WITHOUT RESERVATION",
    "validation": "1024MB, 2 processes",
    "utilization_percent": 42,
    "unreserved_users": ["bob"],
    "process_info": "used by PID 12345 (2h3m), PID 67890 (5m)",
    "model": {
      "provider": "codellama",
      "model": "codellama/CodeLlama-7b-Instruct-hf"
    }
  },
  {
    "gpu_id": 3,
    "status": "IN_USE",
    "user": "charlie",
    "duration": "1h 2m 15s",
    "type": "MANUAL",
    "details": "expires in 3h 15m 45s",
    "validation": "no usage detected"
  }
]
```

**Status Types:**
- `AVAILABLE`: GPU is free to use
- `IN_USE`: GPU is properly reserved
- `UNRESERVED`: GPU is being used without proper reservation

**Table Columns:**
- `GPU`: GPU ID number
- `STATUS`: Current state (AVAILABLE, IN_USE, UNRESERVED, ERROR)
- `USER`: Username who reserved the GPU (or who is using it unreserved)
- `DURATION`: How long the GPU has been reserved
- `TYPE`: Reservation type (RUN, MANUAL)
- `MODEL`: Detected AI model (if any)
- `DETAILS`: Additional information (heartbeat, expiry, process info)
- `MEMORY`: GPU memory usage and process count reported by the provider
- `UTIL`: GPU utilization percentage from the provider (nvidia-smi `utilization.gpu`, 0-100)

## run

Reserve GPUs and run a command with automatic cleanup.

```bash
canhazgpu run [--gpus <count> | --gpu-ids <ids>] [--timeout <duration>] [--nonblock] [--wait <duration>] -- <command>
```

**[→ Detailed Run Guide](usage-run.md)**

**Options:**
- `--gpus`: Number of GPUs to reserve (default: 1)
- `--gpu-ids`: Specific GPU IDs to reserve (comma-separated, e.g., 1,3,5)
- `--timeout`: Maximum time to run command before killing it (default: none)
- `--nonblock`: Fail immediately if GPUs are unavailable instead of waiting in queue
- `--wait`: Maximum time to wait for GPUs (e.g., 30m, 2h). Default: wait forever.

!!! note "GPU Selection Options"
    You can use `--gpus` alone, `--gpu-ids` alone, or both together if:
    - `--gpus` matches the number of GPU IDs specified, or
    - `--gpus` is 1 (the default value)

    If specific GPU IDs are requested and any are not available, the command will wait in the queue until those specific IDs become available.

!!! tip "Queueing Behavior"
    By default, if GPUs are not immediately available, `run` will wait in a FCFS (First Come First Served) queue until resources become available. Use `--nonblock` to fail immediately instead, or `--wait` to set a maximum wait time.

**Timeout formats:**
- `30s` (30 seconds)
- `30m` (30 minutes)
- `2h` (2 hours)  
- `1d` (1 day)
- `0.5h` (30 minutes with decimal)

**Examples:**
```bash
# Single GPU training
canhazgpu run --gpus 1 -- python train.py

# Multi-GPU distributed training
canhazgpu run --gpus 2 -- python -m torch.distributed.launch train.py

# Reserve specific GPU IDs
canhazgpu run --gpu-ids 1,3 -- python train.py

# Complex command with arguments
canhazgpu run --gpus 1 -- python train.py --batch-size 32 --epochs 100

# Training with timeout to prevent runaway processes
canhazgpu run --gpus 1 --timeout 2h -- python train.py

# Fail immediately if GPUs unavailable (no queuing)
canhazgpu run --nonblock --gpus 4 -- python train.py

# Wait up to 30 minutes for GPUs, then fail
canhazgpu run --wait 30m --gpus 4 -- python train.py
```

**Behavior:**
1. Validates actual GPU availability using nvidia-smi
2. Excludes GPUs that are in use without reservation
3. If GPUs unavailable, waits in queue (unless `--nonblock` is set)
4. Reserves the requested number of GPUs using MRU-per-user allocation (with LRU fallback)
5. Sets `CUDA_VISIBLE_DEVICES` to the allocated GPU IDs
6. Runs your command
7. Automatically releases GPUs when the command finishes
8. Maintains a heartbeat while running to keep the reservation active

**With `--nonblock`:**
```bash
❯ canhazgpu run --nonblock --gpus 2 -- python train.py
Error: Not enough GPUs available. Requested: 2, Available: 1 (1 GPUs in use without reservation - run 'canhazgpu status' for details)
```

**Default (waiting in queue):**
```bash
❯ canhazgpu run --gpus 2 -- python train.py
Waiting for GPUs... (position 1 in queue, 0/2 allocated)
Reserved 2 GPU(s): [0, 1] for command execution
...
```

## reserve

Manually reserve GPUs for a specified duration.

```bash
canhazgpu reserve [--gpus <count> | --gpu-ids <ids>] [--duration <time>] [--nonblock] [--wait <duration>]
                  [--idle-timeout <time>] [--start <time>] [--end <time>]
```

**[→ Detailed Reserve Guide](usage-reserve.md)**

**Options:**
- `--gpus`: Number of GPUs to reserve (default: 1)
- `--gpu-ids`: Specific GPU IDs to reserve (comma-separated, e.g., 1,3,5)
- `--duration`: Duration to reserve GPUs (default: 30m)
- `--nonblock`: Fail immediately if GPUs are unavailable instead of waiting in queue
- `--wait`: Maximum time to wait for GPUs (e.g., 30m, 2h). Default: wait forever.
- `--short`: Output only GPU IDs (for use with command substitution)
- `--idle-timeout`: Release the reservation if no GPU usage is detected for this long (default: 15m, `0` disables, maximum 3h)
- `--claim`: Reserve a GPU that only your own unreserved process is using; if anyone else is on it, wait in the queue
- `--start`: Book the GPUs for a future window starting then, instead of reserving now
- `--end`: End of the window (defaults to `--duration` after the start)

!!! tip "Idle Reservations Are Released"
    By default a manual reservation that shows no GPU activity for 15 minutes is released so the GPUs return to the pool. See [Idle Reservation Timeout](features-idle-timeout.md).

!!! tip "Booking Ahead"
    With `--start`, `reserve` creates a booking for a future time window rather than reserving immediately. See [Scheduled Bookings](usage-schedule.md).

!!! note "GPU Selection Options"
    You can use `--gpus` alone, `--gpu-ids` alone, or both together if:
    - `--gpus` matches the number of GPU IDs specified, or
    - `--gpus` is 1 (the default value)

    If specific GPU IDs are requested and any are not available, the command will wait in the queue until those specific IDs become available.

!!! tip "Queueing Behavior"
    By default, if GPUs are not immediately available, `reserve` will wait in a FCFS (First Come First Served) queue until resources become available. Use `--nonblock` to fail immediately instead, or `--wait` to set a maximum wait time.

**Duration Formats:**
- `30m`: 30 minutes
- `2h`: 2 hours
- `1d`: 1 day
- `0.5h`: 30 minutes (decimal values supported)

**Examples:**
```bash
# Reserve 1 GPU for 30 minutes (default)
canhazgpu reserve

# Reserve 2 GPUs for 4 hours
canhazgpu reserve --gpus 2 --duration 4h

# Reserve specific GPU IDs
canhazgpu reserve --gpu-ids 0,2 --duration 2h

# Reserve 1 GPU for 30 minutes
canhazgpu reserve --duration 30m

# Reserve 1 GPU for 2 days
canhazgpu reserve --gpus 1 --duration 2d

# Keep the reservation even if the GPU sits unused
canhazgpu reserve --gpus 1 --duration 8h --idle-timeout 0

# Book 2 GPUs from 14:00 to 16:00 instead of reserving now
canhazgpu reserve --start 14:00 --end 16:00 --gpus 2
```

**Important Note:**
Unlike the `run` command, `reserve` does NOT automatically set `CUDA_VISIBLE_DEVICES`. You can use `--short` for easy shell integration:
```bash
export CUDA_VISIBLE_DEVICES=$(canhazgpu reserve --gpus 2 --short)
```

**Use Cases:**
- Interactive development sessions
- Jupyter notebook workflows
- Preparing for batch jobs
- Blocking GPUs for maintenance

## release

Release manually reserved GPUs held by the current user.

```bash
canhazgpu release [--gpu-ids <ids>]
```

**[→ Detailed Release Guide](usage-release.md)**

**Options:**
- `-G, --gpu-ids`: Specific GPU IDs to release (comma-separated, e.g., 1,3,5)

**Examples:**
```bash
# Release all manually reserved GPUs
❯ canhazgpu release
Released 2 GPU(s): [1, 3]

# Release specific GPUs
❯ canhazgpu release --gpu-ids 1,3
Released 2 GPU(s): [1, 3]

❯ canhazgpu release  
No manually reserved GPUs found for current user
```

!!! note "Scope"
    By default, releases all manually reserved GPUs. With `--gpu-ids`, can release specific GPUs including both manual reservations (from `reserve` command) and run-type reservations (from `run` command).

## report

Generate GPU reservation reports showing historical reservation patterns by user.

```bash
canhazgpu report [--days <num>]
```

**Options:**
- `--days`: Number of days to include in the report (default: 30)

**Examples:**
```bash
# Show reservations for the last 30 days (default)
canhazgpu report

# Show reservations for the last 7 days
canhazgpu report --days 7

# Show reservations for the last 24 hours
canhazgpu report --days 1
```

**Example Output:**
```bash
=== GPU Reservation Report ===
Period: 2025-05-31 to 2025-06-30 (30 days)

User                       GPU Hours      Percentage        Run     Manual
---------------------------------------------------------------------------
alice                          24.50          55.2%         12          8
bob                            15.25          34.4%          6         15
charlie                         4.60          10.4%          3          2
---------------------------------------------------------------------------
TOTAL                          44.35         100.0%         21         25

Total reservations: 46
Unique users: 3
```

**Report Features:**
- Shows GPU hours consumed by each user
- Percentage of total usage
- Breakdown by reservation type (run vs manual)
- Total statistics for the period
- Includes both completed and in-progress reservations

## queue

Show the GPU reservation queue.

```bash
canhazgpu queue [--json]
```

**Options:**
- `--json`: Output queue status as JSON

When GPUs are not immediately available, `run` and `reserve` commands add entries to a queue and wait for resources to become available. This command shows all entries currently waiting in the queue.

**Example Output:**
```bash
❯ canhazgpu queue
GPU Reservation Queue
=====================

Position  User            Requested       Allocated    Waiting
--------  ----            ---------       ---------    -------
1         alice           4 GPUs          0/4          5m 30s
2         bob             2 GPUs          0/2          2m 15s

Total: 2 entries waiting for 6 GPUs (0 partially allocated)
```

**Queue Behavior:**
- **FCFS (First Come First Served)**: Only the first entry in the queue can acquire newly available GPUs
- **Full-Request Allocation**: the first entry whose complete request can be satisfied is allocated; entries that cannot start yet never hold GPUs while waiting
- **Heartbeat Cleanup**: Stale queue entries (crashed processes) are automatically cleaned up after 2 minutes
- **Ctrl+C Handling**: Pressing Ctrl+C while waiting removes the entry from the queue

**JSON Output:**
```bash
❯ canhazgpu queue --json
{
  "entries": [
    {
      "id": "abc123",
      "user": "alice",
      "requested_count": 4,
      "allocated_gpus": [0, 1],
      "allocated_count": 2,
      "reservation_type": "run",
      "wait_time": "5m 30s",
      "wait_time_seconds": 330
    }
  ],
  "total_waiting": 1,
  "total_gpus_requested": 4,
  "total_gpus_allocated": 2
}
```

## schedule

Show the GPU booking schedule for a day, and cancel bookings.

```bash
canhazgpu schedule [--date <day>] [--days <n>] [--json] [--cancel <id>] [--force]
```

**[→ Detailed Schedule Guide](usage-schedule.md)**

**Options:**
- `--date`: Day to display (`YYYY-MM-DD`, `today`, `tomorrow`, `+2d`; default: `today`)
- `--days`: Number of days to display, starting at `--date` (default: 1)
- `--json`: Output the schedule as JSON
- `--cancel`: Cancel a booking by (abbreviated) ID
- `--force`: Allow cancelling a booking that belongs to someone else
- `--no-color`: Disable colored output

Bookings themselves are created with `canhazgpu reserve --start ...`.

**Example Output:**
```bash
❯ canhazgpu schedule
GPU Booking Schedule - 2026-08-03 (Mon)

      00 01 02 03 04 05 06 07 08 09 10 11 12 13 14 15 16 17 18 19 20 21 22 23    NOW
GPU0  ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· aa !! bb ·· ·· ·· ·· ·· ·· ·· ··    alice MANUAL 8452MB
GPU1  ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· bb bb ·· ·· ·· ·· ·· ·· ·· ··    free
GPU2  ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ··    carol UNRESERVED 2048MB
GPU3  ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ·· ··    dave MANUAL booked idle 4m
                                             ^ now 13:06

  a  alice        13:06-15:06           GPU 0        manual reservation  note: interactive work
  b  bob          14:00-16:00           GPU 0,1      booking 4f89853e (pending)  note: fine-tune
  ·  free
```

Each row is a GPU and each pair of characters is one hour, so a character covers 30 minutes. `·` is free time, letters refer to the legend, and `!` marks GPUs where two entries overlap — typically a booking that is about to take over from an earlier reservation.

The **NOW** column on the right shows the live state of each GPU: who holds it, whether the GPU is actually in use (memory), whether the reservation came from a booking, and how long it has been idle. It is printed only for the day containing the current time.

**Cancelling:**
```bash
❯ canhazgpu schedule --cancel 4f89853e
Cancelled booking 4f89853e: bob, GPU 0,1, 14:00-16:00
```

## guard

Watch the GPUs continuously and react to usage that bypasses reservations.

```bash
canhazgpu guard [--enforce] [--interval <time>] [--once] [--dry-run]
```

**[→ Detailed Guard Guide](features-guard.md)**

**Key options:**
- `--enforce`: Terminate offending processes once warnings are exhausted (default: warn only)
- `--dry-run`: With `--enforce`, report terminations without sending signals
- `--interval`: How often to scan (default: 15s)
- `--once`: Single scan and exit, for cron
- `--grace`: How long a process may use a GPU before it is reported (default: 60s)
- `--confirmations`: Consecutive scans required before acting (default: 2)
- `--warn-interval`: Time between repeated warnings (default: 5m)
- `--max-warnings`: Warnings before enforcement starts (default: 3)
- `--kill-grace`: Wait between SIGINT, SIGTERM and SIGKILL (default: 30s)
- `--max-kills-per-hour`: Safety limit on terminations (default: 3, 0 disables)
- `--channels`: Warning channels: `process`, `tty`, `log`, `wall` (default: `process,tty,log`)
- `--log-file`: Also append warnings to this file
- `--exclude-users` / `--exclude-commands`: Allow lists
- `--min-memory`: Ignore processes below this many MB
- `--notify-holder`: Tell the reservation holder when somebody else uses their GPU (default: true)
- `--no-maintenance`: Do not activate bookings or release expired reservations

**Examples:**
```bash
# Warn only, scanning every 15 seconds
canhazgpu guard

# See what enforcement would do without touching anything
canhazgpu guard --enforce --dry-run

# Warn, then terminate offenders
canhazgpu guard --enforce

# Single scan from cron
canhazgpu guard --once
```

Warnings are written into the standard error of the offending process, so they
appear in the terminal running the job, plus any terminal that user has open.
Two kinds of violation are detected: `unreserved` (nobody reserved the GPU) and
`foreign` (somebody else reserved it).

The guard is the only long-running part of canhazgpu, so it also activates
scheduled bookings and releases expired, stale and idle reservations.

!!! warning "Run as root"
    Warning and terminating other users' processes requires root; normally the
    guard runs from systemd. As a normal user it still detects and records
    violations but can only reach your own processes.

!!! note "Single instance"
    Only one guard runs at a time, enforced with a Redis lock.

## violations

Show GPU usage that bypassed the reservation system, as recorded by the guard.

```bash
canhazgpu violations [--json] [--history] [--days <n>]
```

**Options:**
- `--json`: Output as JSON
- `--history`: Show resolved violations instead of open ones
- `--days`: How many days of history to include (default: 7)

**Example Output:**
```bash
❯ canhazgpu violations
 GPU │ KIND       │ USER                  │ PID   │ MEMORY  │ DURATION │ COMMAND                  │ ACTION
─────┼────────────┼───────────────────────┼───────┼─────────┼──────────┼──────────────────────────┼──────────────────────
 2   │ UNRESERVED │ bob                   │ 12345 │ 8452MB  │ 15m      │ python train.py --mod... │ 3 warning(s), SIGINT
 3   │ FOREIGN    │ carol (holder: alice) │ 23456 │ 40960MB │ 5m       │ vllm serve               │ 1 warning(s)
```

Resolved violations are kept for 90 days, so `--history` answers "who keeps
bypassing the tool?".

## web

Start a web server providing a dashboard for real-time monitoring and reports.

```bash
canhazgpu web [--port <port>] [--host <host>] [--demo]
```

**Options:**
- `--port, -p`: Port to run the web server on (default: 8080)
- `--host`: Host to bind the web server to (default: 0.0.0.0)
- `--demo`: Run in demo mode with simulated data (no Redis required)

**Examples:**
```bash
# Start web server on default port 8080
canhazgpu web

# Start on a custom port
canhazgpu web --port 3000

# Bind to localhost only
canhazgpu web --host 127.0.0.1 --port 8080

# Run on a specific interface
canhazgpu web --host 192.168.1.100 --port 8888

# Run in demo mode for testing
canhazgpu web --demo
```

![Web Dashboard Screenshot](images/web-screenshot.png)

The dashboard displays:
- System hostname in the header for easy identification
- GPU cards showing status, user, duration, and validation info
- Color-coded status badges (green=available, blue=in use, red=unreserved)
- Reservation queue with wait times and progress bars (when entries are waiting)
- Reservation report with usage statistics and visual bars
- Quick links to documentation and GitHub repository

**Dashboard Features:**
- **Real-time GPU Status**: Automatically refreshes every 30 seconds
- **Reservation Queue**: Live queue display with progress bars and color-coded wait times (refreshes every 5 seconds)
- **Interactive Reservation Reports**: Customizable time periods (1-90 days)
- **Visual Design**: Dark/light theme toggle with color-coded status indicators
- **Mobile Responsive**: Works on desktop and mobile devices
- **Multi-Host Support**: View all configured remote hosts in one dashboard
- **API Endpoints**:
  - `/api/status` - Current GPU status as JSON
  - `/api/queue` - Current queue status as JSON
  - `/api/hosts` - List of configured hosts
  - `/api/hosts/status` - Status for all hosts (multi-host view)
  - `/api/hosts/status?host=<name>` - Status for a specific host
  - `/api/report?days=N` - Usage report as JSON

### Multi-Host Support

When remote hosts are configured in `~/.canhazgpu.yaml`, the web dashboard automatically enables a multi-host view:

```yaml
# ~/.canhazgpu.yaml
remote_hosts:
  - gpu-server-1
  - gpu-server-2
  - workstation-3
```

**Multi-Host Features:**
- **Hosts Overview**: Summary cards showing GPU availability for each host
- **Click to Expand**: Select a host to view detailed GPU status
- **Parallel Fetching**: Status is fetched from all hosts concurrently
- **Error Handling**: Failed hosts show error messages without blocking others
- **Graceful Degradation**: If localhost Redis is unavailable but remote hosts are configured, the dashboard continues with remote hosts only

**Single-Host Behavior:**
When no remote hosts are configured, the dashboard shows the traditional single-host view with GPU cards and reservation reports.

**Use Cases:**
- Team dashboards on shared displays
- Remote monitoring without SSH access
- Multi-system GPU cluster monitoring
- Integration with monitoring systems via API
- Mobile access for on-the-go checks

## Command Interactions

### Validation and Conflicts

All allocation commands (`run` and `reserve`) automatically:

1. **Scan for unreserved usage** using nvidia-smi
2. **Exclude unreserved GPUs** from the available pool
3. **Hold back GPUs needed by scheduled bookings** during the window the reservation would cover
4. **Provide detailed error messages** if insufficient GPUs remain

### MRU-per-User Allocation

When multiple GPUs are available, the system uses **Most Recently Used per User** allocation:

- Prioritizes GPUs that you have used most recently (based on your usage history)
- Falls back to global LRU (Least Recently Used) for GPUs you haven't used
- Provides GPU affinity for better cache locality and workflow continuity
- Ensures fair distribution across all users while respecting individual preferences

### Reservation Types

- **Run-type reservations**: Maintained by heartbeat, auto-released when process ends
- **Manual reservations**: Time-based expiry, require explicit release or timeout, and are also released when they sit [idle](features-idle-timeout.md)
- **Bookings**: A future time window that becomes a manual reservation when it starts, see [Scheduled Bookings](usage-schedule.md)

### Status Integration

The `status` command shows comprehensive information about all reservation types and validates actual usage against reservations, making it easy to identify and resolve conflicts.