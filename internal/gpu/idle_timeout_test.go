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

func TestGPUState_IdleSince(t *testing.T) {
	start := time.Date(2026, 8, 3, 12, 0, 0, 0, time.Local)
	activity := start.Add(30 * time.Minute)

	// Without observed activity, idle time is measured from the reservation
	// start so the owner gets the full grace period to start working
	state := &types.GPUState{StartTime: types.FlexibleTime{Time: start}}
	assert.Equal(t, start, state.IdleSince())

	state.LastActivity = types.FlexibleTime{Time: activity}
	assert.Equal(t, activity, state.IdleSince())

	assert.Equal(t, 15*time.Minute, (&types.GPUState{IdleTimeout: 900}).IdleTimeoutDuration())
	assert.Equal(t, time.Duration(0), (&types.GPUState{}).IdleTimeoutDuration())
}

func TestCleanupReservations_ReleasesIdleManualReservation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, client, ctx := setupBookingTestEngine(t, 3)
	now := time.Now()

	// GPU 0: manual reservation, idle for longer than its timeout
	require.NoError(t, client.SetGPUState(ctx, 0, &types.GPUState{
		User:         "alice",
		ActualUser:   "alice",
		StartTime:    types.FlexibleTime{Time: now.Add(-time.Hour)},
		Type:         types.ReservationTypeManual,
		ExpiryTime:   types.FlexibleTime{Time: now.Add(time.Hour)},
		IdleTimeout:  int64((15 * time.Minute).Seconds()),
		LastActivity: types.FlexibleTime{Time: now.Add(-20 * time.Minute)},
	}))

	// GPU 1: same, but used recently
	require.NoError(t, client.SetGPUState(ctx, 1, &types.GPUState{
		User:         "bob",
		ActualUser:   "bob",
		StartTime:    types.FlexibleTime{Time: now.Add(-time.Hour)},
		Type:         types.ReservationTypeManual,
		ExpiryTime:   types.FlexibleTime{Time: now.Add(time.Hour)},
		IdleTimeout:  int64((15 * time.Minute).Seconds()),
		LastActivity: types.FlexibleTime{Time: now.Add(-time.Minute)},
	}))

	// GPU 2: run-type reservation without an idle timeout. The process decides
	// when it is done, so it must be left alone even if it never touches the
	// GPU.
	require.NoError(t, client.SetGPUState(ctx, 2, &types.GPUState{
		User:          "carol",
		ActualUser:    "carol",
		StartTime:     types.FlexibleTime{Time: now.Add(-time.Hour)},
		Type:          types.ReservationTypeRun,
		LastHeartbeat: types.FlexibleTime{Time: now},
	}))

	// Empty usage map: no GPU is in use, but usage detection did succeed
	require.NoError(t, engine.cleanupReservations(ctx, map[int]*types.GPUUsage{}))

	state, err := client.GetGPUState(ctx, 0)
	require.NoError(t, err)
	assert.Empty(t, state.User, "idle manual reservation should be released")
	assert.False(t, state.LastReleased.ToTime().IsZero())

	state, err = client.GetGPUState(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, "bob", state.User, "recently used reservation should be kept")

	state, err = client.GetGPUState(ctx, 2)
	require.NoError(t, err)
	assert.Equal(t, "carol", state.User, "run-type reservations without an idle timeout are exempt from idle release")
}

func TestCleanupReservations_ReleasesIdleRunReservation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, client, ctx := setupBookingTestEngine(t, 1)
	now := time.Now()

	// A run reservation whose GPU processes died: the wrapper shell is still
	// alive (fresh heartbeat) but nothing has touched the GPU for longer than
	// the idle timeout. This is exactly the vLLM-zombie scenario that used to
	// hold GPUs forever.
	require.NoError(t, client.SetGPUState(ctx, 0, &types.GPUState{
		User:          "alice",
		ActualUser:    "alice",
		StartTime:     types.FlexibleTime{Time: now.Add(-time.Hour)},
		Type:          types.ReservationTypeRun,
		LastHeartbeat: types.FlexibleTime{Time: now},
		IdleTimeout:   int64((15 * time.Minute).Seconds()),
		LastActivity:  types.FlexibleTime{Time: now.Add(-20 * time.Minute)},
	}))

	require.NoError(t, engine.cleanupReservations(ctx, map[int]*types.GPUUsage{}))

	state, err := client.GetGPUState(ctx, 0)
	require.NoError(t, err)
	assert.Empty(t, state.User, "idle run reservation should be released")
}

func TestCleanupReservations_SkipsIdleChecksWithoutUsageData(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, client, ctx := setupBookingTestEngine(t, 1)
	now := time.Now()

	require.NoError(t, client.SetGPUState(ctx, 0, &types.GPUState{
		User:         "alice",
		ActualUser:   "alice",
		StartTime:    types.FlexibleTime{Time: now.Add(-time.Hour)},
		Type:         types.ReservationTypeManual,
		ExpiryTime:   types.FlexibleTime{Time: now.Add(time.Hour)},
		IdleTimeout:  int64((15 * time.Minute).Seconds()),
		LastActivity: types.FlexibleTime{Time: now.Add(-time.Hour)},
	}))

	// nil usage means the GPU provider could not be queried: without knowing
	// whether the GPU is busy, the reservation must be left alone
	require.NoError(t, engine.cleanupReservations(ctx, nil))

	state, err := client.GetGPUState(ctx, 0)
	require.NoError(t, err)
	assert.Equal(t, "alice", state.User)
}

func TestCleanupReservations_BusyGPURefreshesActivity(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, client, ctx := setupBookingTestEngine(t, 1)
	now := time.Now()

	// Idle for longer than the timeout according to the stored timestamp, but
	// the GPU is in fact busy right now
	require.NoError(t, client.SetGPUState(ctx, 0, &types.GPUState{
		User:         "alice",
		ActualUser:   "alice",
		StartTime:    types.FlexibleTime{Time: now.Add(-time.Hour)},
		Type:         types.ReservationTypeManual,
		ExpiryTime:   types.FlexibleTime{Time: now.Add(time.Hour)},
		IdleTimeout:  int64((15 * time.Minute).Seconds()),
		LastActivity: types.FlexibleTime{Time: now.Add(-time.Hour)},
	}))

	usage := map[int]*types.GPUUsage{
		0: {GPUID: 0, MemoryMB: types.MemoryThresholdMB + 1024},
	}
	require.NoError(t, engine.cleanupReservations(ctx, usage))

	state, err := client.GetGPUState(ctx, 0)
	require.NoError(t, err)
	assert.Equal(t, "alice", state.User, "a busy reservation must never be released as idle")
	assert.WithinDuration(t, time.Now(), state.LastActivity.ToTime(), 5*time.Second,
		"activity timestamp should have been refreshed")
}

func TestCleanupReservations_SomebodyElsesUsageDoesNotKeepReservationAlive(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, client, ctx := setupBookingTestEngine(t, 2)
	now := time.Now()

	// Both GPUs are reserved by alice and busy, but only GPU 1 is busy with
	// alice's own work
	for _, gpuID := range []int{0, 1} {
		require.NoError(t, client.SetGPUState(ctx, gpuID, &types.GPUState{
			User:         "Alice Smith",
			ActualUser:   "alice",
			StartTime:    types.FlexibleTime{Time: now.Add(-time.Hour)},
			Type:         types.ReservationTypeManual,
			ExpiryTime:   types.FlexibleTime{Time: now.Add(time.Hour)},
			IdleTimeout:  int64((15 * time.Minute).Seconds()),
			LastActivity: types.FlexibleTime{Time: now.Add(-30 * time.Minute)},
		}))
	}

	usage := map[int]*types.GPUUsage{
		// GPU 0: bob squats on alice's reservation
		0: {GPUID: 0, MemoryMB: 8452, Processes: []types.GPUProcessInfo{
			{PID: 111, User: "bob", MemoryMB: 8452},
		}},
		// GPU 1: alice is actually working
		1: {GPUID: 1, MemoryMB: 8452, Processes: []types.GPUProcessInfo{
			{PID: 222, User: "alice", MemoryMB: 8452},
		}},
	}

	require.NoError(t, engine.cleanupReservations(ctx, usage))

	state, err := client.GetGPUState(ctx, 0)
	require.NoError(t, err)
	assert.Empty(t, state.User,
		"memory burned by somebody else must not keep the reservation alive")

	state, err = client.GetGPUState(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, "Alice Smith", state.User, "the holder's own work keeps the reservation")
	assert.WithinDuration(t, time.Now(), state.LastActivity.ToTime(), 5*time.Second)
}

func TestGetGPUStatus_ReportsForeignUsageAndIdleTime(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, client, ctx := setupBookingTestEngine(t, 1)
	now := time.Now()

	require.NoError(t, client.SetGPUState(ctx, 0, &types.GPUState{
		User:         "Alice Smith",
		ActualUser:   "alice",
		StartTime:    types.FlexibleTime{Time: now.Add(-time.Hour)},
		Type:         types.ReservationTypeManual,
		ExpiryTime:   types.FlexibleTime{Time: now.Add(time.Hour)},
		IdleTimeout:  int64((time.Hour).Seconds()),
		LastActivity: types.FlexibleTime{Time: now.Add(-10 * time.Minute)},
	}))

	usage := &types.GPUUsage{GPUID: 0, MemoryMB: 8452, Processes: []types.GPUProcessInfo{
		{PID: 111, User: "bob", ProcessName: "python", MemoryMB: 8452},
	}}

	status := engine.buildGPUStatus(0, mustGetState(t, client, ctx, 0), usage)

	assert.Equal(t, "IN_USE", status.Status, "the reservation itself is still valid")
	assert.True(t, status.HasForeignUsage())
	assert.Equal(t, []string{"bob"}, status.ForeignUsers)
	assert.Equal(t, 1, status.ForeignProcesses)
	assert.Equal(t, 8452, status.ForeignMemoryMB)
	assert.InDelta(t, (10 * time.Minute).Seconds(), status.IdleFor.Seconds(), 5,
		"the holder is idle even though the GPU is busy")
}

func mustGetState(t *testing.T, client *redis_client.Client, ctx context.Context, gpuID int) *types.GPUState {
	t.Helper()

	state, err := client.GetGPUState(ctx, gpuID)
	require.NoError(t, err)
	return state
}

func TestCleanupReservations_IdleReleaseCompletesBooking(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, client, ctx := setupBookingTestEngine(t, 1)
	now := time.Now()

	booking := testBooking("booking-idle", []int{0},
		now.Add(-30*time.Minute), now.Add(time.Hour), types.BookingStatusActive)
	require.NoError(t, client.SaveBooking(ctx, booking))

	require.NoError(t, client.SetGPUState(ctx, 0, &types.GPUState{
		User:         booking.User,
		ActualUser:   booking.User,
		StartTime:    types.FlexibleTime{Time: now.Add(-30 * time.Minute)},
		Type:         types.ReservationTypeManual,
		ExpiryTime:   booking.EndTime,
		BookingID:    booking.ID,
		IdleTimeout:  int64((15 * time.Minute).Seconds()),
		LastActivity: types.FlexibleTime{Time: now.Add(-30 * time.Minute)},
	}))

	require.NoError(t, engine.cleanupReservations(ctx, map[int]*types.GPUUsage{}))

	state, err := client.GetGPUState(ctx, 0)
	require.NoError(t, err)
	assert.Empty(t, state.User)

	stored, err := client.GetBooking(ctx, booking.ID)
	require.NoError(t, err)
	assert.Equal(t, types.BookingStatusCompleted, stored.Status,
		"releasing the reservation ends the booking that created it")
}

func TestCleanupReservations_ExpiredAndStaleStillReleased(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, client, ctx := setupBookingTestEngine(t, 2)
	now := time.Now()

	require.NoError(t, client.SetGPUState(ctx, 0, &types.GPUState{
		User:       "alice",
		StartTime:  types.FlexibleTime{Time: now.Add(-2 * time.Hour)},
		Type:       types.ReservationTypeManual,
		ExpiryTime: types.FlexibleTime{Time: now.Add(-time.Minute)},
	}))

	require.NoError(t, client.SetGPUState(ctx, 1, &types.GPUState{
		User:          "bob",
		StartTime:     types.FlexibleTime{Time: now.Add(-2 * time.Hour)},
		Type:          types.ReservationTypeRun,
		LastHeartbeat: types.FlexibleTime{Time: now.Add(-types.HeartbeatTimeout - time.Minute)},
	}))

	require.NoError(t, engine.cleanupReservations(ctx, nil))

	for _, gpuID := range []int{0, 1} {
		state, err := client.GetGPUState(ctx, gpuID)
		require.NoError(t, err)
		assert.Empty(t, state.User, "GPU %d should have been released", gpuID)
	}
}

func TestAllocateGPUs_ManualReservationStoresIdleTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, client, ctx := setupBookingTestEngine(t, 2)
	expiry := time.Now().Add(time.Hour)

	allocated, err := engine.AllocateGPUs(ctx, &types.AllocationRequest{
		GPUCount:        1,
		User:            "alice",
		ActualUser:      "alice",
		ReservationType: types.ReservationTypeManual,
		ExpiryTime:      &expiry,
		IdleTimeout:     15 * time.Minute,
	})
	require.NoError(t, err)
	require.Len(t, allocated, 1)

	state, err := client.GetGPUState(ctx, allocated[0])
	require.NoError(t, err)
	assert.Equal(t, int64(900), state.IdleTimeout)
	assert.False(t, state.LastActivity.ToTime().IsZero(),
		"the idle clock starts when the reservation is created")

	// Run-type reservations store the idle timeout too: the supervisor uses it
	// to release a job whose GPU process died while the wrapper lives on
	runAllocated, err := engine.AllocateGPUs(ctx, &types.AllocationRequest{
		GPUCount:        1,
		User:            "bob",
		ActualUser:      "bob",
		ReservationType: types.ReservationTypeRun,
		IdleTimeout:     15 * time.Minute,
	})
	require.NoError(t, err)
	require.Len(t, runAllocated, 1)

	state, err = client.GetGPUState(ctx, runAllocated[0])
	require.NoError(t, err)
	assert.Equal(t, int64(900), state.IdleTimeout)
	assert.False(t, state.LastActivity.ToTime().IsZero(),
		"the idle clock starts when the run reservation is created")
}

func TestAllocateSpecificGPUs_StoresIdleTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, client, ctx := setupBookingTestEngine(t, 2)
	expiry := time.Now().Add(time.Hour)

	allocated, err := engine.AllocateGPUs(ctx, &types.AllocationRequest{
		GPUIDs:          []int{1},
		GPUCount:        1,
		User:            "alice",
		ActualUser:      "alice",
		ReservationType: types.ReservationTypeManual,
		ExpiryTime:      &expiry,
		IdleTimeout:     30 * time.Minute,
	})
	require.NoError(t, err)
	assert.Equal(t, []int{1}, allocated)

	state, err := client.GetGPUState(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, int64(1800), state.IdleTimeout)
	assert.False(t, state.LastActivity.ToTime().IsZero())
}
