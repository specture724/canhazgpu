package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/russellb/canhazgpu/internal/gpu"
	"github.com/russellb/canhazgpu/internal/redis_client"
	"github.com/russellb/canhazgpu/internal/types"
	"github.com/russellb/canhazgpu/internal/utils"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// ExitCodeError is an error that carries an exit code
type ExitCodeError struct {
	Code    int
	Message string
}

func (e *ExitCodeError) Error() string {
	return e.Message
}

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Reserve GPUs and run a command with device visibility set",
	Long: `Reserve GPUs and run a command with the provider device visibility variable automatically set.

The command will:
1. Reserve the requested number of GPUs (or specific GPU IDs)
2. Set CUDA_VISIBLE_DEVICES (NVIDIA/AMD) or ASCEND_RT_VISIBLE_DEVICES (Ascend) to the allocated device IDs
3. Run your command with full interactive terminal support
4. Automatically release GPUs when the command finishes
5. Maintain a heartbeat while running to keep the reservation active

Interactive programs (like Python REPL, codex, vim, etc.) are fully supported.
Signals like Ctrl-C go directly to your command.

By default, if GPUs are not available, the command will wait in a queue until
resources become available (FCFS - First Come First Served). Use --nonblock to
fail immediately instead.

You can reserve GPUs in two ways:
- By count: --gpus N (allocates N GPUs using MRU-per-user strategy)
- By specific IDs: --gpu-ids 1,3,5 (reserves exactly those GPU IDs)

When using --gpu-ids, the --gpus flag is optional if:
- It matches the number of GPU IDs specified, or
- It is 1 (the default value)

If specific GPU IDs are requested and any are not available, the entire
reservation will wait in the queue until those specific IDs become available.

Optionally, you can set a timeout to automatically terminate the command and release
GPUs after a specified duration. When the timeout is reached, SIGINT will be sent to
the entire process group (including all child processes) for graceful shutdown,
followed by a 30-second grace period. If any processes haven't exited after the
grace period, the entire process group will be force-killed with SIGKILL.
This is useful for preventing runaway processes from holding GPUs indefinitely.

Reservations are also released automatically when the reserved GPUs show no
holder usage for the idle timeout (30 minutes by default). This prevents a
wrapper script that outlived its GPU process from holding GPUs forever.
Use --idle-timeout 0 to disable idle release (e.g. for interactive sessions).

Example usage:
  canhazgpu run --gpus 1 -- python train.py
  canhazgpu run --gpus 2 -- python -m torch.distributed.launch train.py
  canhazgpu run --gpu-ids 1,3 -- python train.py
  canhazgpu run --gpus 1 --timeout 2h -- python long_training.py
  canhazgpu run --nonblock --gpus 4 -- python train.py  # Fail if unavailable
  canhazgpu run --wait 30m --gpus 4 -- python train.py  # Wait up to 30 minutes

Timeout formats supported:
- 30s (30 seconds)
- 30m (30 minutes)
- 2h (2 hours)
- 1d (1 day)
- 0.5h (30 minutes with decimal)

The '--' separator is required - it tells canhazgpu where its options end
and your command begins.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		gpuCount := viper.GetInt("run.gpus")
		gpuIDs := viper.GetIntSlice("run.gpu-ids")
		timeoutStr := viper.GetString("run.timeout")
		idleTimeoutStr := viper.GetString("run.idle-timeout")
		note := viper.GetString("run.note")
		customUser := viper.GetString("run.user")
		nonblock := viper.GetBool("run.nonblock")
		waitStr := viper.GetString("run.wait")

		// Check if "--" separator was used
		dashIndex := cmd.ArgsLenAtDash()

		// Validate command arguments (requires "--" separator)
		if err := validateRunCommand(args, dashIndex); err != nil {
			return err
		}

		err := runRun(cmd.Context(), gpuCount, gpuIDs, timeoutStr, idleTimeoutStr, note, customUser, nonblock, waitStr, args)

		// Handle exit code errors
		if exitErr, ok := err.(*ExitCodeError); ok {
			os.Exit(exitErr.Code)
		}

		return err
	},
	DisableFlagsInUseLine: true,
}

func init() {
	runCmd.Flags().IntP("gpus", "g", 1, "Number of GPUs to reserve")
	runCmd.Flags().IntSliceP("gpu-ids", "G", nil, "Specific GPU IDs to reserve (comma-separated, e.g., 1,3,5)")
	runCmd.Flags().StringP("timeout", "t", "", "Timeout duration for graceful command termination (e.g., 30m, 2h, 1d). Disabled by default.")
	runCmd.Flags().String("idle-timeout", utils.FormatDurationShort(types.DefaultRunIdleTimeout),
		"Release the reservation if no GPU usage is detected for this long (0 to disable)")
	runCmd.Flags().StringP("note", "n", "", "Optional note describing the reservation purpose")
	runCmd.Flags().StringP("user", "u", "", "Custom user identifier (e.g., your name when using a shared account)")
	runCmd.Flags().Bool("nonblock", false, "Fail immediately if GPUs are unavailable instead of waiting in queue")
	runCmd.Flags().StringP("wait", "w", "", "Maximum time to wait for GPUs (e.g., 30m, 2h). Default: wait forever.")

	// Require explicit -- separator: only parse flags before --, everything after is treated as opaque args
	runCmd.Flags().SetInterspersed(false)

	rootCmd.AddCommand(runCmd)
}

// validateRunCommand validates that a command was provided with required "--" separator
func validateRunCommand(args []string, dashIndex int) error {
	// Case 1: No arguments at all
	if len(args) == 0 {
		return fmt.Errorf("no command specified. You must provide a command to run.\n\nUsage: canhazgpu run [flags] -- <command>\n\nExamples:\n  canhazgpu run --gpus 1 -- python train.py\n  canhazgpu run --gpu-ids 0,2 -- python -m torch.distributed.launch train.py\n\nNote: The '--' separator is required to separate canhazgpu flags from your command")
	}

	// Case 2: Check if "--" separator was used
	// dashIndex == -1 means no "--" was found
	if dashIndex == -1 {
		return fmt.Errorf("missing '--' separator. You must use '--' to separate canhazgpu flags from your command.\n\nYou provided: canhazgpu run [flags] %s\nCorrect usage: canhazgpu run [flags] -- %s\n\nExamples:\n  canhazgpu run --gpus 1 -- python train.py\n  canhazgpu run --gpu-ids 0,1 -- %s",
			strings.Join(args, " "), strings.Join(args, " "), args[0])
	}

	return nil
}

func runRun(ctx context.Context, gpuCount int, gpuIDs []int, timeoutStr string, idleTimeoutStr string, note string, customUser string, nonblock bool, waitStr string, command []string) error {
	// Cobra has already processed the "--" separator and given us just the command args

	// "0" disables the timeout, equivalent to leaving it unset
	if timeoutStr == "0" {
		timeoutStr = ""
	}

	gpuCount = normalizeRunGPUCount(gpuCount, gpuIDs)

	config := getConfig()

	// Validate timeout format early (before allocating GPUs)
	if timeoutStr != "" {
		if _, err := utils.ParseDuration(timeoutStr); err != nil {
			return fmt.Errorf("invalid timeout format: %v", err)
		}
	}

	// Parse wait timeout if provided
	var waitTimeout *time.Duration
	if waitStr != "" {
		wt, err := utils.ParseDuration(waitStr)
		if err != nil {
			return fmt.Errorf("invalid wait timeout format: %v", err)
		}
		waitTimeout = &wt
	}

	// Parse idle timeout (defaults to types.DefaultRunIdleTimeout; 0 disables)
	idleTimeout, err := parseIdleTimeout(idleTimeoutStr)
	if err != nil {
		return fmt.Errorf("invalid idle timeout: %v", err)
	}

	client := redis_client.NewClient(config)
	// Note: We don't defer close here because we'll exec() and the process will be replaced

	// Test Redis connection
	if err := client.Ping(ctx); err != nil {
		_ = client.Close()
		return fmt.Errorf("failed to connect to Redis: %v", err)
	}
	providerName, err := client.GetAvailableProvider(ctx)
	if err != nil {
		_ = client.Close()
		return fmt.Errorf("failed to get cached provider information: %v", err)
	}

	// Create allocation engine
	engine := gpu.NewAllocationEngine(client, config)

	// Get actual OS user and determine display user
	actualUser := getCurrentUser()
	displayUser := actualUser
	if customUser != "" {
		displayUser = customUser
	}

	// Create allocation request
	request := &gpu.QueuedAllocationRequest{
		AllocationRequest: &types.AllocationRequest{
			GPUCount:        gpuCount,
			GPUIDs:          gpuIDs,
			User:            displayUser,
			ActualUser:      actualUser,
			ReservationType: types.ReservationTypeRun,
			ExpiryTime:      nil, // No expiry for run-type reservations
			Note:            note,
			IdleTimeout:     idleTimeout,
		},
		Blocking:    !nonblock,
		WaitTimeout: waitTimeout,
	}

	// Allocate GPUs (with queue support)
	result, err := engine.AllocateGPUsWithQueue(ctx, request)
	if err != nil {
		_ = client.Close()
		return fmt.Errorf("%v", err)
	}
	allocatedGPUs := result.AllocatedGPUs

	// Verify we got the requested number of GPUs
	expectedCount := gpuCount
	if len(gpuIDs) > 0 {
		expectedCount = len(gpuIDs)
	}
	if len(allocatedGPUs) != expectedCount {
		_ = client.Close()
		return fmt.Errorf("failed to allocate requested GPUs: requested %d, got %d", expectedCount, len(allocatedGPUs))
	}

	// Sort GPU IDs for consistent ordering in output and environment variable
	sort.Ints(allocatedGPUs)

	// Build GPU list string for supervisor
	gpuListParts := make([]string, len(allocatedGPUs))
	for i, gpuID := range allocatedGPUs {
		gpuListParts[i] = strconv.Itoa(gpuID)
	}
	gpuListStr := strings.Join(gpuListParts, ",")

	// Print reservation info
	if timeoutStr != "" {
		timeout, _ := utils.ParseDuration(timeoutStr)
		fmt.Printf("Reserved %d GPU(s): %v for command execution (timeout: %s)\n",
			len(allocatedGPUs), allocatedGPUs, utils.FormatDuration(timeout))
	} else {
		fmt.Printf("Reserved %d GPU(s): %v for command execution\n",
			len(allocatedGPUs), allocatedGPUs)
	}

	if idleTimeout > 0 {
		fmt.Printf("Released automatically if no GPU usage is detected for %s\n",
			utils.FormatDurationShort(idleTimeout))
	}

	// Close Redis client before spawning supervisor (supervisor will create its own)
	_ = client.Close()

	// Get our own executable path for spawning supervisor
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable path: %v", err)
	}

	// Build supervisor command arguments
	supervisorArgs := buildSupervisorArgs(executable, config, gpuListStr, displayUser, os.Getpid(), timeoutStr, idleTimeout)

	// Start supervisor process (detached, will monitor us)
	supervisorCmd := exec.Command(supervisorArgs[0], supervisorArgs[1:]...)
	supervisorCmd.Stdout = nil       // Detach stdout
	supervisorCmd.Stderr = os.Stderr // Keep stderr for error messages
	supervisorCmd.Stdin = nil        // No stdin needed

	// Detach the supervisor from our process group so it survives our exec()
	supervisorCmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	if err := supervisorCmd.Start(); err != nil {
		return fmt.Errorf("failed to start supervisor: %v", err)
	}

	// Give supervisor a moment to initialize
	time.Sleep(50 * time.Millisecond)

	// Find the binary to exec
	binary, err := exec.LookPath(command[0])
	if err != nil {
		// Kill supervisor since we can't exec
		if supervisorCmd.Process != nil {
			_ = supervisorCmd.Process.Kill()
		}
		return fmt.Errorf("command not found: %s", command[0])
	}

	// Set the provider-specific visibility variable, replacing only its existing value.
	env := withVisibleDevicesEnv(os.Environ(), providerName, gpuListStr)

	// Exec the user's command - this replaces the current process
	// The supervisor will continue running and monitor our PID
	// When we exit, the supervisor will detect it and release GPUs
	err = syscall.Exec(binary, command, env)

	// If we get here, exec failed
	// Kill supervisor since we couldn't exec
	if supervisorCmd.Process != nil {
		_ = supervisorCmd.Process.Kill()
	}
	return fmt.Errorf("failed to exec command: %v", err)
}

// normalizeRunGPUCount applies the CLI default without changing a request for
// specific device IDs. Keeping this separate makes the behavior testable
// without invoking the exec-based run path.
func normalizeRunGPUCount(gpuCount int, gpuIDs []int) int {
	if gpuCount == 0 && len(gpuIDs) == 0 {
		return 1
	}
	return gpuCount
}

func withVisibleDevicesEnv(environment []string, providerName string, deviceIDs string) []string {
	variable := gpu.VisibleDevicesEnvVar(providerName)
	prefix := variable + "="
	env := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			env = append(env, entry)
		}
	}
	return append(env, fmt.Sprintf("%s=%s", variable, deviceIDs))
}

// buildSupervisorArgs builds the command line for the supervisor we spawn. The
// Redis settings are passed explicitly: the supervisor is a fresh process, so
// without them it would fall back to the default Redis instead of the one this
// reservation lives in.
func buildSupervisorArgs(executable string, config *types.Config, gpuList string, user string, pid int, timeout string, idleTimeout time.Duration) []string {
	args := []string{
		executable,
		"supervisor",
		"--gpus", gpuList,
		"--user", user,
		"--pid", strconv.Itoa(pid),
		"--redis-host", config.RedisHost,
		"--redis-port", strconv.Itoa(config.RedisPort),
		"--redis-db", strconv.Itoa(config.RedisDB),
	}
	if timeout != "" && timeout != "0" {
		args = append(args, "--timeout", timeout)
	}
	if idleTimeout > 0 {
		args = append(args, "--idle-timeout", utils.FormatDurationShort(idleTimeout))
	}
	return args
}
