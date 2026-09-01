package gpu

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/russellb/canhazgpu/internal/types"
)

const (
	// AscendProviderName is the canonical provider name stored in Redis.
	AscendProviderName = "ascend"

	// AscendVisibleDevicesEnv is the CANN runtime variable that restricts a
	// process to a set of logical Ascend device IDs.
	AscendVisibleDevicesEnv = "ASCEND_RT_VISIBLE_DEVICES"
)

// AscendProvider implements GPUProvider for Huawei Ascend NPUs using npu-smi.
// Despite the GPUProvider name, the reservation model applies equally to NPUs.
type AscendProvider struct{}

// NewAscendProvider creates an Ascend NPU provider.
func NewAscendProvider() *AscendProvider {
	return &AscendProvider{}
}

// Name returns the provider name used in configuration and Redis.
func (a *AscendProvider) Name() string {
	return AscendProviderName
}

// IsAvailable verifies that npu-smi can query the devices for the current
// user. Checking only that the binary exists would incorrectly report success
// when the user is not in the Ascend runtime group.
func (a *AscendProvider) IsAvailable() bool {
	return exec.Command("npu-smi", "info").Run() == nil
}

// DetectGPUUsage reads device and process information from one npu-smi call.
func (a *AscendProvider) DetectGPUUsage(ctx context.Context) (map[int]*types.GPUUsage, error) {
	usage, err := a.queryGPUUsage(ctx)
	if err != nil {
		return nil, err
	}

	for _, npuUsage := range usage {
		for i := range npuUsage.Processes {
			process := &npuUsage.Processes[i]
			user, err := getProcessOwner(process.PID)
			if err != nil {
				user = "unknown"
			}
			process.User = user
			process.ElapsedSeconds = getProcessElapsedSeconds(process.PID)
			npuUsage.Users[user] = true
		}
	}

	return usage, nil
}

// GetGPUCount returns the number of logical Ascend devices reported by
// npu-smi. Logical IDs are the IDs accepted by ASCEND_RT_VISIBLE_DEVICES.
func (a *AscendProvider) GetGPUCount(ctx context.Context) (int, error) {
	usage, err := a.queryGPUUsage(ctx)
	if err != nil {
		return 0, err
	}
	return len(usage), nil
}

func (a *AscendProvider) queryGPUUsage(ctx context.Context) (map[int]*types.GPUUsage, error) {
	output, err := exec.CommandContext(ctx, "npu-smi", "info").CombinedOutput()
	if err != nil {
		if details := strings.TrimSpace(string(output)); details != "" {
			return nil, fmt.Errorf("npu-smi info failed: %w: %s", err, details)
		}
		return nil, fmt.Errorf("npu-smi info failed: %w", err)
	}

	usage, err := parseAscendSMIOutput(string(output))
	if err != nil {
		return nil, fmt.Errorf("failed to parse npu-smi info: %w", err)
	}
	return usage, nil
}

type ascendDeviceInfo struct {
	model              string
	utilizationPercent int
}

// parseAscendSMIOutput parses the human-readable table emitted by
// "npu-smi info". npu-smi reports a persistent HBM allocation even when a
// device has no user process, so MemoryMB is derived from its process table
// instead of the HBM-Usage column.
func parseAscendSMIOutput(output string) (map[int]*types.GPUUsage, error) {
	devices := make(map[int]*ascendDeviceInfo)
	processes := make(map[int][]types.GPUProcessInfo)
	inProcessTable := false
	currentNPU := -1

	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		lowerLine := strings.ToLower(line)
		if strings.Contains(lowerLine, "process id") && strings.Contains(lowerLine, "process memory") {
			inProcessTable = true
			continue
		}

		fields := splitSMITableRow(line)
		if len(fields) == 0 {
			continue
		}

		if inProcessTable {
			if npuID, process, ok := parseAscendProcessRow(fields); ok {
				processes[npuID] = append(processes[npuID], process)
			}
			continue
		}

		npuID, model, ok := parseAscendDeviceRow(fields)
		if !ok {
			continue
		}

		if model != "" {
			device, exists := devices[npuID]
			if !exists {
				device = &ascendDeviceInfo{}
				devices[npuID] = device
			}
			device.model = model
			currentNPU = npuID
			continue
		}

		// The detail row starts with the Chip ID, not the logical NPU ID.
		// It belongs to the preceding model row.
		if device, exists := devices[currentNPU]; exists {
			if utilization, ok := parseAscendUtilization(fields); ok {
				device.utilizationPercent = utilization
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(devices) == 0 {
		return nil, fmt.Errorf("no Ascend NPUs found in npu-smi output")
	}

	usage := make(map[int]*types.GPUUsage, len(devices))
	for npuID, device := range devices {
		npuProcesses := processes[npuID]
		memoryMB := 0
		for _, process := range npuProcesses {
			memoryMB += process.MemoryMB
		}

		usage[npuID] = &types.GPUUsage{
			GPUID:              npuID,
			MemoryMB:           memoryMB,
			UtilizationPercent: device.utilizationPercent,
			Processes:          npuProcesses,
			Users:              make(map[string]bool),
			Provider:           "Ascend",
			Model:              device.model,
		}
	}

	return usage, nil
}

func splitSMITableRow(line string) []string {
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return nil
	}

	parts := strings.Split(line, "|")
	if len(parts) < 3 {
		return nil
	}

	fields := make([]string, 0, len(parts)-2)
	for _, part := range parts[1 : len(parts)-1] {
		fields = append(fields, strings.TrimSpace(part))
	}
	return fields
}

func parseAscendDeviceRow(fields []string) (int, string, bool) {
	if len(fields) == 0 {
		return 0, "", false
	}

	parts := strings.Fields(fields[0])
	if len(parts) == 0 {
		return 0, "", false
	}

	npuID, err := strconv.Atoi(parts[0])
	if err != nil || npuID < 0 {
		return 0, "", false
	}
	return npuID, strings.Join(parts[1:], " "), true
}

func parseAscendUtilization(fields []string) (int, bool) {
	if len(fields) == 0 {
		return 0, false
	}

	// In npu-smi 25.5 the last table cell combines AICore, memory, and HBM
	// counters. AICore is its first number. The first cell is a Chip ID and
	// must not be treated as utilization.
	value, ok := firstNonNegativeInt(fields[len(fields)-1])
	if ok && value <= 100 {
		return value, true
	}
	return 0, false
}

func parseAscendProcessRow(fields []string) (int, types.GPUProcessInfo, bool) {
	if len(fields) < 4 {
		return 0, types.GPUProcessInfo{}, false
	}

	// Current 25.5 output combines NPU and Chip in its first cell, followed
	// by Process id, Process name, and Process memory. Older releases can use
	// separate NPU and Chip cells, so accept both layouts.
	npuID, ok := firstNonNegativeInt(fields[0])
	if !ok {
		return 0, types.GPUProcessInfo{}, false
	}

	pidIndex := 1
	if len(fields) >= 5 {
		pidIndex = 2
	}
	pid, ok := firstNonNegativeInt(fields[pidIndex])
	if !ok || pid == 0 {
		return 0, types.GPUProcessInfo{}, false
	}

	nameIndex := pidIndex + 1
	memoryIndex := nameIndex + 1
	if memoryIndex >= len(fields) {
		return 0, types.GPUProcessInfo{}, false
	}
	memoryMB, _ := firstNonNegativeInt(fields[memoryIndex])

	return npuID, types.GPUProcessInfo{
		PID:         pid,
		ProcessName: strings.TrimSpace(fields[nameIndex]),
		MemoryMB:    memoryMB,
	}, true
}

func firstNonNegativeInt(value string) (int, bool) {
	for _, field := range strings.Fields(value) {
		number, err := strconv.Atoi(field)
		if err == nil && number >= 0 {
			return number, true
		}
	}
	return 0, false
}
