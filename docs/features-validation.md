# GPU Validation

canhazgpu integrates with nvidia-smi to provide real-time validation of GPU usage, ensuring that reservations match actual resource utilization and detecting unreserved usage.

## How Validation Works

### nvidia-smi Integration
The system uses nvidia-smi to query actual GPU processes and memory usage:

```bash
# canhazgpu internally runs commands like:
nvidia-smi --query-gpu=memory.used --format=csv,noheader,nounits
nvidia-smi --query-compute-apps=pid,process_name,gpu_uuid,used_memory --format=csv,noheader
```

### Process Owner Detection
For each GPU process, canhazgpu identifies the owner:

1. **Primary method**: Read `/proc/{pid}/status` to get the UID
2. **Fallback method**: Use `ps -o uid= -p {pid}` if `/proc` is unavailable
3. **Username resolution**: Convert UID to username using system user database

### Memory Threshold
GPUs with more than the configured memory threshold are considered "in use" (default: **100 MB**):

- **Below threshold**: Baseline GPU driver usage, considered available
- **Above threshold**: Active workload detected, GPU marked as in use

The threshold can be customized using the `--memory-threshold` flag:

```bash
# Use a lower threshold (512 MB) to detect lighter GPU usage
canhazgpu status --memory-threshold 512

# Use a higher threshold (2 GB) to ignore smaller allocations
canhazgpu status --memory-threshold 2048

# Apply to allocation commands as well
canhazgpu run --memory-threshold 512 --gpus 1 -- python train.py
```

## Validation Output

### Status Display
The validation information appears in the **MEMORY** column of the status output:

```text
❯ canhazgpu status
 GPU │ STATUS      │ USER    │ DURATION │ TYPE   │ DETAILS                                        │ MEMORY               │ MODEL           │ NOTE │ UTIL
─────┼─────────────┼─────────┼──────────┼────────┼────────────────────────────────────────────────┼──────────────────────┼─────────────────┼──────┼──────
 0   │ ● AVAILABLE │ -       │ -        │ -      │ free for 0h 30m 15s                            │ 45MB used            │ -               │ -    │ 0%
 1   │ ● IN_USE    │ alice   │ 0h 15m   │ RUN    │ heartbeat 5s ago, processes: PID 12345 (2h3m)  │ 8452MB, 1 processes  │ llama-2-7b-chat │ -    │ 87%
 2   │ ⚠ UNRESERVED│ bob     │ -        │ -      │ used by PID 12345 (python3, 2h3m)              │ 1024MB, 1 processes  │ -               │ -    │ 42%
 3   │ ● IN_USE    │ charlie │ 1h 2m    │ MANUAL │ expires in 3h 15m, idle 10m                     │ no usage detected    │ -               │ -    │ 5%
```

### Validation States

#### Normal Available GPU
```bash
45MB used
```
- Low memory usage indicates GPU is available
- Only driver baseline memory consumption
- Safe to allocate

#### Confirmed Reservation Usage
```bash
8452MB, 1 processes
```
- High memory usage confirms GPU is actively used
- Process count shows number of applications using GPU
- Validates that reservation matches actual usage

#### No Usage Detected
```bash
no usage detected
```
- GPU is reserved but not actually being used
- Could indicate:
  - Preparation phase (normal)
  - Finished work but reservation not released
  - Stale reservation (should be cleaned up)

#### Unauthorized Usage Detail
```text
 2   │ ⚠ UNRESERVED │ bob │ - │ - │ used by PID 12345 (python3, 2h3m), PID 67890 (jupyter, 5m) │ 1024MB, 2 processes
```
- `UNRESERVED` status identifies the GPU as used without a reservation
- DETAILS lists PIDs and process names (with `-v`/`-vv`), plus how long they have been running
- MEMORY quantifies the unreserved resource consumption

## Validation Benefits

### Prevents Double Allocation
Without validation, you might have scenarios like:
- GPU 1 is "AVAILABLE" according to Redis
- But someone is actually using GPU 1 without reservation
- New reservation could conflict with existing usage

With validation:
- GPU 1 would show `UNRESERVED`
- GPU 1 is automatically excluded from allocation
- Prevents conflicts and out-of-memory errors

### User Accountability
```text
 GPU │ STATUS       │ USER    │ DETAILS                               │ MEMORY
─────┼──────────────┼─────────┼───────────────────────────────────────┼─────────────────────
 2   │ ⚠ UNRESERVED │ bob     │ used by PID 12345 (python3, 2h3m)     │ 1024MB, 1 processes
 5   │ ⚠ UNRESERVED │ charlie │ used by PID 23456 (jupyter, 1h5m)     │ 8192MB, 1 processes
```

Clear identification of which users need to be contacted about policy compliance.

### Resource Optimization
```text
 GPU │ STATUS    │ USER  │ DURATION │ TYPE   │ DETAILS                    │ MEMORY            │ NOTE │ UTIL
─────┼───────────┼───────┼──────────┼────────┼────────────────────────────┼───────────────────┼──────┼──────
 3   │ ● IN_USE  │ alice │ 8h 0m    │ MANUAL │ expires in 30m, idle 10m    │ no usage detected │ -    │ 0%
```

Identifies stale reservations that could be released early to improve resource availability.

## Validation in Allocation

### Pre-Allocation Scanning
Before any GPU allocation, canhazgpu:

1. **Scans all GPUs** using nvidia-smi
2. **Identifies unreserved usage** and excludes those GPUs
3. **Updates available GPU pool** with only truly available GPUs
4. **Proceeds with allocation** using the validated pool

### Error Messages with Context
```bash
❯ canhazgpu run --gpus 3 -- python train.py
Error: Not enough GPUs available. Requested: 3, Available: 1 (2 GPUs in use without reservation - run 'canhazgpu status' for details)
```

The error message indicates:
- You requested 3 GPUs
- Only 1 is actually available
- 2 GPUs are being used without proper reservations
- Suggests checking status for more details

## Validation Edge Cases

### Multiple Users Per GPU
```text
 GPU │ STATUS       │ USER              │ DETAILS                                                        │ MEMORY
─────┼──────────────┼───────────────────┼───────────────────────────────────────────────────────────────┼─────────────────────
 4   │ ⚠ UNRESERVED │ alice,bob,charlie │ used by PID 12345 (python3, 2h3m), PID 23456 (pytorch, 5m) ... │ 2048MB, 4 processes
```

When multiple users have processes on the same GPU:
- All usernames are listed
- Process details may be truncated for readability
- Total memory usage is shown

### Process Information Limitations
Sometimes process details may be limited:
```text
 GPU │ STATUS       │ USER    │ DETAILS                                    │ MEMORY
─────┼──────────────┼─────────┼────────────────────────────────────────────┼─────────────────────
 4   │ ⚠ UNRESERVED │ unknown │ used by PID 12345 (unknown process, 2h3m)  │ 1024MB, 1 processes
```

This can happen when:
- Process owner cannot be determined (permission issues)
- Process name is not available
- Process terminates between detection and information gathering

### High Memory Baseline
```bash
850MB used
```

Some systems may have higher baseline GPU memory usage due to:
- Multiple GPU monitoring tools
- Persistent CUDA contexts
- Background ML services

The default threshold is **100 MB** and can be adjusted with `--memory-threshold` or `memory.threshold` in the configuration.

## Validation Performance

### Caching and Efficiency
- Validation runs only during allocation and status commands
- nvidia-smi queries are batched for efficiency
- Process information is gathered in parallel where possible

### Impact on System Performance
- Minimal overhead: validation takes typically <1 second
- No continuous monitoring or background processes
- GPU validation doesn't interfere with actual GPU workloads

## Integration with Other Features

### MRU-per-User Allocation
Validation integrates with the allocation strategy:
- Only validated available GPUs are considered for ranking
- Unauthorized GPUs are excluded from the candidate pool
- Last release timestamps are preserved for the LRU fallback

### Race Condition Protection
Validation is integrated into the atomic allocation process:
- Redis Lua scripts receive unreserved GPU lists
- Allocation fails if requested GPUs become unreserved during allocation
- Ensures consistent state between validation and allocation

### Heartbeat System
Validation complements the heartbeat system:
- Heartbeat tracks reservation liveness
- Validation tracks actual GPU usage
- Together they provide complete resource lifecycle management

## Troubleshooting Validation

### nvidia-smi Not Available
```bash
Error: nvidia-smi command not found
```

Ensure NVIDIA drivers are properly installed:
```bash
# Test nvidia-smi availability
nvidia-smi

# If not available, install NVIDIA drivers
sudo apt install nvidia-driver-*  # Ubuntu
```

### Permission Issues
```bash
Warning: Could not determine owner for PID 12345
```

This may occur when:
- `/proc` filesystem has restricted access
- User lacks permissions to query process information
- Process terminates between detection and query

The system will still function but with less detailed process information.

### Memory Reporting Discrepancies
Different tools may report slightly different GPU memory usage:
- nvidia-smi vs. CUDA runtime memory reports
- Shared memory vs. process-specific memory
- Memory allocated vs. memory actually used

canhazgpu uses nvidia-smi reporting for consistency across all processes.

GPU validation ensures that canhazgpu maintains accurate, real-time awareness of GPU resource utilization, preventing conflicts and enabling efficient resource sharing.