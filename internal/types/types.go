package types

import (
	"fmt"
	"strconv"
	"time"
)

// GPUState represents the state of a GPU in Redis
type GPUState struct {
	User           string       `json:"user,omitempty"`        // Display name (custom user if provided, otherwise OS user)
	ActualUser     string       `json:"actual_user,omitempty"` // Actual OS account name
	StartTime      FlexibleTime `json:"start_time,omitempty"`
	LastHeartbeat  FlexibleTime `json:"last_heartbeat,omitempty"`
	Type           string       `json:"type,omitempty"` // "run" or "manual"
	ExpiryTime     FlexibleTime `json:"expiry_time,omitempty"`
	LastReleased   FlexibleTime `json:"last_released,omitempty"`
	Note           string       `json:"note,omitempty"`
	PartialQueueID string       `json:"partial_queue_id,omitempty"` // Queue entry ID for partial allocations
	IdleTimeout    int64        `json:"idle_timeout,omitempty"`     // Seconds a manual reservation may stay unused before auto-release (0 = disabled)
	LastActivity   FlexibleTime `json:"last_activity,omitempty"`    // Last time real GPU usage was observed for this reservation
	BookingID      string       `json:"booking_id,omitempty"`       // Scheduled booking that created this reservation
	TaskID         string       `json:"task_id,omitempty"`          // Short handle for the job holding this GPU, used by 'cancel'
	PID            int          `json:"pid,omitempty"`              // Process to signal when cancelling a run-type reservation
}

// IdleTimeoutDuration returns the idle timeout as a time.Duration
func (gs *GPUState) IdleTimeoutDuration() time.Duration {
	return time.Duration(gs.IdleTimeout) * time.Second
}

// IdleSince returns the timestamp idle time is measured from. Reservations
// created before the idle timeout feature (or that were never observed in use)
// fall back to their start time, which gives the owner the full idle grace
// period to actually start using the GPU.
func (gs *GPUState) IdleSince() time.Time {
	if !gs.LastActivity.ToTime().IsZero() {
		return gs.LastActivity.ToTime()
	}
	return gs.StartTime.ToTime()
}

// FlexibleTime handles both Unix timestamps and RFC3339 time strings
type FlexibleTime struct {
	time.Time
}

func (ft *FlexibleTime) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}

	// Remove quotes if present
	s := string(data)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}

	// Try parsing as Unix timestamp first (Python compatibility)
	if timestamp, err := strconv.ParseFloat(s, 64); err == nil {
		ft.Time = time.Unix(int64(timestamp), 0)
		return nil
	}

	// Try parsing as RFC3339 string
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		ft.Time = t
		return nil
	}

	// Try parsing as Unix timestamp string
	if timestamp, err := strconv.ParseInt(s, 10, 64); err == nil {
		ft.Time = time.Unix(timestamp, 0)
		return nil
	}

	return fmt.Errorf("cannot parse time: %s", s)
}

func (ft FlexibleTime) MarshalJSON() ([]byte, error) {
	if ft.IsZero() {
		return []byte("null"), nil
	}
	return ft.Time.MarshalJSON()
}

// ToTime converts FlexibleTime to time.Time
func (ft FlexibleTime) ToTime() time.Time {
	return ft.Time
}

// GPUUsage represents actual GPU usage detected via nvidia-smi
type GPUUsage struct {
	GPUID              int              `json:"gpu_id"`
	MemoryMB           int              `json:"memory_mb"`
	UtilizationPercent int              `json:"utilization_percent,omitempty"` // GPU utilization reported by the provider (0-100)
	Processes          []GPUProcessInfo `json:"processes"`
	Users              map[string]bool  `json:"users"`
	Provider           string           `json:"provider"` // "nvidia" or "amd"
	Model              string           `json:"model"`    // GPU model name (e.g., "H100", "RTX 4090") or "AMD"
}

// GPUProcessInfo represents a process using a GPU
type GPUProcessInfo struct {
	PID            int    `json:"pid"`
	ProcessName    string `json:"process_name"`
	User           string `json:"user"`
	MemoryMB       int    `json:"memory_mb"`
	ElapsedSeconds int64  `json:"elapsed_seconds,omitempty"` // How long the process has been running (0 when unknown)
}

// AllocationRequest represents a request to allocate GPUs
type AllocationRequest struct {
	GPUCount        int    // Number of GPUs to allocate (ignored if GPUIDs is specified)
	GPUIDs          []int  // Specific GPU IDs to allocate (mutually exclusive with GPUCount)
	User            string // Display name (custom user if --user flag provided)
	ActualUser      string // Actual OS account name
	ReservationType string
	ExpiryTime      *time.Time
	Force           bool          // If true, allow reserving GPUs that are in unreserved use
	ClaimOwned      bool          // If true, allow reserving unreserved GPUs used only by the requester's own processes
	Note            string        // Optional note describing the reservation purpose
	IdleTimeout     time.Duration // Auto-release a manual reservation after this long without GPU usage (0 = disabled)
	BlockedGPUs     []int         // GPUs held back for upcoming scheduled bookings (filled in by the allocation engine)
	TaskID          string        // Short handle stored on the reservation (generated by the allocation engine when empty)
	PID             int           // Process to signal when cancelling (run-type reservations only)
}

// Validate checks if the allocation request is valid
func (ar *AllocationRequest) Validate() error {
	// Check that either GPUCount or GPUIDs is specified, but not both
	hasGPUCount := ar.GPUCount > 0
	hasGPUIDs := len(ar.GPUIDs) > 0

	if !hasGPUCount && !hasGPUIDs {
		return fmt.Errorf("either gpu count or specific gpu ids must be specified")
	}

	if hasGPUCount && hasGPUIDs {
		// Allow if GPUCount matches the number of GPU IDs
		if ar.GPUCount == len(ar.GPUIDs) {
			// This is fine - user specified matching count and IDs
		} else if ar.GPUCount == 1 {
			// Allow if GPUCount is 1 (likely the default) regardless of GPU ID count
		} else {
			return fmt.Errorf("conflicting gpu count (%d) and gpu ids (count: %d)", ar.GPUCount, len(ar.GPUIDs))
		}
	}

	if hasGPUCount && ar.GPUCount <= 0 {
		return fmt.Errorf("gpu count must be positive, got %d", ar.GPUCount)
	}

	if hasGPUIDs {
		// Check for duplicate GPU IDs
		seen := make(map[int]bool)
		for _, id := range ar.GPUIDs {
			if id < 0 {
				return fmt.Errorf("gpu id must be non-negative, got %d", id)
			}
			if seen[id] {
				return fmt.Errorf("duplicate gpu id: %d", id)
			}
			seen[id] = true
		}
	}

	if ar.User == "" {
		return fmt.Errorf("user cannot be empty")
	}

	if ar.ReservationType != ReservationTypeRun && ar.ReservationType != ReservationTypeManual {
		return fmt.Errorf("invalid reservation type: %s", ar.ReservationType)
	}

	return nil
}

// AllocationResult represents the result of a GPU allocation
type AllocationResult struct {
	AllocatedGPUs []int
	Error         error
}

// UsageRecord represents a historical GPU usage record
type UsageRecord struct {
	User            string       `json:"user"`
	GPUID           int          `json:"gpu_id"`
	StartTime       FlexibleTime `json:"start_time"`
	EndTime         FlexibleTime `json:"end_time"`
	Duration        float64      `json:"duration_seconds"`
	ReservationType string       `json:"reservation_type"`
}

// Config represents the application configuration
type Config struct {
	HostSocket      string            // Optional host guard bridge (shared Redis and host device view)
	HostPID         int               // PID in the host namespace, obtained from Unix peer credentials
	HostUser        string            // Host account owning this process/container
	ContainerOwners map[string]string // Host-only overrides keyed by full container ID
	RedisHost       string
	RedisPort       int
	RedisDB         int
	MemoryThreshold int
	RemoteHosts     []string // SSH addresses (can use ~/.ssh/config entries for friendly names)

	// BookingProtectionWindow is how far ahead a reservation without a fixed end
	// time (i.e. 'run') looks for scheduled bookings. GPUs booked within this
	// window are not handed out to such reservations.
	BookingProtectionWindow time.Duration
}

// QueueEntry represents a request waiting in the queue for GPUs
type QueueEntry struct {
	ID              string        `json:"id"`
	User            string        `json:"user"`
	ActualUser      string        `json:"actual_user"`
	RequestedCount  int           `json:"requested_count"`
	RequestedIDs    []int         `json:"requested_ids,omitempty"`
	AllocatedGPUs   []int         `json:"allocated_gpus"`
	ReservationType string        `json:"reservation_type"`
	Force           bool          `json:"force,omitempty"`
	ClaimOwned      bool          `json:"claim_owned,omitempty"`
	ExpiryDuration  time.Duration `json:"expiry_duration,omitempty"`
	IdleTimeout     time.Duration `json:"idle_timeout,omitempty"`
	Note            string        `json:"note,omitempty"`
	EnqueueTime     FlexibleTime  `json:"enqueue_time"`
	LastHeartbeat   FlexibleTime  `json:"last_heartbeat"`
	WaitTimeout     *FlexibleTime `json:"wait_timeout,omitempty"`
	PID             int           `json:"pid,omitempty"` // Waiting process, signalled by 'cancel'
}

// ShortID returns the handle shown by 'queue' and accepted by 'cancel'
func (qe *QueueEntry) ShortID() string {
	if len(qe.ID) <= TaskShortIDLength {
		return qe.ID
	}
	return qe.ID[:TaskShortIDLength]
}

// GetRequestedGPUCount returns the total number of GPUs requested
func (qe *QueueEntry) GetRequestedGPUCount() int {
	if len(qe.RequestedIDs) > 0 {
		return len(qe.RequestedIDs)
	}
	return qe.RequestedCount
}

// IsComplete returns true if all requested GPUs have been allocated
func (qe *QueueEntry) IsComplete() bool {
	return len(qe.AllocatedGPUs) >= qe.GetRequestedGPUCount()
}

// Booking represents a GPU reservation scheduled for a future time window,
// like booking a meeting room. A booking holds specific GPU IDs for
// [StartTime, EndTime) and is turned into a regular manual reservation when
// its start time arrives.
type Booking struct {
	ID          string        `json:"id"`
	User        string        `json:"user"`        // Display name
	ActualUser  string        `json:"actual_user"` // Actual OS account name
	GPUIDs      []int         `json:"gpu_ids"`
	StartTime   FlexibleTime  `json:"start_time"`
	EndTime     FlexibleTime  `json:"end_time"`
	Note        string        `json:"note,omitempty"`
	IdleTimeout time.Duration `json:"idle_timeout,omitempty"` // Applied to the reservation once the booking is activated
	Status      string        `json:"status"`                 // "pending", "active", "completed" or "cancelled"
	CreatedAt   FlexibleTime  `json:"created_at"`
	ActivatedAt FlexibleTime  `json:"activated_at,omitempty"`
}

// ShortID returns an abbreviated booking ID suitable for display and for
// referring to the booking on the command line
func (b *Booking) ShortID() string {
	if len(b.ID) <= BookingShortIDLength {
		return b.ID
	}
	return b.ID[:BookingShortIDLength]
}

// IsScheduled reports whether the booking still holds GPUs (pending or active)
func (b *Booking) IsScheduled() bool {
	return b.Status == BookingStatusPending || b.Status == BookingStatusActive
}

// Overlaps reports whether the booking's window intersects [start, end)
func (b *Booking) Overlaps(start, end time.Time) bool {
	return b.StartTime.ToTime().Before(end) && b.EndTime.ToTime().After(start)
}

// UsesGPU reports whether the booking holds the given GPU
func (b *Booking) UsesGPU(gpuID int) bool {
	for _, id := range b.GPUIDs {
		if id == gpuID {
			return true
		}
	}
	return false
}

// Duration returns the length of the booked window
func (b *Booking) Duration() time.Duration {
	return b.EndTime.ToTime().Sub(b.StartTime.ToTime())
}

// Validate checks that the booking describes a usable time window
func (b *Booking) Validate() error {
	if b.User == "" {
		return fmt.Errorf("user cannot be empty")
	}
	if len(b.GPUIDs) == 0 {
		return fmt.Errorf("booking must hold at least one GPU")
	}
	if b.StartTime.ToTime().IsZero() || b.EndTime.ToTime().IsZero() {
		return fmt.Errorf("booking must have a start and end time")
	}
	if !b.EndTime.ToTime().After(b.StartTime.ToTime()) {
		return fmt.Errorf("booking end time must be after its start time")
	}
	return nil
}

// Violation represents GPU usage that bypasses the reservation system: either a
// process on a GPU nobody reserved, or a process belonging to somebody other
// than the GPU's reservation holder.
type Violation struct {
	GPUID         int          `json:"gpu_id"`
	PID           int          `json:"pid"`
	User          string       `json:"user"`                     // OS account owning the offending process
	Kind          string       `json:"kind"`                     // ViolationKindUnreserved or ViolationKindForeign
	Holder        string       `json:"holder,omitempty"`         // Reservation holder as displayed, for foreign usage
	HolderAccount string       `json:"holder_account,omitempty"` // OS account of the holder, used to reach them
	Command       string       `json:"command,omitempty"`
	MemoryMB      int          `json:"memory_mb"`
	FirstSeen     FlexibleTime `json:"first_seen"`
	LastSeen      FlexibleTime `json:"last_seen"`
	Sightings     int          `json:"sightings"`  // Consecutive detections, used to confirm
	WarnCount     int          `json:"warn_count"` // Warnings delivered so far
	LastWarned    FlexibleTime `json:"last_warned,omitempty"`
	Signals       []string     `json:"signals,omitempty"` // Termination signals already sent
	LastSignal    FlexibleTime `json:"last_signal,omitempty"`
	EndTime       FlexibleTime `json:"end_time,omitempty"` // Set when the violation is resolved
	Resolution    string       `json:"resolution,omitempty"`
}

// ViolationID identifies a violation by the GPU and process it concerns
func (v *Violation) ViolationID() string {
	return fmt.Sprintf("%d:%d", v.GPUID, v.PID)
}

// Duration returns how long the violation has been going on
func (v *Violation) Duration() time.Duration {
	end := v.EndTime.ToTime()
	if end.IsZero() {
		end = v.LastSeen.ToTime()
	}
	return end.Sub(v.FirstSeen.ToTime())
}

// Describe renders a short human readable description of the violation
func (v *Violation) Describe() string {
	if v.Kind == ViolationKindForeign {
		return fmt.Sprintf("GPU %d is reserved by %s, but PID %d belongs to %s",
			v.GPUID, v.Holder, v.PID, v.User)
	}
	return fmt.Sprintf("GPU %d has no reservation, but PID %d (%s) is using it",
		v.GPUID, v.PID, v.User)
}

// QueueStatus represents the current queue status for display
type QueueStatus struct {
	Entries            []*QueueEntry `json:"entries"`
	TotalWaiting       int           `json:"total_waiting"`
	TotalGPUsRequested int           `json:"total_gpus_requested"`
	TotalGPUsAllocated int           `json:"total_gpus_allocated"`
}

// Constants
const (
	ReservationTypeRun    = "run"
	ReservationTypeManual = "manual"

	BookingStatusPending   = "pending"
	BookingStatusActive    = "active"
	BookingStatusCompleted = "completed"
	BookingStatusCancelled = "cancelled"

	// ViolationKindUnreserved is GPU usage on a GPU nobody reserved
	ViolationKindUnreserved = "unreserved"
	// ViolationKindForeign is usage by somebody other than the reservation holder
	ViolationKindForeign = "foreign"

	RedisKeyPrefix         = "canhazgpu:"
	RedisKeyGPUCount       = RedisKeyPrefix + "gpu_count"
	RedisKeyProvider       = RedisKeyPrefix + "provider"
	RedisKeyAllocationLock = RedisKeyPrefix + "allocation_lock"
	RedisKeyUsageHistory   = RedisKeyPrefix + "usage_history:"
	RedisKeyQueue          = RedisKeyPrefix + "queue"
	RedisKeyQueueEntry     = RedisKeyPrefix + "queue:entry:"
	RedisKeyBookings       = RedisKeyPrefix + "bookings"
	RedisKeyBooking        = RedisKeyPrefix + "booking:"

	RedisKeyViolations       = RedisKeyPrefix + "violations"
	RedisKeyViolation        = RedisKeyPrefix + "violation:"
	RedisKeyViolationHistory = RedisKeyPrefix + "violation_history_sorted"
	RedisKeyGuardLock        = RedisKeyPrefix + "guard:lock"
	RedisKeyGuardKills       = RedisKeyPrefix + "guard:kills"

	HeartbeatInterval   = 60 * time.Second
	HeartbeatTimeout    = 5 * time.Minute
	HealthCheckInterval = 15 * time.Second
	LockTimeout         = 10 * time.Second
	MaxLockRetries      = 5

	QueueHeartbeatInterval = 30 * time.Second
	QueueHeartbeatTimeout  = 2 * time.Minute
	QueuePollInterval      = 2 * time.Second

	MemoryThresholdMB = 100

	// DefaultIdleTimeout is how long a manual reservation may sit without any
	// detected GPU usage before it is released automatically
	DefaultIdleTimeout = 15 * time.Minute

	// DefaultRunIdleTimeout is how long a run-type reservation may sit without
	// any detected GPU usage before the supervisor releases it. It defaults to
	// 30 minutes: long enough for a job to load and touch the GPU, short enough
	// that a wrapper shell which outlived its GPU process cannot squat on the
	// GPUs for long.
	DefaultRunIdleTimeout = 30 * time.Minute

	// MaxIdleTimeout is the upper bound for a reservation's idle timeout;
	// larger values are rejected so forgotten reservations cannot hold GPUs
	// indefinitely
	MaxIdleTimeout = 3 * time.Hour

	// DefaultBookingProtectionWindow is how far ahead reservations without a
	// fixed end time look for scheduled bookings
	DefaultBookingProtectionWindow = 30 * time.Minute

	// BookingRetention is how long completed/cancelled bookings are kept for
	// reference before being pruned
	BookingRetention = 7 * 24 * time.Hour

	// BookingSlotDuration is the granularity of the schedule grid
	BookingSlotDuration = 30 * time.Minute

	// BookingShortIDLength is the number of ID characters shown (and accepted)
	// when referring to a booking
	BookingShortIDLength = 8

	// TaskShortIDLength is the number of ID characters shown by 'queue' and
	// accepted by 'cancel' for queued and running jobs
	TaskShortIDLength = 8

	// Guard defaults: how the monitoring daemon reacts to GPU usage that
	// bypasses the reservation system
	DefaultGuardInterval        = 15 * time.Second
	DefaultGuardGracePeriod     = 60 * time.Second
	DefaultGuardConfirmations   = 2
	DefaultGuardWarnInterval    = 5 * time.Minute
	DefaultGuardMaxWarnings     = 3
	DefaultGuardKillGrace       = 30 * time.Second
	DefaultGuardMaxKillsPerHour = 3

	// GuardLockTTL is how long a guard instance holds its singleton lock
	// without refreshing it
	GuardLockTTL = 2 * time.Minute

	// ViolationRetention is how long resolved violations are kept for reporting
	ViolationRetention = 90 * 24 * time.Hour
)
