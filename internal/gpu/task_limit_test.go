package gpu

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/russellb/canhazgpu/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGuardTaskLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping Redis integration test in short mode")
	}
	guard, _, _, ctx := setupGuard(t, 8, DefaultGuardConfig())
	limit, err := guard.client.GetMaxTasksPerUser(ctx)
	require.NoError(t, err)
	assert.Zero(t, limit, "pools without guard policy remain unlimited")
	_, err = guard.RunOnce(ctx)
	require.NoError(t, err)
	limit, err = guard.client.GetMaxTasksPerUser(ctx)
	require.NoError(t, err)
	assert.Equal(t, 4, limit)

	// A separate allocation engine sees policy published by the guard.
	engine := NewAllocationEngine(guard.client, guard.config)
	for i := 0; i < 4; i++ {
		request := &types.AllocationRequest{
			GPUCount: 1, User: fmt.Sprintf("Alice job %d", i), ActualUser: "alice",
			ReservationType: types.ReservationTypeRun,
		}
		if i == 0 {
			request.GPUCount = 2
			request.ReservationType = types.ReservationTypeManual
			expiry := time.Now().Add(time.Hour)
			request.ExpiryTime = &expiry
		}
		_, err := engine.AllocateGPUs(ctx, request)
		require.NoError(t, err, "a multi-GPU reservation counts as one task")
	}
	request := &QueuedAllocationRequest{AllocationRequest: &types.AllocationRequest{
		GPUIDs: []int{7}, User: "Another display name", ActualUser: "alice",
		ReservationType: types.ReservationTypeRun, Force: true,
	}}
	_, err = engine.AllocateGPUsWithQueue(ctx, request)
	require.ErrorIs(t, err, ErrTaskLimitReached, "nonblocking and force cannot bypass the limit")

	request.ActualUser = "bob"
	result, err := engine.AllocateGPUsWithQueue(ctx, request)
	require.NoError(t, err)
	assert.Equal(t, []int{7}, result.AllocatedGPUs)

	guard.settings.MaxTasksPerUser = 0
	_, err = guard.RunOnce(ctx)
	require.NoError(t, err)
	request.ActualUser = "alice"
	request.GPUIDs = []int{6}
	request.TaskID = ""
	_, err = engine.AllocateGPUsWithQueue(ctx, request)
	require.NoError(t, err, "zero removes the existing shared limit")
}

func TestTaskLimitQueuesAndResumes(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping Redis integration test in short mode")
	}
	engine, client, ctx := setupBookingTestEngine(t, 4)
	require.NoError(t, client.SetMaxTasksPerUser(ctx, 1, ""))
	held, err := engine.AllocateGPUs(ctx, &types.AllocationRequest{
		GPUCount: 1, User: "alice", ReservationType: types.ReservationTypeRun,
	})
	require.NoError(t, err)

	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	results := make(chan *QueuedAllocationResult, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		result, err := engine.AllocateGPUsWithQueue(waitCtx, &QueuedAllocationRequest{
			AllocationRequest: &types.AllocationRequest{
				GPUCount: 2, User: "Alice training", ActualUser: "alice",
				ReservationType: types.ReservationTypeRun,
			}, Blocking: true,
		})
		if result == nil {
			result = &QueuedAllocationResult{}
		}
		result.Error = err
		results <- result
	}()
	// Wait for cleanup before the shared test database is cleared.
	defer func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(15 * time.Second):
			t.Error("queued allocation did not stop")
		}
	}()
	require.Eventually(t, func() bool {
		count, err := client.GetQueueLength(ctx)
		return err == nil && count == 1
	}, 3*time.Second, 10*time.Millisecond)
	entries, err := client.GetAllQueueEntries(ctx)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	waiting := entries[0]
	assert.Empty(t, waiting.AllocatedGPUs)

	bob := engine.createQueueEntry(&QueuedAllocationRequest{AllocationRequest: &types.AllocationRequest{
		GPUCount: 1, User: "bob", ReservationType: types.ReservationTypeRun,
	}})
	require.NoError(t, client.AddToQueue(ctx, bob))
	first, err := engine.isFirstSatisfiableInQueue(ctx, bob.ID)
	require.NoError(t, err)
	assert.True(t, first, "a capped account does not block other accounts")
	result, err := engine.tryAllocateForQueueEntry(ctx, waiting, nil)
	require.NoError(t, err)
	assert.Nil(t, result, "admission rechecks the limit under the allocation lock")
	result, err = engine.tryAllocateForQueueEntry(ctx, bob, nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NoError(t, client.RemoveFromQueue(ctx, bob.ID))

	_, err = engine.ReleaseSpecificGPUs(ctx, "alice", held)
	require.NoError(t, err)
	select {
	case result := <-results:
		require.NoError(t, result.Error)
		assert.Len(t, result.AllocatedGPUs, 2)
	case <-waitCtx.Done():
		t.Fatal("queued task did not start after a slot was released")
	}
	count, err := client.GetQueueLength(ctx)
	require.NoError(t, err)
	assert.Zero(t, count)
}

func TestTaskLimitConcurrentAdmission(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping Redis integration test in short mode")
	}
	engine, client, ctx := setupBookingTestEngine(t, 8)
	require.NoError(t, client.SetMaxTasksPerUser(ctx, 1, ""))
	start := make(chan struct{})
	results := make(chan error, 6)
	for i := 0; i < cap(results); i++ {
		entry := engine.createQueueEntry(&QueuedAllocationRequest{AllocationRequest: &types.AllocationRequest{
			GPUCount: 1, User: "alice", ReservationType: types.ReservationTypeRun,
		}})
		require.NoError(t, client.AddToQueue(ctx, entry))
		go func(i int, entry *types.QueueEntry) {
			<-start
			if i%2 == 0 {
				_, err := engine.AllocateGPUs(ctx, &types.AllocationRequest{
					GPUCount: 1, User: "alice", ReservationType: types.ReservationTypeRun,
				})
				results <- err
			} else {
				_, err := engine.tryAllocateForQueueEntry(ctx, entry, nil)
				results <- err
			}
		}(i, entry)
	}
	close(start)
	for i := 0; i < cap(results); i++ {
		err := <-results
		if err != nil {
			assert.ErrorIs(t, err, ErrTaskLimitReached)
		}
	}
	tasks, err := engine.GetRunningTasks(ctx)
	require.NoError(t, err)
	assert.Len(t, tasks, 1, "simultaneous direct and queued requests share one slot")
}

func TestTaskLimitDefersBooking(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping Redis integration test in short mode")
	}
	engine, client, ctx := setupBookingTestEngine(t, 2)
	require.NoError(t, client.SetMaxTasksPerUser(ctx, 1, ""))
	held, err := engine.AllocateGPUs(ctx, &types.AllocationRequest{
		GPUIDs: []int{0}, User: "alice", ReservationType: types.ReservationTypeRun,
	})
	require.NoError(t, err)
	now := time.Now()
	booking := testBooking("limited-booking", []int{1}, now.Add(-time.Minute), now.Add(time.Hour), types.BookingStatusPending)
	require.NoError(t, client.SaveBooking(ctx, booking))
	activated, err := engine.ActivateDueBookings(ctx)
	require.NoError(t, err)
	assert.Empty(t, activated)
	stored, err := client.GetBooking(ctx, booking.ID)
	require.NoError(t, err)
	assert.Equal(t, types.BookingStatusPending, stored.Status)
	state, err := client.GetGPUState(ctx, 1)
	require.NoError(t, err)
	assert.Empty(t, state.User)

	_, err = engine.ReleaseSpecificGPUs(ctx, "alice", held)
	require.NoError(t, err)
	activated, err = engine.ActivateDueBookings(ctx)
	require.NoError(t, err)
	assert.Len(t, activated, 1)
}

func TestGuardTaskLimitHours(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping Redis integration test in short mode")
	}
	now := time.Now()
	start, end := now.Add(-time.Hour).Format("15:04"), now.Add(time.Hour).Format("15:04")
	settings := DefaultGuardConfig()
	settings.MaxTasksPerUser = 1
	settings.TaskLimitHours = start + "-" + end
	guard, _, _, ctx := setupGuard(t, 3, settings)
	_, err := guard.RunOnce(ctx)
	require.NoError(t, err)
	engine := NewAllocationEngine(guard.client, guard.config)
	request := &QueuedAllocationRequest{AllocationRequest: &types.AllocationRequest{
		GPUCount: 1, User: "alice", ReservationType: types.ReservationTypeRun,
	}}
	_, err = engine.AllocateGPUsWithQueue(ctx, request)
	require.NoError(t, err)
	_, err = engine.AllocateGPUsWithQueue(ctx, request)
	require.ErrorIs(t, err, ErrTaskLimitReached)
	entry := engine.createQueueEntry(request)
	require.NoError(t, guard.client.AddToQueue(ctx, entry))
	first, err := engine.isFirstSatisfiableInQueue(ctx, entry.ID)
	require.NoError(t, err)
	assert.False(t, first)

	guard.settings.TaskLimitHours = end + "-" + start
	_, err = guard.RunOnce(ctx)
	require.NoError(t, err)
	first, err = engine.isFirstSatisfiableInQueue(ctx, entry.ID)
	require.NoError(t, err)
	assert.True(t, first, "queued tasks become eligible outside the window")
	result, err := engine.tryAllocateForQueueEntry(ctx, entry, request)
	require.NoError(t, err)
	require.NotNil(t, result, "outside the window a task can start even when its account is at the limit")

	guard.settings.TaskLimitHours = start + "-" + end
	_, err = guard.RunOnce(ctx)
	require.NoError(t, err)
	tasks, err := engine.GetRunningTasks(ctx)
	require.NoError(t, err)
	assert.Len(t, tasks, 2, "entering the window leaves existing tasks running")
	_, err = engine.AllocateGPUsWithQueue(ctx, request)
	require.ErrorIs(t, err, ErrTaskLimitReached)
}
