# Unreserved Usage Detection

One of canhazgpu's key features is detecting and handling GPUs that are being used without proper reservations. This prevents resource conflicts and enforces fair resource sharing policies.

## What is Unreserved Usage?

Unreserved usage occurs when:
- A GPU has active processes consuming more than the memory threshold (default 100 MB)
- No proper reservation exists for that GPU in the system
- The GPU usage was not coordinated through canhazgpu

### Common Scenarios
```bash
# These create unreserved usage:
CUDA_VISIBLE_DEVICES=0,1 python train.py           # Direct GPU access
jupyter notebook                                    # Jupyter using default GPUs
docker run --gpus all pytorch/pytorch python       # Container with GPU access
python -c "import torch; torch.cuda.set_device(2)" # Explicit GPU selection
```

```bash
# These are proper usage:
canhazgpu run --gpus 2 -- python train.py          # Proper reservation
canhazgpu reserve --gpus 1 && jupyter notebook     # Manual reservation first
```

## Detection Methods

### Real-Time Scanning
canhazgpu detects unreserved usage through:

1. **nvidia-smi Integration**: Queries actual GPU processes and memory usage
2. **Process Ownership Detection**: Identifies which users are running processes
3. **Memory Threshold**: Considers GPUs above the threshold (default 100 MB) as "in use"
4. **Cross-Reference**: Compares actual usage against Redis reservation database

### Detection Timing
Unreserved usage detection runs:
- **Before every allocation** (`run` and `reserve` commands)
- **During status checks** (`status` command)
- **Atomically during allocation** (within Redis transactions)

## Status Display

### Single Unreserved User
```text
 GPU │ STATUS       │ USER │ DETAILS                                                │ MEMORY               │ MODEL
─────┼──────────────┼──────┼────────────────────────────────────────────────────────┼──────────────────────┼────────────────────────────
 2   │ ⚠ UNRESERVED │ bob  │ used by PID 12345 (python3, 2h3m), PID 67890 (jupyter) │ 1024MB, 2 processes  │ mistralai/Mistral-7B-...
```

**Information shown:**
- `STATUS`: `UNRESERVED` marks the GPU as used without a reservation
- `USER`: The user running unreserved processes (bob)
- `DETAILS`: Process IDs, names and how long they have been running
- `MEMORY`: Total memory consumption and process count

### Multiple Unreserved Users
```text
 GPU │ STATUS       │ USER              │ DETAILS                                                       │ MEMORY
─────┼──────────────┼───────────────────┼──────────────────────────────────────────────────────────────┼─────────────────────
 3   │ ⚠ UNRESERVED │ alice,bob,charlie │ used by PID 12345 (python3, 2h3m), PID 23456 (pytorch, 5m)... │ 2048MB, 4 processes
```

**Information shown:**
- `USER`: All users with processes on this GPU (alice,bob,charlie)
- `DETAILS`: Process IDs, names and runtimes (truncated for readability)
- `MEMORY`: Total memory consumption and process count

### Process Details
The system attempts to show:
- **Process ID (PID)**: Unique identifier for the process
- **Process name**: Executable name (python3, jupyter, etc.)
- **User ownership**: Which user launched the process

## Impact on Allocation

### Automatic Exclusion
When unreserved usage is detected:

1. **Pre-allocation scan** identifies GPUs with unreserved usage
2. **Exclusion from available pool** removes those GPUs from allocation candidates
3. **LRU calculation** operates only on truly available GPUs
4. **Error reporting** provides detailed feedback about unavailable resources

### Error Messages
```bash
❯ canhazgpu run --gpus 3 -- python train.py
Error: Not enough GPUs available. Requested: 3, Available: 1 (2 GPUs in use without reservation - run 'canhazgpu status' for details)
```

**Message breakdown:**
- `Requested: 3`: Number of GPUs you asked for
- `Available: 1`: Number of GPUs actually available for allocation
- `2 GPUs in use without reservation`: Number of GPUs excluded due to unreserved usage
- Suggests running `status` for detailed information

## Handling Unreserved Usage

### Investigation Steps

1. **Check detailed status**:
   ```bash
   canhazgpu status
   ```

2. **Identify unreserved users and processes**:
```text
 GPU │ STATUS       │ USER │ DETAILS                                                │ MEMORY               │ MODEL
─────┼──────────────┼──────┼────────────────────────────────────────────────────────┼──────────────────────┼────────────────────────────
 2   │ ⚠ UNRESERVED │ bob  │ used by PID 12345 (python3, 2h3m), PID 67890 (jupyter) │ 1024MB, 2 processes  │ mistralai/Mistral-7B-...
```

3. **Contact the user** to coordinate proper usage

4. **Verify process details** if needed:
   ```bash
   ps -p 12345 -o pid,ppid,user,command
   ```

### Resolution Options

#### Option 1: User Stops Unreserved Usage
```bash
# User bob stops their processes
kill 12345 67890

# Or gracefully shuts down
# Ctrl+C in their terminal, close Jupyter, etc.
```

#### Option 2: User Creates Proper Reservation
```bash
# User bob creates a proper reservation
canhazgpu reserve --gpus 1 --duration 4h

# Then continues their work
# GPU will now show as properly reserved
```

#### Option 3: Wait for Completion
```bash
# Wait for unreserved processes to finish naturally
# Then GPU will become available again
```

## User Education and Policies

### Training Users
Help users understand proper GPU usage:

```bash
# Wrong way
CUDA_VISIBLE_DEVICES=0 python train.py

# Right way  
canhazgpu run --gpus 1 -- python train.py
```

### Policy Enforcement Examples

#### Gentle Reminder
```bash
#!/bin/bash
# unreserved_reminder.sh
canhazgpu status --json | jq -r '.[] | select(.status == "UNRESERVED") |
    "Reminder: GPU \(.gpu_id) is used without a reservation by \(.unreserved_users | join(","))"'
```

#### Automated Notification
```bash
#!/bin/bash
# unreserved_notify.sh
canhazgpu status --json | jq -r '.[] | select(.status == "UNRESERVED") | .unreserved_users[]' | \
    sort -u | while read -r user; do
        echo "Please use canhazgpu for GPU reservations" | wall -n "$user"
    done
```

## Advanced Detection Scenarios

### Multi-GPU Unreserved Usage
```text
 GPU │ STATUS       │ USER  │ DETAILS                                │ MEMORY               │ MODEL
─────┼──────────────┼───────┼────────────────────────────────────────┼──────────────────────┼─────────────────────────────
 1   │ ⚠ UNRESERVED │ alice │ used by PID 11111 (python3, 2h3m)      │ 2048MB, 1 processes  │ NousResearch/Nous-Hermes...
 2   │ ⚠ UNRESERVED │ alice │ used by PID 11111 (python3, 2h3m)      │ 2048MB, 1 processes  │ NousResearch/Nous-Hermes...
 5   │ ⚠ UNRESERVED │ alice │ used by PID 11111 (python3, 2h3m)      │ 2048MB, 1 processes  │ NousResearch/Nous-Hermes...
```

Same process using multiple GPUs - user should reserve all needed GPUs properly.

### Mixed Usage Patterns
```text
 GPU │ STATUS       │ USER    │ DURATION │ TYPE   │ DETAILS                                        │ MEMORY               │ MODEL
─────┼──────────────┼─────────┼──────────┼────────┼────────────────────────────────────────────────┼──────────────────────┼────────────────────────────
 0   │ ● IN_USE     │ bob     │ 1h 30m   │ RUN    │ heartbeat 5s ago                                │ 8452MB, 1 processes  │ microsoft/DialoGPT-large
 1   │ ⚠ UNRESERVED │ alice   │ -        │ -      │ used by PID 22222 (jupyter, 1h5m)              │ 1024MB, 1 processes  │ teknium/OpenHermes-2.5...
 2   │ ● AVAILABLE  │ -       │ -        │ -      │ free for 2h                                    │ 45MB used            │ -
 3   │ ● IN_USE     │ charlie │ 45m     │ MANUAL │ expires in 7h 15m                               │ no usage detected    │ -
```

Mix of proper usage, unreserved usage, and available GPUs.

### Container-Based Unreserved Usage
```text
 GPU │ STATUS       │ USER      │ DETAILS                                                          │ MEMORY
─────┼──────────────┼───────────┼──────────────────────────────────────────────────────────────────┼─────────────────────
 1   │ ⚠ UNRESERVED │ root,alice│ used by PID 33333 (dockerd, 1h2m), PID 44444 (python3, 30m)...   │ 3072MB, 3 processes
```

Docker containers running with GPU access - users should coordinate container GPU usage through canhazgpu.

## System Integration

### Monitoring Integration
```python
def check_unreserved_usage():
    """Check for unreserved GPU usage and return details"""
    result = subprocess.run(['canhazgpu', 'status', '--json'],
                          capture_output=True, text=True)
    statuses = json.loads(result.stdout)

    return [
        {
            'gpu_id': gpu['gpu_id'],
            'users': gpu.get('unreserved_users', []),
            'memory_mb': gpu.get('memory_mb', 0),
            'process_info': gpu.get('process_info', ''),
        }
        for gpu in statuses
        if gpu['status'] == 'UNRESERVED'
    ]

# Usage in monitoring
violations = check_unreserved_usage()
if violations:
    for v in violations:
        logger.warning(f"Unreserved GPU usage: {v['users']} using {v['memory_mb']}MB")
```

### Automated Response

Detecting and reacting to unreserved usage is built in — use `canhazgpu guard`
instead of scraping `status` output:

```bash
# Warn offenders in the terminal running their job, every 15 seconds
canhazgpu guard

# Warn, then terminate processes that ignore the warnings
canhazgpu guard --enforce

# Single scan, for cron
canhazgpu guard --once
```

The guard warns people where they will actually see it (the standard error of the
offending process and their open terminals), repeats the warning on an interval,
records every violation for later reporting, and optionally escalates to
SIGINT → SIGTERM → SIGKILL. It also detects usage that `status` cannot show:
a process belonging to somebody other than the GPU's reservation holder.

**[→ Guard: Enforcing Reservations](features-guard.md)**

For site-specific reactions such as mail or chat notifications, drive them from
the recorded violations rather than from parsed table output:

```bash
#!/bin/bash
# unreserved_response.sh - run from cron, alongside 'canhazgpu guard'

canhazgpu violations --json > /tmp/violations.json

# Log the violations
jq -r '.[] | "\(.kind) GPU \(.gpu_id) PID \(.pid) user \(.user) \(.memory_mb)MB"' \
    /tmp/violations.json | while read -r line; do
        echo "$(date): $line" >> /var/log/gpu_violations.log
    done

# Notify users who have already been warned at least twice
jq -r '.[] | select(.warn_count >= 2) | .user' /tmp/violations.json | sort -u | \
    while read -r user; do
        echo "GPU Policy: please reserve GPUs with 'canhazgpu reserve' or 'canhazgpu run'" | \
            mail -s "GPU Usage Policy" "$user@company.com"
    done

# Alert administrators when anything is outstanding
if [ -s /tmp/violations.json ] && [ "$(jq length /tmp/violations.json)" -gt 0 ]; then
    jq -r '.[] | "\(.user) on GPU \(.gpu_id) (\(.memory_mb)MB)"' /tmp/violations.json | \
        mail -s "GPU Policy Violations Detected" admin@company.com
fi
```

## Best Practices

### For Users
- **Always use canhazgpu** for GPU access
- **Check status first** with `canhazgpu status` before starting work
- **Reserve appropriately** - don't over-reserve, but don't skip reservations
- **Clean up** - release manual reservations when done

### For Administrators
- **Run the guard** - `canhazgpu guard` detects and warns automatically, see [Guard](features-guard.md)
- **Educate users** - provide training on proper GPU reservation practices
- **Set clear policies** - document expected GPU usage procedures, and whether the guard runs with `--enforce`
- **Review the history** - `canhazgpu violations --history --days 30` shows who repeatedly bypasses the tool

### For System Integration
- **Integrate with job schedulers** - ensure SLURM/PBS jobs use canhazgpu
- **Container orchestration** - configure Kubernetes/Docker to respect reservations
- **Development tools** - configure IDEs and notebooks to check for reservations

Unreserved usage detection ensures fair resource sharing and prevents the frustrating "GPU out of memory" errors that occur when multiple users unknowingly compete for the same resources.