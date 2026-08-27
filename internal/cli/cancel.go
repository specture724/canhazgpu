package cli

import (
	"context"
	"fmt"

	"github.com/russellb/canhazgpu/internal/gpu"
	"github.com/russellb/canhazgpu/internal/redis_client"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var cancelCmd = &cobra.Command{
	Use:   "cancel <task-id>...",
	Short: "Cancel your queued or running GPU tasks",
	Long: `Cancel tasks by the ID shown in 'canhazgpu queue'.

A queued task is removed from the queue. A running task is stopped by
signalling the process that holds the reservation (SIGTERM, then SIGKILL if it
does not exit) and any GPU processes belonging to the task, which releases its
GPUs even when the job's wrapper shell has outlived its GPU children. Manual
reservations have no process of their own and are simply released.

Only your own tasks can be cancelled unless --force is given.

Examples:
  canhazgpu cancel 4f89853e            # cancel one task
  canhazgpu cancel 4f89853e 7b2c1d90   # cancel several
  sudo canhazgpu cancel 4f89853e --force  # cancel someone else's task`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runCancel(cmd.Context(), args, viper.GetBool("cancel.force"))
	},
}

func init() {
	cancelCmd.Flags().Bool("force", false, "Allow cancelling a task that belongs to someone else")
	rootCmd.AddCommand(cancelCmd)
}

func runCancel(ctx context.Context, refs []string, force bool) error {
	config := getConfig()
	client := redis_client.NewClient(config)
	defer func() {
		if err := client.Close(); err != nil {
			fmt.Printf("Warning: failed to close Redis client: %v\n", err)
		}
	}()

	if err := client.Ping(ctx); err != nil {
		return fmt.Errorf("failed to connect to Redis: %v", err)
	}

	engine := gpu.NewAllocationEngine(client, config)

	var failed int
	for _, ref := range refs {
		result, err := engine.CancelTask(ctx, ref, getCurrentUser(), force)
		if err != nil {
			fmt.Printf("✗ %s: %v\n", ref, err)
			failed++
			continue
		}
		fmt.Println(describeCancel(result))
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d tasks could not be cancelled", failed, len(refs))
	}
	return nil
}

func describeCancel(result *gpu.CancelResult) string {
	var base string
	switch {
	case result.Queued:
		if result.Signalled > 0 {
			base = fmt.Sprintf("✓ %s: removed from the queue (signalled PID %d)", result.TaskID, result.Signalled)
		} else {
			base = fmt.Sprintf("✓ %s: removed from the queue", result.TaskID)
		}
	case result.Killed:
		base = fmt.Sprintf("✓ %s: PID %d ignored SIGTERM and was killed, GPUs %s released",
			result.TaskID, result.Signalled, formatGPUList(result.GPUs))
	case result.Signalled > 0:
		base = fmt.Sprintf("✓ %s: PID %d terminated, GPUs %s released",
			result.TaskID, result.Signalled, formatGPUList(result.GPUs))
	default:
		base = fmt.Sprintf("✓ %s: reservation released, GPUs %s", result.TaskID, formatGPUList(result.GPUs))
	}
	if result.GPUProcessesKilled > 0 {
		plural := ""
		if result.GPUProcessesKilled > 1 {
			plural = "s"
		}
		base += fmt.Sprintf(" (%d GPU process%s stopped)", result.GPUProcessesKilled, plural)
	}
	return base
}
