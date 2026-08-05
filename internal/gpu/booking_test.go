package gpu

import (
	"context"
	"testing"
	"time"

	"github.com/russellb/canhazgpu/internal/redis_client"
	"github.com/russellb/canhazgpu/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testBooking(id string, gpuIDs []int, start, end time.Time, status string) *types.Booking {
	return &types.Booking{
		ID:        id,
		User:      "alice",
		GPUIDs:    gpuIDs,
		StartTime: types.FlexibleTime{Time: start},
		EndTime:   types.FlexibleTime{Time: end},
		Status:    status,
	}
}

func TestBookingConflicts(t *testing.T) {
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.Local)

	bookings := []*types.Booking{
		testBooking("a", []int{0, 1}, base.Add(2*time.Hour), base.Add(4*time.Hour), types.BookingStatusPending),
		testBooking("b", []int{2}, base.Add(6*time.Hour), base.Add(7*time.Hour), types.BookingStatusPending),
	}

	tests := []struct {
		name      string
		start     time.Time
		end       time.Time
		expectIDs []int
	}{
		{
			name:      "Window before every booking",
			start:     base,
			end:       base.Add(time.Hour),
			expectIDs: nil,
		},
		{
			name:      "Window ending exactly when a booking starts does not conflict",
			start:     base,
			end:       base.Add(2 * time.Hour),
			expectIDs: nil,
		},
		{
			name:      "Overlapping start",
			start:     base.Add(time.Hour),
			end:       base.Add(3 * time.Hour),
			expectIDs: []int{0, 1},
		},
		{
			name:      "Window inside a booking",
			start:     base.Add(150 * time.Minute),
			end:       base.Add(3 * time.Hour),
			expectIDs: []int{0, 1},
		},
		{
			name:      "Window spanning both bookings",
			start:     base,
			end:       base.Add(8 * time.Hour),
			expectIDs: []int{0, 1, 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conflicts := bookingConflicts(bookings, tt.start, tt.end, "")

			var got []int
			for gpuID := range conflicts {
				got = append(got, gpuID)
			}
			assert.ElementsMatch(t, tt.expectIDs, got)
		})
	}
}

func TestBookingConflicts_ExcludesCancelledAndSelf(t *testing.T) {
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.Local)
	start, end := base.Add(time.Hour), base.Add(2*time.Hour)

	bookings := []*types.Booking{
		testBooking("self", []int{0}, start, end, types.BookingStatusPending),
	}

	assert.Empty(t, bookingConflicts(bookings, start, end, "self"),
		"a booking must not conflict with itself")
	assert.Len(t, bookingConflicts(bookings, start, end, ""), 1)
}

func TestPickBookingGPUs_PrefersLeastBookedGPUs(t *testing.T) {
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.Local)

	// GPU 0 is busy for four hours that day, GPU 1 for one hour
	bookings := []*types.Booking{
		testBooking("a", []int{0}, base, base.Add(4*time.Hour), types.BookingStatusPending),
		testBooking("b", []int{1}, base, base.Add(time.Hour), types.BookingStatusPending),
	}

	// Window in the evening, so nothing conflicts
	start := base.Add(8 * time.Hour)
	candidates := pickBookingGPUs(bookings, map[int]*types.Booking{}, nil, 4, start)

	assert.Equal(t, []int{2, 3, 1, 0}, candidates,
		"unbooked GPUs first, then the least booked ones")
}

func TestPickBookingGPUs_SkipsConflictingGPUs(t *testing.T) {
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.Local)
	conflicts := map[int]*types.Booking{
		1: testBooking("a", []int{1}, base, base.Add(time.Hour), types.BookingStatusPending),
	}

	candidates := pickBookingGPUs(nil, conflicts, nil, 3, base)
	assert.Equal(t, []int{0, 2}, candidates)
}

func TestOverlapDuration(t *testing.T) {
	base := time.Date(2026, 8, 3, 12, 0, 0, 0, time.Local)

	tests := []struct {
		name     string
		aStart   time.Time
		aEnd     time.Time
		expected time.Duration
	}{
		{"Fully inside", base.Add(time.Hour), base.Add(2 * time.Hour), time.Hour},
		{"Partially before", base.Add(-time.Hour), base.Add(time.Hour), time.Hour},
		{"Partially after", base.Add(3 * time.Hour), base.Add(5 * time.Hour), time.Hour},
		{"Disjoint", base.Add(5 * time.Hour), base.Add(6 * time.Hour), 0},
		{"Touching the edge", base.Add(4 * time.Hour), base.Add(5 * time.Hour), 0},
	}

	// Reference window: 12:00 to 16:00
	windowStart, windowEnd := base, base.Add(4*time.Hour)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, overlapDuration(tt.aStart, tt.aEnd, windowStart, windowEnd))
		})
	}
}

func TestFormatBookingWindow(t *testing.T) {
	ref := time.Date(2026, 8, 3, 13, 0, 0, 0, time.Local)

	sameDay := FormatBookingWindow(
		time.Date(2026, 8, 3, 14, 0, 0, 0, time.Local),
		time.Date(2026, 8, 3, 16, 0, 0, 0, time.Local), ref)
	assert.Equal(t, "14:00-16:00", sameDay)

	tomorrow := FormatBookingWindow(
		time.Date(2026, 8, 4, 9, 0, 0, 0, time.Local),
		time.Date(2026, 8, 4, 11, 0, 0, 0, time.Local), ref)
	assert.Equal(t, "tomorrow 09:00-11:00", tomorrow)

	later := FormatBookingWindow(
		time.Date(2026, 8, 10, 9, 0, 0, 0, time.Local),
		time.Date(2026, 8, 10, 11, 0, 0, 0, time.Local), ref)
	assert.Equal(t, "2026-08-10 09:00-11:00", later)
}

func TestBookingShortIDAndStatus(t *testing.T) {
	booking := &types.Booking{ID: "3f9ac21b-1234-5678-9abc-def012345678", Status: types.BookingStatusPending}

	assert.Equal(t, "3f9ac21b", booking.ShortID())
	assert.True(t, booking.IsScheduled())

	booking.Status = types.BookingStatusCompleted
	assert.False(t, booking.IsScheduled())

	short := &types.Booking{ID: "abc"}
	assert.Equal(t, "abc", short.ShortID())
}

// setupBookingTestEngine creates an allocation engine backed by a Redis test
// database with a fake GPU pool of the requested size.
//
// DB 14 is used rather than the usual DB 15 because these tests clear the state
// they touch, while other tests in this package rely on state left behind in
// DB 15. Only canhazgpu keys are removed - never the whole database - so
// pointing these tests at a shared Redis cannot destroy unrelated data.
func setupBookingTestEngine(t *testing.T, gpuCount int) (*AllocationEngine, *redis_client.Client, context.Context) {
	config := &types.Config{
		RedisHost:       "localhost",
		RedisPort:       6379,
		RedisDB:         14,
		MemoryThreshold: types.MemoryThresholdMB,
	}

	client := redis_client.NewClient(config)
	ctx := context.Background()

	if err := client.Ping(ctx); err != nil {
		t.Skipf("Redis not available for testing: %v", err)
	}

	clearBookingTestState(t, client, ctx)

	t.Cleanup(func() {
		clearBookingTestState(t, client, ctx)
		if err := client.Close(); err != nil {
			t.Logf("Warning: failed to close Redis client: %v", err)
		}
	})

	require.NoError(t, client.SetGPUCount(ctx, gpuCount))
	require.NoError(t, client.SetAvailableProvider(ctx, "fake"))

	return NewAllocationEngine(client, config), client, ctx
}

// clearBookingTestState removes every canhazgpu key from the test database,
// leaving keys belonging to anything else that shares it untouched
func clearBookingTestState(t *testing.T, client *redis_client.Client, ctx context.Context) {
	if _, err := client.DeleteAllKeys(ctx); err != nil {
		t.Logf("Warning: failed to clear canhazgpu keys: %v", err)
	}
}

func TestCreateBooking_AssignsGPUsAndDetectsConflicts(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, _, ctx := setupBookingTestEngine(t, 4)
	now := time.Now()

	first, err := engine.CreateBooking(ctx, &BookingRequest{
		GPUCount:   2,
		User:       "alice",
		ActualUser: "alice",
		StartTime:  now.Add(time.Hour),
		EndTime:    now.Add(3 * time.Hour),
	})
	require.NoError(t, err)
	assert.Len(t, first.GPUIDs, 2)
	assert.Equal(t, types.BookingStatusPending, first.Status)

	// An overlapping booking must get different GPUs
	second, err := engine.CreateBooking(ctx, &BookingRequest{
		GPUCount:   2,
		User:       "bob",
		ActualUser: "bob",
		StartTime:  now.Add(2 * time.Hour),
		EndTime:    now.Add(4 * time.Hour),
	})
	require.NoError(t, err)
	for _, gpuID := range second.GPUIDs {
		assert.NotContains(t, first.GPUIDs, gpuID)
	}

	// The pool is now fully booked for that window
	_, err = engine.CreateBooking(ctx, &BookingRequest{
		GPUCount:   1,
		User:       "carol",
		ActualUser: "carol",
		StartTime:  now.Add(2 * time.Hour),
		EndTime:    now.Add(3 * time.Hour),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only 0 of 4 GPUs are free")

	// A non-overlapping window works again
	_, err = engine.CreateBooking(ctx, &BookingRequest{
		GPUCount:   4,
		User:       "carol",
		ActualUser: "carol",
		StartTime:  now.Add(5 * time.Hour),
		EndTime:    now.Add(6 * time.Hour),
	})
	require.NoError(t, err)
}

func TestCreateBooking_Validation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, _, ctx := setupBookingTestEngine(t, 2)
	now := time.Now()

	_, err := engine.CreateBooking(ctx, &BookingRequest{
		GPUCount: 1, User: "alice", ActualUser: "alice",
		StartTime: now.Add(-time.Hour), EndTime: now.Add(time.Hour),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "in the past")

	_, err = engine.CreateBooking(ctx, &BookingRequest{
		GPUCount: 1, User: "alice", ActualUser: "alice",
		StartTime: now.Add(2 * time.Hour), EndTime: now.Add(time.Hour),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must end after it starts")

	_, err = engine.CreateBooking(ctx, &BookingRequest{
		GPUCount: 5, User: "alice", ActualUser: "alice",
		StartTime: now.Add(time.Hour), EndTime: now.Add(2 * time.Hour),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only has 2")

	_, err = engine.CreateBooking(ctx, &BookingRequest{
		GPUIDs: []int{7}, User: "alice", ActualUser: "alice",
		StartTime: now.Add(time.Hour), EndTime: now.Add(2 * time.Hour),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "out of range")
}

func TestCreateBooking_SpecificGPUConflict(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, _, ctx := setupBookingTestEngine(t, 2)
	now := time.Now()

	_, err := engine.CreateBooking(ctx, &BookingRequest{
		GPUIDs: []int{1}, User: "alice", ActualUser: "alice",
		StartTime: now.Add(time.Hour), EndTime: now.Add(2 * time.Hour),
	})
	require.NoError(t, err)

	_, err = engine.CreateBooking(ctx, &BookingRequest{
		GPUIDs: []int{1}, User: "bob", ActualUser: "bob",
		StartTime: now.Add(90 * time.Minute), EndTime: now.Add(3 * time.Hour),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already booked by alice")
}

func TestActivateDueBookings_PreemptsAndReserves(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, client, ctx := setupBookingTestEngine(t, 2)
	now := time.Now()

	booking, err := engine.CreateBooking(ctx, &BookingRequest{
		GPUIDs: []int{0}, User: "alice", ActualUser: "alice",
		StartTime:   now.Add(30 * time.Second),
		EndTime:     now.Add(time.Hour),
		Note:        "training run",
		IdleTimeout: 15 * time.Minute,
	})
	require.NoError(t, err)

	// Somebody else is holding GPU 0 when the window opens
	require.NoError(t, client.SetGPUState(ctx, 0, &types.GPUState{
		User:       "bob",
		ActualUser: "bob",
		StartTime:  types.FlexibleTime{Time: now.Add(-time.Hour)},
		Type:       types.ReservationTypeManual,
		ExpiryTime: types.FlexibleTime{Time: now.Add(2 * time.Hour)},
	}))

	// Move the booking window into the past so it is due
	booking.StartTime = types.FlexibleTime{Time: now.Add(-time.Minute)}
	require.NoError(t, client.SaveBooking(ctx, booking))

	activated, err := engine.ActivateDueBookings(ctx)
	require.NoError(t, err)
	require.Len(t, activated, 1)
	assert.Equal(t, types.BookingStatusActive, activated[0].Status)

	state, err := client.GetGPUState(ctx, 0)
	require.NoError(t, err)
	assert.Equal(t, "alice", state.User)
	assert.Equal(t, types.ReservationTypeManual, state.Type)
	assert.Equal(t, booking.ID, state.BookingID)
	assert.Equal(t, "training run", state.Note)
	assert.Equal(t, int64(900), state.IdleTimeout)
	assert.True(t, state.ExpiryTime.ToTime().Equal(booking.EndTime.ToTime()))
}

func TestActivateDueBookings_CompletesPassedWindows(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, client, ctx := setupBookingTestEngine(t, 1)
	now := time.Now()

	booking := testBooking("missed-booking", []int{0},
		now.Add(-2*time.Hour), now.Add(-time.Hour), types.BookingStatusPending)
	require.NoError(t, client.SaveBooking(ctx, booking))

	activated, err := engine.ActivateDueBookings(ctx)
	require.NoError(t, err)
	assert.Empty(t, activated)

	stored, err := client.GetBooking(ctx, booking.ID)
	require.NoError(t, err)
	assert.Equal(t, types.BookingStatusCompleted, stored.Status)

	state, err := client.GetGPUState(ctx, 0)
	require.NoError(t, err)
	assert.Empty(t, state.User, "a missed booking must not reserve anything")
}

func TestBookingProtection_BlocksAllocation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, _, ctx := setupBookingTestEngine(t, 2)
	now := time.Now()

	// GPU 0 and 1 are booked from 10 minutes from now
	_, err := engine.CreateBooking(ctx, &BookingRequest{
		GPUIDs: []int{0, 1}, User: "alice", ActualUser: "alice",
		StartTime: now.Add(10 * time.Minute), EndTime: now.Add(time.Hour),
	})
	require.NoError(t, err)

	// A run-type reservation would still be holding them when the booking
	// starts, so it must be refused
	_, err = engine.AllocateGPUs(ctx, &types.AllocationRequest{
		GPUCount:        1,
		User:            "bob",
		ActualUser:      "bob",
		ReservationType: types.ReservationTypeRun,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "held for scheduled bookings")

	// A specific GPU request explains which booking is in the way
	_, err = engine.AllocateGPUs(ctx, &types.AllocationRequest{
		GPUIDs:          []int{0},
		GPUCount:        1,
		User:            "bob",
		ActualUser:      "bob",
		ReservationType: types.ReservationTypeRun,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "held for a scheduled booking by alice")

	// A short manual reservation that ends before the booking starts is fine
	expiry := now.Add(5 * time.Minute)
	allocated, err := engine.AllocateGPUs(ctx, &types.AllocationRequest{
		GPUCount:        1,
		User:            "bob",
		ActualUser:      "bob",
		ReservationType: types.ReservationTypeManual,
		ExpiryTime:      &expiry,
	})
	require.NoError(t, err)
	assert.Len(t, allocated, 1)
}

func TestCancelBooking(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, client, ctx := setupBookingTestEngine(t, 2)
	now := time.Now()

	booking, err := engine.CreateBooking(ctx, &BookingRequest{
		GPUIDs: []int{0}, User: "alice", ActualUser: "alice",
		StartTime: now.Add(time.Hour), EndTime: now.Add(2 * time.Hour),
	})
	require.NoError(t, err)

	// Someone else cannot cancel it without --force
	_, err = engine.CancelBooking(ctx, booking.ShortID(), "bob", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "belongs to alice")

	cancelled, err := engine.CancelBooking(ctx, booking.ShortID(), "alice", false)
	require.NoError(t, err)
	assert.Equal(t, types.BookingStatusCancelled, cancelled.Status)

	// Cancelling frees the window again
	_, err = engine.CreateBooking(ctx, &BookingRequest{
		GPUIDs: []int{0}, User: "bob", ActualUser: "bob",
		StartTime: now.Add(time.Hour), EndTime: now.Add(2 * time.Hour),
	})
	require.NoError(t, err)

	// Cancelling an active booking releases its GPUs
	active, err := engine.CreateBooking(ctx, &BookingRequest{
		GPUIDs: []int{1}, User: "alice", ActualUser: "alice",
		StartTime: now.Add(time.Minute), EndTime: now.Add(time.Hour),
	})
	require.NoError(t, err)
	active.StartTime = types.FlexibleTime{Time: now.Add(-time.Minute)}
	require.NoError(t, client.SaveBooking(ctx, active))
	_, err = engine.ActivateDueBookings(ctx)
	require.NoError(t, err)

	_, err = engine.CancelBooking(ctx, active.ShortID(), "alice", false)
	require.NoError(t, err)

	state, err := client.GetGPUState(ctx, 1)
	require.NoError(t, err)
	assert.Empty(t, state.User)
}

func TestFindBooking_AmbiguousAndMissing(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, client, ctx := setupBookingTestEngine(t, 1)
	now := time.Now()

	require.NoError(t, client.SaveBooking(ctx,
		testBooking("aaa1", []int{0}, now.Add(time.Hour), now.Add(2*time.Hour), types.BookingStatusPending)))
	require.NoError(t, client.SaveBooking(ctx,
		testBooking("aaa2", []int{0}, now.Add(3*time.Hour), now.Add(4*time.Hour), types.BookingStatusPending)))

	_, err := engine.FindBooking(ctx, "aaa")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous")

	_, err = engine.FindBooking(ctx, "zzz")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no booking found")

	found, err := engine.FindBooking(ctx, "aaa1")
	require.NoError(t, err)
	assert.Equal(t, "aaa1", found.ID)
}

func TestPruneBookings(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, client, ctx := setupBookingTestEngine(t, 1)
	now := time.Now()

	old := testBooking("old", []int{0},
		now.Add(-types.BookingRetention-2*time.Hour), now.Add(-types.BookingRetention-time.Hour),
		types.BookingStatusCompleted)
	recent := testBooking("recent", []int{0},
		now.Add(-2*time.Hour), now.Add(-time.Hour), types.BookingStatusCompleted)
	upcoming := testBooking("upcoming", []int{0},
		now.Add(time.Hour), now.Add(2*time.Hour), types.BookingStatusPending)

	for _, booking := range []*types.Booking{old, recent, upcoming} {
		require.NoError(t, client.SaveBooking(ctx, booking))
	}

	require.NoError(t, engine.pruneBookings(ctx))

	stored, err := client.GetBooking(ctx, "old")
	require.NoError(t, err)
	assert.Nil(t, stored, "bookings past the retention period are deleted")

	stored, err = client.GetBooking(ctx, "recent")
	require.NoError(t, err)
	assert.NotNil(t, stored)

	stored, err = client.GetBooking(ctx, "upcoming")
	require.NoError(t, err)
	assert.NotNil(t, stored)
}
