package gpu

import (
	"context"
	"testing"
	"time"

	"github.com/russellb/canhazgpu/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reserveForTest puts a reservation on a GPU directly, bypassing allocation
func reserveForTest(t *testing.T, engine *AllocationEngine, gpuID int, state *types.GPUState) {
	t.Helper()
	require.NoError(t, engine.client.SetGPUState(context.Background(), gpuID, state))
}

func TestRunningTasksAndCancel(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	client := setupQueueTestRedis(t)
	ctx := context.Background()
	require.NoError(t, client.SetGPUCount(ctx, 4))

	engine := NewAllocationEngine(client, &types.Config{MemoryThreshold: types.MemoryThresholdMB})
	now := time.Now()

	// One task holding two GPUs, one holding a single GPU, and a reservation
	// from before task IDs existed
	reserveForTest(t, engine, 0, &types.GPUState{
		User: "alice", ActualUser: "alice", Type: types.ReservationTypeRun,
		StartTime: types.FlexibleTime{Time: now.Add(-time.Minute)}, TaskID: "aaaa1111",
	})
	reserveForTest(t, engine, 1, &types.GPUState{
		User: "alice", ActualUser: "alice", Type: types.ReservationTypeRun,
		StartTime: types.FlexibleTime{Time: now.Add(-time.Minute)}, TaskID: "aaaa1111",
	})
	reserveForTest(t, engine, 2, &types.GPUState{
		User: "bob", ActualUser: "bob", Type: types.ReservationTypeManual,
		StartTime: types.FlexibleTime{Time: now}, TaskID: "bbbb2222",
	})
	reserveForTest(t, engine, 3, &types.GPUState{
		User: "carol", ActualUser: "carol", Type: types.ReservationTypeManual,
		StartTime: types.FlexibleTime{Time: now},
	})

	tasks, err := engine.GetRunningTasks(ctx)
	require.NoError(t, err)
	require.Len(t, tasks, 3, "GPUs of one task are grouped, untagged reservations stand alone")
	assert.Equal(t, []int{0, 1}, tasks[0].GPUs)
	assert.Equal(t, "aaaa1111", tasks[0].TaskID)
	assert.Equal(t, "", tasks[2].TaskID)

	// An abbreviated ID resolves, an unknown one does not
	task, err := engine.FindTask(ctx, "aaaa")
	require.NoError(t, err)
	require.NotNil(t, task)
	assert.Equal(t, "aaaa1111", task.TaskID)

	task, err = engine.FindTask(ctx, "cccc")
	require.NoError(t, err)
	assert.Nil(t, task)

	// Someone else's task needs --force
	_, err = engine.CancelTask(ctx, "bbbb2222", "alice", false)
	require.Error(t, err)
	state, err := client.GetGPUState(ctx, 2)
	require.NoError(t, err)
	assert.Equal(t, "bob", state.User, "a refused cancel leaves the reservation alone")

	// A manual reservation has no process to signal, so it is released here
	result, err := engine.CancelTask(ctx, "bbbb2222", "bob", false)
	require.NoError(t, err)
	assert.Equal(t, []int{2}, result.GPUs)
	assert.Zero(t, result.Signalled)
	state, err = client.GetGPUState(ctx, 2)
	require.NoError(t, err)
	assert.Empty(t, state.User, "the GPU is free again")

	// Reservations without a task ID cannot be resolved
	_, err = engine.CancelTask(ctx, "whatever", "carol", false)
	require.Error(t, err)
}

func TestCancelQueuedTask(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	client := setupQueueTestRedis(t)
	ctx := context.Background()
	require.NoError(t, client.SetGPUCount(ctx, 2))

	engine := NewAllocationEngine(client, &types.Config{MemoryThreshold: types.MemoryThresholdMB})

	entry := &types.QueueEntry{
		ID:              "12345678-0000-0000-0000-000000000000",
		User:            "alice",
		ActualUser:      "alice",
		RequestedCount:  2,
		AllocatedGPUs:   []int{},
		ReservationType: types.ReservationTypeRun,
		EnqueueTime:     types.FlexibleTime{Time: time.Now()},
		LastHeartbeat:   types.FlexibleTime{Time: time.Now()},
		// No live process: cancel still has to clear the entry
		PID: 0,
	}
	require.NoError(t, client.AddToQueue(ctx, entry))

	_, err := engine.CancelTask(ctx, "12345678", "bob", false)
	require.Error(t, err, "cancelling someone else's queue entry needs --force")

	result, err := engine.CancelTask(ctx, "12345678", "alice", false)
	require.NoError(t, err)
	assert.True(t, result.Queued)

	length, err := client.GetQueueLength(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, length)
}
