package gpu

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/russellb/canhazgpu/internal/types"
	"github.com/russellb/canhazgpu/internal/utils"
)

// getProcessOwner determines the owner of a process
func getProcessOwner(pid int) (string, error) {
	// Try /proc filesystem first
	user, err := getProcessOwnerFromProc(pid)
	if err != nil {
		// Fallback to ps command
		user, err = getProcessOwnerFromPS(pid)
		if err != nil {
			return "", err
		}
	}

	return user, nil
}

// GetProcessOwner is the exported version for tests
func GetProcessOwner(pid int) (string, error) {
	return getProcessOwner(pid)
}

// getProcessOwnerFromProc reads process owner from /proc filesystem
func getProcessOwnerFromProc(pid int) (string, error) {
	statusFile := fmt.Sprintf("/proc/%d/status", pid)

	file, err := os.Open(statusFile)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := file.Close(); err != nil {
			fmt.Printf("Warning: failed to close file: %v\n", err)
		}
	}()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Uid:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				uid, err := strconv.Atoi(fields[1])
				if err != nil {
					return "", err
				}
				return utils.GetUsernameFromUID(uid)
			}
		}
	}

	return "", fmt.Errorf("could not find UID in %s", statusFile)
}

// getProcessOwnerFromPS uses ps command to get process owner
func getProcessOwnerFromPS(pid int) (string, error) {
	cmd := exec.Command("ps", "-o", "user=", "-p", strconv.Itoa(pid))
	output, err := cmd.Output()
	if err != nil {
		return "", err
	}

	user := strings.TrimSpace(string(output))
	if user == "" {
		return "", fmt.Errorf("empty user from ps command")
	}

	return user, nil
}

// getProcessElapsedSeconds reports how long a process has been running, or 0
// when the duration cannot be determined. The primary source is /proc (process
// start time vs. system uptime); ps -o etimes is the fallback.
func getProcessElapsedSeconds(pid int) int64 {
	if seconds, err := processElapsedFromProc(pid); err == nil {
		return seconds
	}
	if seconds, err := processElapsedFromPS(pid); err == nil {
		return seconds
	}
	return 0
}

// processElapsedFromProc reads /proc/<pid>/stat and /proc/uptime (Linux).
func processElapsedFromProc(pid int) (int64, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	uptime, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, err
	}

	// comm may contain spaces and parentheses, so split after the last ')'.
	// Field 22 (starttime, in clock ticks since boot) is index 19 after that.
	rest := string(stat)
	if idx := strings.LastIndexByte(rest, ')'); idx >= 0 {
		rest = rest[idx+1:]
	}
	fields := strings.Fields(rest)
	if len(fields) < 20 {
		return 0, fmt.Errorf("unexpected /proc stat format")
	}
	startTicks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0, err
	}

	upParts := strings.Fields(string(uptime))
	if len(upParts) == 0 {
		return 0, fmt.Errorf("unexpected /proc/uptime format")
	}
	upSeconds, err := strconv.ParseFloat(upParts[0], 64)
	if err != nil {
		return 0, err
	}

	// Linux USER_HZ is 100 on virtually every distribution.
	elapsed := upSeconds - float64(startTicks)/100.0
	if elapsed < 0 {
		elapsed = 0
	}
	return int64(elapsed), nil
}

// processElapsedFromPS uses ps -o etimes to get elapsed seconds.
func processElapsedFromPS(pid int) (int64, error) {
	cmd := exec.Command("ps", "-o", "etimes=", "-p", strconv.Itoa(pid))
	output, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	seconds, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil {
		return 0, err
	}
	if seconds < 0 {
		seconds = 0
	}
	return seconds, nil
}

// formatProcessInfo renders the processes using a GPU as
// "PID 12345 (python, 2h 3m), PID 67890 (jupyter, 5m)".
func formatProcessInfo(processes []types.GPUProcessInfo) string {
	parts := make([]string, 0, len(processes))
	for _, proc := range processes {
		desc := fmt.Sprintf("PID %d (%s", proc.PID, proc.ProcessName)
		if proc.ElapsedSeconds > 0 {
			desc += ", " + utils.FormatDurationShort(time.Duration(proc.ElapsedSeconds)*time.Second)
		}
		desc += ")"
		parts = append(parts, desc)
	}
	return strings.Join(parts, ", ")
}

// FilterOutClaimableOwnedGPUs removes GPUs whose unreserved processes all
// belong to user from the unreserved list, so their owner can claim them
// immediately. A GPU with any unowned or unidentified process stays in the
// list (and therefore stays out of reach for the claiming request).
func FilterOutClaimableOwnedGPUs(unreserved []int, usage map[int]*types.GPUUsage, user string) []int {
	if user == "" {
		return unreserved
	}

	claimable := make(map[int]bool)
	for _, gpuID := range unreserved {
		gpuUsage := usage[gpuID]
		if gpuUsage == nil || len(gpuUsage.Processes) == 0 {
			continue
		}

		onlyMine := true
		for _, proc := range gpuUsage.Processes {
			if proc.User == "" || proc.User != user {
				onlyMine = false
				break
			}
		}
		if onlyMine {
			claimable[gpuID] = true
		}
	}

	if len(claimable) == 0 {
		return unreserved
	}

	filtered := make([]int, 0, len(unreserved))
	for _, gpuID := range unreserved {
		if !claimable[gpuID] {
			filtered = append(filtered, gpuID)
		}
	}
	return filtered
}

// GetUnreservedGPUs returns list of GPU IDs that are in use without proper reservations
func GetUnreservedGPUs(ctx context.Context, usage map[int]*types.GPUUsage, memoryThreshold int) []int {
	var unreserved []int

	for gpuID, gpuUsage := range usage {
		if gpuUsage.MemoryMB > memoryThreshold {
			unreserved = append(unreserved, gpuID)
		}
	}

	return unreserved
}

// IsGPUInUnreservedUse checks if a specific GPU is in unreserved use
func IsGPUInUnreservedUse(usage *types.GPUUsage, memoryThreshold int) bool {
	return IsGPUBusy(usage, memoryThreshold)
}

// IsGPUBusy reports whether real usage was detected on a GPU, independently of
// who is responsible for it
func IsGPUBusy(usage *types.GPUUsage, memoryThreshold int) bool {
	return usage != nil && usage.MemoryMB > memoryThreshold
}

// ReservationAccount returns the OS account a reservation belongs to. The
// display name is only a fallback: it may be a custom --user value.
func ReservationAccount(state *types.GPUState) string {
	if state.ActualUser != "" {
		return state.ActualUser
	}
	return state.User
}

// IsReservationHolderActive reports whether the user holding a reservation is
// the one actually using the GPU. Usage by anybody else must not keep the
// reservation alive: otherwise somebody squatting on a reserved GPU would hold
// it open indefinitely on the holder's behalf.
//
// When usage cannot be attributed - no process list, or processes whose owner
// could not be determined - the reservation is given the benefit of the doubt,
// so an unverifiable reservation is never released as idle.
func IsReservationHolderActive(state *types.GPUState, usage *types.GPUUsage, memoryThreshold int) bool {
	if !IsGPUBusy(usage, memoryThreshold) {
		return false
	}

	if len(usage.Processes) == 0 {
		return true
	}

	holder := ReservationAccount(state)
	for _, process := range usage.Processes {
		if process.User == "" || process.User == holder {
			return true
		}
	}

	return false
}

// ForeignUsage describes GPU usage on a reserved GPU that belongs to somebody
// other than the reservation holder
type ForeignUsage struct {
	Users     []string
	Processes int
	MemoryMB  int
}

// DetectForeignUsage reports processes running on a reserved GPU that do not
// belong to the reservation holder. It returns nil when there are none.
func DetectForeignUsage(state *types.GPUState, usage *types.GPUUsage) *ForeignUsage {
	if state.User == "" || usage == nil {
		return nil
	}

	holder := ReservationAccount(state)
	foreign := &ForeignUsage{}
	seen := make(map[string]bool)

	for _, process := range usage.Processes {
		// An unidentified owner might well be the holder's own process
		if process.User == "" || process.User == holder {
			continue
		}

		foreign.Processes++
		foreign.MemoryMB += process.MemoryMB
		if !seen[process.User] {
			seen[process.User] = true
			foreign.Users = append(foreign.Users, process.User)
		}
	}

	if foreign.Processes == 0 {
		return nil
	}

	return foreign
}
