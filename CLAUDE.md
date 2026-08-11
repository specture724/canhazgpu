# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

This is `canhazgpu`, a GPU reservation tool for single host shared development systems. It's a Go CLI application that uses Redis as a backend to coordinate GPU allocations across multiple users and processes, with comprehensive validation to detect and prevent unreserved GPU usage. The system supports both NVIDIA and AMD GPUs through a unified provider abstraction.

## Architecture

The tool is a Go application structured as a CLI with internal packages that implements eleven main commands:
- `admin`: Initialize and configure the GPU pool with optional --force flag and --provider selection
- `status`: Show current GPU allocation status with automatic provider-specific validation
- `run`: Reserve GPU(s) and execute a command with `CUDA_VISIBLE_DEVICES` set (blocks if GPUs unavailable)
- `reserve`: Manually reserve GPU(s) for a specified duration, or book a future time window with --start (blocks if GPUs unavailable)
- `release`: Release all manually reserved GPUs for the current user
- `report`: Generate GPU reservation reports showing historical reservation patterns by user
- `queue`: Show the GPU reservation queue with wait times and allocation progress
- `schedule`: Show the day's GPU booking schedule as a grid, and cancel bookings
- `guard`: Long-running daemon that warns (and optionally terminates) processes bypassing the reservation system, and performs periodic maintenance
- `violations`: Show current and historical GPU usage that bypassed reservations
- `web`: Start a web server providing a dashboard for real-time monitoring and reports

### Core Components

- **Redis Integration**: Uses Redis (localhost:6379) for persistent state management with keys under `canhazgpu:` prefix
- **GPU Provider System**: Unified abstraction supporting both NVIDIA (nvidia-smi) and AMD (amd-smi) GPUs
- **GPU Allocation Logic**: Tracks GPU state with JSON objects containing user, timestamps, heartbeat data, and reservation types
- **Heartbeat System**: Background goroutine sends periodic heartbeats (60s interval) to maintain run-type reservations
- **Auto-cleanup**: GPUs are automatically released when heartbeat expires (5 min timeout), manual reservations expire, or processes terminate
- **Idle Timeout**: Manual reservations with no detected GPU usage for their idle timeout (default 15 min) are released automatically; run-type reservations are exempt since they end with their process
- **Scheduled Bookings**: Meeting-room style bookings claim specific GPUs for a future window, block conflicting reservations beforehand, and preempt whatever still holds those GPUs when the window starts
- **Guard/Enforcement**: Optional daemon (`canhazgpu guard`) compares process owners against reservation holders, warns offenders by writing into their process's stderr and terminals, escalates to SIGINT/SIGTERM/SIGKILL with `--enforce`, and records violations for reporting
- **Unreserved Usage Detection**: Provider-specific integration detects GPUs in use without proper reservations
- **User Accountability**: Process ownership detection identifies which users are running unreserved processes
- **MRU-per-User Allocation**: Most Recently Used per user strategy provides GPU affinity with LRU fallback for fair distribution
- **Specific GPU Reservation**: Users can reserve exact GPU IDs (e.g., --gpu-ids 1,3) when specific hardware is needed
- **Race Condition Protection**: Redis-based distributed locking prevents allocation conflicts
- **Fair Queueing System**: FCFS queue; the first entry whose full request can be satisfied is allocated (no partial allocation, so GPUs are never held by a job that cannot start), with heartbeat-based stale entry cleanup and queue status monitoring

## Development Commands

### Build and Installation

```bash
# Option 1: Install directly from GitHub (recommended for users)
go install github.com/russellb/canhazgpu@latest

# Option 2: Build from source
make build            # Build the Go binary to ./build/canhazgpu
make install          # Build and install to /usr/local/bin with bash completion
make test-short       # Run Go tests

# Option 3: Install from local source
go install .          # Installs to $GOPATH/bin or $HOME/go/bin

# Optional: Create short alias symlink (after installing to /usr/local/bin)
sudo ln -s /usr/local/bin/canhazgpu /usr/local/bin/chg
```

### Usage Examples
```bash
# Initialize GPU pool (auto-detects provider)
./build/canhazgpu admin --gpus 8

# Initialize with specific provider
./build/canhazgpu admin --gpus 4 --provider nvidia
./build/canhazgpu admin --gpus 2 --provider amd

# Force reinitialize with different count
./build/canhazgpu admin --gpus 4 --force

# Check status with automatic provider-specific validation
./build/canhazgpu status

# Get JSON output for programmatic use
./build/canhazgpu status --json

# Run command with GPU reservation (by count)
./build/canhazgpu run --gpus 1 -- python train.py

# Run command with specific GPU IDs
./build/canhazgpu run --gpu-ids 1,3 -- python train.py

# Run command with timeout to prevent runaway processes
./build/canhazgpu run --gpus 1 --timeout 2h -- python train.py

# Manual GPU reservation (by count)
./build/canhazgpu reserve --gpus 2 --duration 4h

# Manual reservation of specific GPU IDs
./build/canhazgpu reserve --gpu-ids 0,2,4 --duration 2h

# Claim a GPU that only your own unreserved process is using
./build/canhazgpu reserve --gpu-ids 2 --claim --duration 2h

# Release manual reservations
./build/canhazgpu release

# Generate reservation report for last 7 days
./build/canhazgpu report --days 7

# Customize memory threshold for GPU usage detection (default: 100 MB)
./build/canhazgpu status --memory-threshold 512
./build/canhazgpu run --memory-threshold 2048 --gpus 1 -- python train.py

# Use a configuration file
./build/canhazgpu --config /path/to/config.yaml status
./build/canhazgpu --config config.json run --gpus 2 -- python train.py

# Queueing: wait for GPUs if unavailable (default behavior)
./build/canhazgpu run --gpus 4 -- python train.py  # Waits in queue until GPUs available

# Fail immediately if GPUs are unavailable (no queueing)
./build/canhazgpu run --nonblock --gpus 4 -- python train.py

# Wait up to 30 minutes for GPUs, then fail
./build/canhazgpu run --wait 30m --gpus 4 -- python train.py

# Check the reservation queue
./build/canhazgpu queue
./build/canhazgpu queue --json

# Idle timeout: release a manual reservation nobody uses (default 15m, 0 disables)
./build/canhazgpu reserve --gpus 1 --duration 8h --idle-timeout 1h
./build/canhazgpu reserve --gpus 1 --duration 8h --idle-timeout 0

# Book a future time window (meeting-room style)
./build/canhazgpu reserve --start 14:00 --end 16:00 --gpus 2
./build/canhazgpu reserve --start 'tomorrow 09:00' --duration 4h --gpu-ids 0,1

# Show the booking schedule and cancel a booking
./build/canhazgpu schedule
./build/canhazgpu schedule --date tomorrow --days 3
./build/canhazgpu schedule --json
./build/canhazgpu schedule --cancel 4f89853e

# Guard: warn (and optionally terminate) processes bypassing reservations
./build/canhazgpu guard                    # warn only, scan every 15s
./build/canhazgpu guard --once             # single scan, for cron
./build/canhazgpu guard --enforce --dry-run
sudo ./build/canhazgpu guard --enforce     # root needed to reach other users

# Inspect violations
./build/canhazgpu violations
./build/canhazgpu violations --json
./build/canhazgpu violations --history --days 7
```

### GPU Provider Examples
```bash
# NVIDIA GPU setup
canhazgpu admin --gpus $(nvidia-smi --list-gpus | wc -l)

# AMD GPU setup
canhazgpu admin --gpus $(amd-smi list --json | jq 'length')

# Fake GPU setup (for development/testing without real GPUs)
canhazgpu admin --gpus 4 --provider fake

# Check provider availability
nvidia-smi --help >/dev/null 2>&1 && echo "NVIDIA available" || echo "NVIDIA not available"
amd-smi --help >/dev/null 2>&1 && echo "AMD available" || echo "AMD not available"

# View cached provider information
redis-cli get "canhazgpu:provider"
```

## Dependencies

- Go 1.25+ with modules:
  - `github.com/go-redis/redis/v8`: Redis client library
  - `github.com/spf13/cobra`: CLI framework
  - `github.com/spf13/viper`: Configuration management
- System requirements: 
  - Redis server running on localhost:6379
  - **For NVIDIA GPUs**: nvidia-smi command available
  - **For AMD GPUs**: amd-smi command available (ROCm 5.7+)
  - Access to /proc filesystem or ps command for user detection

## Key Implementation Details

### Project Structure

```text
├── main.go                          # Entry point
├── internal/
│   ├── cli/                        # Cobra CLI commands
│   │   ├── root.go                 # Root command and global config
│   │   ├── admin.go                # admin command implementation
│   │   ├── status.go               # status command implementation  
│   │   ├── run.go                  # run command implementation
│   │   ├── reserve.go              # reserve command implementation
│   │   ├── release.go              # release command implementation
│   │   ├── report.go               # report command implementation
│   │   ├── queue.go                # queue command implementation
│   │   ├── schedule.go             # schedule command (booking grid, cancellation)
│   │   ├── guard.go                # guard command (monitoring daemon)
│   │   └── violations.go           # violations command
│   ├── gpu/                        # GPU management logic
│   │   ├── allocation.go           # MRU-per-user allocation, maintenance, idle timeout
│   │   ├── booking.go              # Scheduled bookings: creation, activation, preemption
│   │   ├── guard.go                # Violation detection, warning escalation, termination
│   │   ├── notify.go               # Warning delivery channels (process stderr, tty, log, wall)
│   │   ├── validation.go           # GPU usage validation and process detection
│   │   ├── model_detection.go      # AI model detection from process commands
│   │   ├── heartbeat.go            # Background heartbeat system
│   │   ├── queue_heartbeat.go      # Queue-specific heartbeat system
│   │   ├── provider.go             # GPU provider interface and manager
│   │   ├── nvidia_provider.go      # NVIDIA GPU provider implementation
│   │   ├── amd_provider.go         # AMD GPU provider implementation
│   │   └── fake_provider.go        # Fake GPU provider for development/testing
│   ├── redis_client/               # Redis operations
│   │   └── client.go               # Redis client with Lua scripts
│   ├── types/                      # Shared types and constants
│   │   └── types.go                # Config, GPUState, and other types
│   └── utils/                      # Utility functions
│       └── utils.go                # Common utility functions
├── autocomplete_canhazgpu.sh       # Custom bash completion script
└── go.mod                          # Go module definition
```

### GPU Provider System

- **Provider Interface**: `GPUProvider` interface in `internal/gpu/provider.go` defines standard operations
- **NVIDIA Provider**: `nvidia_provider.go` implements NVIDIA GPU management using nvidia-smi commands
- **AMD Provider**: `amd_provider.go` implements AMD GPU management using amd-smi commands
- **Fake Provider**: `fake_provider.go` simulates GPU behavior for development and testing without real hardware
- **Provider Manager**: `ProviderManager` in `provider.go` handles provider detection, caching, and instance management
- **Auto-detection**: Automatically detects available providers during system initialization

### GPU Validation and Detection

- `DetectAllGPUUsage()` in `internal/gpu/provider.go`: Uses provider-specific commands to query actual GPU processes and memory usage
- `GetProcessOwner()` in `internal/gpu/validation.go`: Identifies process owners via /proc filesystem or ps command
- Unreserved usage detection excludes GPUs from allocation pool automatically
- Configurable memory threshold (default: 100 MB) determines if GPU is considered "in use" via --memory-threshold flag
- **Model Detection**: `DetectModelFromProcesses()` in `internal/gpu/model_detection.go` identifies running AI models:
  - **vLLM-specific detection**: Recognizes vLLM serve commands with positional or --model arguments
  - **Generic --model detection**: Detects `--model value` or `--model=value` patterns in any command
  - **Examples**: Supports `python train.py --model meta-llama/Llama-2-7b-chat-hf`, `inference-server --model=mistralai/Mistral-7B-Instruct-v0.1`
  - **Provider extraction**: Automatically extracts provider names (e.g., "openai", "meta-llama") from model identifiers

### Allocation Strategy

- `AtomicReserveGPUs()` in `internal/redis_client/client.go`: MRU-per-user allocation with unreserved usage exclusion and LRU fallback
- `AtomicReserveGPUs()` in `internal/redis_client/client.go`: Race-condition-safe reservation with Lua scripts
- Enhanced Redis Lua scripts validate unreserved usage list during atomic operations
- Detailed error messages when allocation fails due to unreserved usage

### Reservation Types

- **Run-type**: Maintained by heartbeat, auto-released when process ends
- **Manual-type**: Time-based expiry, explicit release required, released early when idle (see below)
- **Bookings**: Future time windows (`reserve --start`) that activate into manual reservations
- `LastReleased` timestamp tracking for global LRU fallback allocation
- Usage history tracking for MRU-per-user preferences  
- Support for flexible duration formats (30m, 2h, 1d) via `ParseDuration()`

### Maintenance Pass, Idle Timeout and Bookings

- `CleanupExpiredReservations()` in `internal/gpu/allocation.go` is the shared maintenance hook every command runs before reading or handing out GPUs. It detects GPU usage once (cached for 1s), activates due bookings, releases expired/stale/idle reservations, and prunes finished bookings
- Idle timeout: manual reservations store `idle_timeout` (seconds) and `last_activity`; the maintenance pass refreshes `last_activity` whenever the **holder's own** usage is observed (`IsReservationHolderActive()` in `validation.go` attributes usage by process owner) and releases the reservation once `now - last_activity` exceeds the timeout. Skipped entirely when usage detection fails or cannot be attributed, so unverifiable reservations are never released
- `--idle-timeout` on `reserve` (default 15m, `0` disables); run-type reservations never carry one
- Bookings (`internal/gpu/booking.go`): GPUs are chosen at booking time; `applyBookingProtection()` excludes booked GPUs from allocation for the window a reservation would cover (`ExpiryTime`, or `--booking-protection-window` ahead for run-type); `activateDueBookings()` preempts remaining holders and writes a manual reservation expiring at the window's end
- Releasing a booked GPU (`release`, idle timeout, expiry, `schedule --cancel`) marks the booking completed

### Guard and Enforcement

- `internal/gpu/guard.go`: `Guard.RunOnce()` is one scan — maintenance, usage detection, violation reconciliation, then warn or terminate. `Guard.Run()` loops on `--interval` while holding the singleton guard lock
- `classifyProcess()` compares each process's owner against the reservation account (`ReservationAccount()`: `ActualUser`, falling back to `User`): no reservation → `unreserved`, different owner → `foreign`
- `status` shows the same condition: `DetectForeignUsage()` fills `ForeignUsers/Processes/MemoryMB` on `GPUStatusInfo`, the STATUS cell renders `⚠ FOREIGN`, and DETAILS gains `used by <user> (<n>MB)`. The JSON `status` stays `IN_USE` for compatibility, with the detail in `foreign_*` fields
- Violations carry both `holder` (display name, for messages) and `holder_account` (OS account, used to actually reach the holder)
- False-positive protection: `--grace` (60s), `--confirmations` (2 scans), allow lists (`root` and display/monitoring tools by default), `--min-memory`, plus the global memory threshold
- Escalation: warnings every `--warn-interval` up to `--max-warnings`, then (with `--enforce` only) SIGINT → SIGTERM → SIGKILL `--kill-grace` apart. Guarded by `--max-kills-per-hour` (Redis-backed), `--dry-run`, and a rule never to terminate processes with an unidentified owner
- `internal/gpu/notify.go`: warning channels. `process` writes into `/proc/<pid>/fd/2` only when it resolves to a terminal (never a job's log file or `/dev/null`); `tty` writes to `/dev/pts` entries owned by the offender; both need root for other users
- The guard's maintenance pass is what makes booking activation and idle reclamation timely without anyone running commands

### Locking and Concurrency

- Global allocation lock (`AcquireAllocationLock`, `ReleaseAllocationLock`) prevents race conditions
- Exponential backoff with jitter for lock acquisition retries
- Atomic operations using Redis Lua scripts prevent partial allocation failures
- Lock timeout of 10 seconds with up to 5 retry attempts

### Status and Monitoring

- Real-time validation shows actual vs reserved GPU usage
- User accountability displays specific users running unreserved processes
- Table format with GPU, STATUS, USER, DURATION, TYPE, DETAILS, MEMORY, MODEL, NOTE, UTIL columns
- Default `status` output also prints today's schedule; `--no-schedule` disables it
- DETAILS shows PID and elapsed time per process; `-v` adds process names (max 2), `-vv` shows all
- `free for` time accounts for the last observed usage, including unreserved activity
- Validation info format: `XMB, Y processes` (no prefix)
- "UNRESERVED" status for unreserved usage
- Model detection displays identified AI models in MODEL column
- Provider-specific validation using appropriate GPU management tools

### Time Handling

- `FlexibleTime` type in `internal/types/types.go` handles both Unix timestamps and RFC3339 strings
- Supports multiple timestamp formats for flexibility and data migration scenarios

### Configuration Management

- Configuration via command-line flags, configuration files, and environment variables
- Supports YAML, JSON, and TOML configuration file formats
- Default config file location: `$HOME/.canhazgpu.yaml`
- Environment variables with `CANHAZGPU_` prefix (e.g., `CANHAZGPU_MEMORY_THRESHOLD=512`)
- Priority order: CLI flags > environment variables > config file > defaults

## Redis Schema

### Core Keys

- `canhazgpu:gpu_count`: Total number of available GPUs
- `canhazgpu:provider`: Cached GPU provider type ("nvidia", "amd", or "fake")
- `canhazgpu:allocation_lock`: Global allocation lock for race condition prevention
- `canhazgpu:usage_history:{timestamp}:{user}:{gpu_id}`: Historical usage records for reporting

### Queue Keys

- `canhazgpu:queue`: Sorted set of queue entries (score = enqueue timestamp)
- `canhazgpu:queue:entry:{id}`: JSON object for each queue entry details

### Booking Keys

- `canhazgpu:bookings`: Sorted set of booking IDs (score = start timestamp)
- `canhazgpu:booking:{id}`: JSON object per booking (`gpu_ids`, `start_time`, `end_time`, `status`, `idle_timeout`, `note`)

### Guard Keys

- `canhazgpu:violations`: Sorted set of open violation IDs (`{gpu}:{pid}`, score = first seen)
- `canhazgpu:violation:{gpu}:{pid}`: JSON object per open violation (24h TTL)
- `canhazgpu:violation_history_sorted`: Resolved violations (90-day expiry)
- `canhazgpu:guard:lock`: Singleton lock held by the running guard (`host:pid`, refreshed each scan)
- `canhazgpu:guard:kills`: Sorted set of terminations, used for the hourly circuit breaker

### GPU State Objects (`canhazgpu:gpu:{id}`)

Available state: `{'last_released': timestamp}` or `{}`

Reserved state:

```json
{
  "user": "username",
  "start_time": timestamp,
  "last_heartbeat": timestamp,
  "type": "run|manual",
  "expiry_time": timestamp,   // Only for manual reservations
  "idle_timeout": seconds,    // Only for manual reservations with idle detection
  "last_activity": timestamp, // Last time real GPU usage was observed
  "booking_id": "uuid"        // Set when a scheduled booking created this reservation
}
```

### Validation Integration

- Unreserved usage detection runs during allocation
- MRU-per-user allocation excludes GPUs in unreserved use
- Redis Lua scripts receive unreserved GPU lists for atomic validation
- Process ownership data enriches status display but not stored in Redis

### Reservation Tracking and Reporting

- Historical usage records automatically created when GPUs are released
- Records include user, GPU ID, start/end times, duration, and reservation type
- Usage data stored in Redis with 90-day expiration to prevent unbounded growth
- `report` command aggregates usage by user with configurable time windows
- Supports both historical completed usage and current in-progress reservations

## GPU Provider Support

### NVIDIA GPUs
- **Requirements**: nvidia-smi command available
- **Driver Support**: CUDA drivers with nvidia-smi
- **Process Detection**: Uses `nvidia-smi --query-compute-apps` for process information
- **Memory Detection**: Uses `nvidia-smi --query-gpu` for memory usage
- **Validation**: Real-time validation of GPU usage and process ownership

### AMD GPUs
- **Requirements**: amd-smi command available (ROCm 5.7+)
- **Driver Support**: ROCm drivers with amd-smi
- **Process Detection**: Uses `amd-smi process --json` for process information
- **Memory Detection**: Uses `amd-smi metric --json` for memory usage
- **Validation**: Real-time validation of GPU usage and process ownership

### Fake GPUs (Development/Testing)
- **Requirements**: None - no real GPU hardware or drivers needed
- **Use Cases**: Development on laptops, CI/CD testing, demonstrations
- **Process Detection**: Returns empty process list (no real processes)
- **Memory Detection**: Returns 0 MB usage for all GPUs
- **Validation**: All GPUs shown as available with "Fake GPU" model name
- **Setup**: `canhazgpu admin --gpus 4 --provider fake`

### Provider Selection
- **Auto-detection**: Automatically detects available providers during initialization
- **Manual Selection**: Use `--provider nvidia`, `--provider amd`, or `--provider fake`
- **Single Provider**: System assumes only one GPU provider type per system

## Documentation

- Documentation is in the docs/ directory. All user facing features should be documented.
