# Configuration

canhazgpu supports configuration files to set default values for any command-line option. This allows you to customize behavior without needing to specify options every time.

## Configuration File Locations

canhazgpu looks for configuration files in these locations (in order):

1. File specified with `--config` flag
2. `.canhazgpu.yaml` in your home directory
3. `.canhazgpu.yaml` in the current directory

## Supported Formats

Configuration files can be in YAML, JSON, or TOML format:

- `.canhazgpu.yaml` or `.canhazgpu.yml` (YAML)
- `.canhazgpu.json` (JSON)
- `.canhazgpu.toml` (TOML)

## Configuration Structure

The configuration file uses a hierarchical structure where each command's options are grouped under the command name:

```yaml
# Redis connection settings
redis:
  host: "localhost"
  port: 6379
  db: 0

# Memory threshold for GPU usage detection (in MB)
memory:
  threshold: 100

# Scheduled bookings
booking:
  # How far ahead 'run' reservations avoid GPUs needed by a booking
  protection_window: "30m"

# Guard: monitoring and enforcement of the reservation system
guard:
  interval: "15s"
  grace: "60s"
  confirmations: 2
  warn-interval: "5m"
  max-warnings: 3
  enforce: false            # Terminate offenders once warnings are exhausted
  kill-grace: "30s"
  max-kills-per-hour: 3
  channels: ["process", "tty", "log"]
  log-file: ""
  exclude-users: ["root"]
  exclude-commands: ["Xorg", "nvidia-smi", "amd-smi", "dcgm-exporter", "nvidia-persistenced"]
  notify-holder: true

# Default settings for 'run' command
run:
  gpus: 1
  timeout: "2h"

# Default settings for 'reserve' command  
reserve:
  gpus: 1
  duration: "8h"
  # Release a manual reservation after this long without GPU usage ("0" disables)
  idle-timeout: "15m"

# Default settings for 'report' command
report:
  days: 30
```

## Command-Line Priority

Command-line arguments always take priority over configuration file values:

```bash
# Config file sets run.timeout: "2h"
# This command uses 30m timeout instead
canhazgpu run --timeout 30m -- python train.py
```

## Common Configuration Examples

### Basic Setup
```yaml
# ~/.canhazgpu.yaml
redis:
  host: "gpu-server.local"
  port: 6379

memory:
  threshold: 2048  # 2GB threshold for GPU usage detection
```

### Development Environment
```yaml
# ~/.canhazgpu.yaml
run:
  gpus: 1
  timeout: "30m"  # Shorter timeout for development

reserve:
  duration: "2h"  # Shorter default reservations

report:
  days: 7  # Focus on recent usage
```

### Production Environment
```yaml
# ~/.canhazgpu.yaml
run:
  gpus: 2
  timeout: "12h"  # Longer timeout for production workloads

reserve:
  duration: "1d"  # Longer default reservations

memory:
  threshold: 512  # More sensitive usage detection
```

### Multi-User Server
```yaml
# /etc/canhazgpu/config.yaml (system-wide)
redis:
  host: "redis.internal"
  port: 6379

memory:
  threshold: 100

# Conservative defaults to encourage sharing
run:
  timeout: "4h"

reserve:
  duration: "4h"
```

## Environment Variables

You can also set configuration values using environment variables with the `CANHAZGPU_` prefix:

```bash
export CANHAZGPU_REDIS_HOST="redis.example.com"
export CANHAZGPU_REDIS_PORT="6380"
export CANHAZGPU_RUN_TIMEOUT="1h"
export CANHAZGPU_MEMORY_THRESHOLD="2048"
```

## Configuration Priority Order

Values are applied in this order (highest priority first):

1. Command-line flags
2. Environment variables (with `CANHAZGPU_` prefix)
3. Configuration file values
4. Built-in defaults

## Timeout Configuration

The `run.timeout` setting is particularly useful for preventing runaway processes:

```yaml
# Set a default timeout for all run commands
run:
  timeout: "2h"
```

This ensures that any `canhazgpu run` command will be automatically killed after 2 hours, preventing processes from holding GPUs indefinitely.

### Timeout Examples

```yaml
# Different timeout strategies
run:
  timeout: "30m"   # Short timeout for development/testing
  # timeout: "4h"   # Medium timeout for typical training
  # timeout: "24h"  # Long timeout for extended training
  # timeout: ""     # No timeout (default behavior)
```

## Testing Configuration

To test your configuration without running commands:

```bash
# Check what values are being used
canhazgpu run --help  # Shows current defaults

# Use a specific config file
canhazgpu --config /path/to/config.yaml status

# Check if config file is being loaded
canhazgpu status  # Prints "Using config file: ..." if found
```

## Example Complete Configuration

```yaml
# Complete example configuration
# ~/.canhazgpu.yaml

# Redis connection
redis:
  host: "localhost"
  port: 6379
  db: 0

# GPU usage detection threshold
memory:
  threshold: 100

# Scheduled bookings
booking:
  protection_window: "30m"  # 'run' avoids GPUs booked within this window

# Run command defaults
run:
  gpus: 1
  timeout: "2h"  # Kill commands after 2 hours

# Reserve command defaults
reserve:
  gpus: 1
  duration: "8h"
  idle-timeout: "15m"  # Release reservations nobody uses

# Status and reporting
report:
  days: 30

# Web dashboard
web:
  port: 8080
  host: "0.0.0.0"
```

This configuration provides sensible defaults while allowing easy customization for different environments and use cases.