package gpu

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/russellb/canhazgpu/internal/redis_client"
	"github.com/russellb/canhazgpu/internal/types"
	"github.com/russellb/canhazgpu/internal/utils"
)

// escalationSignals is the ladder the guard walks when terminating a process
// that keeps using a GPU it has no reservation for
var escalationSignals = []struct {
	name   string
	signal syscall.Signal
}{
	{"SIGINT", syscall.SIGINT},
	{"SIGTERM", syscall.SIGTERM},
	{"SIGKILL", syscall.SIGKILL},
}

// GuardConfig controls how the guard reacts to GPU usage that bypasses the
// reservation system
type GuardConfig struct {
	Interval      time.Duration // How often to scan
	GracePeriod   time.Duration // How long a process may run before it is reported
	Confirmations int           // Consecutive scans needed before acting
	WarnInterval  time.Duration // Time between repeated warnings
	MaxWarnings   int           // Warnings delivered before enforcement kicks in

	Enforce         bool          // Terminate offenders once warnings are exhausted
	KillGrace       time.Duration // Wait between escalation signals
	MaxKillsPerHour int           // Circuit breaker for terminations
	DryRun          bool          // Report terminations without sending signals

	ExcludeUsers    []string // Users that are never reported
	ExcludeCommands []string // Process name fragments that are never reported
	MinMemoryMB     int      // Ignore processes below this much memory (0 = no filter)

	Maintenance     bool // Also activate due bookings and reclaim reservations
	NotifyHolder    bool // Tell the reservation holder when somebody squats their GPU
	MaxTasksPerUser int  // Maximum concurrent reservations per OS account (0 = disabled)
}

// DefaultGuardConfig returns the guard configuration used unless overridden.
// Enforcement is off: terminating other people's processes is opt-in.
func DefaultGuardConfig() GuardConfig {
	return GuardConfig{
		Interval:        types.DefaultGuardInterval,
		GracePeriod:     types.DefaultGuardGracePeriod,
		Confirmations:   types.DefaultGuardConfirmations,
		WarnInterval:    types.DefaultGuardWarnInterval,
		MaxWarnings:     types.DefaultGuardMaxWarnings,
		Enforce:         false,
		KillGrace:       types.DefaultGuardKillGrace,
		MaxKillsPerHour: types.DefaultGuardMaxKillsPerHour,
		ExcludeUsers:    []string{"root"},
		ExcludeCommands: []string{"Xorg", "nvidia-smi", "amd-smi", "dcgm-exporter", "nvidia-persistenced"},
		Maintenance:     true,
		NotifyHolder:    true,
		MaxTasksPerUser: 4,
	}
}

// Guard watches the GPUs and reacts to usage that bypasses reservations
type Guard struct {
	engine   *AllocationEngine
	client   *redis_client.Client
	config   *types.Config
	settings GuardConfig
	notifier Notifier
	output   io.Writer

	// detectUsage reports what is running on each GPU. Tests replace it to
	// simulate processes without real hardware.
	detectUsage func(context.Context) (map[int]*types.GPUUsage, error)
}

func NewGuard(engine *AllocationEngine, client *redis_client.Client, config *types.Config,
	settings GuardConfig, notifier Notifier) *Guard {
	return &Guard{
		engine:      engine,
		client:      client,
		config:      config,
		settings:    settings,
		notifier:    notifier,
		output:      os.Stderr,
		detectUsage: engine.detectGPUUsage,
	}
}

// SetOutput redirects the guard's own reporting, used by tests
func (g *Guard) SetOutput(w io.Writer) {
	g.output = w
}

// GuardPass summarises what one scan found and did
type GuardPass struct {
	Processes  int
	Violations []*types.Violation
	Warned     int
	Signalled  int
	Resolved   int
}

// Run scans in a loop until the context is cancelled or the process is
// interrupted. Only one guard may run against a Redis instance at a time.
func (g *Guard) Run(ctx context.Context) error {
	owner, err := guardOwnerID()
	if err != nil {
		return err
	}

	acquired, holder, err := g.client.AcquireGuardLock(ctx, owner)
	if err != nil {
		return fmt.Errorf("failed to claim the guard role: %v", err)
	}
	if !acquired {
		return fmt.Errorf("another guard is already running (%s); stop it first", holder)
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := g.client.ReleaseGuardLock(releaseCtx, owner); err != nil {
			fmt.Fprintf(g.output, "canhazgpu guard: failed to release the guard lock: %v\n", err)
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigChan)

	g.reportStartup()

	ticker := time.NewTicker(g.settings.Interval)
	defer ticker.Stop()

	// Scan immediately rather than waiting out the first interval
	if _, err := g.RunOnce(ctx); err != nil {
		fmt.Fprintf(g.output, "canhazgpu guard: scan failed: %v\n", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil

		case sig := <-sigChan:
			fmt.Fprintf(g.output, "canhazgpu guard: received %v, shutting down\n", sig)
			return nil

		case <-ticker.C:
			if err := g.client.RefreshGuardLock(ctx, owner); err != nil {
				return fmt.Errorf("lost the guard role: %v", err)
			}
			if _, err := g.RunOnce(ctx); err != nil {
				fmt.Fprintf(g.output, "canhazgpu guard: scan failed: %v\n", err)
			}
		}
	}
}

func (g *Guard) reportStartup() {
	mode := "warn only"
	if g.settings.Enforce {
		mode = fmt.Sprintf("warn, then terminate after %d warnings", g.settings.MaxWarnings)
		if g.settings.DryRun {
			mode += " (dry run: no signals sent)"
		}
	}

	fmt.Fprintf(g.output, "canhazgpu guard: watching every %s, grace period %s, %s\n",
		utils.FormatDurationShort(g.settings.Interval),
		utils.FormatDurationShort(g.settings.GracePeriod), mode)

	if os.Geteuid() != 0 {
		fmt.Fprintf(g.output, "canhazgpu guard: running as a normal user - warnings can only reach your own processes, and termination will fail for other users\n")
	}
}

// RunOnce performs a single scan: detect usage, reconcile it with the known
// violations, then warn or terminate as configured
func (g *Guard) RunOnce(ctx context.Context) (*GuardPass, error) {
	// Publish policy for all clients, including ones without the guard's config.
	if err := g.client.SetMaxTasksPerUser(ctx, g.settings.MaxTasksPerUser); err != nil {
		return nil, fmt.Errorf("failed to publish task limit: %w", err)
	}

	// The guard is the only always-on part of canhazgpu, so it also drives the
	// housekeeping that commands would otherwise have to trigger
	if g.settings.Maintenance {
		if err := g.engine.CleanupExpiredReservations(ctx); err != nil {
			fmt.Fprintf(g.output, "canhazgpu guard: maintenance failed: %v\n", err)
		}
	}

	usage, err := g.detectUsage(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to detect GPU usage: %v", err)
	}

	gpuCount, err := g.client.GetGPUCount(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	pass := &GuardPass{}

	observed, processes, err := g.observeViolations(ctx, gpuCount, usage, now)
	if err != nil {
		return nil, err
	}
	pass.Processes = processes

	stored, err := g.client.GetAllViolations(ctx)
	if err != nil {
		return nil, err
	}

	known := make(map[string]*types.Violation, len(stored))
	for _, violation := range stored {
		known[violation.ViolationID()] = violation
	}

	// Violations that are no longer observed have been resolved
	for id, violation := range known {
		if _, stillThere := observed[id]; stillThere {
			continue
		}
		g.resolveViolation(ctx, violation, now)
		pass.Resolved++
	}

	ids := make([]string, 0, len(observed))
	for id := range observed {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		current := observed[id]

		violation := mergeViolation(known[id], current, now)
		g.act(ctx, violation, now, pass)

		if err := g.client.SaveViolation(ctx, violation); err != nil {
			fmt.Fprintf(g.output, "canhazgpu guard: failed to record violation on GPU %d: %v\n",
				violation.GPUID, err)
		}
		pass.Violations = append(pass.Violations, violation)
	}

	return pass, nil
}

// observeViolations compares the processes on each GPU against its reservation
func (g *Guard) observeViolations(ctx context.Context, gpuCount int, usage map[int]*types.GPUUsage,
	now time.Time) (map[string]*types.Violation, int, error) {
	observed := make(map[string]*types.Violation)
	processes := 0

	for gpuID := 0; gpuID < gpuCount; gpuID++ {
		gpuUsage := usage[gpuID]
		if gpuUsage == nil || len(gpuUsage.Processes) == 0 {
			continue
		}

		// Stay consistent with the rest of the tool: a GPU below the memory
		// threshold does not count as being in use
		if !IsGPUBusy(gpuUsage, g.config.MemoryThreshold) {
			continue
		}

		state, err := g.client.GetGPUState(ctx, gpuID)
		if err != nil {
			return nil, processes, err
		}

		for _, process := range gpuUsage.Processes {
			processes++

			kind, holder := classifyProcess(state, process)
			if kind == "" {
				continue
			}
			if g.isExcluded(process) {
				continue
			}

			violation := &types.Violation{
				GPUID:     gpuID,
				PID:       process.PID,
				User:      processOwner(process),
				Kind:      kind,
				Holder:    holder,
				Command:   process.ProcessName,
				MemoryMB:  processMemory(process, gpuUsage),
				FirstSeen: types.FlexibleTime{Time: now},
				LastSeen:  types.FlexibleTime{Time: now},
			}
			if kind == types.ViolationKindForeign {
				// Warnings have to reach an account, not a display name that
				// --user may have set to anything
				violation.HolderAccount = ReservationAccount(state)
			}
			observed[violation.ViolationID()] = violation
		}
	}

	return observed, processes, nil
}

// classifyProcess decides whether a process on a GPU is allowed to be there.
// An empty kind means the process is covered by a reservation.
func classifyProcess(state *types.GPUState, process types.GPUProcessInfo) (kind string, holder string) {
	if state.User == "" {
		return types.ViolationKindUnreserved, ""
	}

	// Reservations record both the OS account and the display name; the process
	// owner has to match the OS account
	holderAccount := ReservationAccount(state)

	if process.User == holderAccount {
		return "", ""
	}

	return types.ViolationKindForeign, state.User
}

// isExcluded reports whether a process is covered by the allow lists
func (g *Guard) isExcluded(process types.GPUProcessInfo) bool {
	for _, user := range g.settings.ExcludeUsers {
		if process.User == user {
			return true
		}
	}

	for _, command := range g.settings.ExcludeCommands {
		if command != "" && strings.Contains(process.ProcessName, command) {
			return true
		}
	}

	if g.settings.MinMemoryMB > 0 && process.MemoryMB > 0 && process.MemoryMB < g.settings.MinMemoryMB {
		return true
	}

	return false
}

// mergeViolation folds a fresh observation into what is already known about it
func mergeViolation(known *types.Violation, current *types.Violation, now time.Time) *types.Violation {
	if known == nil {
		current.Sightings = 1
		return current
	}

	known.LastSeen = types.FlexibleTime{Time: now}
	known.Sightings++
	known.MemoryMB = current.MemoryMB
	known.Kind = current.Kind
	known.Holder = current.Holder
	known.HolderAccount = current.HolderAccount
	if known.User == "" || known.User == "unknown" {
		known.User = current.User
	}
	if known.Command == "" {
		known.Command = current.Command
	}

	return known
}

// act warns about a confirmed violation and, when enforcement is enabled,
// escalates to terminating the process
func (g *Guard) act(ctx context.Context, violation *types.Violation, now time.Time, pass *GuardPass) {
	// Wait until the violation is confirmed and past its grace period, so short
	// lived processes and reservations still starting up are left alone
	if violation.Sightings < g.settings.Confirmations {
		return
	}
	if now.Sub(violation.FirstSeen.ToTime()) < g.settings.GracePeriod {
		return
	}

	if violation.WarnCount < g.settings.MaxWarnings {
		if violation.WarnCount > 0 && now.Sub(violation.LastWarned.ToTime()) < g.settings.WarnInterval {
			return
		}

		violation.WarnCount++
		violation.LastWarned = types.FlexibleTime{Time: now}
		g.deliverWarning(violation)
		pass.Warned++
		return
	}

	if !g.settings.Enforce {
		return
	}

	// The last warning also needs time to be acted on before signals start
	if now.Sub(violation.LastWarned.ToTime()) < g.settings.WarnInterval {
		return
	}
	if !violation.LastSignal.ToTime().IsZero() && now.Sub(violation.LastSignal.ToTime()) < g.settings.KillGrace {
		return
	}
	if len(violation.Signals) >= len(escalationSignals) {
		return
	}

	// An unidentified owner might be a system process - never terminate those
	if violation.User == "" || violation.User == "unknown" {
		return
	}

	if g.terminate(ctx, violation, now) {
		pass.Signalled++
	}
}

func (g *Guard) deliverWarning(violation *types.Violation) {
	delivered := g.notifier.Notify(violation, g.warningMessage(violation))

	if len(delivered) == 0 {
		fmt.Fprintf(g.output, "canhazgpu guard: %s (warning %d/%d, nobody could be reached)\n",
			violation.Describe(), violation.WarnCount, g.settings.MaxWarnings)
	}

	holderAccount := violation.HolderAccount
	if holderAccount == "" {
		holderAccount = violation.Holder
	}

	if violation.Kind == types.ViolationKindForeign && g.settings.NotifyHolder && holderAccount != "" {
		g.notifier.NotifyUser(holderAccount, fmt.Sprintf(
			"[canhazgpu] GPU %d that you reserved is being used by %s (PID %d, %dMB).\n"+
				"Check the details with: canhazgpu status",
			violation.GPUID, violation.User, violation.PID, violation.MemoryMB))
	}
}

// terminate sends the next signal in the escalation ladder. It reports whether a
// signal was actually sent.
func (g *Guard) terminate(ctx context.Context, violation *types.Violation, now time.Time) bool {
	step := escalationSignals[len(violation.Signals)]

	// Circuit breaker: a storm of terminations is more likely a bug in the
	// guard than a room full of policy violations
	kills, err := g.client.CountGuardKillsSince(ctx, now.Add(-time.Hour))
	if err != nil {
		fmt.Fprintf(g.output, "canhazgpu guard: cannot check the termination rate, skipping: %v\n", err)
		return false
	}
	if g.settings.MaxKillsPerHour > 0 && kills >= g.settings.MaxKillsPerHour {
		fmt.Fprintf(g.output,
			"canhazgpu guard: termination limit reached (%d in the last hour), only warning about %s\n",
			kills, violation.Describe())
		return false
	}

	if g.settings.DryRun {
		fmt.Fprintf(g.output, "canhazgpu guard: [dry run] would send %s to PID %d (%s)\n",
			step.name, violation.PID, violation.Describe())
		violation.Signals = append(violation.Signals, step.name+" (dry run)")
		violation.LastSignal = types.FlexibleTime{Time: now}
		return false
	}

	if err := syscall.Kill(violation.PID, step.signal); err != nil {
		if err == syscall.ESRCH {
			// Already gone; the next scan will resolve the violation
			return false
		}
		fmt.Fprintf(g.output, "canhazgpu guard: failed to send %s to PID %d: %v\n",
			step.name, violation.PID, err)
		if os.IsPermission(err) && os.Geteuid() != 0 {
			fmt.Fprintf(g.output, "canhazgpu guard: run the guard as root to terminate other users' processes\n")
		}
		return false
	}

	violation.Signals = append(violation.Signals, step.name)
	violation.LastSignal = types.FlexibleTime{Time: now}

	fmt.Fprintf(g.output, "canhazgpu guard: sent %s to PID %d (%s)\n",
		step.name, violation.PID, violation.Describe())

	g.notifier.Notify(violation, fmt.Sprintf(
		"[canhazgpu] %s was sent to PID %d on GPU %d: it kept using the GPU after %d warnings.\n"+
			"Reserve GPUs with 'canhazgpu run' or 'canhazgpu reserve' before starting a job.",
		step.name, violation.PID, violation.GPUID, violation.WarnCount))

	// Only the first signal counts against the hourly limit, so escalating to
	// SIGKILL on the same process does not eat the budget
	if len(violation.Signals) == 1 {
		if err := g.client.RecordGuardKill(ctx, now, violation.Describe()); err != nil {
			fmt.Fprintf(g.output, "canhazgpu guard: failed to record the termination: %v\n", err)
		}
	}

	return true
}

// resolveViolation closes a violation that is no longer observed and files it in
// the history
func (g *Guard) resolveViolation(ctx context.Context, violation *types.Violation, now time.Time) {
	violation.EndTime = types.FlexibleTime{Time: now}

	switch {
	case !isProcessAlive(violation.PID):
		violation.Resolution = "process exited"
	case len(violation.Signals) > 0:
		violation.Resolution = "stopped using the GPU after termination"
	default:
		violation.Resolution = "no longer using the GPU without a reservation"
	}

	if violation.WarnCount > 0 || len(violation.Signals) > 0 {
		fmt.Fprintf(g.output, "canhazgpu guard: resolved - %s (%s after %s)\n",
			violation.Describe(), violation.Resolution,
			utils.FormatDurationShort(violation.Duration()))
	}

	if err := g.client.RecordViolationHistory(ctx, violation); err != nil {
		fmt.Fprintf(g.output, "canhazgpu guard: failed to record violation history: %v\n", err)
	}
	if err := g.client.DeleteViolation(ctx, violation.ViolationID()); err != nil {
		fmt.Fprintf(g.output, "canhazgpu guard: failed to clear violation: %v\n", err)
	}
}

// warningMessage builds the text delivered to the offender
func (g *Guard) warningMessage(violation *types.Violation) string {
	var builder strings.Builder

	if violation.Kind == types.ViolationKindForeign {
		fmt.Fprintf(&builder,
			"[canhazgpu] GPU %d is reserved by %s, but your process is using it (PID %d, %dMB).\n"+
				"Move your job to a GPU you reserved:\n"+
				"  canhazgpu status                          # see what is free\n"+
				"  canhazgpu run --gpus 1 -- <your command>  # reserve and run",
			violation.GPUID, violation.Holder, violation.PID, violation.MemoryMB)
	} else {
		fmt.Fprintf(&builder,
			"[canhazgpu] GPU %d is being used without a reservation (PID %d, %dMB).\n"+
				"Please reserve GPUs before using them:\n"+
				"  canhazgpu run --gpus 1 -- <your command>  # reserve and run\n"+
				"  canhazgpu reserve --gpus 1 --duration 2h  # reserve for later",
			violation.GPUID, violation.PID, violation.MemoryMB)
	}

	fmt.Fprintf(&builder, "\nWarning %d of %d.", violation.WarnCount, g.settings.MaxWarnings)

	if g.settings.Enforce {
		if g.settings.DryRun {
			builder.WriteString(" Termination is being simulated, no signal will be sent.")
		} else {
			fmt.Fprintf(&builder, " After the last warning the process is terminated (%s later).",
				utils.FormatDurationShort(g.settings.WarnInterval))
		}
	}

	return builder.String()
}

// processOwner returns the account owning a process, or "unknown" when it could
// not be determined
func processOwner(process types.GPUProcessInfo) string {
	if process.User == "" {
		return "unknown"
	}
	return process.User
}

// processMemory reports how much memory a process uses, falling back to the
// GPU's total when per-process figures are unavailable
func processMemory(process types.GPUProcessInfo, usage *types.GPUUsage) int {
	if process.MemoryMB > 0 {
		return process.MemoryMB
	}
	return usage.MemoryMB
}

// isProcessAlive reports whether a PID still exists and is not a zombie.
// Zombies answer signal 0 but are effectively finished.
func isProcessAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	return !utils.ProcessIsZombie(pid)
}

// guardOwnerID identifies this guard instance in the singleton lock
func guardOwnerID() (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("failed to determine the hostname: %v", err)
	}
	return fmt.Sprintf("%s:%d", hostname, os.Getpid()), nil
}
