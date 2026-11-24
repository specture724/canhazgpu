package gpu

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/russellb/canhazgpu/internal/types"
)

// RunningTask is one job holding GPUs right now: the GPUs of a single
// reservation grouped under the handle 'cancel' accepts
type RunningTask struct {
	TaskID     string             `json:"task_id"`
	User       string             `json:"user"`
	ActualUser string             `json:"actual_user,omitempty"`
	Type       string             `json:"type"`
	GPUs       []int              `json:"gpus"`
	PID        int                `json:"pid,omitempty"`
	StartTime  types.FlexibleTime `json:"start_time"`
	ExpiryTime types.FlexibleTime `json:"expiry_time,omitempty"`
	Note       string             `json:"note,omitempty"`
}

// Account returns the OS account that owns the task
func (t *RunningTask) Account() string {
	if t.ActualUser != "" {
		return t.ActualUser
	}
	return t.User
}

// Duration is how long the task has held its GPUs
func (t *RunningTask) Duration() time.Duration {
	return time.Since(t.StartTime.ToTime())
}

// GetRunningTasks lists the reservations currently holding GPUs, grouped by
// task handle. Reservations made before task handles existed have no ID and are
// reported one per GPU, so they still show up even though 'cancel' cannot
// resolve them.
func (ae *AllocationEngine) GetRunningTasks(ctx context.Context) ([]*RunningTask, error) {
	gpuCount, err := ae.client.GetGPUCount(ctx)
	if err != nil {
		return nil, err
	}

	byID := make(map[string]*RunningTask)
	var tasks []*RunningTask

	for gpuID := 0; gpuID < gpuCount; gpuID++ {
		state, err := ae.client.GetGPUState(ctx, gpuID)
		if err != nil {
			return nil, fmt.Errorf("failed to read state of GPU %d: %w", gpuID, err)
		}
		if state.User == "" {
			continue
		}

		if task, seen := byID[state.TaskID]; seen && state.TaskID != "" {
			task.GPUs = append(task.GPUs, gpuID)
			continue
		}

		task := &RunningTask{
			TaskID:     state.TaskID,
			User:       state.User,
			ActualUser: state.ActualUser,
			Type:       state.Type,
			GPUs:       []int{gpuID},
			PID:        state.PID,
			StartTime:  state.StartTime,
			ExpiryTime: state.ExpiryTime,
			Note:       state.Note,
		}
		if state.TaskID != "" {
			byID[state.TaskID] = task
		}
		tasks = append(tasks, task)
	}

	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].StartTime.ToTime().Before(tasks[j].StartTime.ToTime())
	})

	return tasks, nil
}

// ErrTaskLimitReached means the account has reached its maximum number of tasks.
var ErrTaskLimitReached = errors.New("task limit reached")

// ErrGPULimitReached means the request would exceed the account's total GPU limit.
var ErrGPULimitReached = errors.New("GPU limit reached")

func isLimitError(err error) bool {
	return errors.Is(err, ErrTaskLimitReached) || errors.Is(err, ErrGPULimitReached)
}

func accountName(user, actualUser string) string {
	if actualUser != "" {
		return actualUser
	}
	return user
}

// checkSubmissionLimits must be called under the allocation lock when submitting
// a new request (immediate allocation or enqueueing). It counts both running
// tasks and queued entries owned by the OS account.
func (ae *AllocationEngine) checkSubmissionLimits(ctx context.Context, user, actualUser string, requestedGPUs int) error {
	limits, err := ae.client.GetTaskLimits(ctx)
	if err != nil {
		return err
	}
	if limits.MaxTasksPerUser == 0 && limits.MaxGPUsPerUser == 0 {
		return nil
	}

	account := accountName(user, actualUser)

	runningTasks, err := ae.GetRunningTasks(ctx)
	if err != nil {
		return err
	}
	queueEntries, err := ae.client.GetAllQueueEntries(ctx)
	if err != nil {
		return err
	}

	runningCount := 0
	runningGPUs := 0
	runningByTaskID := make(map[string]int)
	for _, task := range runningTasks {
		if task.Account() != account {
			continue
		}
		runningCount++
		runningGPUs += len(task.GPUs)
		if task.TaskID != "" {
			runningByTaskID[task.TaskID] = len(task.GPUs)
		}
	}

	queuedCount := 0
	queuedGPUs := 0
	for _, entry := range queueEntries {
		if entry.Account() != account {
			continue
		}
		reqGPUs := entry.GetRequestedGPUCount()
		if existingGPUs, hasPartial := runningByTaskID[entry.ShortID()]; hasPartial {
			// Legacy partial allocation: task is already counted in runningCount;
			// only add any unallocated GPUs toward total GPU usage.
			if reqGPUs > existingGPUs {
				queuedGPUs += reqGPUs - existingGPUs
			}
			continue
		}
		queuedCount++
		queuedGPUs += reqGPUs
	}

	totalTasks := runningCount + queuedCount
	totalGPUs := runningGPUs + queuedGPUs

	if limits.MaxTasksPerUser > 0 && totalTasks+1 > limits.MaxTasksPerUser {
		return fmt.Errorf("%w for %s (%d/%d submitted tasks: %d running, %d queued)",
			ErrTaskLimitReached, account, totalTasks, limits.MaxTasksPerUser, runningCount, queuedCount)
	}
	if limits.MaxGPUsPerUser > 0 && totalGPUs+requestedGPUs > limits.MaxGPUsPerUser {
		return fmt.Errorf("%w for %s (requested %d + current %d = %d > %d max GPUs; %d running, %d queued)",
			ErrGPULimitReached, account, requestedGPUs, totalGPUs, totalGPUs+requestedGPUs, limits.MaxGPUsPerUser, runningGPUs, queuedGPUs)
	}
	return nil
}

// checkRunningLimits verifies that starting an already-queued task or activating
// a due booking will not exceed the account's active task or GPU limits.
func (ae *AllocationEngine) checkRunningLimits(ctx context.Context, user, actualUser string, requestedGPUs int, taskID string) error {
	limits, err := ae.client.GetTaskLimits(ctx)
	if err != nil {
		return err
	}
	if limits.MaxTasksPerUser == 0 && limits.MaxGPUsPerUser == 0 {
		return nil
	}

	account := accountName(user, actualUser)

	runningTasks, err := ae.GetRunningTasks(ctx)
	if err != nil {
		return err
	}

	runningCount := 0
	runningGPUs := 0
	for _, task := range runningTasks {
		if task.Account() != account {
			continue
		}
		if taskID != "" && task.TaskID == taskID {
			continue
		}
		runningCount++
		runningGPUs += len(task.GPUs)
	}

	if limits.MaxTasksPerUser > 0 && runningCount+1 > limits.MaxTasksPerUser {
		return fmt.Errorf("%w for %s (%d/%d running tasks)",
			ErrTaskLimitReached, account, runningCount, limits.MaxTasksPerUser)
	}
	if limits.MaxGPUsPerUser > 0 && runningGPUs+requestedGPUs > limits.MaxGPUsPerUser {
		return fmt.Errorf("%w for %s (requested %d + running %d = %d > %d max GPUs)",
			ErrGPULimitReached, account, requestedGPUs, runningGPUs, runningGPUs+requestedGPUs, limits.MaxGPUsPerUser)
	}
	return nil
}

// FindTask resolves an ID prefix against the tasks that hold GPUs right now
func (ae *AllocationEngine) FindTask(ctx context.Context, ref string) (*RunningTask, error) {
	tasks, err := ae.GetRunningTasks(ctx)
	if err != nil {
		return nil, err
	}

	var matches []*RunningTask
	for _, task := range tasks {
		if task.TaskID != "" && strings.HasPrefix(task.TaskID, ref) {
			matches = append(matches, task)
		}
	}

	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("task ID %q is ambiguous (matches %d running tasks)", ref, len(matches))
	}
}

// FindQueuedTask resolves an ID prefix against the entries waiting in the queue
func (ae *AllocationEngine) FindQueuedTask(ctx context.Context, ref string) (*types.QueueEntry, error) {
	entries, err := ae.client.GetAllQueueEntries(ctx)
	if err != nil {
		return nil, err
	}

	var matches []*types.QueueEntry
	for _, entry := range entries {
		if strings.HasPrefix(entry.ID, ref) {
			matches = append(matches, entry)
		}
	}

	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		return matches[0], nil
	default:
		return nil, fmt.Errorf("task ID %q is ambiguous (matches %d queued tasks)", ref, len(matches))
	}
}
