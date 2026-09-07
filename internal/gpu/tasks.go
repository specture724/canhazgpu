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

// ErrTaskLimitReached means the account must wait for a running task to finish.
var ErrTaskLimitReached = errors.New("concurrent task limit reached")

// checkTaskLimit must be called under the allocation lock when admitting a task.
// Queue eligibility also uses it to skip accounts that cannot start yet.
func (ae *AllocationEngine) checkTaskLimit(ctx context.Context, user, actualUser, taskID string) error {
	limit, err := ae.client.GetMaxTasksPerUser(ctx)
	if err != nil {
		return err
	}
	if limit == 0 {
		return nil
	}
	if actualUser == "" {
		actualUser = user
	}
	tasks, err := ae.GetRunningTasks(ctx)
	if err != nil {
		return err
	}
	count := 0
	for _, task := range tasks {
		if task.Account() != actualUser {
			continue
		}
		// Completing a partial allocation does not start another task.
		if taskID != "" && task.TaskID == taskID {
			return nil
		}
		count++
	}
	if count >= limit {
		return fmt.Errorf("%w for %s (%d/%d)", ErrTaskLimitReached, actualUser, count, limit)
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
