package gpu

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/russellb/canhazgpu/internal/redis_client"
	"github.com/russellb/canhazgpu/internal/types"
	"github.com/russellb/canhazgpu/internal/utils"
)

const (
	// usageCacheTTL keeps GPU usage detection results for long enough that a
	// single command (maintenance followed by allocation or status) queries the
	// GPU provider once instead of twice
	usageCacheTTL = time.Second

	// activityRefreshInterval limits how often the observed-activity timestamp
	// of an idle-timeout reservation is written back to Redis
	activityRefreshInterval = 30 * time.Second
)

type AllocationEngine struct {
	client *redis_client.Client
	config *types.Config

	usageMu     sync.Mutex
	usageCache  map[int]*types.GPUUsage
	usageCached time.Time
}

func NewAllocationEngine(client *redis_client.Client, config *types.Config) *AllocationEngine {
	return &AllocationEngine{
		client: client,
		config: config,
	}
}

func (ae *AllocationEngine) detectGPUUsage(ctx context.Context) (map[int]*types.GPUUsage, error) {
	ae.usageMu.Lock()
	if ae.usageCache != nil && time.Since(ae.usageCached) < usageCacheTTL {
		cached := ae.usageCache
		ae.usageMu.Unlock()
		return cached, nil
	}
	ae.usageMu.Unlock()

	usage, err := ae.queryGPUUsage(ctx)
	if err != nil {
		return nil, err
	}

	ae.usageMu.Lock()
	ae.usageCache = usage
	ae.usageCached = time.Now()
	ae.usageMu.Unlock()

	return usage, nil
}

func (ae *AllocationEngine) queryGPUUsage(ctx context.Context) (map[int]*types.GPUUsage, error) {
	providerName, err := ae.client.GetAvailableProvider(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get cached provider information: %v", err)
	}

	var pm *ProviderManager
	if providerName == "fake" {
		// For fake provider, get GPU count from Redis to configure the provider
		gpuCount, err := ae.client.GetGPUCount(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get GPU count for fake provider: %v", err)
		}
		pm = NewProviderManagerWithFake(gpuCount)
	} else {
		pm = NewProviderManagerFromNames([]string{providerName})
	}

	return pm.DetectAllGPUUsageWithoutChecks(ctx)
}

// AllocateGPUs allocates GPUs using MRU-per-user strategy with race condition protection
func (ae *AllocationEngine) AllocateGPUs(ctx context.Context, request *types.AllocationRequest) ([]int, error) {
	// Validate the allocation request first
	if err := request.Validate(); err != nil {
		return nil, err
	}

	// Best effort maintenance so this request sees an up-to-date pool: due
	// bookings take their GPUs, expired and idle reservations are freed
	_ = ae.CleanupExpiredReservations(ctx)

	// Validate GPU availability using cached provider information
	usage, err := ae.detectGPUUsage(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to validate GPU usage: %v", err)
	}

	// Get list of unreserved GPUs
	unreservedGPUs := GetUnreservedGPUs(ctx, usage, ae.config.MemoryThreshold)

	// If force flag is set, clear unreserved GPUs list to allow allocation
	if request.Force {
		unreservedGPUs = []int{}
	} else if request.ClaimOwned {
		// With --claim, GPUs used only by the requester's own processes are
		// claimable immediately; anything with other users' processes stays
		// out of reach and the request queues for it as usual
		unreservedGPUs = FilterOutClaimableOwnedGPUs(unreservedGPUs, usage, request.ActualUser)
	}

	// Hold back GPUs that a scheduled booking needs while this reservation
	// would be holding them
	if err := ae.applyBookingProtection(ctx, request); err != nil {
		return nil, err
	}

	// Acquire allocation lock
	if err := ae.client.AcquireAllocationLock(ctx); err != nil {
		return nil, err
	}
	defer func() {
		if err := ae.client.ReleaseAllocationLock(ctx); err != nil {
			// Log error but don't fail the operation
			fmt.Printf("Warning: failed to release allocation lock: %v\n", err)
		}
	}()

	// Perform atomic allocation
	allocatedGPUs, err := ae.client.AtomicReserveGPUs(ctx, request, unreservedGPUs)
	if err != nil {
		// Check if it's an availability error and provide detailed message
		if err.Error() == "Not enough GPUs available" {
			gpuCount, _ := ae.client.GetGPUCount(ctx)

			excluded := make(map[int]bool, len(unreservedGPUs)+len(request.BlockedGPUs))
			for _, gpuID := range unreservedGPUs {
				excluded[gpuID] = true
			}
			for _, gpuID := range request.BlockedGPUs {
				excluded[gpuID] = true
			}
			available := gpuCount - len(excluded)

			var details []string
			if len(unreservedGPUs) > 0 {
				details = append(details, fmt.Sprintf("%d GPUs in use without reservation - run 'canhazgpu status' for details", len(unreservedGPUs)))
			}
			if len(request.BlockedGPUs) > 0 {
				details = append(details, fmt.Sprintf("%d GPUs held for scheduled bookings - run 'canhazgpu schedule' for details", len(request.BlockedGPUs)))
			}

			var detailMsg string
			if len(details) > 0 {
				detailMsg = " (" + strings.Join(details, "; ") + ")"
			}

			return nil, fmt.Errorf("not enough GPUs available. Requested: %d, Available: %d%s",
				request.GPUCount, available, detailMsg)
		}
		// For specific GPU ID errors, pass through the detailed error message
		return nil, err
	}

	return allocatedGPUs, nil
}

// bookingProtectionWindow returns how far ahead reservations without a fixed
// end time look for scheduled bookings
func (ae *AllocationEngine) bookingProtectionWindow() time.Duration {
	if ae.config.BookingProtectionWindow > 0 {
		return ae.config.BookingProtectionWindow
	}
	return types.DefaultBookingProtectionWindow
}

// reservationWindowEnd returns the point in time up to which a reservation
// needs its GPUs. Manual reservations know when they expire; run-type
// reservations do not, so a protection window is used instead.
func (ae *AllocationEngine) reservationWindowEnd(now time.Time, expiryTime *time.Time) time.Time {
	if expiryTime != nil {
		return *expiryTime
	}
	return now.Add(ae.bookingProtectionWindow())
}

// applyBookingProtection records which GPUs must be kept free for upcoming
// bookings, and rejects the request outright when it explicitly asks for one of
// them
func (ae *AllocationEngine) applyBookingProtection(ctx context.Context, request *types.AllocationRequest) error {
	now := time.Now()

	blocked, err := ae.BlockedGPUsForWindow(ctx, now, ae.reservationWindowEnd(now, request.ExpiryTime))
	if err != nil {
		return fmt.Errorf("failed to check scheduled bookings: %v", err)
	}

	request.BlockedGPUs = nil
	for gpuID := range blocked {
		request.BlockedGPUs = append(request.BlockedGPUs, gpuID)
	}
	sort.Ints(request.BlockedGPUs)

	// Specific GPU IDs deserve an explanation rather than a generic failure
	for _, gpuID := range request.GPUIDs {
		if booking, held := blocked[gpuID]; held {
			return fmt.Errorf("GPU %d is held for a scheduled booking by %s (%s, booking %s)",
				gpuID, booking.User,
				FormatBookingWindow(booking.StartTime.ToTime(), booking.EndTime.ToTime(), now),
				booking.ShortID())
		}
	}

	return nil
}

// ReleaseGPUs releases manually reserved GPUs for a user
func (ae *AllocationEngine) ReleaseGPUs(ctx context.Context, user string) ([]int, error) {
	gpuCount, err := ae.client.GetGPUCount(ctx)
	if err != nil {
		return nil, err
	}

	var releasedGPUs []int
	now := time.Now()

	for gpuID := 0; gpuID < gpuCount; gpuID++ {
		state, err := ae.client.GetGPUState(ctx, gpuID)
		if err != nil {
			continue
		}

		// Only release manual reservations by this user
		if state.User == user && state.Type == types.ReservationTypeManual {
			// Record usage history
			duration := now.Sub(state.StartTime.ToTime()).Seconds()
			usageRecord := &types.UsageRecord{
				User:            state.User,
				GPUID:           gpuID,
				StartTime:       state.StartTime,
				EndTime:         types.FlexibleTime{Time: now},
				Duration:        duration,
				ReservationType: state.Type,
			}

			if err := ae.client.RecordUsageHistory(ctx, usageRecord); err != nil {
				// Log error but don't fail the release
				fmt.Fprintf(os.Stderr, "Warning: failed to record usage history: %v\n", err)
			}

			// Mark as available with last_released timestamp
			availableState := &types.GPUState{
				LastReleased: types.FlexibleTime{Time: now},
			}

			if err := ae.client.SetGPUState(ctx, gpuID, availableState); err != nil {
				return nil, fmt.Errorf("failed to release GPU %d: %v", gpuID, err)
			}

			// Releasing early ends the booking that created this reservation
			ae.completeBooking(ctx, state.BookingID)

			releasedGPUs = append(releasedGPUs, gpuID)
		}
	}

	return releasedGPUs, nil
}

// ReleaseSpecificGPUs releases specific GPUs owned by a user (both manual and run-type reservations)
func (ae *AllocationEngine) ReleaseSpecificGPUs(ctx context.Context, user string, gpuIDs []int) ([]int, error) {
	var releasedGPUs []int
	now := time.Now()

	for _, gpuID := range gpuIDs {
		state, err := ae.client.GetGPUState(ctx, gpuID)
		if err != nil {
			continue
		}

		// Release GPU if it's reserved by this user (either manual or run type)
		if state.User == user && (state.Type == types.ReservationTypeManual || state.Type == types.ReservationTypeRun) {
			// Record usage history
			duration := now.Sub(state.StartTime.ToTime()).Seconds()
			usageRecord := &types.UsageRecord{
				User:            state.User,
				GPUID:           gpuID,
				StartTime:       state.StartTime,
				EndTime:         types.FlexibleTime{Time: now},
				Duration:        duration,
				ReservationType: state.Type,
			}
			if err := ae.client.RecordUsageHistory(ctx, usageRecord); err != nil {
				// Log error but don't fail the release
				fmt.Fprintf(os.Stderr, "Warning: failed to record usage history: %v\n", err)
			}

			// Mark as available with last_released timestamp
			availableState := &types.GPUState{
				LastReleased: types.FlexibleTime{Time: now},
			}
			if err := ae.client.SetGPUState(ctx, gpuID, availableState); err != nil {
				return nil, fmt.Errorf("failed to release GPU %d: %v", gpuID, err)
			}

			// Releasing early ends the booking that created this reservation
			ae.completeBooking(ctx, state.BookingID)

			releasedGPUs = append(releasedGPUs, gpuID)
		}
	}

	return releasedGPUs, nil
}

// GetGPUStatus returns the current status of all GPUs with validation
func (ae *AllocationEngine) GetGPUStatus(ctx context.Context) ([]GPUStatusInfo, error) {
	gpuCount, err := ae.client.GetGPUCount(ctx)
	if err != nil {
		return nil, err
	}

	// Get actual GPU usage using cached provider information
	usage, err := ae.detectGPUUsage(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to validate GPU usage: %v", err)
	}

	var statuses []GPUStatusInfo

	for gpuID := 0; gpuID < gpuCount; gpuID++ {
		state, err := ae.client.GetGPUState(ctx, gpuID)
		if err != nil {
			statuses = append(statuses, GPUStatusInfo{
				GPUID:  gpuID,
				Status: "ERROR",
				Error:  fmt.Sprintf("Failed to get state: %v", err),
			})
			continue
		}

		status := ae.buildGPUStatus(gpuID, state, usage[gpuID])
		statuses = append(statuses, status)
	}

	return statuses, nil
}

// GPUStatusInfo represents the status of a single GPU
type GPUStatusInfo struct {
	GPUID              int
	Status             string // "AVAILABLE", "IN_USE", "UNRESERVED", "ERROR"
	User               string
	ReservationType    string
	Duration           time.Duration
	LastHeartbeat      time.Time
	ExpiryTime         time.Time
	LastReleased       time.Time
	ValidationInfo     string
	UnreservedUsers    []string
	ProcessInfo        string
	Error              string
	ModelInfo          *ModelInfo    `json:"model_info,omitempty"` // Detected AI model information
	Provider           string        `json:"provider,omitempty"`   // GPU provider (e.g., "NVIDIA", "AMD")
	GPUModel           string        `json:"gpu_model,omitempty"`  // GPU model (e.g., "H100", "RTX 4090")
	Note               string        `json:"note,omitempty"`       // Optional note describing the reservation purpose
	IdleTimeout        time.Duration `json:"idle_timeout,omitempty"`
	IdleFor            time.Duration `json:"idle_for,omitempty"` // How long the reservation has been without GPU usage
	BookingID          string        `json:"booking_id,omitempty"`
	MemoryMB           int           `json:"memory_mb,omitempty"`           // Memory in use as reported by the GPU provider
	UtilizationPercent int           `json:"utilization_percent,omitempty"` // GPU utilization reported by the provider (0-100)

	// Processes currently using this GPU, with owner and elapsed time
	Processes []types.GPUProcessInfo `json:"processes,omitempty"`

	// Usage on a reserved GPU that belongs to somebody other than the holder
	ForeignUsers     []string `json:"foreign_users,omitempty"`
	ForeignProcesses int      `json:"foreign_processes,omitempty"`
	ForeignMemoryMB  int      `json:"foreign_memory_mb,omitempty"`
}

// HasForeignUsage reports whether somebody other than the reservation holder is
// using this GPU
func (s *GPUStatusInfo) HasForeignUsage() bool {
	return len(s.ForeignUsers) > 0
}

func (ae *AllocationEngine) buildGPUStatus(gpuID int, state *types.GPUState, usage *types.GPUUsage) GPUStatusInfo {
	status := GPUStatusInfo{GPUID: gpuID}

	// Check if GPU is reserved first - if it has a valid reservation, it's not unauthorized
	if state.User != "" {
		status.Status = "IN_USE"
		// If user and actual_user differ, show "user (actual_user)" format
		if state.ActualUser != "" && state.User != state.ActualUser {
			status.User = fmt.Sprintf("%s (%s)", state.ActualUser, state.User)
		} else {
			status.User = state.User
		}
		status.ReservationType = state.Type
		status.Duration = time.Since(state.StartTime.ToTime())
		status.LastHeartbeat = state.LastHeartbeat.ToTime()
		status.ExpiryTime = state.ExpiryTime.ToTime()
		status.Note = state.Note
		status.BookingID = state.BookingID

		// Idle tracking, for manual reservations that have an idle timeout
		if state.Type == types.ReservationTypeManual && state.IdleTimeout > 0 {
			status.IdleTimeout = state.IdleTimeoutDuration()
			if !IsReservationHolderActive(state, usage, ae.config.MemoryThreshold) {
				status.IdleFor = time.Since(state.IdleSince())
			}
		}

		// Usage by somebody other than the holder, which the status display
		// would otherwise show as a perfectly normal reservation
		if foreign := DetectForeignUsage(state, usage); foreign != nil {
			status.ForeignUsers = foreign.Users
			status.ForeignProcesses = foreign.Processes
			status.ForeignMemoryMB = foreign.MemoryMB
		}

		// Build validation info
		if usage != nil && usage.MemoryMB > ae.config.MemoryThreshold {
			if len(usage.Processes) > 0 {
				status.ValidationInfo = fmt.Sprintf("[validated: %dMB, %d processes]",
					usage.MemoryMB, len(usage.Processes))
			} else {
				status.ValidationInfo = fmt.Sprintf("[validated: %dMB used]", usage.MemoryMB)
			}
		} else {
			status.ValidationInfo = "[validated: no usage detected]"
		}
	} else {
		// GPU has no reservation - check if it's being used without reservation
		if IsGPUInUnreservedUse(usage, ae.config.MemoryThreshold) {
			status.Status = "UNRESERVED"

			// Get users from processes
			var users []string
			for user := range usage.Users {
				users = append(users, user)
			}
			status.UnreservedUsers = users

			// The VALIDATION column carries the memory figure; DETAILS keeps the
			// process list and how long each process has been running
			if len(usage.Processes) > 0 {
				status.ValidationInfo = fmt.Sprintf("[validated: %dMB, %d processes]",
					usage.MemoryMB, len(usage.Processes))
				status.ProcessInfo = "used by " + formatProcessInfo(usage.Processes)
			} else {
				status.ValidationInfo = fmt.Sprintf("[validated: %dMB used]", usage.MemoryMB)
				status.ProcessInfo = "used"
			}
		} else {
			status.Status = "AVAILABLE"
			status.LastReleased = state.LastReleased.ToTime()

			// If the GPU was seen in use without a reservation after its last
			// release, the free clock starts when that usage was last observed
			if activity := state.LastActivity.ToTime(); activity.After(status.LastReleased) {
				status.LastReleased = activity
			}

			// Show memory usage for available GPUs
			if usage != nil {
				status.ValidationInfo = fmt.Sprintf("[validated: %dMB used]", usage.MemoryMB)
			}
		}
	}

	// Detect model information from processes if available
	if usage != nil && len(usage.Processes) > 0 {
		status.ModelInfo = DetectModelFromProcesses(usage.Processes)
	}

	// Add GPU provider and model information if available
	if usage != nil {
		status.Provider = usage.Provider
		status.GPUModel = usage.Model
		status.MemoryMB = usage.MemoryMB
		status.UtilizationPercent = usage.UtilizationPercent
		status.Processes = usage.Processes
	}

	return status
}

// CleanupExpiredReservations performs the housekeeping that every command runs
// before it looks at or hands out GPUs:
//   - bookings whose window has started become real reservations
//   - reservations that expired, lost their heartbeat, or sat idle are released
//   - booking records that are long finished are pruned
//
// It is best effort by design: GPU usage detection failures only disable the
// idle checks, they never abort the rest of the pass.
func (ae *AllocationEngine) CleanupExpiredReservations(ctx context.Context) error {
	// Without usage information we cannot tell an idle reservation from a busy
	// one, so those checks are skipped rather than guessed at
	usage, err := ae.detectGPUUsage(ctx)
	if err != nil {
		usage = nil
	}

	if _, err := ae.activateDueBookings(ctx, usage); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to activate scheduled bookings: %v\n", err)
	}

	if err := ae.cleanupReservations(ctx, usage); err != nil {
		return err
	}

	if err := ae.pruneBookings(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to prune old bookings: %v\n", err)
	}

	return nil
}

// cleanupReservations releases reservations that expired, lost their heartbeat,
// or - for manual reservations with an idle timeout - have not touched their
// GPUs for too long. usage may be nil when GPU usage could not be detected.
func (ae *AllocationEngine) cleanupReservations(ctx context.Context, usage map[int]*types.GPUUsage) error {
	gpuCount, err := ae.client.GetGPUCount(ctx)
	if err != nil {
		return err
	}

	now := time.Now()

	for gpuID := 0; gpuID < gpuCount; gpuID++ {
		state, err := ae.client.GetGPUState(ctx, gpuID)
		if err != nil {
			continue
		}
		// An unreserved GPU with real processes on it is still "occupied":
		// remember when it was last seen so the free-for clock starts at the
		// right moment once the processes stop
		if state.User == "" && usage != nil {
			ae.trackUnreservedActivity(ctx, gpuID, state, usage[gpuID], now)
		}

		if state.User == "" {
			continue
		}

		// Note down that the GPU really is being used - this is what the idle
		// timeout below is measured against
		ae.refreshActivity(ctx, gpuID, state, usage[gpuID], now)

		var shouldRelease bool
		var reason string

		switch {
		// Expired manual reservations
		case state.Type == types.ReservationTypeManual &&
			!state.ExpiryTime.ToTime().IsZero() &&
			now.After(state.ExpiryTime.ToTime()):
			shouldRelease = true
			reason = "expired"

		// Stale heartbeats (run-type reservations)
		case state.Type == types.ReservationTypeRun &&
			!state.LastHeartbeat.ToTime().IsZero() &&
			now.Sub(state.LastHeartbeat.ToTime()) > types.HeartbeatTimeout:
			shouldRelease = true
			reason = "stale heartbeat"

		// Manual reservations nobody is actually using. Run-type reservations
		// are excluded: they are tied to the lifetime of a process, which may
		// legitimately spend a long time before touching the GPU.
		case usage != nil &&
			state.Type == types.ReservationTypeManual &&
			state.IdleTimeout > 0 &&
			now.Sub(state.IdleSince()) > state.IdleTimeoutDuration():
			shouldRelease = true
			reason = "idle"
			fmt.Fprintf(os.Stderr,
				"Released GPU %d reserved by %s: no GPU usage detected for %s (idle timeout %s)\n",
				gpuID, state.User,
				utils.FormatDurationShort(now.Sub(state.IdleSince())),
				utils.FormatDurationShort(state.IdleTimeoutDuration()))
		}

		if !shouldRelease {
			continue
		}

		if err := ae.recordUsage(ctx, gpuID, state, now); err != nil {
			// Log error but don't fail the cleanup
			fmt.Fprintf(os.Stderr, "Warning: failed to record usage history for %s: %v\n", reason, err)
		}

		availableState := &types.GPUState{
			LastReleased: types.FlexibleTime{Time: now},
		}
		if err := ae.client.SetGPUState(ctx, gpuID, availableState); err != nil {
			fmt.Printf("Warning: failed to set GPU %d state to available: %v\n", gpuID, err)
			continue
		}

		ae.completeBooking(ctx, state.BookingID)
	}

	return nil
}

// refreshActivity records the current time as the last observed activity of a
// reservation that has an idle timeout, provided the GPU is genuinely in use.
// Writes are rate limited so that frequent maintenance passes do not hammer
// Redis.
func (ae *AllocationEngine) refreshActivity(ctx context.Context, gpuID int, state *types.GPUState, usage *types.GPUUsage, now time.Time) {
	if state.Type != types.ReservationTypeManual || state.IdleTimeout <= 0 {
		return
	}
	// Only the holder's own work counts: somebody else's processes must not keep
	// a reservation alive that its owner is not using
	if !IsReservationHolderActive(state, usage, ae.config.MemoryThreshold) {
		return
	}

	refreshAfter := activityRefreshInterval
	if quarter := state.IdleTimeoutDuration() / 4; quarter < refreshAfter {
		refreshAfter = quarter
	}
	if now.Sub(state.LastActivity.ToTime()) < refreshAfter {
		return
	}

	state.LastActivity = types.FlexibleTime{Time: now}
	if err := ae.client.SetGPUState(ctx, gpuID, state); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to record activity for GPU %d: %v\n", gpuID, err)
	}
}

// trackUnreservedActivity records when an unreserved GPU was last seen with
// real processes on it. Once the processes stop, the AVAILABLE "free for" time
// is measured from this timestamp instead of an arbitrarily old last release.
// Writes are rate limited so frequent maintenance passes do not hammer Redis.
func (ae *AllocationEngine) trackUnreservedActivity(ctx context.Context, gpuID int, state *types.GPUState, usage *types.GPUUsage, now time.Time) {
	if state.User != "" || usage == nil {
		return
	}
	if !IsGPUInUnreservedUse(usage, ae.config.MemoryThreshold) {
		return
	}
	if now.Sub(state.LastActivity.ToTime()) < activityRefreshInterval {
		return
	}

	state.LastActivity = types.FlexibleTime{Time: now}
	if err := ae.client.SetGPUState(ctx, gpuID, state); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to record activity for GPU %d: %v\n", gpuID, err)
	}
}

// recordUsage writes a usage history record for a reservation that is ending
func (ae *AllocationEngine) recordUsage(ctx context.Context, gpuID int, state *types.GPUState, endTime time.Time) error {
	return ae.client.RecordUsageHistory(ctx, &types.UsageRecord{
		User:            state.User,
		GPUID:           gpuID,
		StartTime:       state.StartTime,
		EndTime:         types.FlexibleTime{Time: endTime},
		Duration:        endTime.Sub(state.StartTime.ToTime()).Seconds(),
		ReservationType: state.Type,
	})
}

// QueuedAllocationRequest extends AllocationRequest with queue-specific options
type QueuedAllocationRequest struct {
	*types.AllocationRequest
	Blocking    bool           // If true, wait in queue when GPUs unavailable
	WaitTimeout *time.Duration // Max time to wait (nil = forever)
}

// QueuedAllocationResult represents the result of a queued allocation
type QueuedAllocationResult struct {
	AllocatedGPUs []int
	QueueEntry    *types.QueueEntry // Non-nil if allocation is still pending
	Error         error
}

// AllocateGPUsWithQueue allocates GPUs, optionally waiting in a queue if unavailable
func (ae *AllocationEngine) AllocateGPUsWithQueue(ctx context.Context, request *QueuedAllocationRequest) (*QueuedAllocationResult, error) {
	// First, try immediate allocation
	allocatedGPUs, err := ae.AllocateGPUs(ctx, request.AllocationRequest)
	if err == nil {
		return &QueuedAllocationResult{AllocatedGPUs: allocatedGPUs}, nil
	}

	// If not blocking, return the error immediately
	if !request.Blocking {
		return nil, err
	}

	// Create a queue entry
	queueEntry := ae.createQueueEntry(request)

	// Add to queue
	if err := ae.client.AddToQueue(ctx, queueEntry); err != nil {
		return nil, fmt.Errorf("failed to add to queue: %v", err)
	}

	// Start queue heartbeat
	queueHeartbeat := NewQueueHeartbeatManager(ae.client, queueEntry.ID)
	if err := queueHeartbeat.Start(); err != nil {
		_ = ae.client.RemoveFromQueue(ctx, queueEntry.ID)
		return nil, fmt.Errorf("failed to start queue heartbeat: %v", err)
	}

	// Wait for GPUs
	result, err := ae.waitForGPUs(ctx, queueEntry, request, queueHeartbeat)
	if err != nil {
		// Cleanup on error
		queueHeartbeat.Stop()
		ae.cleanupQueueEntry(ctx, queueEntry)
		return nil, err
	}

	queueHeartbeat.Stop()
	return result, nil
}

// createQueueEntry creates a new queue entry from an allocation request
func (ae *AllocationEngine) createQueueEntry(request *QueuedAllocationRequest) *types.QueueEntry {
	now := time.Now()

	entry := &types.QueueEntry{
		ID:              uuid.New().String(),
		User:            request.User,
		ActualUser:      request.ActualUser,
		RequestedCount:  request.GPUCount,
		RequestedIDs:    request.GPUIDs,
		AllocatedGPUs:   []int{},
		ReservationType: request.ReservationType,
		Force:           request.Force,
		ClaimOwned:      request.ClaimOwned,
		IdleTimeout:     request.IdleTimeout,
		Note:            request.Note,
		EnqueueTime:     types.FlexibleTime{Time: now},
		LastHeartbeat:   types.FlexibleTime{Time: now},
	}

	if request.ExpiryTime != nil {
		entry.ExpiryDuration = request.ExpiryTime.Sub(now)
	}

	if request.WaitTimeout != nil {
		waitTimeout := now.Add(*request.WaitTimeout)
		entry.WaitTimeout = &types.FlexibleTime{Time: waitTimeout}
	}

	return entry
}

// isFirstSatisfiableInQueue reports whether this queue entry is the first one
// in FIFO order whose full request can be satisfied with the GPUs that are free
// right now. Entries that cannot be fully satisfied are skipped, so they never
// hold GPUs while waiting for the rest of their request.
func (ae *AllocationEngine) isFirstSatisfiableInQueue(ctx context.Context, queueID string) (bool, error) {
	entries, err := ae.client.GetAllQueueEntries(ctx)
	if err != nil {
		return false, err
	}

	usage, err := ae.detectGPUUsage(ctx)
	if err != nil {
		return false, err
	}
	unreservedGPUs := GetUnreservedGPUs(ctx, usage, ae.config.MemoryThreshold)
	now := time.Now()

	seenSelf := false
	for _, entry := range entries {
		if entry.ID == queueID {
			seenSelf = true
		}

		satisfiable, err := ae.queueEntrySatisfiable(ctx, entry, usage, unreservedGPUs, now)
		if err != nil {
			return false, err
		}
		if satisfiable {
			return entry.ID == queueID, nil
		}
		if seenSelf {
			// We are not satisfiable and no earlier entry is either; keep waiting
			return false, nil
		}
	}

	return false, nil
}

// queueEntrySatisfiable reports whether the entry's full request can currently
// be satisfied with free GPUs, respecting unreserved usage, scheduled bookings
// and any specific GPU IDs. GPUs already allocated to the entry count toward
// its request, so partially started (legacy) entries can be completed.
func (ae *AllocationEngine) queueEntrySatisfiable(ctx context.Context, entry *types.QueueEntry, usage map[int]*types.GPUUsage, unreservedGPUs []int, now time.Time) (bool, error) {
	needed := entry.GetRequestedGPUCount() - len(entry.AllocatedGPUs)
	if needed <= 0 {
		return true, nil
	}

	unreserved := unreservedGPUs
	if entry.Force {
		unreserved = []int{}
	} else if entry.ClaimOwned {
		unreserved = FilterOutClaimableOwnedGPUs(unreserved, usage, entry.ActualUser)
	}

	var expiryTime *time.Time
	if entry.ReservationType == types.ReservationTypeManual && entry.ExpiryDuration > 0 {
		expiry := now.Add(entry.ExpiryDuration)
		expiryTime = &expiry
	}
	blockedGPUs, err := ae.BlockedGPUsForWindow(ctx, now, ae.reservationWindowEnd(now, expiryTime))
	if err != nil {
		return false, err
	}

	gpuCount, err := ae.client.GetGPUCount(ctx)
	if err != nil {
		return false, err
	}

	available := 0
	for gpuID := 0; gpuID < gpuCount; gpuID++ {
		if containsInt(entry.AllocatedGPUs, gpuID) {
			continue
		}
		if containsInt(unreserved, gpuID) {
			continue
		}
		if _, held := blockedGPUs[gpuID]; held {
			continue
		}
		if len(entry.RequestedIDs) > 0 && !containsInt(entry.RequestedIDs, gpuID) {
			continue
		}

		state, err := ae.client.GetGPUState(ctx, gpuID)
		if err != nil {
			continue
		}
		if state.User != "" {
			continue
		}

		available++
		if available >= needed {
			return true, nil
		}
	}

	return false, nil
}

// containsInt reports whether values contains target
func containsInt(values []int, target int) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

// waitForGPUs polls for GPU availability. The first queue entry whose full
// request can be satisfied is allocated; entries that cannot start yet never
// hold GPUs while waiting.
func (ae *AllocationEngine) waitForGPUs(ctx context.Context, queueEntry *types.QueueEntry, request *QueuedAllocationRequest, heartbeat *QueueHeartbeatManager) (*QueuedAllocationResult, error) {
	ticker := time.NewTicker(types.QueuePollInterval)
	defer ticker.Stop()

	// Set up signal handling for Ctrl+C
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	position, _ := ae.client.GetQueuePosition(ctx, queueEntry.ID)
	fmt.Printf("Waiting for GPUs... (queue position: %d)\n", position+1)

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()

		case <-sigChan:
			return nil, fmt.Errorf("interrupted while waiting in queue")

		case <-ticker.C:
			// Cleanup stale queue entries
			if _, err := ae.client.CleanupStaleQueueEntries(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to cleanup stale queue entries: %v\n", err)
			}

			// Cleanup expired reservations
			if err := ae.CleanupExpiredReservations(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to cleanup expired reservations: %v\n", err)
			}

			// Check wait timeout
			if queueEntry.WaitTimeout != nil && time.Now().After(queueEntry.WaitTimeout.ToTime()) {
				return nil, fmt.Errorf("wait timeout exceeded")
			}

			// Check whether we are the first entry whose full request can be
			// satisfied right now; earlier entries that cannot start are skipped
			isFirst, err := ae.isFirstSatisfiableInQueue(ctx, queueEntry.ID)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to check queue position: %v\n", err)
				continue
			}

			if !isFirst {
				// Update position display
				newPosition, _ := ae.client.GetQueuePosition(ctx, queueEntry.ID)
				if newPosition != position {
					position = newPosition
					fmt.Printf("Queue position updated: %d\n", position+1)
				}
				continue
			}

			// We can be fully satisfied - try to allocate
			result, err := ae.tryAllocateForQueueEntry(ctx, queueEntry, request)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: allocation attempt failed: %v\n", err)
				continue
			}

			if result != nil {
				// Successfully allocated all GPUs
				// Remove from queue
				if err := ae.client.RemoveFromQueue(ctx, queueEntry.ID); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to remove from queue: %v\n", err)
				}
				return result, nil
			}
		}
	}
}

// tryAllocateForQueueEntry attempts to allocate GPUs for the first queue entry
func (ae *AllocationEngine) tryAllocateForQueueEntry(ctx context.Context, queueEntry *types.QueueEntry, request *QueuedAllocationRequest) (*QueuedAllocationResult, error) {
	// Acquire lock
	if err := ae.client.AcquireAllocationLock(ctx); err != nil {
		return nil, err
	}
	defer func() { _ = ae.client.ReleaseAllocationLock(ctx) }()

	// Re-fetch the queue entry to get latest allocated GPUs
	entry, err := ae.client.GetQueueEntry(ctx, queueEntry.ID)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, fmt.Errorf("queue entry no longer exists")
	}

	// Check if already complete
	if entry.IsComplete() {
		return ae.finalizeAllocation(ctx, entry, request)
	}

	// Refresh heartbeats for already-allocated GPUs to prevent them from
	// being considered stale while waiting for the remaining GPUs.
	now := time.Now()
	for _, gpuID := range entry.AllocatedGPUs {
		state, err := ae.client.GetGPUState(ctx, gpuID)
		if err != nil {
			continue
		}
		if state.User == entry.User && state.Type == types.ReservationTypeRun {
			state.LastHeartbeat = types.FlexibleTime{Time: now}
			_ = ae.client.SetGPUState(ctx, gpuID, state)
		}
	}

	// Get available GPUs
	usage, err := ae.detectGPUUsage(ctx)
	if err != nil {
		return nil, err
	}

	unreservedGPUs := GetUnreservedGPUs(ctx, usage, ae.config.MemoryThreshold)
	if entry.Force {
		unreservedGPUs = []int{}
	} else if entry.ClaimOwned {
		unreservedGPUs = FilterOutClaimableOwnedGPUs(unreservedGPUs, usage, entry.ActualUser)
	}

	// GPUs needed by upcoming scheduled bookings stay out of reach
	var expiryTime *time.Time
	if entry.ReservationType == types.ReservationTypeManual && entry.ExpiryDuration > 0 {
		expiry := now.Add(entry.ExpiryDuration)
		expiryTime = &expiry
	}
	blockedGPUs, err := ae.BlockedGPUsForWindow(ctx, now, ae.reservationWindowEnd(now, expiryTime))
	if err != nil {
		return nil, fmt.Errorf("failed to check scheduled bookings: %v", err)
	}

	// Find available GPUs
	gpuCount, err := ae.client.GetGPUCount(ctx)
	if err != nil {
		return nil, err
	}

	var availableGPUs []int
	for gpuID := 0; gpuID < gpuCount; gpuID++ {
		// Skip already allocated to this entry
		alreadyAllocated := false
		for _, allocatedID := range entry.AllocatedGPUs {
			if gpuID == allocatedID {
				alreadyAllocated = true
				break
			}
		}
		if alreadyAllocated {
			continue
		}

		// Skip unreserved GPUs
		isUnreserved := false
		for _, unreservedID := range unreservedGPUs {
			if gpuID == unreservedID {
				isUnreserved = true
				break
			}
		}
		if isUnreserved {
			continue
		}

		// Skip GPUs held for a scheduled booking
		if _, held := blockedGPUs[gpuID]; held {
			continue
		}

		// Check if GPU is available
		state, err := ae.client.GetGPUState(ctx, gpuID)
		if err != nil {
			continue
		}

		if state.User == "" {
			// If specific IDs requested, only consider those
			if len(entry.RequestedIDs) > 0 {
				isRequested := false
				for _, reqID := range entry.RequestedIDs {
					if gpuID == reqID {
						isRequested = true
						break
					}
				}
				if !isRequested {
					continue
				}
			}

			availableGPUs = append(availableGPUs, gpuID)
		}
	}

	// No new GPUs available
	if len(availableGPUs) == 0 {
		return nil, nil
	}

	// Only allocate when the whole request can be satisfied: a partial
	// allocation would hold GPUs for a job that cannot start yet
	needed := entry.GetRequestedGPUCount() - len(entry.AllocatedGPUs)
	if len(availableGPUs) < needed {
		return nil, nil
	}

	// Allocate exactly what the request needs
	now = time.Now()
	for i := 0; i < needed; i++ {
		gpuID := availableGPUs[i]

		// Create reservation state with partial queue ID
		gpuState := &types.GPUState{
			User:           entry.User,
			ActualUser:     entry.ActualUser,
			StartTime:      types.FlexibleTime{Time: now},
			Type:           entry.ReservationType,
			Note:           entry.Note,
			PartialQueueID: entry.ID,
		}

		if entry.ReservationType == types.ReservationTypeRun {
			gpuState.LastHeartbeat = types.FlexibleTime{Time: now}
		} else if entry.ReservationType == types.ReservationTypeManual {
			if entry.ExpiryDuration > 0 {
				gpuState.ExpiryTime = types.FlexibleTime{Time: now.Add(entry.ExpiryDuration)}
			}
			if entry.IdleTimeout > 0 {
				gpuState.IdleTimeout = int64(entry.IdleTimeout.Seconds())
				gpuState.LastActivity = types.FlexibleTime{Time: now}
			}
		}

		if err := ae.client.SetGPUState(ctx, gpuID, gpuState); err != nil {
			return nil, fmt.Errorf("failed to reserve GPU %d: %v", gpuID, err)
		}

		entry.AllocatedGPUs = append(entry.AllocatedGPUs, gpuID)
	}

	// Update queue entry
	if err := ae.client.UpdateQueueEntry(ctx, entry); err != nil {
		return nil, err
	}

	// Check if complete
	if entry.IsComplete() {
		return ae.finalizeAllocation(ctx, entry, request)
	}

	return nil, nil // Still waiting for more GPUs
}

// finalizeAllocation converts partial allocations to final reservations
func (ae *AllocationEngine) finalizeAllocation(ctx context.Context, entry *types.QueueEntry, request *QueuedAllocationRequest) (*QueuedAllocationResult, error) {
	now := time.Now()

	// Clear the partial queue ID from all allocated GPUs
	for _, gpuID := range entry.AllocatedGPUs {
		state, err := ae.client.GetGPUState(ctx, gpuID)
		if err != nil {
			continue
		}

		// Clear the partial queue ID to mark as fully allocated
		state.PartialQueueID = ""

		// Update expiry time for manual reservations (use the time of final allocation)
		if state.Type == types.ReservationTypeManual && entry.ExpiryDuration > 0 {
			state.ExpiryTime = types.FlexibleTime{Time: now.Add(entry.ExpiryDuration)}
		}

		// The idle clock starts once the whole request is satisfied: partially
		// allocated GPUs cannot be used yet
		if state.Type == types.ReservationTypeManual && state.IdleTimeout > 0 {
			state.LastActivity = types.FlexibleTime{Time: now}
		}

		if err := ae.client.SetGPUState(ctx, gpuID, state); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to finalize GPU %d: %v\n", gpuID, err)
		}
	}

	return &QueuedAllocationResult{
		AllocatedGPUs: entry.AllocatedGPUs,
	}, nil
}

// cleanupQueueEntry removes a queue entry and releases any partial allocations
func (ae *AllocationEngine) cleanupQueueEntry(ctx context.Context, entry *types.QueueEntry) {
	now := time.Now()

	// Re-fetch the queue entry from Redis to get the latest allocated GPUs,
	// since the local copy may be stale (e.g., waitForGPUs reassigns its
	// local pointer but the caller retains the original).
	latestEntry, err := ae.client.GetQueueEntry(ctx, entry.ID)
	if err == nil && latestEntry != nil {
		entry = latestEntry
	}

	// Release partial allocations
	for _, gpuID := range entry.AllocatedGPUs {
		// Record usage history before releasing
		state, stateErr := ae.client.GetGPUState(ctx, gpuID)
		if stateErr == nil && state.User == entry.User {
			duration := now.Sub(state.StartTime.ToTime()).Seconds()
			usageRecord := &types.UsageRecord{
				User:            state.User,
				GPUID:           gpuID,
				StartTime:       state.StartTime,
				EndTime:         types.FlexibleTime{Time: now},
				Duration:        duration,
				ReservationType: state.Type,
			}
			if err := ae.client.RecordUsageHistory(ctx, usageRecord); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to record usage history: %v\n", err)
			}
		}

		availableState := &types.GPUState{
			LastReleased: types.FlexibleTime{Time: now},
		}
		if err := ae.client.SetGPUState(ctx, gpuID, availableState); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to release partial allocation for GPU %d: %v\n", gpuID, err)
		}
	}

	// Remove from queue
	if err := ae.client.RemoveFromQueue(ctx, entry.ID); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to remove queue entry: %v\n", err)
	}
}

// GetQueueStatus returns the current queue status for display
func (ae *AllocationEngine) GetQueueStatus(ctx context.Context) (*types.QueueStatus, error) {
	return ae.client.GetQueueStatus(ctx)
}
