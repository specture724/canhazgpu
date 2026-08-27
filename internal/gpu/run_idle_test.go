package gpu

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/russellb/canhazgpu/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunIdleMonitor_NoTimeoutNeverReleases(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, client, ctx := setupBookingTestEngine(t, 1)
	require.NoError(t, client.SetGPUState(ctx, 0, &types.GPUState{
		User:          "alice",
		ActualUser:    "alice",
		StartTime:     types.FlexibleTime{Time: time.Now().Add(-time.Hour)},
		Type:          types.ReservationTypeRun,
		LastHeartbeat: types.FlexibleTime{Time: time.Now()},
	}))

	monitor := NewRunIdleMonitor(engine, []int{0}, "alice", 0)
	idleFor, release := monitor.Check(ctx)
	assert.False(t, release)
	assert.Zero(t, idleFor)
}

func TestRunIdleMonitor_ReleasesWhenIdlePastTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, client, ctx := setupBookingTestEngine(t, 1)
	require.NoError(t, client.SetGPUState(ctx, 0, &types.GPUState{
		User:          "alice",
		ActualUser:    "alice",
		StartTime:     types.FlexibleTime{Time: time.Now().Add(-time.Hour)},
		Type:          types.ReservationTypeRun,
		LastHeartbeat: types.FlexibleTime{Time: time.Now()},
		IdleTimeout:   int64((15 * time.Minute).Seconds()),
		LastActivity:  types.FlexibleTime{Time: time.Now().Add(-20 * time.Minute)},
	}))

	monitor := NewRunIdleMonitor(engine, []int{0}, "alice", 15*time.Minute)
	monitor.lastActivity = time.Now().Add(-20 * time.Minute)

	idleFor, release := monitor.Check(ctx)
	assert.True(t, release, "a reservation idle past its timeout must be released")
	assert.InDelta(t, (20 * time.Minute).Seconds(), idleFor.Seconds(), 5)
}

func TestRunIdleMonitor_HoldersUsageResetsClock(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, client, ctx := setupBookingTestEngine(t, 1)
	require.NoError(t, client.SetGPUState(ctx, 0, &types.GPUState{
		User:          "alice",
		ActualUser:    "alice",
		StartTime:     types.FlexibleTime{Time: time.Now().Add(-time.Hour)},
		Type:          types.ReservationTypeRun,
		LastHeartbeat: types.FlexibleTime{Time: time.Now()},
		IdleTimeout:   int64((15 * time.Minute).Seconds()),
	}))

	monitor := NewRunIdleMonitor(engine, []int{0}, "alice", 15*time.Minute)
	monitor.lastActivity = time.Now().Add(-20 * time.Minute)
	monitor.detectUsage = func(context.Context) (map[int]*types.GPUUsage, error) {
		return map[int]*types.GPUUsage{
			0: {GPUID: 0, MemoryMB: 1024, Processes: []types.GPUProcessInfo{
				{PID: 42, User: "alice", MemoryMB: 1024},
			}},
		}, nil
	}

	idleFor, release := monitor.Check(ctx)
	assert.False(t, release, "the holder's own usage keeps the reservation alive")
	assert.Zero(t, idleFor)
	assert.WithinDuration(t, time.Now(), monitor.lastActivity, 2*time.Second,
		"activity timestamp should have been refreshed")
}

func TestRunIdleMonitor_SomebodyElsesUsageDoesNotResetClock(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, client, ctx := setupBookingTestEngine(t, 1)
	require.NoError(t, client.SetGPUState(ctx, 0, &types.GPUState{
		User:          "alice",
		ActualUser:    "alice",
		StartTime:     types.FlexibleTime{Time: time.Now().Add(-time.Hour)},
		Type:          types.ReservationTypeRun,
		LastHeartbeat: types.FlexibleTime{Time: time.Now()},
		IdleTimeout:   int64((15 * time.Minute).Seconds()),
	}))

	monitor := NewRunIdleMonitor(engine, []int{0}, "alice", 15*time.Minute)
	monitor.lastActivity = time.Now().Add(-20 * time.Minute)
	monitor.detectUsage = func(context.Context) (map[int]*types.GPUUsage, error) {
		return map[int]*types.GPUUsage{
			0: {GPUID: 0, MemoryMB: 1024, Processes: []types.GPUProcessInfo{
				{PID: 42, User: "bob", MemoryMB: 1024},
			}},
		}, nil
	}

	_, release := monitor.Check(ctx)
	assert.True(t, release, "a squatter must not keep the holder's reservation alive")
}

func TestRunIdleMonitor_UsageDetectionFailureGivesBenefitOfDoubt(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, client, ctx := setupBookingTestEngine(t, 1)
	require.NoError(t, client.SetGPUState(ctx, 0, &types.GPUState{
		User:          "alice",
		ActualUser:    "alice",
		StartTime:     types.FlexibleTime{Time: time.Now().Add(-time.Hour)},
		Type:          types.ReservationTypeRun,
		LastHeartbeat: types.FlexibleTime{Time: time.Now()},
		IdleTimeout:   int64((15 * time.Minute).Seconds()),
	}))

	monitor := NewRunIdleMonitor(engine, []int{0}, "alice", 15*time.Minute)
	monitor.lastActivity = time.Now().Add(-time.Hour)
	monitor.detectUsage = func(context.Context) (map[int]*types.GPUUsage, error) {
		return nil, errors.New("provider unavailable")
	}

	_, release := monitor.Check(ctx)
	assert.False(t, release, "an unverifiable reservation must never be released")
}

func TestRunIdleMonitor_ReleasesWhenReservationAlreadyEnded(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, client, ctx := setupBookingTestEngine(t, 1)
	// The GPU is no longer reserved by our user (maintenance or a release
	// already happened): the supervisor has nothing left to keep alive.
	require.NoError(t, client.SetGPUState(ctx, 0, &types.GPUState{}))

	monitor := NewRunIdleMonitor(engine, []int{0}, "alice", 15*time.Minute)
	_, release := monitor.Check(ctx)
	assert.True(t, release)
}

func TestGetGPUStatus_ReportsIdleTimeForRunReservation(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	engine, client, ctx := setupBookingTestEngine(t, 1)
	now := time.Now()

	require.NoError(t, client.SetGPUState(ctx, 0, &types.GPUState{
		User:          "alice",
		ActualUser:    "alice",
		StartTime:     types.FlexibleTime{Time: now.Add(-time.Hour)},
		Type:          types.ReservationTypeRun,
		LastHeartbeat: types.FlexibleTime{Time: now},
		IdleTimeout:   int64((time.Hour).Seconds()),
		LastActivity:  types.FlexibleTime{Time: now.Add(-10 * time.Minute)},
	}))

	status := engine.buildGPUStatus(0, mustGetState(t, client, ctx, 0), &types.GPUUsage{
		GPUID: 0, MemoryMB: 0,
	})

	assert.Equal(t, "IN_USE", status.Status)
	assert.Equal(t, time.Hour, status.IdleTimeout)
	assert.InDelta(t, (10 * time.Minute).Seconds(), status.IdleFor.Seconds(), 5,
		"run reservations with an idle timeout show the idle countdown too")
}
