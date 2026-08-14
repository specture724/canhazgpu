package gpu

import (
	"context"
	"fmt"
	"strings"
	"syscall"
	"time"

	"github.com/russellb/canhazgpu/internal/types"
)

// CancelResult describes what cancelling a task did
type CancelResult struct {
	TaskID    string
	User      string
	Queued    bool  // The task was waiting in the queue, not holding GPUs
	GPUs      []int // GPUs the task held (or had partially allocated)
	Signalled int   // Process that was signalled, 0 if there was none to signal
	Killed    bool  // The process ignored SIGTERM and was killed
	Released  bool  // The reservation was released directly, without a process
}

const (
	// cancelKillGrace is how long a signalled job may take to exit before it is
	// killed outright
	cancelKillGrace = 10 * time.Second

	// cancelReleaseGrace is how long the job's supervisor gets to hand the GPUs
	// back before cancel releases them itself. The supervisor notices an exit
	// within half a second, so this only ever elapses when it is gone too.
	cancelReleaseGrace = 2 * time.Second
)

// CancelTask stops a queued or running task by its (abbreviated) handle. Jobs
// held by a live process are signalled so they clean up after themselves - the
// reservation is only released directly when no process is left to do it.
func (ae *AllocationEngine) CancelTask(ctx context.Context, ref string, actualUser string, force bool) (*CancelResult, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("no task ID given")
	}

	entry, err := ae.FindQueuedTask(ctx, ref)
	if err != nil {
		return nil, err
	}

	task, err := ae.FindTask(ctx, ref)
	if err != nil {
		return nil, err
	}

	if entry != nil && task != nil && task.TaskID != entry.ShortID() {
		return nil, fmt.Errorf("task ID %q is ambiguous (matches a queued and a running task)", ref)
	}

	switch {
	case entry != nil:
		return ae.cancelQueuedTask(ctx, entry, actualUser, force)
	case task != nil:
		return ae.cancelRunningTask(ctx, task, actualUser, force)
	default:
		return nil, fmt.Errorf("no queued or running task found matching %q", ref)
	}
}

func (ae *AllocationEngine) cancelQueuedTask(ctx context.Context, entry *types.QueueEntry, actualUser string, force bool) (*CancelResult, error) {
	owner := entry.ActualUser
	if owner == "" {
		owner = entry.User
	}
	if !force && owner != actualUser {
		return nil, fmt.Errorf("task %s belongs to %s (use --force to cancel it anyway)", entry.ShortID(), entry.User)
	}

	result := &CancelResult{
		TaskID: entry.ShortID(),
		User:   entry.User,
		Queued: true,
		GPUs:   entry.AllocatedGPUs,
	}

	// The waiting process removes its own entry when signalled; doing it here
	// as well keeps the queue clean if that process is already gone
	if signalProcess(entry.PID, syscall.SIGTERM) {
		result.Signalled = entry.PID
		waitForExit(entry.PID, cancelKillGrace)
	}

	ae.cleanupQueueEntry(ctx, entry)
	return result, nil
}

func (ae *AllocationEngine) cancelRunningTask(ctx context.Context, task *RunningTask, actualUser string, force bool) (*CancelResult, error) {
	if task.TaskID == "" {
		return nil, fmt.Errorf("this reservation predates task IDs - release it with 'canhazgpu release'")
	}
	if !force && task.Account() != actualUser {
		return nil, fmt.Errorf("task %s belongs to %s (use --force to cancel it anyway)", task.TaskID, task.User)
	}

	result := &CancelResult{
		TaskID: task.TaskID,
		User:   task.User,
		GPUs:   task.GPUs,
	}

	// A run-type reservation is held by a live process: killing it makes the
	// supervisor release the GPUs, which also stops the job itself
	if signalProcess(task.PID, syscall.SIGTERM) {
		result.Signalled = task.PID
		if !waitForExit(task.PID, cancelKillGrace) {
			if signalProcess(task.PID, syscall.SIGKILL) {
				result.Killed = true
			}
			waitForExit(task.PID, cancelKillGrace)
		}
		// The supervisor releases the GPUs once it sees the job exit
		if waitForRelease(ctx, ae, task, cancelReleaseGrace) {
			return result, nil
		}
	}

	// Nothing (left) to signal, so release the reservation here
	released, err := ae.ReleaseSpecificGPUs(ctx, task.User, task.GPUs)
	if err != nil {
		return nil, err
	}
	result.Released = len(released) > 0
	return result, nil
}

// signalProcess sends a signal to a PID and reports whether it was delivered
func signalProcess(pid int, signal syscall.Signal) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, signal) == nil
}

// waitForExit waits for a process to disappear, reporting whether it did
func waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !isProcessAlive(pid) {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return !isProcessAlive(pid)
}

// waitForRelease waits for the job's supervisor to hand the GPUs back
func waitForRelease(ctx context.Context, ae *AllocationEngine, task *RunningTask, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		held := false
		for _, gpuID := range task.GPUs {
			state, err := ae.client.GetGPUState(ctx, gpuID)
			if err == nil && state.TaskID == task.TaskID {
				held = true
				break
			}
		}
		if !held {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(200 * time.Millisecond)
	}
}
