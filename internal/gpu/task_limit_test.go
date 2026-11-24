package gpu

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/russellb/canhazgpu/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGuardTaskAndGPULimits(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping Redis integration test in short mode")
	}
	guard, _, _, ctx := setupGuard(t, 12, DefaultGuardConfig())
	limits, err := guard.client.GetTaskLimits(ctx)
	require.NoError(t, err)
	assert.Zero(t, limits, "pools without guard policy remain unlimited")

	_, err = guard.RunOnce(ctx)
	require.NoError(t, err)
	limits, err = guard.client.GetTaskLimits(ctx)
	require.NoError(t, err)
	assert.Equal(t, types.TaskLimits{MaxTasksPerUser: 4, MaxGPUsPerUser: 8}, limits)

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

	// 5th task is rejected immediately even with Blocking=true and Force=true.
	request := &QueuedAllocationRequest{
		AllocationRequest: &types.AllocationRequest{
			GPUCount: 1, User: "Another display name", ActualUser: "alice",
			ReservationType: types.ReservationTypeRun, Force: true,
		},
		Blocking: true,
	}
	_, err = engine.AllocateGPUsWithQueue(ctx, request)
	require.ErrorIs(t, err, ErrTaskLimitReached, "blocking and force cannot bypass the task limit")
	queueLen, err := guard.client.GetQueueLength(ctx)
	require.NoError(t, err)
	assert.Zero(t, queueLen, "rejected requests must not enter the queue")

	// Other accounts are unaffected.
	request.ActualUser = "bob"
	request.Blocking = false
	result, err := engine.AllocateGPUsWithQueue(ctx, request)
	require.NoError(t, err)
	assert.Len(t, result.AllocatedGPUs, 1)

	// Disable only the task count limit; Alice currently holds 5 GPUs, so 3 more fit (total 8) and +1 fails.
	guard.settings.MaxTasksPerUser = 0
	_, err = guard.RunOnce(ctx)
	require.NoError(t, err)

	_, err = engine.AllocateGPUsWithQueue(ctx, &QueuedAllocationRequest{
		AllocationRequest: &types.AllocationRequest{
			GPUCount: 3, User: "Alice 5th task", ActualUser: "alice",
			ReservationType: types.ReservationTypeRun,
		},
		Blocking: true,
	})
	require.NoError(t, err, "8 total GPUs is within the default GPU limit")

	_, err = engine.AllocateGPUsWithQueue(ctx, &QueuedAllocationRequest{
		AllocationRequest: &types.AllocationRequest{
			GPUCount: 1, User: "Alice 9th GPU", ActualUser: "alice",
			ReservationType: types.ReservationTypeRun,
		},
		Blocking: true,
	})
	require.ErrorIs(t, err, ErrGPULimitReached, "exceeding 8 GPUs is rejected immediately even when blocking")
	queueLen, err = guard.client.GetQueueLength(ctx)
	require.NoError(t, err)
	assert.Zero(t, queueLen, "GPU-limit-rejected requests must not enter the queue")

	// Disabling both limits restores unlimited admission.
	guard.settings.MaxGPUsPerUser = 0
	_, err = guard.RunOnce(ctx)
	require.NoError(t, err)
	_, err = engine.AllocateGPUsWithQueue(ctx, &QueuedAllocationRequest{
		AllocationRequest: &types.AllocationRequest{
			GPUCount: 1, User: "Alice unlimited", ActualUser: "alice",
			ReservationType: types.ReservationTypeRun,
		},
		Blocking: false,
	})
	require.NoError(t, err, "zero removes both shared limits")
}

func TestSubmissionTaskLimitCountsRunningAndQueued(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping Redis integration test in short mode")
	}
	engine, client, ctx := setupBookingTestEngine(t, 4)
	require.NoError(t, client.SetTaskLimits(ctx, 4, 8, ""))

	// Alice holds 2 running tasks (2 GPUs); Bob holds the other 2 GPUs so further requests queue.
	heldAlice1, err := engine.AllocateGPUs(ctx, &types.AllocationRequest{
		GPUCount: 1, User: "alice-1", ActualUser: "alice", ReservationType: types.ReservationTypeRun,
	})
	require.NoError(t, err)
	_, err = engine.AllocateGPUs(ctx, &types.AllocationRequest{
		GPUCount: 1, User: "alice-2", ActualUser: "alice", ReservationType: types.ReservationTypeRun,
	})
	require.NoError(t, err)
	_, err = engine.AllocateGPUs(ctx, &types.AllocationRequest{
		GPUCount: 2, User: "bob", ActualUser: "bob", ReservationType: types.ReservationTypeRun,
	})
	require.NoError(t, err)

	// Alice submits 2 blocking requests -> both enter the queue (2 running + 2 queued = 4).
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, _ = engine.AllocateGPUsWithQueue(waitCtx, &QueuedAllocationRequest{
				AllocationRequest: &types.AllocationRequest{
					GPUCount: 1, User: fmt.Sprintf("alice-q-%d", idx), ActualUser: "alice",
					ReservationType: types.ReservationTypeRun,
				},
				Blocking: true,
			})
		}(i)
	}

	require.Eventually(t, func() bool {
		count, err := client.GetQueueLength(ctx)
		return err == nil && count == 2
	}, 3*time.Second, 10*time.Millisecond)

	// 5th submission (blocking or non-blocking, different display name) must be rejected immediately.
	_, err = engine.AllocateGPUsWithQueue(ctx, &QueuedAllocationRequest{
		AllocationRequest: &types.AllocationRequest{
			GPUCount: 1, User: "alias-user", ActualUser: "alice",
			ReservationType: types.ReservationTypeRun,
		},
		Blocking: true,
	})
	require.ErrorIs(t, err, ErrTaskLimitReached)
	count, err := client.GetQueueLength(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, count, "rejected 5th task must not be added to queue")

	// Even if a GPU frees up, Alice still has 1 running + 2 queued = 3 tasks once 1 running task finishes,
	// allowing one queued task to start and freeing a submission slot.
	_, err = engine.ReleaseSpecificGPUs(ctx, "alice-1", heldAlice1)
	require.NoError(t, err)

	// Wait until one of the queued tasks starts (leaving 2 running + 1 queued = 3 submitted).
	require.Eventually(t, func() bool {
		qLen, err := client.GetQueueLength(ctx)
		return err == nil && qLen == 1
	}, 5*time.Second, 50*time.Millisecond)

	// Now Alice has 3 submitted tasks (2 running + 1 queued), so a 4th submission is allowed to queue.
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = engine.AllocateGPUsWithQueue(waitCtx, &QueuedAllocationRequest{
			AllocationRequest: &types.AllocationRequest{
				GPUCount: 1, User: "alice-4th", ActualUser: "alice",
				ReservationType: types.ReservationTypeRun,
			},
			Blocking: true,
		})
	}()
	require.Eventually(t, func() bool {
		qLen, err := client.GetQueueLength(ctx)
		return err == nil && qLen == 2
	}, 3*time.Second, 10*time.Millisecond)
}

func TestSubmissionGPULimitCountsRunningAndQueued(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping Redis integration test in short mode")
	}
	engine, client, ctx := setupBookingTestEngine(t, 8)
	require.NoError(t, client.SetTaskLimits(ctx, 4, 8, ""))

	// Single request exceeding 8 GPUs is rejected immediately even with Blocking=true.
	_, err := engine.AllocateGPUsWithQueue(ctx, &QueuedAllocationRequest{
		AllocationRequest: &types.AllocationRequest{
			GPUCount: 9, User: "alice", ActualUser: "alice", ReservationType: types.ReservationTypeRun,
		},
		Blocking: true,
	})
	require.ErrorIs(t, err, ErrGPULimitReached)
	qLen, err := client.GetQueueLength(ctx)
	require.NoError(t, err)
	assert.Zero(t, qLen)

	// Alice holds 1 running task with 4 GPUs; Bob holds the other 4 GPUs.
	_, err = engine.AllocateGPUs(ctx, &types.AllocationRequest{
		GPUCount: 4, User: "alice-run", ActualUser: "alice", ReservationType: types.ReservationTypeRun,
	})
	require.NoError(t, err)
	bobGPUs, err := engine.AllocateGPUs(ctx, &types.AllocationRequest{
		GPUCount: 4, User: "bob", ActualUser: "bob", ReservationType: types.ReservationTypeRun,
	})
	require.NoError(t, err)

	// Alice submits a 4-GPU blocking task -> enters queue (total: 2 tasks, 8 GPUs).
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	results := make(chan *QueuedAllocationResult, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		res, err := engine.AllocateGPUsWithQueue(waitCtx, &QueuedAllocationRequest{
			AllocationRequest: &types.AllocationRequest{
				GPUCount: 4, User: "alice-q", ActualUser: "alice", ReservationType: types.ReservationTypeRun,
			},
			Blocking: true,
		})
		if res == nil {
			res = &QueuedAllocationResult{}
		}
		res.Error = err
		results <- res
	}()
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

	// A 3rd task for 1 GPU exceeds 8 total GPUs (4 running + 4 queued + 1 = 9) and is rejected immediately.
	_, err = engine.AllocateGPUsWithQueue(ctx, &QueuedAllocationRequest{
		AllocationRequest: &types.AllocationRequest{
			GPUCount: 1, User: "alice-extra", ActualUser: "alice", ReservationType: types.ReservationTypeRun,
		},
		Blocking: true,
	})
	require.ErrorIs(t, err, ErrGPULimitReached)

	// Specific GPUIDs also count by len(GPUIDs) even when GPUCount is default 1.
	_, err = engine.AllocateGPUsWithQueue(ctx, &QueuedAllocationRequest{
		AllocationRequest: &types.AllocationRequest{
			GPUCount: 1, GPUIDs: []int{0, 1, 2}, User: "alice-ids", ActualUser: "alice",
			ReservationType: types.ReservationTypeRun,
		},
		Blocking: true,
	})
	require.ErrorIs(t, err, ErrGPULimitReached)

	qLen, err = client.GetQueueLength(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, qLen)

	// Once Bob releases his 4 GPUs, Alice's queued 4-GPU task starts (total running becomes 8 GPUs, no double-count).
	_, err = engine.ReleaseSpecificGPUs(ctx, "bob", bobGPUs)
	require.NoError(t, err)

	select {
	case res := <-results:
		require.NoError(t, res.Error)
		assert.Len(t, res.AllocatedGPUs, 4)
	case <-waitCtx.Done():
		t.Fatal("queued 4-GPU task did not start when GPUs became free")
	}
}

func TestSubmissionLimitsConcurrentSubmissions(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping Redis integration test in short mode")
	}

	t.Run("task count cap under concurrent queueing", func(t *testing.T) {
		engine, client, ctx := setupBookingTestEngine(t, 2)
		require.NoError(t, client.SetTaskLimits(ctx, 4, 8, ""))

		// Occupy all GPUs with Bob so every Alice submission must queue.
		_, err := engine.AllocateGPUs(ctx, &types.AllocationRequest{
			GPUCount: 2, User: "bob", ReservationType: types.ReservationTypeRun,
		})
		require.NoError(t, err)

		waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		start := make(chan struct{})
		rejected := make(chan error, 8)
		var wg sync.WaitGroup
		defer func() {
			cancel()
			wg.Wait()
		}()

		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				<-start
				_, err := engine.AllocateGPUsWithQueue(waitCtx, &QueuedAllocationRequest{
					AllocationRequest: &types.AllocationRequest{
						GPUCount: 1, User: fmt.Sprintf("alice-%d", idx), ActualUser: "alice",
						ReservationType: types.ReservationTypeRun,
					},
					Blocking: true,
				})
				if isLimitError(err) {
					rejected <- err
				}
			}(i)
		}
		close(start)

		// Exactly 4 should be rejected immediately with ErrTaskLimitReached and 4 queued.
		for i := 0; i < 4; i++ {
			select {
			case err := <-rejected:
				assert.ErrorIs(t, err, ErrTaskLimitReached)
			case <-time.After(5 * time.Second):
				t.Fatalf("expected 4 immediate rejections, got %d", i)
			}
		}
		require.Eventually(t, func() bool {
			qLen, err := client.GetQueueLength(ctx)
			return err == nil && qLen == 4
		}, 3*time.Second, 10*time.Millisecond)
		// Ensure no extra rejections slip in.
		assert.Empty(t, rejected)
	})

	t.Run("GPU count cap under concurrent queueing", func(t *testing.T) {
		engine, client, ctx := setupBookingTestEngine(t, 4)
		require.NoError(t, client.SetTaskLimits(ctx, 4, 8, ""))

		// Occupy all GPUs with Bob so every Alice submission must queue.
		_, err := engine.AllocateGPUs(ctx, &types.AllocationRequest{
			GPUCount: 4, User: "bob", ReservationType: types.ReservationTypeRun,
		})
		require.NoError(t, err)

		waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		start := make(chan struct{})
		rejected := make(chan error, 6)
		var wg sync.WaitGroup
		defer func() {
			cancel()
			wg.Wait()
		}()

		// Each task asks for 3 GPUs: at most 2 fit in 8 GPUs (6 <= 8, 9 > 8).
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				<-start
				_, err := engine.AllocateGPUsWithQueue(waitCtx, &QueuedAllocationRequest{
					AllocationRequest: &types.AllocationRequest{
						GPUCount: 3, User: fmt.Sprintf("alice-%d", idx), ActualUser: "alice",
						ReservationType: types.ReservationTypeRun,
					},
					Blocking: true,
				})
				if isLimitError(err) {
					rejected <- err
				}
			}(i)
		}
		close(start)

		for i := 0; i < 4; i++ {
			select {
			case err := <-rejected:
				assert.ErrorIs(t, err, ErrGPULimitReached)
			case <-time.After(5 * time.Second):
				t.Fatalf("expected 4 immediate GPU-limit rejections, got %d", i)
			}
		}
		require.Eventually(t, func() bool {
			qLen, err := client.GetQueueLength(ctx)
			return err == nil && qLen == 2
		}, 3*time.Second, 10*time.Millisecond)
	})

	t.Run("concurrent direct and queued admission respects single slot", func(t *testing.T) {
		engine, client, ctx := setupBookingTestEngine(t, 8)
		require.NoError(t, client.SetTaskLimits(ctx, 1, 8, ""))
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
	})
}

func TestQueuedTaskRespectsLoweredLimitsWithoutBlockingOthers(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping Redis integration test in short mode")
	}
	engine, client, ctx := setupBookingTestEngine(t, 8)

	// Before limits are active, Alice has 2 running tasks (3 GPUs each = 6 GPUs)
	// and a queued task requesting 4 GPUs.
	held1, err := engine.AllocateGPUs(ctx, &types.AllocationRequest{
		GPUCount: 3, User: "alice-1", ActualUser: "alice", ReservationType: types.ReservationTypeRun,
	})
	require.NoError(t, err)
	_, err = engine.AllocateGPUs(ctx, &types.AllocationRequest{
		GPUCount: 3, User: "alice-2", ActualUser: "alice", ReservationType: types.ReservationTypeRun,
	})
	require.NoError(t, err)

	aliceQueued := engine.createQueueEntry(&QueuedAllocationRequest{AllocationRequest: &types.AllocationRequest{
		GPUCount: 4, User: "alice-q", ActualUser: "alice", ReservationType: types.ReservationTypeRun,
	}})
	require.NoError(t, client.AddToQueue(ctx, aliceQueued))
	bobQueued := engine.createQueueEntry(&QueuedAllocationRequest{AllocationRequest: &types.AllocationRequest{
		GPUCount: 1, User: "bob", ActualUser: "bob", ReservationType: types.ReservationTypeRun,
	}})
	require.NoError(t, client.AddToQueue(ctx, bobQueued))

	// Now guard enables 4 tasks / 8 GPUs limit.
	// 2 GPUs are free; Alice's queued 4-GPU task cannot run (needs 4 GPUs AND 6+4=10 > 8),
	// and must not block Bob's queued 1-GPU task.
	require.NoError(t, client.SetTaskLimits(ctx, 4, 8, ""))

	first, err := engine.isFirstSatisfiableInQueue(ctx, bobQueued.ID)
	require.NoError(t, err)
	assert.True(t, first, "a GPU-capped account does not block other accounts in the queue")

	res, err := engine.tryAllocateForQueueEntry(ctx, aliceQueued, nil)
	require.NoError(t, err)
	assert.Nil(t, res, "queued task waits while running GPUs + requested GPUs exceeds limit")

	res, err = engine.tryAllocateForQueueEntry(ctx, bobQueued, nil)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.NoError(t, client.RemoveFromQueue(ctx, bobQueued.ID))

	// Release one of Alice's 3-GPU tasks: now 3 running GPUs, 4 free GPUs, 3+4=7 <= 8.
	_, err = engine.ReleaseSpecificGPUs(ctx, "alice-1", held1)
	require.NoError(t, err)

	first, err = engine.isFirstSatisfiableInQueue(ctx, aliceQueued.ID)
	require.NoError(t, err)
	assert.True(t, first)
	res, err = engine.tryAllocateForQueueEntry(ctx, aliceQueued, nil)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.Len(t, res.AllocatedGPUs, 4)
}

func TestTaskAndGPULimitDefersBooking(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping Redis integration test in short mode")
	}

	t.Run("defers on task count limit", func(t *testing.T) {
		engine, client, ctx := setupBookingTestEngine(t, 2)
		require.NoError(t, client.SetTaskLimits(ctx, 1, 8, ""))
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

		_, err = engine.ReleaseSpecificGPUs(ctx, "alice", held)
		require.NoError(t, err)
		activated, err = engine.ActivateDueBookings(ctx)
		require.NoError(t, err)
		assert.Len(t, activated, 1)
	})

	t.Run("defers on GPU count limit", func(t *testing.T) {
		engine, client, ctx := setupBookingTestEngine(t, 4)
		require.NoError(t, client.SetTaskLimits(ctx, 4, 3, ""))
		held, err := engine.AllocateGPUs(ctx, &types.AllocationRequest{
			GPUIDs: []int{0, 1}, User: "alice", ReservationType: types.ReservationTypeRun,
		})
		require.NoError(t, err)
		now := time.Now()
		booking := testBooking("gpu-limited-booking", []int{2, 3}, now.Add(-time.Minute), now.Add(time.Hour), types.BookingStatusPending)
		require.NoError(t, client.SaveBooking(ctx, booking))
		activated, err := engine.ActivateDueBookings(ctx)
		require.NoError(t, err)
		assert.Empty(t, activated, "2 running + 2 booked = 4 > 3 max GPUs defers booking")

		_, err = engine.ReleaseSpecificGPUs(ctx, "alice", held)
		require.NoError(t, err)
		activated, err = engine.ActivateDueBookings(ctx)
		require.NoError(t, err)
		assert.Len(t, activated, 1)
	})
}

func TestGuardTaskLimitHours(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping Redis integration test in short mode")
	}
	now := time.Now()
	start, end := now.Add(-time.Hour).Format("15:04"), now.Add(time.Hour).Format("15:04")
	settings := DefaultGuardConfig()
	settings.MaxTasksPerUser = 2
	settings.MaxGPUsPerUser = 2
	settings.TaskLimitHours = start + "-" + end
	guard, _, _, ctx := setupGuard(t, 4, settings)
	_, err := guard.RunOnce(ctx)
	require.NoError(t, err)

	engine := NewAllocationEngine(guard.client, guard.config)
	request := &QueuedAllocationRequest{
		AllocationRequest: &types.AllocationRequest{
			GPUCount: 2, User: "alice", ReservationType: types.ReservationTypeRun,
		},
		Blocking: true,
	}
	_, err = engine.AllocateGPUsWithQueue(ctx, request)
	require.NoError(t, err)

	// Inside window: next 1-GPU request exceeds 2-GPU limit and is rejected immediately.
	smallReq := &QueuedAllocationRequest{
		AllocationRequest: &types.AllocationRequest{
			GPUCount: 1, User: "alice", ReservationType: types.ReservationTypeRun,
		},
		Blocking: true,
	}
	_, err = engine.AllocateGPUsWithQueue(ctx, smallReq)
	require.ErrorIs(t, err, ErrGPULimitReached)

	// Pre-existing queue entry is ineligible inside the window.
	entry := engine.createQueueEntry(smallReq)
	require.NoError(t, guard.client.AddToQueue(ctx, entry))
	first, err := engine.isFirstSatisfiableInQueue(ctx, entry.ID)
	require.NoError(t, err)
	assert.False(t, first)

	// Outside window: queued task becomes eligible and starts.
	guard.settings.TaskLimitHours = end + "-" + start
	_, err = guard.RunOnce(ctx)
	require.NoError(t, err)
	first, err = engine.isFirstSatisfiableInQueue(ctx, entry.ID)
	require.NoError(t, err)
	assert.True(t, first, "queued tasks become eligible outside the window")
	result, err := engine.tryAllocateForQueueEntry(ctx, entry, smallReq)
	require.NoError(t, err)
	require.NotNil(t, result, "outside the window a task can start even when its account is at the limit")
	require.NoError(t, guard.client.RemoveFromQueue(ctx, entry.ID))

	// Re-entering window leaves existing tasks running and rejects new submissions.
	guard.settings.TaskLimitHours = start + "-" + end
	_, err = guard.RunOnce(ctx)
	require.NoError(t, err)
	tasks, err := engine.GetRunningTasks(ctx)
	require.NoError(t, err)
	assert.Len(t, tasks, 2, "entering the window leaves existing tasks running")
	_, err = engine.AllocateGPUsWithQueue(ctx, smallReq)
	require.True(t, errors.Is(err, ErrTaskLimitReached) || errors.Is(err, ErrGPULimitReached))
}

func TestSubmissionLimitsIgnoreStaleQueueEntriesAndPartialDoubleCount(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping Redis integration test in short mode")
	}

	t.Run("stale queue entries are cleaned up before counting quota", func(t *testing.T) {
		engine, client, ctx := setupBookingTestEngine(t, 4)
		require.NoError(t, client.SetTaskLimits(ctx, 1, 2, ""))

		staleTime := time.Now().Add(-5 * time.Minute)
		require.NoError(t, client.AddToQueue(ctx, &types.QueueEntry{
			ID:              "stale-q-entry",
			User:            "alice",
			ActualUser:      "alice",
			RequestedCount:  2,
			AllocatedGPUs:   []int{},
			ReservationType: types.ReservationTypeRun,
			EnqueueTime:     types.FlexibleTime{Time: staleTime},
			LastHeartbeat:   types.FlexibleTime{Time: staleTime},
		}))

		allocated, err := engine.AllocateGPUs(ctx, &types.AllocationRequest{
			GPUCount: 2, User: "alice", ActualUser: "alice", ReservationType: types.ReservationTypeRun,
		})
		require.NoError(t, err, "expired queue entry should be cleaned up and not consume quota")
		assert.Len(t, allocated, 2)
	})

	t.Run("legacy partial allocation is not double-counted", func(t *testing.T) {
		engine, client, ctx := setupBookingTestEngine(t, 8)
		require.NoError(t, client.SetTaskLimits(ctx, 2, 6, ""))

		now := time.Now()
		entry := &types.QueueEntry{
			ID:              "partial1-full-uuid",
			User:            "alice",
			ActualUser:      "alice",
			RequestedCount:  4,
			AllocatedGPUs:   []int{0, 1},
			ReservationType: types.ReservationTypeRun,
			EnqueueTime:     types.FlexibleTime{Time: now},
			LastHeartbeat:   types.FlexibleTime{Time: now},
		}
		require.NoError(t, client.AddToQueue(ctx, entry))
		for _, gpuID := range []int{0, 1} {
			require.NoError(t, client.SetGPUState(ctx, gpuID, &types.GPUState{
				User:           "alice",
				ActualUser:     "alice",
				StartTime:      types.FlexibleTime{Time: now},
				LastHeartbeat:  types.FlexibleTime{Time: now},
				Type:           types.ReservationTypeRun,
				PartialQueueID: entry.ID,
				TaskID:         entry.ShortID(),
			}))
		}

		// Current usage is 1 task / 4 GPUs (not 2 tasks / 6 GPUs), so a 2-GPU task fits (2 tasks / 6 GPUs).
		allocated, err := engine.AllocateGPUs(ctx, &types.AllocationRequest{
			GPUCount: 2, User: "alice", ActualUser: "alice", ReservationType: types.ReservationTypeRun,
		})
		require.NoError(t, err)
		assert.Len(t, allocated, 2)

		// Any additional GPU now exceeds both task limit (2) and GPU limit (6).
		_, err = engine.AllocateGPUs(ctx, &types.AllocationRequest{
			GPUCount: 1, User: "alice", ActualUser: "alice", ReservationType: types.ReservationTypeRun,
		})
		require.ErrorIs(t, err, ErrTaskLimitReached)
	})
}
