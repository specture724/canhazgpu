package gpu

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/russellb/canhazgpu/internal/types"
	"github.com/stretchr/testify/require"
)

func TestDockerHostPIDStoredForRunAndQueue(t *testing.T) {
	client := setupQueueTestRedis(t)
	ctx := context.Background()
	require.NoError(t, client.SetGPUCount(ctx, 2))
	require.NoError(t, client.SetAvailableProvider(ctx, "fake"))
	engine := NewAllocationEngine(client, &types.Config{HostPID: 7654321, MemoryThreshold: 100})
	request := &types.AllocationRequest{GPUCount: 1, User: "alice", ActualUser: "alice", ReservationType: types.ReservationTypeRun}
	ids, err := engine.AllocateGPUs(ctx, request)
	require.NoError(t, err)
	state, err := client.GetGPUState(ctx, ids[0])
	require.NoError(t, err)
	require.Equal(t, 7654321, state.PID)
	entry := engine.createQueueEntry(&QueuedAllocationRequest{AllocationRequest: request})
	require.Equal(t, 7654321, entry.PID)
}

func TestCancelRejectsPIDWhoseOwnerDoesNotMatchReservation(t *testing.T) {
	client := setupQueueTestRedis(t)
	ctx := context.Background()
	require.NoError(t, client.SetGPUCount(ctx, 1))
	engine := NewAllocationEngine(client, &types.Config{MemoryThreshold: 100})
	child := exec.Command("sleep", "60")
	require.NoError(t, child.Start())
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	reserveForTest(t, engine, 0, &types.GPUState{
		User: "not-the-process-owner", ActualUser: "not-the-process-owner", Type: types.ReservationTypeRun,
		TaskID: "bad01234", PID: child.Process.Pid, StartTime: types.FlexibleTime{Time: time.Now()},
	})
	_, err := engine.CancelTask(ctx, "bad01234", "not-the-process-owner", false)
	require.ErrorContains(t, err, "refusing to signal")
	require.True(t, isProcessAlive(child.Process.Pid))
	state, err := client.GetGPUState(ctx, 0)
	require.NoError(t, err)
	require.Equal(t, "bad01234", state.TaskID, "do not release a task we failed to stop")
}
