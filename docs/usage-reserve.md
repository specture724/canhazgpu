# Manual GPU Reservations

The `reserve` command allows you to manually reserve GPUs for a specific duration without immediately running a command. This is useful for interactive development, planning work sessions, or blocking GPUs for maintenance.

## Basic Usage

```bash
canhazgpu reserve [--gpus <count> | --gpu-ids <ids>] [--duration <time>]
```

**Defaults:**
- `--gpus`: 1 GPU
- `--duration`: 30 minutes

**Options:**
- `--gpus, -g`: Number of GPUs to reserve
- `--gpu-ids`: Specific GPU IDs to reserve (comma-separated, e.g., 1,3,5)
- `--duration, -d`: How long to reserve the GPUs
- `--short, -s`: Output only GPU IDs (for use with command substitution)
- `--idle-timeout`: Release the reservation if no GPU usage is detected for this long (default: 15m, `0` disables, maximum 3h)
- `--claim`: Reserve a GPU that only your own unreserved process is using; if anyone else is on it, wait in the queue
- `--start`: Book the GPUs for a future time window instead of reserving now
- `--end`: End of that window (defaults to `--duration` after the start)

!!! note "GPU Selection"
    - Use `--gpus` to let canhazgpu select GPUs using the MRU-per-user strategy (with LRU fallback)
    - Use `--gpu-ids` when you need specific GPUs (e.g., for hardware requirements)
    - You can use both options together if `--gpus` matches the GPU ID count or is 1 (default)

## Duration Formats

canhazgpu supports flexible duration formats:

| Format | Description | Example |
|--------|-------------|---------|
| `30m` | 30 minutes | `--duration 30m` |
| `2h` | 2 hours | `--duration 2h` |
| `1d` | 1 day | `--duration 1d` |
| `0.5h` | 30 minutes (decimal) | `--duration 0.5h` |
| `90m` | 90 minutes | `--duration 90m` |
| `3.5d` | 3.5 days | `--duration 3.5d` |

## Idle Reservations Are Released

A manual reservation that shows no GPU activity for 15 minutes is released automatically, so a forgotten reservation does not keep GPUs away from other people:

```bash
# Default: dropped after 15 minutes without GPU usage
canhazgpu reserve --gpus 1 --duration 8h

# More grace for a slow start-up
canhazgpu reserve --gpus 1 --duration 8h --idle-timeout 1h

# Disable idle detection for this reservation
canhazgpu reserve --gpus 1 --duration 8h --idle-timeout 0
```

The clock starts when the reservation is made and resets whenever usage is detected, and `canhazgpu status` shows the countdown. `run` reservations are not affected — they end with their process.

**[→ Idle Reservation Timeout](features-idle-timeout.md)**

## Claiming a GPU You Are Already Using

If you started a process on a GPU without reserving it, `--claim` lets you adopt that GPU: when every process on it belongs to you, the reservation succeeds immediately. If somebody else is also using it (or the GPU is reserved by someone else), the request behaves like any other `reserve` and waits in the queue until the GPU is free.

```bash
# Adopt the GPU your own running job is already using
canhazgpu reserve --gpu-ids 2 --claim --duration 2h --note "adopt my job"
```

## Booking a Time Slot

With `--start`, `reserve` claims a future window instead of GPUs right now, like booking a meeting room:

```bash
# 14:00 to 16:00 today
canhazgpu reserve --start 14:00 --end 16:00 --gpus 2

# Four hours tomorrow morning
canhazgpu reserve --start 'tomorrow 09:00' --duration 4h --gpus 8
```

`canhazgpu schedule` prints the day's booking sheet. GPUs held by a booking are not handed out to reservations that would still be using them when the window starts.

**[→ Scheduled Bookings](usage-schedule.md)**

## Common Examples

### Quick Development Sessions
```bash
# Reserve 1 GPU for 2 hours
canhazgpu reserve --duration 2h

# Reserve 1 GPU for 30 minutes of testing
canhazgpu reserve --duration 30m
```

### Multi-GPU Development
```bash
# Reserve 2 GPUs for 4 hours
canhazgpu reserve --gpus 2 --duration 4h

# Reserve 4 GPUs for distributed development
canhazgpu reserve --gpus 4 --duration 6h

# Reserve specific GPU IDs
canhazgpu reserve --gpu-ids 0,2 --duration 4h
```

### Extended Work Sessions
```bash
# Full day (8 hours)
canhazgpu reserve

# Multi-day project work
canhazgpu reserve --gpus 2 --duration 2d

# Week-long research sprint
canhazgpu reserve --gpus 1 --duration 7d
```

## Use Cases

### Interactive Development
Perfect for Jupyter notebooks, IPython sessions, or iterative model development:

```bash
# Reserve GPU for notebook session
canhazgpu reserve --duration 4h
# Note the GPU IDs from the output, e.g., "Reserved 1 GPU(s): [2]"

# Manually set CUDA_VISIBLE_DEVICES
export CUDA_VISIBLE_DEVICES=2

# Start Jupyter with the reserved GPU
jupyter notebook

# Your notebooks now have exclusive GPU access
```

### Batch Job Preparation
Reserve GPUs while you prepare and test your batch jobs:

```bash
# Reserve GPUs for job prep
canhazgpu reserve --gpus 2 --duration 2h
# Note the GPU IDs from the output, e.g., "Reserved 2 GPU(s): [1, 3]"

# Manually set CUDA_VISIBLE_DEVICES
export CUDA_VISIBLE_DEVICES=1,3

# Test your scripts with the reserved GPUs
python test_distributed.py

# Run the actual job (using same GPUs)
python distributed_training.py

# Release when done
canhazgpu release
```

### Maintenance Windows
Block GPUs during system maintenance or updates:

```bash
# Block GPU during driver updates
canhazgpu reserve --gpus 8 --duration 1h

# Perform maintenance
sudo apt update && sudo apt upgrade nvidia-driver-*

# Release after maintenance
canhazgpu release
```

### Meeting and Presentation Prep
Ensure GPUs are available for demos and presentations:

```bash
# Reserve before important demo
canhazgpu reserve --gpus 1 --duration 3h

# Run demo applications
python demo_inference.py
jupyter notebook presentation.ipynb

# Release after presentation
canhazgpu release
```

## How Manual Reservations Work

### Allocation Process
1. **Validation**: Checks actual GPU usage with nvidia-smi
2. **Conflict Detection**: Excludes GPUs in unreserved use
3. **LRU Selection**: Chooses least recently used GPUs
4. **Time-based Expiry**: Sets expiration time based on duration
5. **Persistent Storage**: Saves reservation in Redis

### Environment Setup
Unlike `run` commands, manual reservations don't automatically set environment variables. You need to check which GPUs were allocated:

```bash
# Reserve GPUs
❯ canhazgpu reserve --gpus 2 --duration 4h
Reserved 2 GPU(s): [1, 3] for 4h 0m 0s

# Check current allocations
❯ canhazgpu status
 GPU │ STATUS    │ USER  │ DURATION │ TYPE   │ DETAILS          │ MEMORY            │ NOTE │ UTIL
─────┼───────────┼───────┼──────────┼────────┼──────────────────┼───────────────────┼──────┼──────
 1   │ ● IN_USE  │ alice │ 30s      │ MANUAL │ expires in 3h 59m │ no usage detected │ -    │ 0%
 3   │ ● IN_USE  │ alice │ 30s      │ MANUAL │ expires in 3h 59m │ no usage detected │ -    │ 0%

# Manually set CUDA_VISIBLE_DEVICES
export CUDA_VISIBLE_DEVICES=1,3
python your_script.py
```

### Expiration and Cleanup
Manual reservations automatically expire after the specified duration:

```text
❯ canhazgpu status
 GPU │ STATUS      │ USER    │ DURATION │ TYPE   │ DETAILS                    │ MEMORY            │ NOTE │ UTIL
─────┼─────────────┼─────────┼──────────┼────────┼────────────────────────────┼───────────────────┼──────┼──────
 1   │ ● IN_USE    │ alice   │ 30s      │ MANUAL │ expires in 3h 59m          │ no usage detected │ -    │ 0%
 2   │ ● IN_USE    │ bob     │ 1h 30m   │ RUN    │ heartbeat 5s ago           │ 8452MB, 1 processes│ -   │ 0%
 3   │ ● IN_USE    │ alice   │ 30s      │ MANUAL │ expires in 3h 59m          │ no usage detected │ -    │ 0%
 0   │ ● AVAILABLE │ -       │ -        │ -      │ free for 1h 15m            │ 4MB used          │ -    │ 0%
```

## Releasing Reservations

### Manual Release
Release all your manual reservations immediately:

```bash
❯ canhazgpu release
Released 2 GPU(s): [1, 3]
```

### Checking Your Reservations
Use `status` to see your current reservations:

```text
❯ canhazgpu status
 GPU │ STATUS      │ USER    │ DURATION │ TYPE   │ DETAILS                    │ MEMORY            │ NOTE │ UTIL
─────┼─────────────┼─────────┼──────────┼────────┼────────────────────────────┼───────────────────┼──────┼──────
 1   │ ● IN_USE    │ alice   │ 30s      │ MANUAL │ expires in 3h 59m          │ no usage detected │ -    │ 0%
 2   │ ● IN_USE    │ bob     │ 1h 30m   │ RUN    │ heartbeat 5s ago           │ 8452MB, 1 processes│ -   │ 0%
 3   │ ● IN_USE    │ alice   │ 30s      │ MANUAL │ expires in 3h 59m          │ no usage detected │ -    │ 0%
 0   │ ● AVAILABLE │ -       │ -        │ -      │ free for 1h 15m            │ 4MB used          │ -    │ 0%
```

## Error Handling

### Insufficient GPUs
```bash
❯ canhazgpu reserve --gpus 4 --duration 2h
Error: Not enough GPUs available. Requested: 4, Available: 2 (2 GPUs in use without reservation - run 'canhazgpu status' for details)
```

Check the status and try again with fewer GPUs or wait for others to finish.

### Invalid Duration Format
```bash
❯ canhazgpu reserve --duration 2hours
Error: Invalid duration format. Use formats like: 30m, 2h, 1d, 0.5h
```

Use the supported duration formats listed above.

### Allocation Lock Contention
```bash
❯ canhazgpu reserve --gpus 2
Error: Failed to acquire allocation lock after 5 attempts
```

Multiple users are trying to allocate GPUs simultaneously. Wait a few seconds and try again.

## Best Practices

### Duration Planning
- **Start conservative**: Reserve for shorter periods initially
- **Extend if needed**: Use `reserve` again to extend (requires releasing first)
- **Plan for interruptions**: Don't reserve longer than you'll actually use

### Resource Efficiency
```bash
# Good: Reserve what you need
canhazgpu reserve --gpus 1 --duration 2h

# Wasteful: Over-reserving
canhazgpu reserve --gpus 8 --duration 24h  # Only if you really need this
```

### Team Coordination
- **Communicate**: Let teammates know about long reservations
- **Release early**: Use `canhazgpu release` when done early
- **Check conflicts**: Use `canhazgpu status` before making large reservations

### Development Workflow
```bash
# Efficient development cycle
canhazgpu reserve --duration 1h           # Start small
# ... work for 45 minutes ...
canhazgpu reserve --duration 30m          # Extend if needed (after releasing)
# ... finish work ...
canhazgpu release                         # Clean up immediately
```

## Integration Examples

### Shell Scripts

The `--short` flag outputs only the GPU IDs, making it easy to set `CUDA_VISIBLE_DEVICES` in scripts:

```bash
#!/bin/bash
set -e

# Reserve GPUs and set environment variable in one step
export CUDA_VISIBLE_DEVICES=$(canhazgpu reserve --gpus 2 --duration 3h --short)

echo "Using GPUs: $CUDA_VISIBLE_DEVICES"
echo "Starting data processing pipeline..."
python preprocess.py
python feature_extraction.py
python model_training.py

echo "Releasing GPUs..."
canhazgpu release

echo "Processing complete!"
```

### Python Integration

Using the `--short` flag simplifies Python integration:

```python
import subprocess
import os

def reserve_gpus(count=1, duration="2h"):
    """Reserve GPUs and set CUDA_VISIBLE_DEVICES"""
    result = subprocess.run([
        "canhazgpu", "reserve",
        "--gpus", str(count),
        "--duration", duration,
        "--short"
    ], capture_output=True, text=True, check=True)

    gpu_ids = result.stdout.strip()
    os.environ['CUDA_VISIBLE_DEVICES'] = gpu_ids
    return gpu_ids

def release_gpus():
    """Release all manual reservations"""
    subprocess.run(["canhazgpu", "release"], check=True)

# Usage
try:
    gpu_ids = reserve_gpus(2, "1h")
    print(f"Using GPUs: {gpu_ids}")

    # Your GPU work here
    import torch
    print(f"PyTorch sees {torch.cuda.device_count()} GPUs")

finally:
    release_gpus()
```

Manual reservations provide fine-grained control over GPU allocation, making them perfect for interactive development and planned work sessions.