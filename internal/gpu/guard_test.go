package gpu

import (
	"context"
	"io"
	"os/exec"
	"os/user"
	"testing"
	"time"

	"github.com/russellb/canhazgpu/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeNotifier records warnings instead of delivering them
type fakeNotifier struct {
	messages      []string
	notifiedUsers []string
}

func (f *fakeNotifier) Notify(violation *types.Violation, message string) []string {
	f.messages = append(f.messages, message)
	return []string{"fake"}
}

func (f *fakeNotifier) NotifyUser(username string, message string) []string {
	f.notifiedUsers = append(f.notifiedUsers, username)
	return []string{"fake"}
}

func TestClassifyProcess(t *testing.T) {
	tests := []struct {
		name       string
		state      *types.GPUState
		process    types.GPUProcessInfo
		wantKind   string
		wantHolder string
	}{
		{
			name:     "No reservation at all",
			state:    &types.GPUState{},
			process:  types.GPUProcessInfo{PID: 10, User: "bob"},
			wantKind: types.ViolationKindUnreserved,
		},
		{
			name: "Reservation holder using their own GPU",
			state: &types.GPUState{
				User: "alice", ActualUser: "alice", Type: types.ReservationTypeRun,
			},
			process:  types.GPUProcessInfo{PID: 10, User: "alice"},
			wantKind: "",
		},
		{
			name: "Somebody else on a reserved GPU",
			state: &types.GPUState{
				User: "alice", ActualUser: "alice", Type: types.ReservationTypeManual,
			},
			process:    types.GPUProcessInfo{PID: 10, User: "bob"},
			wantKind:   types.ViolationKindForeign,
			wantHolder: "alice",
		},
		{
			name: "Custom display name compares against the OS account",
			state: &types.GPUState{
				User: "Alice Smith", ActualUser: "alice", Type: types.ReservationTypeManual,
			},
			process:  types.GPUProcessInfo{PID: 10, User: "alice"},
			wantKind: "",
		},
		{
			name: "Reservation without an OS account falls back to the display name",
			state: &types.GPUState{
				User: "alice", Type: types.ReservationTypeManual,
			},
			process:  types.GPUProcessInfo{PID: 10, User: "alice"},
			wantKind: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, holder := classifyProcess(tt.state, tt.process)
			assert.Equal(t, tt.wantKind, kind)
			assert.Equal(t, tt.wantHolder, holder)
		})
	}
}

func TestGuardIsExcluded(t *testing.T) {
	guard := &Guard{settings: GuardConfig{
		ExcludeUsers:    []string{"root"},
		ExcludeCommands: []string{"Xorg", "dcgm-exporter"},
		MinMemoryMB:     512,
	}}

	assert.True(t, guard.isExcluded(types.GPUProcessInfo{User: "root", MemoryMB: 4096}),
		"excluded users are never reported")
	assert.True(t, guard.isExcluded(types.GPUProcessInfo{User: "bob", ProcessName: "/usr/bin/Xorg", MemoryMB: 4096}),
		"excluded commands are never reported")
	assert.True(t, guard.isExcluded(types.GPUProcessInfo{User: "bob", ProcessName: "python", MemoryMB: 100}),
		"processes below the memory floor are ignored")
	assert.False(t, guard.isExcluded(types.GPUProcessInfo{User: "bob", ProcessName: "python", MemoryMB: 4096}))
}

func TestMergeViolation(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.Local)

	fresh := &types.Violation{
		GPUID: 1, PID: 42, User: "bob", Kind: types.ViolationKindUnreserved,
		MemoryMB: 1024, FirstSeen: types.FlexibleTime{Time: now}, LastSeen: types.FlexibleTime{Time: now},
	}

	first := mergeViolation(nil, fresh, now)
	assert.Equal(t, 1, first.Sightings)
	assert.Equal(t, "1:42", first.ViolationID())

	later := now.Add(15 * time.Second)
	updated := mergeViolation(first, &types.Violation{
		GPUID: 1, PID: 42, User: "bob", Kind: types.ViolationKindUnreserved, MemoryMB: 2048,
	}, later)

	assert.Equal(t, 2, updated.Sightings, "consecutive sightings accumulate")
	assert.Equal(t, 2048, updated.MemoryMB, "memory is refreshed")
	assert.True(t, updated.FirstSeen.ToTime().Equal(now), "first sighting is preserved")
	assert.True(t, updated.LastSeen.ToTime().Equal(later))
}

// setupGuard builds a guard over the test Redis with simulated GPU usage
func setupGuard(t *testing.T, gpuCount int, settings GuardConfig) (*Guard, *fakeNotifier, func(map[int]*types.GPUUsage), context.Context) {
	engine, client, ctx := setupBookingTestEngine(t, gpuCount)

	notifier := &fakeNotifier{}
	guard := NewGuard(engine, client, engine.config, settings, notifier)
	guard.SetOutput(io.Discard)

	usage := make(map[int]*types.GPUUsage)
	guard.detectUsage = func(context.Context) (map[int]*types.GPUUsage, error) {
		return usage, nil
	}
	// Maintenance would query the real provider; the tests drive detection themselves
	guard.settings.Maintenance = false

	setUsage := func(next map[int]*types.GPUUsage) {
		for gpuID := range usage {
			delete(usage, gpuID)
		}
		for gpuID, gpuUsage := range next {
			usage[gpuID] = gpuUsage
		}
	}

	return guard, notifier, setUsage, ctx
}

func busyGPU(gpuID int, pid int, owner string, memoryMB int) *types.GPUUsage {
	return &types.GPUUsage{
		GPUID:    gpuID,
		MemoryMB: memoryMB,
		Processes: []types.GPUProcessInfo{
			{PID: pid, User: owner, ProcessName: "python", MemoryMB: memoryMB},
		},
		Users: map[string]bool{owner: true},
	}
}

func TestGuardWarnsAfterGraceAndConfirmations(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	settings := DefaultGuardConfig()
	settings.Confirmations = 2
	settings.GracePeriod = time.Hour // Nothing should be warned about yet
	settings.ExcludeUsers = nil

	guard, notifier, setUsage, ctx := setupGuard(t, 2, settings)
	setUsage(map[int]*types.GPUUsage{0: busyGPU(0, 4242, "bob", 8452)})

	// First scan only records the violation
	pass, err := guard.RunOnce(ctx)
	require.NoError(t, err)
	require.Len(t, pass.Violations, 1)
	assert.Equal(t, types.ViolationKindUnreserved, pass.Violations[0].Kind)
	assert.Equal(t, 1, pass.Violations[0].Sightings)
	assert.Equal(t, 0, pass.Warned, "one sighting is not enough to act")

	// Second scan confirms it, but the grace period has not passed
	pass, err = guard.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, pass.Violations[0].Sightings)
	assert.Equal(t, 0, pass.Warned, "grace period must be respected")
	assert.Empty(t, notifier.messages)

	// Once the grace period is over, the offender is warned
	guard.settings.GracePeriod = 0
	pass, err = guard.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, pass.Warned)
	require.Len(t, notifier.messages, 1)
	assert.Contains(t, notifier.messages[0], "GPU 0 is being used without a reservation")
	assert.Contains(t, notifier.messages[0], "canhazgpu run")
}

func TestGuardRepeatsWarningsOnInterval(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	settings := DefaultGuardConfig()
	settings.Confirmations = 1
	settings.GracePeriod = 0
	settings.WarnInterval = time.Hour
	settings.ExcludeUsers = nil

	guard, notifier, setUsage, ctx := setupGuard(t, 1, settings)
	setUsage(map[int]*types.GPUUsage{0: busyGPU(0, 4242, "bob", 8452)})

	_, err := guard.RunOnce(ctx)
	require.NoError(t, err)
	assert.Len(t, notifier.messages, 1)

	// Too soon for a second warning
	_, err = guard.RunOnce(ctx)
	require.NoError(t, err)
	assert.Len(t, notifier.messages, 1, "warnings are throttled by --warn-interval")

	guard.settings.WarnInterval = 0
	_, err = guard.RunOnce(ctx)
	require.NoError(t, err)
	assert.Len(t, notifier.messages, 2)
}

func TestGuardDetectsForeignUsageAndNotifiesHolder(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	settings := DefaultGuardConfig()
	settings.Confirmations = 1
	settings.GracePeriod = 0
	settings.ExcludeUsers = nil

	guard, notifier, setUsage, ctx := setupGuard(t, 1, settings)

	// GPU 0 belongs to alice, who reserved it under a display name, but bob's
	// process is on it
	require.NoError(t, guard.client.SetGPUState(ctx, 0, &types.GPUState{
		User:       "Alice Smith",
		ActualUser: "alice",
		StartTime:  types.FlexibleTime{Time: time.Now().Add(-time.Hour)},
		Type:       types.ReservationTypeManual,
		ExpiryTime: types.FlexibleTime{Time: time.Now().Add(time.Hour)},
	}))
	setUsage(map[int]*types.GPUUsage{0: busyGPU(0, 4242, "bob", 8452)})

	pass, err := guard.RunOnce(ctx)
	require.NoError(t, err)
	require.Len(t, pass.Violations, 1)
	assert.Equal(t, types.ViolationKindForeign, pass.Violations[0].Kind)
	assert.Equal(t, "Alice Smith", pass.Violations[0].Holder, "messages name the holder as displayed")
	assert.Equal(t, "alice", pass.Violations[0].HolderAccount, "notifications need the OS account")
	assert.Equal(t, "bob", pass.Violations[0].User)

	require.Len(t, notifier.messages, 1)
	assert.Contains(t, notifier.messages[0], "GPU 0 is reserved by Alice Smith")
	assert.Equal(t, []string{"alice"}, notifier.notifiedUsers,
		"the holder is reached by OS account, not by their display name")
}

func TestGuardIgnoresCompliantAndQuietGPUs(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	settings := DefaultGuardConfig()
	settings.Confirmations = 1
	settings.GracePeriod = 0
	settings.ExcludeUsers = []string{"root"}

	guard, notifier, setUsage, ctx := setupGuard(t, 3, settings)

	// GPU 0: reserved by alice and used by alice
	require.NoError(t, guard.client.SetGPUState(ctx, 0, &types.GPUState{
		User: "alice", ActualUser: "alice", Type: types.ReservationTypeRun,
		StartTime:     types.FlexibleTime{Time: time.Now()},
		LastHeartbeat: types.FlexibleTime{Time: time.Now()},
	}))

	setUsage(map[int]*types.GPUUsage{
		0: busyGPU(0, 1, "alice", 8452),
		// GPU 1: below the memory threshold, so not considered in use
		1: busyGPU(1, 2, "bob", types.MemoryThresholdMB-1),
		// GPU 2: an excluded system user
		2: busyGPU(2, 3, "root", 4096),
	})

	pass, err := guard.RunOnce(ctx)
	require.NoError(t, err)
	assert.Empty(t, pass.Violations)
	assert.Empty(t, notifier.messages)
}

func TestGuardResolvesViolationsIntoHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	settings := DefaultGuardConfig()
	settings.Confirmations = 1
	settings.GracePeriod = 0
	settings.ExcludeUsers = nil

	guard, _, setUsage, ctx := setupGuard(t, 1, settings)
	setUsage(map[int]*types.GPUUsage{0: busyGPU(0, 4242, "bob", 8452)})

	_, err := guard.RunOnce(ctx)
	require.NoError(t, err)

	open, err := guard.client.GetAllViolations(ctx)
	require.NoError(t, err)
	require.Len(t, open, 1)

	// The process goes away
	setUsage(map[int]*types.GPUUsage{})

	pass, err := guard.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, pass.Resolved)

	open, err = guard.client.GetAllViolations(ctx)
	require.NoError(t, err)
	assert.Empty(t, open, "resolved violations are no longer open")

	history, err := guard.client.GetViolationHistory(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, history, 1)
	assert.Equal(t, 4242, history[0].PID)
	assert.NotEmpty(t, history[0].Resolution)
}

func TestGuardEnforcementDryRunSendsNoSignals(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	settings := DefaultGuardConfig()
	settings.Confirmations = 1
	settings.GracePeriod = 0
	settings.WarnInterval = 0
	settings.KillGrace = 0
	settings.MaxWarnings = 1
	settings.Enforce = true
	settings.DryRun = true
	settings.ExcludeUsers = nil

	guard, _, setUsage, ctx := setupGuard(t, 1, settings)

	// A PID that cannot exist, so a real signal would be an error
	setUsage(map[int]*types.GPUUsage{0: busyGPU(0, 1<<30, "bob", 8452)})

	_, err := guard.RunOnce(ctx)
	require.NoError(t, err)

	pass, err := guard.RunOnce(ctx)
	require.NoError(t, err)
	require.Len(t, pass.Violations, 1)
	require.Len(t, pass.Violations[0].Signals, 1)
	assert.Contains(t, pass.Violations[0].Signals[0], "dry run")
	assert.Equal(t, 0, pass.Signalled, "a dry run never counts as a signal sent")
}

func TestGuardEnforcementTerminatesProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	account, err := user.Current()
	require.NoError(t, err)

	// A real process of our own, so termination needs no privileges
	victim := exec.Command("sleep", "300")
	require.NoError(t, victim.Start())

	// Reap the child as soon as it dies: a zombie still answers kill(pid, 0),
	// so waiting on the process is the only reliable liveness check
	exited := make(chan error, 1)
	go func() { exited <- victim.Wait() }()
	t.Cleanup(func() {
		if victim.Process != nil {
			_ = victim.Process.Kill()
		}
	})

	settings := DefaultGuardConfig()
	settings.Confirmations = 1
	settings.GracePeriod = 0
	settings.WarnInterval = 0
	settings.KillGrace = 0
	settings.MaxWarnings = 1
	settings.Enforce = true
	settings.ExcludeUsers = nil

	guard, _, setUsage, ctx := setupGuard(t, 1, settings)
	setUsage(map[int]*types.GPUUsage{0: busyGPU(0, victim.Process.Pid, account.Username, 8452)})

	// First scan warns
	_, err = guard.RunOnce(ctx)
	require.NoError(t, err)

	// Second scan escalates
	pass, err := guard.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, pass.Signalled)
	require.Len(t, pass.Violations[0].Signals, 1)
	assert.Equal(t, "SIGINT", pass.Violations[0].Signals[0])

	// sleep(1) dies on SIGINT
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the offending process was not terminated")
	}

	kills, err := guard.client.CountGuardKillsSince(ctx, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, kills, "terminations are counted for the hourly limit")
}

func TestGuardTerminationRateLimit(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	settings := DefaultGuardConfig()
	settings.Confirmations = 1
	settings.GracePeriod = 0
	settings.WarnInterval = 0
	settings.KillGrace = 0
	settings.MaxWarnings = 1
	settings.Enforce = true
	settings.MaxKillsPerHour = 1
	settings.ExcludeUsers = nil

	guard, _, setUsage, ctx := setupGuard(t, 1, settings)

	// The budget is already used up
	require.NoError(t, guard.client.RecordGuardKill(ctx, time.Now(), "earlier termination"))

	setUsage(map[int]*types.GPUUsage{0: busyGPU(0, 1<<30, "bob", 8452)})

	_, err := guard.RunOnce(ctx)
	require.NoError(t, err)

	pass, err := guard.RunOnce(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, pass.Signalled)
	assert.Empty(t, pass.Violations[0].Signals, "the circuit breaker stops further terminations")
}

func TestGuardNeverTerminatesUnknownOwners(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	settings := DefaultGuardConfig()
	settings.Confirmations = 1
	settings.GracePeriod = 0
	settings.WarnInterval = 0
	settings.KillGrace = 0
	settings.MaxWarnings = 1
	settings.Enforce = true
	settings.ExcludeUsers = nil

	guard, _, setUsage, ctx := setupGuard(t, 1, settings)

	// Owner detection failed, so the process might be a system process
	setUsage(map[int]*types.GPUUsage{0: busyGPU(0, 1<<30, "", 8452)})

	_, err := guard.RunOnce(ctx)
	require.NoError(t, err)

	pass, err := guard.RunOnce(ctx)
	require.NoError(t, err)
	require.Len(t, pass.Violations, 1)
	assert.Equal(t, "unknown", pass.Violations[0].User)
	assert.Empty(t, pass.Violations[0].Signals)
}
