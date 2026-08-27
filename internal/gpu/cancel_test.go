package gpu

import (
	"context"
	"os/exec"
	"os/user"
	"testing"
	"time"

	"github.com/russellb/canhazgpu/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTaskGPUProcessPIDs(t *testing.T) {
	usage := map[int]*types.GPUUsage{
		0: {GPUID: 0, Processes: []types.GPUProcessInfo{
			{PID: 11, User: "alice", MemoryMB: 1024},
			{PID: 22, User: "bob", MemoryMB: 1024},
			{PID: 33, User: "", MemoryMB: 1024},
		}},
		1: {GPUID: 1, Processes: []types.GPUProcessInfo{
			{PID: 11, User: "alice", MemoryMB: 512},
			{PID: 44, User: "alice", MemoryMB: 512},
		}},
	}

	pids := taskGPUProcessPIDs("alice", []int{0, 1}, usage)
	assert.ElementsMatch(t, []int{11, 44}, pids,
		"only the task account's processes are targeted, deduplicated across GPUs")

	// A GPU with no reported usage is simply skipped; the others still count
	assert.ElementsMatch(t, []int{11}, taskGPUProcessPIDs("alice", []int{0, 2}, usage))
	assert.Empty(t, taskGPUProcessPIDs("", []int{0}, usage))
	assert.Empty(t, taskGPUProcessPIDs("alice", nil, usage))
	assert.Empty(t, taskGPUProcessPIDs("alice", []int{0}, nil))
}

func TestGpuProcessesForTask_UsageDetectionFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	client := setupQueueTestRedis(t)
	ctx := context.Background()
	engine := NewAllocationEngine(client, &types.Config{MemoryThreshold: types.MemoryThresholdMB})

	// No provider is configured in the fresh test DB: detection fails, and
	// cancel must fall back to the stored PID rather than guessing
	processes := engine.gpuProcessesForTask(ctx, &RunningTask{
		User: "alice", ActualUser: "alice", GPUs: []int{0},
	})
	assert.Empty(t, processes)
}

func TestCancelRunningTaskSignalsLiveProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	current, err := user.Current()
	require.NoError(t, err)

	client := setupQueueTestRedis(t)
	ctx := context.Background()
	require.NoError(t, client.SetGPUCount(ctx, 1))

	engine := NewAllocationEngine(client, &types.Config{MemoryThreshold: types.MemoryThresholdMB})

	// A live process holding the reservation, as a run job would be
	cmd := exec.Command("sleep", "60")
	require.NoError(t, cmd.Start())
	taskPID := cmd.Process.Pid
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	require.NoError(t, client.SetGPUState(ctx, 0, &types.GPUState{
		User:          current.Username,
		ActualUser:    current.Username,
		Type:          types.ReservationTypeRun,
		StartTime:     types.FlexibleTime{Time: time.Now()},
		LastHeartbeat: types.FlexibleTime{Time: time.Now()},
		TaskID:        "ca11abcd",
		PID:           taskPID,
	}))

	result, err := engine.CancelTask(ctx, "ca11abcd", current.Username, false)
	require.NoError(t, err)
	assert.Equal(t, taskPID, result.Signalled, "the job process itself must be signalled")
	assert.False(t, isProcessAlive(taskPID), "the job process must actually be gone")

	state, err := client.GetGPUState(ctx, 0)
	require.NoError(t, err)
	assert.Empty(t, state.User, "the GPU is released after cancellation")
}
