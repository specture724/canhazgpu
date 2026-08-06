package cli

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/russellb/canhazgpu/internal/gpu"
	"github.com/russellb/canhazgpu/internal/redis_client"
	"github.com/russellb/canhazgpu/internal/types"
	"github.com/russellb/canhazgpu/internal/utils"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var reserveCmd = &cobra.Command{
	Use:   "reserve",
	Short: "Reserve GPUs manually for a specified duration",
	Long: `Reserve GPUs manually for a specified duration without running a command.
This is useful for interactive development sessions or planning work.

By default, if GPUs are not available, the command will wait in a queue until
resources become available (FCFS - First Come First Served). Use --nonblock to
fail immediately instead.

You can reserve GPUs in two ways:
- By count: --gpus N (allocates N GPUs using MRU-per-user strategy)
- By specific IDs: --gpu-ids 1,3,5 (reserves exactly those GPU IDs)

When using --gpu-ids, the --gpus flag is optional if:
- It matches the number of GPU IDs specified, or
- It is 1 (the default value)

If specific GPU IDs are requested and any are not available, the reservation
will wait in the queue until those specific IDs become available.

Use --force to reserve GPUs that are currently in unreserved use. This is
useful when you've started a job without using canhazgpu and want to create
a reservation retroactively.

Reservations are released automatically when nobody uses them: if no GPU
activity is detected within --idle-timeout (default 15m), the reservation is
dropped so the GPUs return to the pool. Use --idle-timeout 0 to disable this.

Scheduling ahead (like booking a meeting room):
- Use --start to book GPUs for a future time window instead of reserving now
- The window length comes from --end or, if absent, from --duration
- GPUs are picked when the booking is made and shown by 'canhazgpu schedule'
- When the window starts, the booking becomes a normal manual reservation
- Queueing options (--nonblock, --wait) and --force do not apply to bookings

GPUs that a scheduled booking needs while this reservation would still be
holding them are not handed out, and --force does not override that. Use
'canhazgpu schedule' to see what is booked.

Duration formats supported:
- 30m (30 minutes)
- 2h (2 hours)
- 1d (1 day)
- 0.5h (30 minutes with decimal)

Time formats supported by --start and --end:
- 14:00 (today, or tomorrow if that time already passed)
- tomorrow 09:30
- 2026-08-04T14:00
- +2h (relative to now)

IMPORTANT: Unlike 'canhazgpu run', this command does NOT automatically set
CUDA_VISIBLE_DEVICES. After reserving, you must manually set the environment
variable based on the GPU IDs shown in the output:
  export CUDA_VISIBLE_DEVICES=1,3

Example usage:
  canhazgpu reserve --gpus 2 --duration 4h
  canhazgpu reserve --gpu-ids 1,3 --duration 2h
  canhazgpu reserve --gpu-ids 0,1,2 --duration 8h --force
  canhazgpu reserve --gpus 1 --duration 8h --idle-timeout 1h  # Longer idle grace
  canhazgpu reserve --nonblock --gpus 4 --duration 2h  # Fail if unavailable
  canhazgpu reserve --wait 30m --gpus 4 --duration 2h  # Wait up to 30 minutes
  canhazgpu reserve --start 14:00 --end 16:00 --gpus 2  # Book a time slot
  canhazgpu reserve --start 'tomorrow 09:00' --duration 4h --gpus 8
  export CUDA_VISIBLE_DEVICES=$(canhazgpu reserve --gpus 2 --short)  # For scripting

The reserved GPUs must be manually released with 'canhazgpu release' or will
automatically expire after the specified duration.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		opts := reserveOptions{
			gpuCount:       viper.GetInt("reserve.gpus"),
			gpuIDs:         viper.GetIntSlice("reserve.gpu-ids"),
			durationStr:    viper.GetString("reserve.duration"),
			force:          viper.GetBool("reserve.force"),
			claim:          viper.GetBool("reserve.claim"),
			note:           viper.GetString("reserve.note"),
			customUser:     viper.GetString("reserve.user"),
			nonblock:       viper.GetBool("reserve.nonblock"),
			waitStr:        viper.GetString("reserve.wait"),
			short:          viper.GetBool("reserve.short"),
			idleTimeoutStr: viper.GetString("reserve.idle-timeout"),
			startStr:       viper.GetString("reserve.start"),
			endStr:         viper.GetString("reserve.end"),
		}

		return runReserve(cmd.Context(), opts)
	},
}

// reserveOptions holds the command line options of the reserve command
type reserveOptions struct {
	gpuCount       int
	gpuIDs         []int
	durationStr    string
	force          bool
	claim          bool
	note           string
	customUser     string
	nonblock       bool
	waitStr        string
	short          bool
	idleTimeoutStr string
	startStr       string
	endStr         string
}

func init() {
	reserveCmd.Flags().IntP("gpus", "g", 1, "Number of GPUs to reserve")
	reserveCmd.Flags().IntSliceP("gpu-ids", "G", nil, "Specific GPU IDs to reserve (comma-separated, e.g., 1,3,5)")
	reserveCmd.Flags().StringP("duration", "d", "30m", "Duration to reserve GPUs (e.g., 30m, 2h, 1d)")
	reserveCmd.Flags().BoolP("force", "f", false, "Force reservation even if GPU is in unreserved use")
	reserveCmd.Flags().Bool("claim", false, "Reserve a GPU that only your own unreserved process is using; if anyone else is on it, wait in the queue")
	reserveCmd.Flags().StringP("note", "n", "", "Optional note describing the reservation purpose")
	reserveCmd.Flags().StringP("user", "u", "", "Custom user identifier (e.g., your name when using a shared account)")
	reserveCmd.Flags().Bool("nonblock", false, "Fail immediately if GPUs are unavailable instead of waiting in queue")
	reserveCmd.Flags().StringP("wait", "w", "", "Maximum time to wait for GPUs (e.g., 30m, 2h). Default: wait forever.")
	reserveCmd.Flags().BoolP("short", "s", false, "Output only the GPU IDs (for use with command substitution)")
	reserveCmd.Flags().String("idle-timeout", utils.FormatDurationShort(types.DefaultIdleTimeout),
		"Release the reservation if no GPU usage is detected for this long (0 to disable)")
	reserveCmd.Flags().String("start", "", "Book GPUs for a future time window starting then (e.g., 14:00, 'tomorrow 09:30')")
	reserveCmd.Flags().String("end", "", "End of the reserved window (e.g., 16:00). Defaults to --duration after the start")

	rootCmd.AddCommand(reserveCmd)
}

func runReserve(ctx context.Context, opts reserveOptions) error {
	gpuCount := opts.gpuCount
	gpuIDs := opts.gpuIDs

	// If neither is specified, default to 1 GPU
	if gpuCount == 0 && len(gpuIDs) == 0 {
		gpuCount = 1
	}

	// Parse duration
	duration, err := utils.ParseDuration(opts.durationStr)
	if err != nil {
		return err
	}

	// Parse idle timeout
	idleTimeout, err := parseIdleTimeout(opts.idleTimeoutStr)
	if err != nil {
		return err
	}

	now := time.Now()

	// --start turns this into a booking for a future window
	if opts.startStr != "" {
		start, err := utils.ParseTimeSpec(opts.startStr, now)
		if err != nil {
			return err
		}

		end := start.Add(duration)
		if opts.endStr != "" {
			end, err = utils.ParseTimeSpecFrom(opts.endStr, start)
			if err != nil {
				return err
			}
		}

		return runBooking(ctx, opts, gpuCount, gpuIDs, start, end, idleTimeout)
	}

	// --end without --start reserves from now until that time
	if opts.endStr != "" {
		end, err := utils.ParseTimeSpec(opts.endStr, now)
		if err != nil {
			return err
		}
		duration = end.Sub(now)
		if duration <= 0 {
			return fmt.Errorf("end time %s is not in the future", end.Format("2006-01-02 15:04"))
		}
	}

	// Parse wait timeout if provided
	var waitTimeout *time.Duration
	if opts.waitStr != "" {
		wt, err := utils.ParseDuration(opts.waitStr)
		if err != nil {
			return fmt.Errorf("invalid wait timeout format: %v", err)
		}
		waitTimeout = &wt
	}

	config := getConfig()
	client := redis_client.NewClient(config)
	defer func() {
		if err := client.Close(); err != nil {
			fmt.Printf("Warning: failed to close Redis client: %v\n", err)
		}
	}()

	// Test Redis connection
	if err := client.Ping(ctx); err != nil {
		return fmt.Errorf("failed to connect to Redis: %v", err)
	}

	// Create allocation engine
	engine := gpu.NewAllocationEngine(client, config)

	// Get actual OS user and determine display user
	actualUser := getCurrentUser()
	displayUser := actualUser
	if opts.customUser != "" {
		displayUser = opts.customUser
	}

	// Create allocation request
	expiryTime := time.Now().Add(duration)
	request := &gpu.QueuedAllocationRequest{
		AllocationRequest: &types.AllocationRequest{
			GPUCount:        gpuCount,
			GPUIDs:          gpuIDs,
			User:            displayUser,
			ActualUser:      actualUser,
			ReservationType: types.ReservationTypeManual,
			ExpiryTime:      &expiryTime,
			Force:           opts.force,
			ClaimOwned:      opts.claim,
			Note:            opts.note,
			IdleTimeout:     idleTimeout,
		},
		Blocking:    !opts.nonblock,
		WaitTimeout: waitTimeout,
	}

	// Allocate GPUs (with queue support)
	result, err := engine.AllocateGPUsWithQueue(ctx, request)
	if err != nil {
		return err
	}
	allocatedGPUs := result.AllocatedGPUs

	// Sort GPU IDs for consistent ordering in output and environment variable
	sort.Ints(allocatedGPUs)

	// Build list for CUDA_VISIBLE_DEVICES
	ids := make([]string, len(allocatedGPUs))
	for i, id := range allocatedGPUs {
		ids[i] = strconv.Itoa(id)
	}

	if opts.short {
		// Short output: just the GPU IDs for command substitution
		fmt.Print(strings.Join(ids, ","))
		return nil
	}

	fmt.Printf("Reserved %d GPU(s): %v for %s\n",
		len(allocatedGPUs), allocatedGPUs, utils.FormatDuration(duration))

	if idleTimeout > 0 {
		fmt.Printf("Released automatically if no GPU usage is detected for %s\n",
			utils.FormatDurationShort(idleTimeout))
	}

	fmt.Printf(
		"\nRun the following command to run only on these GPUs:\nexport CUDA_VISIBLE_DEVICES=%s\n",
		strings.Join(ids, ","),
	)

	return nil
}

// parseIdleTimeout parses the --idle-timeout value, where an empty value or "0"
// disables idle detection
func parseIdleTimeout(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "0" {
		return 0, nil
	}

	idleTimeout, err := utils.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid idle timeout format: %v", err)
	}
	if idleTimeout < 0 {
		return 0, fmt.Errorf("idle timeout cannot be negative")
	}

	return idleTimeout, nil
}

// runBooking books GPUs for a future time window instead of reserving them now
func runBooking(ctx context.Context, opts reserveOptions, gpuCount int, gpuIDs []int, start, end time.Time, idleTimeout time.Duration) error {
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

	actualUser := getCurrentUser()
	displayUser := actualUser
	if opts.customUser != "" {
		displayUser = opts.customUser
	}

	booking, err := engine.CreateBooking(ctx, &gpu.BookingRequest{
		GPUCount:    gpuCount,
		GPUIDs:      gpuIDs,
		User:        displayUser,
		ActualUser:  actualUser,
		StartTime:   start,
		EndTime:     end,
		Note:        opts.note,
		IdleTimeout: idleTimeout,
	})
	if err != nil {
		return err
	}

	ids := make([]string, len(booking.GPUIDs))
	for i, gpuID := range booking.GPUIDs {
		ids[i] = strconv.Itoa(gpuID)
	}

	if opts.short {
		// Short output: the GPU IDs this booking will hand over
		fmt.Print(strings.Join(ids, ","))
		return nil
	}

	now := time.Now()
	when := "starts now"
	if start.After(now) {
		when = fmt.Sprintf("starts in %s", utils.FormatDurationShort(start.Sub(now)))
	}

	fmt.Printf("Booked %d GPU(s): %v  %s (%s, %s long)\n",
		len(booking.GPUIDs), booking.GPUIDs,
		gpu.FormatBookingWindow(start, end, now),
		when, utils.FormatDurationShort(booking.Duration()))
	fmt.Printf("Booking ID: %s\n", booking.ShortID())

	if idleTimeout > 0 {
		fmt.Printf("Released automatically if no GPU usage is detected for %s after it starts\n",
			utils.FormatDurationShort(idleTimeout))
	}

	// Say so up front when the booking is going to interrupt somebody
	if warnings, err := engine.BookingPreemptions(ctx, booking); err == nil {
		for _, warning := range warnings {
			fmt.Printf("Note: %s\n", warning)
		}
	}

	fmt.Printf("\nView the schedule:   canhazgpu schedule\nCancel the booking:  canhazgpu schedule --cancel %s\n",
		booking.ShortID())

	return nil
}
