package gpu

import (
	"context"
	"time"

	"github.com/russellb/canhazgpu/internal/types"
)

// RunIdleCheckInterval is how often the supervisor inspects the GPU usage of a
// run reservation to decide whether its idle timeout has expired. Checking is
// cheap (one nvidia-smi query) and a 15-second resolution keeps the release
// latency far below the multi-minute grace periods people configure.
const RunIdleCheckInterval = 15 * time.Second

// RunIdleMonitor watches a run-type reservation and reports when the holder's
// own GPU usage has been absent for longer than the reservation's idle
// timeout. It exists because the supervisor only monitors the wrapped
// process: a wrapper shell that outlives its GPU child (for example a vLLM
// process that died and became a zombie) would otherwise keep the reservation
// alive forever even though nothing is using the GPUs.
type RunIdleMonitor struct {
	engine      *AllocationEngine
	gpuIDs      []int
	user        string
	idleTimeout time.Duration

	// detectUsage supplies the GPU usage map. It defaults to the allocation
	// engine's provider-backed detection and is a field so tests can inject
	// usage without talking to a real GPU.
	detectUsage func(context.Context) (map[int]*types.GPUUsage, error)

	// lastActivity is when the holder's own GPU usage was last observed on
	// any of the reserved GPUs. It starts at construction time so the job
	// gets the full grace period to start touching the GPU.
	lastActivity time.Time
}

// NewRunIdleMonitor creates a monitor for a run reservation. idleTimeout <= 0
// disables idle release entirely.
func NewRunIdleMonitor(engine *AllocationEngine, gpuIDs []int, user string, idleTimeout time.Duration) *RunIdleMonitor {
	m := &RunIdleMonitor{
		engine:       engine,
		gpuIDs:       gpuIDs,
		user:         user,
		idleTimeout:  idleTimeout,
		lastActivity: time.Now(),
	}
	m.detectUsage = m.engine.detectGPUUsage
	return m
}

// Check refreshes lastActivity whenever the holder's own processes are seen
// using any of the reserved GPUs, and reports how long the reservation has
// been idle plus whether it should be released. It also reports release when
// one of the GPUs is no longer reserved by this user, since the reservation
// has already ended.
//
// When GPU usage cannot be detected the reservation gets the benefit of the
// doubt and is never judged idle, mirroring the maintenance pass: a release
// must never be guessed at.
func (m *RunIdleMonitor) Check(ctx context.Context) (time.Duration, bool) {
	if m.idleTimeout <= 0 {
		return 0, false
	}

	usage, err := m.detectUsage(ctx)
	if err != nil {
		return 0, false
	}

	active := false
	for _, gpuID := range m.gpuIDs {
		state, err := m.engine.client.GetGPUState(ctx, gpuID)
		if err != nil {
			continue
		}
		if state.User != m.user || state.Type != types.ReservationTypeRun {
			return 0, true
		}
		if IsReservationHolderActive(state, usage[gpuID], m.engine.config.MemoryThreshold) {
			active = true
		}
	}

	now := time.Now()
	if active {
		m.lastActivity = now
		return 0, false
	}

	idleFor := now.Sub(m.lastActivity)
	return idleFor, idleFor > m.idleTimeout
}
