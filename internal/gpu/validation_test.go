package gpu

import (
	"context"
	"os"
	"testing"

	"github.com/russellb/canhazgpu/internal/types"
	"github.com/stretchr/testify/assert"
)

// TestParseNvidiaSmiOutput would test parsing nvidia-smi output
// This requires implementing parseNvidiaSmiOutput function if it's internal
// The parsing logic is currently part of queryGPUMemory and queryGPUProcesses
// Skipping detailed parsing tests and testing the public interface instead

func TestGetProcessElapsedSeconds(t *testing.T) {
	// The test process itself must have a resolvable elapsed time on any
	// platform with /proc or ps.
	assert.GreaterOrEqual(t, getProcessElapsedSeconds(os.Getpid()), int64(0))
}

func TestFormatProcessInfo(t *testing.T) {
	processes := []types.GPUProcessInfo{
		{PID: 12345, ProcessName: "python", MemoryMB: 8452, ElapsedSeconds: 7395},
		{PID: 67890, ProcessName: "jupyter", MemoryMB: 512, ElapsedSeconds: 300},
		{PID: 111, ProcessName: "train", MemoryMB: 0},
	}

	got := formatProcessInfo(processes)
	assert.Equal(t,
		"PID 12345 (python, 2h3m), PID 67890 (jupyter, 5m), PID 111 (train)",
		got)
	assert.Empty(t, formatProcessInfo(nil))
}

func TestGetProcessOwner(t *testing.T) {
	tests := []struct {
		name      string
		pid       int
		wantError bool
	}{
		{
			name:      "Current process",
			pid:       1, // init process should exist
			wantError: false,
		},
		{
			name:      "Invalid PID",
			pid:       -1,
			wantError: true,
		},
		{
			name:      "Non-existent PID",
			pid:       999999,
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user, err := getProcessOwner(tt.pid)

			if tt.wantError {
				assert.Error(t, err)
				assert.Empty(t, user)
			} else {
				assert.NoError(t, err)
				assert.NotEmpty(t, user)
			}
		})
	}
}

// TestFilterGPUUsage would test GPU usage filtering
// This requires implementing filterGPUUsage function if it's internal
// The filtering logic is currently part of the main DetectGPUUsage function
// Skipping internal function tests

func TestDetectGPUUsage_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	t.Log("Starting GPU provider integration test - may take 5-10 seconds or timeout")
	t.Log("This test uses the GPU provider system (NVIDIA/AMD/Ascend)")

	// Use the new GPU Provider system
	pm := NewProviderManager()
	availableProviders := pm.GetAvailableProviders()

	if len(availableProviders) == 0 {
		t.Skip("Skipping test: no GPU providers available (nvidia-smi, amd-smi, npu-smi unavailable)")
	}

	t.Logf("Found %d available provider(s):", len(availableProviders))
	for _, provider := range availableProviders {
		t.Logf("  - %s provider", provider.Name())
	}

	// Test GPU usage detection with available providers
	usage, err := pm.DetectAllGPUUsage(context.Background())

	// If no providers are available, the function should handle it gracefully
	if err != nil {
		t.Logf("GPU detection failed: %v (this is expected on non-GPU systems)", err)
		// Should return empty usage, not crash
		assert.Empty(t, usage)
		return
	}

	t.Log("GPU detection completed successfully")

	// If successful, usage should be a valid map
	assert.NotNil(t, usage)

	// Each GPU usage should have valid data
	for gpuID, gpuUsage := range usage {
		assert.GreaterOrEqual(t, gpuID, 0)
		assert.Equal(t, gpuID, gpuUsage.GPUID)
		assert.GreaterOrEqual(t, gpuUsage.MemoryMB, 0) // Memory can be 0 or more

		t.Logf("GPU %d: %dMB memory usage, %d processes", gpuID, gpuUsage.MemoryMB, len(gpuUsage.Processes))

		// Each process should have valid data
		for _, proc := range gpuUsage.Processes {
			assert.Greater(t, proc.PID, 0)
			assert.NotEmpty(t, proc.ProcessName)
			assert.GreaterOrEqual(t, proc.MemoryMB, 0)
			assert.NotEmpty(t, proc.User) // Should have a user (not "unknown")
		}
	}
}

func TestFilterOutClaimableOwnedGPUs(t *testing.T) {
	usage := map[int]*types.GPUUsage{
		0: {GPUID: 0, MemoryMB: 5000, Processes: []types.GPUProcessInfo{
			{PID: 1, User: "alice", MemoryMB: 4000},
			{PID: 2, User: "alice", MemoryMB: 1000},
		}},
		1: {GPUID: 1, MemoryMB: 5000, Processes: []types.GPUProcessInfo{
			{PID: 3, User: "alice", MemoryMB: 4000},
			{PID: 4, User: "bob", MemoryMB: 1000},
		}},
		2: {GPUID: 2, MemoryMB: 5000, Processes: []types.GPUProcessInfo{
			{PID: 5, User: "", MemoryMB: 4000},
		}},
		3: {GPUID: 3, MemoryMB: 5000, Processes: []types.GPUProcessInfo{}},
	}

	// GPU 0 is exclusively alice's and can be claimed; GPUs 1-3 cannot
	result := FilterOutClaimableOwnedGPUs([]int{0, 1, 2, 3}, usage, "alice")
	assert.ElementsMatch(t, []int{1, 2, 3}, result)

	// Another user cannot claim anything alice owns
	result = FilterOutClaimableOwnedGPUs([]int{0, 1, 2, 3}, usage, "carol")
	assert.ElementsMatch(t, []int{0, 1, 2, 3}, result)

	// No user means nothing is claimable
	result = FilterOutClaimableOwnedGPUs([]int{0, 1, 2, 3}, usage, "")
	assert.ElementsMatch(t, []int{0, 1, 2, 3}, result)

	// No claimable GPUs keeps the list intact
	result = FilterOutClaimableOwnedGPUs([]int{1, 2, 3}, usage, "alice")
	assert.ElementsMatch(t, []int{1, 2, 3}, result)
}

func TestGetUnreservedGPUs(t *testing.T) {
	// Test the threshold logic that determines unreserved usage
	usage := map[int]*types.GPUUsage{
		0: {GPUID: 0, MemoryMB: 50},                           // Below threshold - authorized
		1: {GPUID: 1, MemoryMB: types.MemoryThresholdMB + 50}, // Above threshold - unreserved
		2: {GPUID: 2, MemoryMB: types.MemoryThresholdMB},      // At threshold - authorized (uses > not >=)
		3: {GPUID: 3, MemoryMB: 0},                            // No usage - authorized
	}

	unreserved := GetUnreservedGPUs(context.Background(), usage, types.MemoryThresholdMB)

	// Should find only GPU 1 (>1024MB threshold, not >=
	expected := []int{1}
	assert.ElementsMatch(t, expected, unreserved)
}

func TestIsGPUInUnreservedUse(t *testing.T) {
	tests := []struct {
		name     string
		usage    *types.GPUUsage
		expected bool
	}{
		{
			name:     "Nil usage",
			usage:    nil,
			expected: false,
		},
		{
			name:     "Below threshold",
			usage:    &types.GPUUsage{MemoryMB: 50},
			expected: false,
		},
		{
			name:     "At threshold",
			usage:    &types.GPUUsage{MemoryMB: types.MemoryThresholdMB},
			expected: false, // Uses > not >=, so exactly 1024MB is authorized
		},
		{
			name:     "Above threshold",
			usage:    &types.GPUUsage{MemoryMB: 1536},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsGPUInUnreservedUse(tt.usage, types.MemoryThresholdMB)
			assert.Equal(t, tt.expected, result)
		})
	}
}
