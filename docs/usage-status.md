# Status Monitoring

The `status` command provides real-time visibility into GPU allocation and usage across your system. It combines reservation tracking with actual GPU usage validation to give you a complete picture of resource utilization.

## Basic Usage

```bash
# Table output (default)
canhazgpu status

# JSON output for programmatic use
canhazgpu status --json
canhazgpu status -j
```

No options are required for basic usage - the command automatically validates all GPUs and shows comprehensive status information in either table or JSON format. By default the table output also prints **today's schedule** below the status table, so one command shows both what is happening now and what is booked later. Use `--no-schedule` to turn that off.

## Output Formats

### Table Output (Default)

The default output format provides a human-readable table with aligned columns:
```bash
❯ canhazgpu status
GPU  STATUS      USER     DURATION     TYPE    MODEL                    DETAILS                  MEMORY          NOTE  UTIL
---  ------      ----     --------     ----    -----                    -------                  ----------      ----  ----
0    AVAILABLE   -        -            -       -                        free for 0h 30m 15s     45MB used        -     0%
1    IN_USE      alice    0h 15m 30s   RUN     meta-llama/Llama-2-7b-chat-hf  heartbeat 0h 0m 5s ago, processes: PID 12345 (2h3m)   8452MB, 1 processes   -     87%
2    UNRESERVED  user bob -            -       mistralai/Mistral-7B-Instruct-v0.1    used by PID 12345 (2h3m), PID 67890 (5m)  1024MB, 2 processes  -  42%
3    IN_USE      charlie  1h 2m 15s    MANUAL  -                        expires in 3h 15m 45s   no usage detected  -   5%
4    UNRESERVED  users alice, bob and charlie  -  -  meta-llama/Meta-Llama-3-8B-Instruct  used by PID 12345 (python3), PID 23456 (pytorch) and 2 more  2048MB, 3 processes  -  99%
```

### JSON Output

For programmatic integration, use the `--json` or `-j` flag to get structured JSON output:

```bash
❯ canhazgpu status --json
[
  {
    "gpu_id": 0,
    "status": "AVAILABLE",
    "details": "free for 0h 30m 15s",
    "validation": "45MB used",
    "last_released": "2025-07-07T18:24:56.100193782Z",
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
    "utilization_percent": 87,
    "model": {
      "provider": "meta-llama",
      "model": "meta-llama/Llama-2-7b-chat-hf"
    },
    "last_heartbeat": "2025-07-07T18:26:27.627148565Z"
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
      "provider": "mistralai",
      "model": "mistralai/Mistral-7B-Instruct-v0.1"
    }
  },
  {
    "gpu_id": 3,
    "status": "IN_USE",
    "user": "charlie",
    "duration": "1h 2m 15s",
    "type": "MANUAL",
    "details": "expires in 3h 15m 45s",
    "validation": "no usage detected",
    "expiry_time": "2025-07-08T01:48:44Z"
  }
]
```

#### JSON Field Reference

| Field | Type | Description |
|-------|------|-------------|
| `gpu_id` | integer | GPU identifier (0, 1, 2, etc.) |
| `status` | string | Current status: `AVAILABLE`, `IN_USE`, `UNRESERVED`, `ERROR` |
| `user` | string | Username (if GPU is reserved) |
| `duration` | string | How long the GPU has been reserved |
| `type` | string | Reservation type: `RUN`, `MANUAL` |
| `details` | string | Context-specific information |
| `validation` | string | Memory usage and process information |
| `model` | object | Detected AI model information |
| `model.provider` | string | Model provider (e.g., "meta-llama", "openai") |
| `model.model` | string | Full model identifier |
| `last_released` | string | ISO timestamp when GPU was last released |
| `last_heartbeat` | string | ISO timestamp of last heartbeat |
| `expiry_time` | string | ISO timestamp when manual reservation expires |
| `unreserved_users` | array | List of users with unreserved processes |
| `process_info` | string | Process details for unreserved usage, including how long each process has been running |
| `processes` | array | Processes currently using the GPU. Default: `pid` + `elapsed_seconds`; `process_name` is included with `-v` (max 2) or `-vv` (all) |
| `utilization_percent` | integer | GPU utilization reported by the provider (0-100, e.g. nvidia-smi `utilization.gpu`) |
| `idle_timeout` | string | Idle timeout of a manual reservation (e.g. `15m`) |
| `idle_for` | string | How long the holder has not used the GPU |
| `booking_id` | string | Scheduled booking that created this reservation |
| `foreign_users` | array | Users on this GPU who do not hold the reservation |
| `foreign_processes` | integer | Number of their processes |
| `foreign_memory_mb` | integer | Memory those processes hold |
| `error` | string | Error message (for ERROR status) |

The last column, **UTIL**, is the GPU utilization percentage reported by the provider (nvidia-smi `utilization.gpu` for NVIDIA; best-effort for AMD), from 0 to 100%.

## Status Information Explained

### Status Types

#### AVAILABLE
```bash
0    AVAILABLE   -        -            -       -                        free for 0h 30m 15s     45MB used
```

- **Meaning**: GPU is free and can be allocated
- **Time info**: Shows how long it has been free (for LRU allocation). The clock starts at the last release **or** the last time a process was observed on the GPU, including unreserved usage
- **Validation**: Shows current memory usage (usually low baseline usage)

#### IN USE (Proper Reservations)
```bash
1    IN_USE      alice    0h 15m 30s   RUN     openai/whisper-large-v3  heartbeat 0h 0m 5s ago, processes: PID 12345 (2h3m)   8452MB, 1 processes
```

**Components:**
- `alice`: Username who reserved the GPU
- `0h 15m 30s`: How long it's been reserved
- `RUN`: Reservation type (RUN or MANUAL)
- `meta-llama/Llama-2-7b-chat-hf`: Detected AI model (if any)
- `heartbeat 0h 0m 5s ago`: Additional reservation info
- `8452MB, 1 processes`: Actual usage validation

**Reservation Types:**

**Run-type reservations:**
```bash
1    IN_USE      alice    0h 15m 30s   RUN     meta-llama/Llama-2-7b-chat-hf  heartbeat 0h 0m 5s ago, processes: PID 12345 (2h3m)   8452MB, 1 processes
```
- Created by `canhazgpu run` command
- Maintained by periodic heartbeats
- Auto-released when process ends or heartbeat stops
- Shows heartbeat timing in DETAILS column

**Manual reservations:**
```bash
3    IN_USE      charlie  1h 2m 15s    MANUAL  -                        expires in 3h 15m 45s   no usage detected
```
- Created by `canhazgpu reserve` command  
- Time-based expiry
- Must be manually released or will expire
- Shows expiry timing in DETAILS column

#### UNRESERVED
```bash
2    UNRESERVED  user bob -            -       mistralai/Mistral-7B-Instruct-v0.1    used by PID 12345 (2h3m), PID 67890 (5m)  1024MB, 2 processes  -
```

- **Meaning**: Someone is using the GPU without proper reservation
- **User identification**: Shows which user(s) are running unreserved processes
- **Model detection**: Shows detected AI model in MODEL column
- **Process details**: Lists PIDs and how long each process has been running by default; process names appear with `-v` (max 2) or `-vv` (all)
- **Impact**: This GPU will be excluded from allocation until usage stops

**Multiple unreserved users:**
```bash
4    UNRESERVED  users alice, bob and charlie  -  -  meta-llama/Meta-Llama-3-8B-Instruct  used by PID 12345 (2h3m), PID 23456 (5m) and 2 more  2048MB, 3 processes  -
```

#### FOREIGN
```bash
0    ⚠ FOREIGN   alice    1h 0m 0s     MANUAL  -   expires in 59m, idle 10m, used by bob   8452MB, 1 processes
```

- **Meaning**: The GPU *is* reserved, but the processes on it belong to somebody else
- **USER column**: still the person who holds the reservation
- **DETAILS column**: who is actually using the GPU, and how much memory they hold
- **Typical causes**: somebody started a job on a GPU they did not reserve, or a job kept running after a [scheduled booking](usage-schedule.md) preempted its reservation
- **Idle interaction**: usage by others does not keep the reservation alive, so the reservation still goes [idle](features-idle-timeout.md) and is reclaimed on schedule
- **JSON**: `status` stays `IN_USE` for compatibility; the detail is in `foreign_users`, `foreign_processes` and `foreign_memory_mb`

Run [`canhazgpu guard`](features-guard.md) to warn the offender automatically, and
`canhazgpu violations` to see the history.

### Validation Information

The MEMORY column shows actual GPU usage detected via nvidia-smi:

#### Confirms Proper Usage
```bash
8452MB, 1 processes
```
- GPU is reserved and actually being used
- Shows memory usage and process count
- Indicates healthy, proper resource utilization

#### No Usage Detected
```bash
no usage detected
```
- GPU is reserved but no processes are running
- Might indicate:
  - Preparation phase before starting work
  - Finished work but reservation not yet released
  - Stale reservation that should be cleaned up

#### Baseline Usage Only
```bash
45MB used
```
- Available GPU with minimal background usage
- Normal baseline memory usage from GPU drivers
- Safe to allocate

### Process Detail Levels

By default DETAILS shows only the PID and how long the process has been running:

```text
1    IN_USE      alice    0h 15m 30s   RUN     heartbeat 0h 0m 5s ago, processes: PID 12345 (2h3m)   8452MB, 1 processes
```

`-v` adds process names for up to two processes:

```bash
canhazgpu status -v
```

```text
1    IN_USE      alice    0h 15m 30s   RUN     heartbeat 0h 0m 5s ago, processes: PID 12345 (python, 2h3m), PID 67890 (jupyter, 5m)   8452MB, 2 processes
```

`-vv` shows every process with its name:

```bash
canhazgpu status -vv
```

```text
1    IN_USE      alice    0h 15m 30s   RUN     heartbeat 0h 0m 5s ago, processes: PID 12345 (python, 2h3m), PID 67890 (jupyter, 5m), PID 11111 (train, 1m)   8452MB, 3 processes
```

## Monitoring Patterns

### Regular Health Checks
```bash
# Quick status check
canhazgpu status

# Monitor changes over time
watch -n 30 canhazgpu status

# Log status for analysis
canhazgpu status >> gpu_usage_log.txt
```

### Identifying Problems

#### Stale Reservations
```bash
3    IN_USE      alice    8h 45m 0s    MANUAL  -                        expires in 0h 15m 0s    no usage detected
```
- Long reservation with no actual usage
- User likely forgot to release
- Will auto-expire soon

#### Heartbeat Issues
```bash
1    IN_USE      bob      2h 30m 0s    RUN     codellama/CodeLlama-7b-Instruct-hf        heartbeat 0h 5m 30s ago 8452MB, 1 processes
```
- Last heartbeat was 5+ minutes ago (should be <1 minute)
- Possible network issues or process problems
- May auto-release due to heartbeat timeout

#### Unreserved Usage Patterns
```bash
2    UNRESERVED  user charlie  -       -       microsoft/DialoGPT-large     used by PID 12345 (python3)      12288MB, 1 processes
5    UNRESERVED  user charlie  -       -       NousResearch/Nous-Hermes-2-Yi-34B   used by PID 23456 (jupyter)       8192MB, 1 processes
```
- Same user using multiple GPUs without reservation
- High memory usage indicates active workloads
- Should contact user to use proper reservation system

### Team Coordination

#### Planning Allocations
```bash
❯ canhazgpu status
GPU  STATUS     USER   DURATION   TYPE    MODEL            DETAILS                  MEMORY
---  ------     ----   --------   ----    -----            -------                  ----------
0    AVAILABLE  -      -          -       -                free for 2h 0m 0s       1MB used     # Good candidate
1    AVAILABLE  -      -          -       -                free for 0h 30m 0s      1MB used     # Recently used
2    IN_USE     alice  0h 5m 0s   RUN     teknium/OpenHermes-2.5-Mistral-7B  heartbeat 0h 0m 3s ago   2048MB, 1 processes  # Just started
3    IN_USE     bob    3h 45m 0s  MANUAL  -                expires in 0h 15m 0s    no usage detected    # Expiring soon
```

From this, you can see:
- GPU 0 is the best choice (LRU)
- GPU 1 was used recently
- GPU 3 will be available in 15 minutes

#### Resource Conflicts
```bash
❯ canhazgpu status
Available GPUs: 2 out of 8
In use with reservations: 4 GPUs  
Unreserved usage: 2 GPUs
```

Clear indication that unreserved usage is reducing available capacity.

## Advanced Monitoring

### Integration with Monitoring Systems

#### Prometheus/Grafana Integration
```bash
#!/bin/bash
# gpu_metrics.sh - Export metrics for monitoring

STATUS=$(canhazgpu status)

# Count GPU states
AVAILABLE=$(echo "$STATUS" | grep "AVAILABLE" | wc -l)
IN_USE=$(echo "$STATUS" | grep "IN USE by" | wc -l)
UNAUTHORIZED=$(echo "$STATUS" | grep "WITHOUT RESERVATION" | wc -l)

# Export metrics
echo "gpu_available $AVAILABLE"
echo "gpu_in_use $IN_USE"  
echo "gpu_unreserved $UNAUTHORIZED"
echo "gpu_total $((AVAILABLE + IN_USE + UNAUTHORIZED))"
```

#### Log Analysis
```bash
# Capture status with timestamps
while true; do
    echo "$(date): $(canhazgpu status)" >> gpu_monitoring.log
    sleep 300  # Every 5 minutes
done

# Analyze usage patterns
grep "AVAILABLE" gpu_monitoring.log | wc -l
grep "WITHOUT RESERVATION" gpu_monitoring.log | cut -d: -f2- | sort | uniq -c
```

### Automated Alerts

#### Unreserved Usage Detection
```bash
#!/bin/bash
# unreserved_alert.sh

STATUS=$(canhazgpu status)
UNAUTHORIZED=$(echo "$STATUS" | grep "WITHOUT RESERVATION")

if [ -n "$UNAUTHORIZED" ]; then
    echo "ALERT: Unreserved GPU usage detected!"
    echo "$UNAUTHORIZED"
    
    # Send notification (customize as needed)
    echo "$UNAUTHORIZED" | mail -s "GPU Policy Violation" admin@company.com
fi
```

#### Capacity Monitoring
```bash
#!/bin/bash
# capacity_alert.sh

STATUS=$(canhazgpu status)
AVAILABLE=$(echo "$STATUS" | grep "AVAILABLE" | wc -l)
TOTAL=$(echo "$STATUS" | wc -l)
UTILIZATION=$((100 * (TOTAL - AVAILABLE) / TOTAL))

if [ $UTILIZATION -gt 90 ]; then
    echo "WARNING: GPU utilization at ${UTILIZATION}%"
    canhazgpu status
fi
```

## Status Command Integration

### Shell Scripts
```bash
#!/bin/bash
# wait_for_gpu.sh - Wait until GPUs become available

echo "Waiting for GPUs to become available..."
while true; do
    AVAILABLE=$(canhazgpu status | grep "AVAILABLE" | wc -l)
    if [ $AVAILABLE -ge 2 ]; then
        echo "GPUs available! Starting job..."
        canhazgpu run --gpus 2 -- python train.py
        break
    fi
    echo "Only $AVAILABLE GPUs available, waiting..."
    sleep 60
done
```

### Python Integration

#### Using JSON Output (Recommended)
```python
import subprocess
import json
import time
from datetime import datetime, timezone

def get_gpu_status():
    """Get GPU status as structured data using JSON output"""
    result = subprocess.run(['canhazgpu', 'status', '--json'], 
                          capture_output=True, text=True)
    
    if result.returncode != 0:
        raise RuntimeError(f"Status check failed: {result.stderr}")
    
    return json.loads(result.stdout)

def get_available_gpus():
    """Get list of available GPU IDs"""
    status = get_gpu_status()
    return [gpu['gpu_id'] for gpu in status if gpu['status'] == 'AVAILABLE']

def get_gpu_by_user(username):
    """Get GPUs reserved by a specific user"""
    status = get_gpu_status()
    return [gpu for gpu in status if gpu.get('user') == username]

def check_unreserved_usage():
    """Check for unreserved GPU usage"""
    status = get_gpu_status()
    unreserved = [gpu for gpu in status if gpu['status'] == 'UNRESERVED']
    
    if unreserved:
        print("WARNING: Unreserved GPU usage detected!")
        for gpu in unreserved:
            users = gpu.get('unreserved_users', [])
            process_info = gpu.get('process_info', 'Unknown processes')
            print(f"  GPU {gpu['gpu_id']}: Users {users} - {process_info}")
    
    return unreserved

def get_gpu_utilization():
    """Calculate GPU utilization statistics"""
    status = get_gpu_status()
    total = len(status)
    available = len([gpu for gpu in status if gpu['status'] == 'AVAILABLE'])
    in_use = len([gpu for gpu in status if gpu['status'] == 'IN_USE'])
    unreserved = len([gpu for gpu in status if gpu['status'] == 'UNRESERVED'])
    
    return {
        'total': total,
        'available': available,
        'in_use': in_use,
        'unreserved': unreserved,
        'utilization_percent': ((in_use + unreserved) / total) * 100
    }

# Usage examples
print("Available GPUs:", get_available_gpus())
print("Utilization:", get_gpu_utilization())
check_unreserved_usage()
```

#### Legacy Text Parsing
```python
import subprocess
import re
import time

def get_gpu_status_legacy():
    """Parse canhazgpu status text output (legacy method)"""
    result = subprocess.run(['canhazgpu', 'status'], 
                          capture_output=True, text=True)
    
    if result.returncode != 0:
        raise RuntimeError(f"Status check failed: {result.stderr}")
    
    status = {}
    for line in result.stdout.strip().split('\n'):
        if line.startswith('GPU '):
            gpu_id = int(line.split(':')[0].split()[1])
            if 'AVAILABLE' in line:
                status[gpu_id] = 'available'
            elif 'WITHOUT RESERVATION' in line:
                status[gpu_id] = 'unreserved'  
            elif 'IN USE' in line:
                status[gpu_id] = 'reserved'
    
    return status

def wait_for_gpus(count=1, timeout=3600):
    """Wait for specified number of GPUs to become available"""
    start_time = time.time()
    
    while time.time() - start_time < timeout:
        status = get_gpu_status()
        available = sum(1 for state in status.values() if state == 'available')
        
        if available >= count:
            return True
            
        print(f"Waiting for {count} GPUs... ({available} currently available)")
        time.sleep(30)
    
    return False

# Usage
if wait_for_gpus(2):
    print("GPUs available! Starting training...")
    subprocess.run(['canhazgpu', 'run', '--gpus', '2', '--', 'python', 'train.py'])
else:
    print("Timeout waiting for GPUs")
```

The `status` command is your primary tool for understanding GPU resource utilization, identifying conflicts, and coordinating with your team. Regular monitoring helps maintain efficient resource usage and prevents conflicts.