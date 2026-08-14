package gpu

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/russellb/canhazgpu/internal/types"
)

// BookingRequest describes a request to book GPUs for a future time window
type BookingRequest struct {
	GPUCount    int   // Number of GPUs to book (ignored when GPUIDs is set)
	GPUIDs      []int // Specific GPU IDs to book
	User        string
	ActualUser  string
	StartTime   time.Time
	EndTime     time.Time
	Note        string
	IdleTimeout time.Duration // Applied to the reservation created when the booking starts
}

// CreateBooking books GPUs for a future time window. GPUs are picked at booking
// time and recorded on the booking, so the schedule can be displayed and
// conflicts detected without having to guess later.
func (ae *AllocationEngine) CreateBooking(ctx context.Context, req *BookingRequest) (*types.Booking, error) {
	now := time.Now()

	if req.User == "" {
		return nil, fmt.Errorf("user cannot be empty")
	}
	if !req.EndTime.After(req.StartTime) {
		return nil, fmt.Errorf("booking must end after it starts (got %s to %s)",
			FormatBookingTime(req.StartTime, now), FormatBookingTime(req.EndTime, now))
	}
	// A little slack so "--start now" isn't rejected by clock drift or the time
	// it takes to reach this point
	if req.StartTime.Before(now.Add(-time.Minute)) {
		return nil, fmt.Errorf("booking start time %s is in the past", FormatBookingTime(req.StartTime, now))
	}

	gpuCount, err := ae.client.GetGPUCount(ctx)
	if err != nil {
		return nil, err
	}

	requested := req.GPUCount
	if len(req.GPUIDs) > 0 {
		requested = len(req.GPUIDs)
	}
	if requested <= 0 {
		requested = 1
	}
	if requested > gpuCount {
		return nil, fmt.Errorf("cannot book %d GPUs, this host only has %d", requested, gpuCount)
	}

	scheduled, err := ae.scheduledBookings(ctx)
	if err != nil {
		return nil, err
	}
	conflicts := bookingConflicts(scheduled, req.StartTime, req.EndTime, "")

	var gpuIDs []int
	if len(req.GPUIDs) > 0 {
		seen := make(map[int]bool, len(req.GPUIDs))
		for _, gpuID := range req.GPUIDs {
			if gpuID < 0 || gpuID >= gpuCount {
				return nil, fmt.Errorf("GPU ID %d is out of range (0-%d)", gpuID, gpuCount-1)
			}
			if seen[gpuID] {
				return nil, fmt.Errorf("duplicate gpu id: %d", gpuID)
			}
			seen[gpuID] = true

			if conflict, blocked := conflicts[gpuID]; blocked {
				return nil, fmt.Errorf("GPU %d is already booked by %s (%s, booking %s)",
					gpuID, conflict.User,
					FormatBookingWindow(conflict.StartTime.ToTime(), conflict.EndTime.ToTime(), now),
					conflict.ShortID())
			}
		}
		gpuIDs = append([]int(nil), req.GPUIDs...)
	} else {
		// GPUs whose current reservation runs into the booked window are still
		// bookable (the booking preempts them), but picking another GPU avoids
		// interrupting somebody's work for no reason
		occupied, err := ae.gpusHeldDuring(ctx, gpuCount, req.StartTime, req.EndTime)
		if err != nil {
			return nil, err
		}

		candidates := pickBookingGPUs(scheduled, conflicts, occupied, gpuCount, req.StartTime)
		if len(candidates) < requested {
			return nil, fmt.Errorf("cannot book %d GPUs for %s: only %d of %d GPUs are free in that window (see 'canhazgpu schedule')",
				requested, FormatBookingWindow(req.StartTime, req.EndTime, now), len(candidates), gpuCount)
		}
		gpuIDs = candidates[:requested]
	}
	sort.Ints(gpuIDs)

	booking := &types.Booking{
		ID:          uuid.New().String(),
		User:        req.User,
		ActualUser:  req.ActualUser,
		GPUIDs:      gpuIDs,
		StartTime:   types.FlexibleTime{Time: req.StartTime},
		EndTime:     types.FlexibleTime{Time: req.EndTime},
		Note:        req.Note,
		IdleTimeout: req.IdleTimeout,
		Status:      types.BookingStatusPending,
		CreatedAt:   types.FlexibleTime{Time: now},
	}

	if err := booking.Validate(); err != nil {
		return nil, err
	}

	if err := ae.client.SaveBooking(ctx, booking); err != nil {
		return nil, err
	}

	return booking, nil
}

// ListBookings returns every booking whose window overlaps [from, to), oldest
// start time first. Cancelled bookings are excluded.
func (ae *AllocationEngine) ListBookings(ctx context.Context, from, to time.Time) ([]*types.Booking, error) {
	bookings, err := ae.client.GetAllBookings(ctx)
	if err != nil {
		return nil, err
	}

	var result []*types.Booking
	for _, booking := range bookings {
		if booking.Status == types.BookingStatusCancelled {
			continue
		}
		if booking.Overlaps(from, to) {
			result = append(result, booking)
		}
	}

	return result, nil
}

// FindBooking resolves a full or abbreviated booking ID. Cancelled and
// completed bookings are matched too, so that referring to them gives a better
// error message than "not found".
func (ae *AllocationEngine) FindBooking(ctx context.Context, ref string) (*types.Booking, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("no booking ID given")
	}

	bookings, err := ae.client.GetAllBookings(ctx)
	if err != nil {
		return nil, err
	}

	var matches []*types.Booking
	for _, booking := range bookings {
		if strings.HasPrefix(booking.ID, ref) {
			matches = append(matches, booking)
		}
	}

	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no booking found matching %q", ref)
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, len(matches))
		for i, booking := range matches {
			ids[i] = booking.ShortID()
		}
		return nil, fmt.Errorf("booking ID %q is ambiguous (matches %s)", ref, strings.Join(ids, ", "))
	}
}

// CancelBooking cancels a pending booking, or ends an active one and releases
// the GPUs it holds. Bookings belonging to another user require force.
func (ae *AllocationEngine) CancelBooking(ctx context.Context, ref string, actualUser string, force bool) (*types.Booking, error) {
	booking, err := ae.FindBooking(ctx, ref)
	if err != nil {
		return nil, err
	}

	if !booking.IsScheduled() {
		return nil, fmt.Errorf("booking %s is already %s", booking.ShortID(), booking.Status)
	}

	if !force && booking.ActualUser != actualUser && booking.User != actualUser {
		return nil, fmt.Errorf("booking %s belongs to %s (use --force to cancel it anyway)",
			booking.ShortID(), booking.User)
	}

	now := time.Now()

	// An active booking holds real reservations - release them
	if booking.Status == types.BookingStatusActive {
		for _, gpuID := range booking.GPUIDs {
			state, err := ae.client.GetGPUState(ctx, gpuID)
			if err != nil || state.BookingID != booking.ID {
				continue
			}
			if err := ae.recordUsage(ctx, gpuID, state, now); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to record usage history: %v\n", err)
			}
			if err := ae.client.SetGPUState(ctx, gpuID, &types.GPUState{
				LastReleased: types.FlexibleTime{Time: now},
			}); err != nil {
				return nil, fmt.Errorf("failed to release GPU %d: %v", gpuID, err)
			}
		}
	}

	booking.Status = types.BookingStatusCancelled
	if err := ae.client.SaveBooking(ctx, booking); err != nil {
		return nil, err
	}

	return booking, nil
}

// BlockedGPUsForWindow returns the GPUs that must not be handed out for a
// reservation covering [start, end), because a scheduled booking needs them.
func (ae *AllocationEngine) BlockedGPUsForWindow(ctx context.Context, start, end time.Time) (map[int]*types.Booking, error) {
	scheduled, err := ae.scheduledBookings(ctx)
	if err != nil {
		return nil, err
	}
	return bookingConflicts(scheduled, start, end, ""), nil
}

// ActivateDueBookings turns bookings whose start time has arrived into real
// reservations. It detects GPU usage itself; callers that already have usage
// information should use activateDueBookings.
func (ae *AllocationEngine) ActivateDueBookings(ctx context.Context) ([]*types.Booking, error) {
	usage, err := ae.detectGPUUsage(ctx)
	if err != nil {
		usage = nil
	}
	return ae.activateDueBookings(ctx, usage)
}

// activateDueBookings activates bookings that have started and completes those
// whose window has passed. usage may be nil when GPU usage is unknown.
func (ae *AllocationEngine) activateDueBookings(ctx context.Context, usage map[int]*types.GPUUsage) ([]*types.Booking, error) {
	bookings, err := ae.client.GetAllBookings(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	var activated []*types.Booking

	for _, booking := range bookings {
		switch booking.Status {
		case types.BookingStatusPending:
			if now.Before(booking.StartTime.ToTime()) {
				continue
			}

			// Nobody ran canhazgpu while the window was open - the booking
			// simply passed by
			if !now.Before(booking.EndTime.ToTime()) {
				booking.Status = types.BookingStatusCompleted
				if err := ae.client.SaveBooking(ctx, booking); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to complete missed booking %s: %v\n", booking.ShortID(), err)
				}
				continue
			}

			if err := ae.activateBooking(ctx, booking, usage, now); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to activate booking %s: %v\n", booking.ShortID(), err)
				continue
			}
			activated = append(activated, booking)

		case types.BookingStatusActive:
			// The reservation itself expires via its expiry time; here we only
			// close the booking record
			if !now.Before(booking.EndTime.ToTime()) {
				booking.Status = types.BookingStatusCompleted
				if err := ae.client.SaveBooking(ctx, booking); err != nil {
					fmt.Fprintf(os.Stderr, "Warning: failed to complete booking %s: %v\n", booking.ShortID(), err)
				}
			}
		}
	}

	return activated, nil
}

// activateBooking converts a booking into manual reservations on its GPUs,
// preempting whatever reservation currently holds them.
func (ae *AllocationEngine) activateBooking(ctx context.Context, booking *types.Booking, usage map[int]*types.GPUUsage, now time.Time) error {
	if err := ae.client.AcquireAllocationLock(ctx); err != nil {
		return err
	}
	defer func() {
		if err := ae.client.ReleaseAllocationLock(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to release allocation lock: %v\n", err)
		}
	}()

	for _, gpuID := range booking.GPUIDs {
		state, err := ae.client.GetGPUState(ctx, gpuID)
		if err != nil {
			return fmt.Errorf("failed to read state of GPU %d: %v", gpuID, err)
		}

		// Preempt whoever holds the GPU now: the booking was made first
		if state.User != "" && state.BookingID != booking.ID {
			if err := ae.recordUsage(ctx, gpuID, state, now); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to record usage history: %v\n", err)
			}
			fmt.Fprintf(os.Stderr, "Booking %s started: released %s reservation by %s on GPU %d\n",
				booking.ShortID(), state.Type, state.User, gpuID)
		}

		// Unreserved processes cannot be preempted - warn instead
		if usage != nil && IsGPUInUnreservedUse(usage[gpuID], ae.config.MemoryThreshold) {
			fmt.Fprintf(os.Stderr, "Warning: booking %s activated on GPU %d while %d process(es) run there without a reservation\n",
				booking.ShortID(), gpuID, len(usage[gpuID].Processes))
		}

		newState := &types.GPUState{
			User:         booking.User,
			ActualUser:   booking.ActualUser,
			StartTime:    types.FlexibleTime{Time: now},
			Type:         types.ReservationTypeManual,
			ExpiryTime:   booking.EndTime,
			Note:         booking.Note,
			BookingID:    booking.ID,
			TaskID:       booking.ShortID(),
			LastActivity: types.FlexibleTime{Time: now},
			IdleTimeout:  int64(booking.IdleTimeout.Seconds()),
		}

		if err := ae.client.SetGPUState(ctx, gpuID, newState); err != nil {
			return fmt.Errorf("failed to reserve GPU %d: %v", gpuID, err)
		}
	}

	booking.Status = types.BookingStatusActive
	booking.ActivatedAt = types.FlexibleTime{Time: now}

	return ae.client.SaveBooking(ctx, booking)
}

// pruneBookings deletes bookings that ended long enough ago to be irrelevant
func (ae *AllocationEngine) pruneBookings(ctx context.Context) error {
	bookings, err := ae.client.GetAllBookings(ctx)
	if err != nil {
		return err
	}

	cutoff := time.Now().Add(-types.BookingRetention)
	for _, booking := range bookings {
		if booking.IsScheduled() {
			continue
		}
		if booking.EndTime.ToTime().Before(cutoff) {
			if err := ae.client.DeleteBooking(ctx, booking.ID); err != nil {
				return err
			}
		}
	}

	return nil
}

// completeBooking marks a booking as completed, used when the reservation it
// created is released early
func (ae *AllocationEngine) completeBooking(ctx context.Context, bookingID string) {
	if bookingID == "" {
		return
	}

	booking, err := ae.client.GetBooking(ctx, bookingID)
	if err != nil || booking == nil || !booking.IsScheduled() {
		return
	}

	booking.Status = types.BookingStatusCompleted
	if err := ae.client.SaveBooking(ctx, booking); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to complete booking %s: %v\n", booking.ShortID(), err)
	}
}

// scheduledBookings returns all bookings that still hold GPUs
func (ae *AllocationEngine) scheduledBookings(ctx context.Context) ([]*types.Booking, error) {
	bookings, err := ae.client.GetAllBookings(ctx)
	if err != nil {
		return nil, err
	}

	var scheduled []*types.Booking
	for _, booking := range bookings {
		if booking.IsScheduled() {
			scheduled = append(scheduled, booking)
		}
	}

	return scheduled, nil
}

// bookingConflicts maps GPU IDs to the booking that holds them during
// [start, end). excludeID, when set, ignores one booking (used when checking a
// booking against the others).
func bookingConflicts(bookings []*types.Booking, start, end time.Time, excludeID string) map[int]*types.Booking {
	conflicts := make(map[int]*types.Booking)

	for _, booking := range bookings {
		if booking.ID == excludeID || !booking.Overlaps(start, end) {
			continue
		}
		for _, gpuID := range booking.GPUIDs {
			// Keep the earliest conflicting booking for clearer messages
			if existing, ok := conflicts[gpuID]; ok && existing.StartTime.ToTime().Before(booking.StartTime.ToTime()) {
				continue
			}
			conflicts[gpuID] = booking
		}
	}

	return conflicts
}

// gpusHeldDuring returns the GPUs whose current reservation would still be in
// place during [start, end). Run-type reservations have no known end, so they
// count as held whenever the window starts within the protection window.
func (ae *AllocationEngine) gpusHeldDuring(ctx context.Context, gpuCount int, start, end time.Time) (map[int]bool, error) {
	held := make(map[int]bool)
	now := time.Now()

	for gpuID := 0; gpuID < gpuCount; gpuID++ {
		state, err := ae.client.GetGPUState(ctx, gpuID)
		if err != nil {
			return nil, err
		}
		if state.User == "" {
			continue
		}

		switch {
		case !state.ExpiryTime.ToTime().IsZero():
			if state.ExpiryTime.ToTime().After(start) {
				held[gpuID] = true
			}
		case start.Before(now.Add(ae.bookingProtectionWindow())):
			held[gpuID] = true
		}
	}

	return held, nil
}

// BookingPreemptions describes, for each GPU of a booking, a reservation that
// will be released when the booking starts. It is used to warn the person
// making the booking that somebody else is going to be interrupted.
func (ae *AllocationEngine) BookingPreemptions(ctx context.Context, booking *types.Booking) ([]string, error) {
	var warnings []string
	now := time.Now()

	for _, gpuID := range booking.GPUIDs {
		state, err := ae.client.GetGPUState(ctx, gpuID)
		if err != nil {
			return nil, err
		}
		if state.User == "" || state.BookingID == booking.ID {
			continue
		}

		// A reservation that ends before the window opens is not affected
		expiry := state.ExpiryTime.ToTime()
		if !expiry.IsZero() && !expiry.After(booking.StartTime.ToTime()) {
			continue
		}

		if expiry.IsZero() {
			warnings = append(warnings, fmt.Sprintf("GPU %d currently runs a %s reservation by %s, which will be released when the booking starts",
				gpuID, state.Type, state.User))
			continue
		}

		warnings = append(warnings, fmt.Sprintf("GPU %d is reserved by %s until %s, which will be released when the booking starts",
			gpuID, state.User, FormatBookingTime(expiry, now)))
	}

	return warnings, nil
}

// pickBookingGPUs returns bookable GPUs in preference order: GPUs nobody is
// using now come first, then the least booked ones, so that bookings spread out
// over the pool instead of piling onto GPU 0
func pickBookingGPUs(bookings []*types.Booking, conflicts map[int]*types.Booking, occupied map[int]bool, gpuCount int, start time.Time) []int {
	dayStart := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
	dayEnd := dayStart.AddDate(0, 0, 1)

	load := make(map[int]time.Duration)
	for _, booking := range bookings {
		booked := overlapDuration(booking.StartTime.ToTime(), booking.EndTime.ToTime(), dayStart, dayEnd)
		if booked <= 0 {
			continue
		}
		for _, gpuID := range booking.GPUIDs {
			load[gpuID] += booked
		}
	}

	var candidates []int
	for gpuID := 0; gpuID < gpuCount; gpuID++ {
		if _, blocked := conflicts[gpuID]; !blocked {
			candidates = append(candidates, gpuID)
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if occupied[candidates[i]] != occupied[candidates[j]] {
			return !occupied[candidates[i]]
		}
		if load[candidates[i]] != load[candidates[j]] {
			return load[candidates[i]] < load[candidates[j]]
		}
		return candidates[i] < candidates[j]
	})

	return candidates
}

// overlapDuration returns how much of [aStart, aEnd) falls inside [bStart, bEnd)
func overlapDuration(aStart, aEnd, bStart, bEnd time.Time) time.Duration {
	start := aStart
	if bStart.After(start) {
		start = bStart
	}
	end := aEnd
	if bEnd.Before(end) {
		end = bEnd
	}
	if !end.After(start) {
		return 0
	}
	return end.Sub(start)
}

// FormatBookingTime renders a point in time relative to ref: bare clock time
// for today, "tomorrow HH:MM", or the full date further out
func FormatBookingTime(t time.Time, ref time.Time) string {
	refDay := time.Date(ref.Year(), ref.Month(), ref.Day(), 0, 0, 0, 0, ref.Location())
	switch days := int(time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).Sub(refDay).Hours() / 24); days {
	case 0:
		return t.Format("15:04")
	case 1:
		return "tomorrow " + t.Format("15:04")
	default:
		return t.Format("2006-01-02 15:04")
	}
}

// FormatBookingWindow renders a booking window relative to ref, e.g.
// "14:00-16:00" for today or "2026-08-05 09:00-11:00"
func FormatBookingWindow(start, end time.Time, ref time.Time) string {
	sameDay := start.Year() == end.Year() && start.YearDay() == end.YearDay()
	if sameDay {
		return fmt.Sprintf("%s-%s", FormatBookingTime(start, ref), end.Format("15:04"))
	}
	return fmt.Sprintf("%s-%s", FormatBookingTime(start, ref), FormatBookingTime(end, ref))
}
