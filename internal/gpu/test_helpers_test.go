package gpu

import (
	"os/exec"
	"testing"
)

// gpuTestRedisDB is dedicated to Redis-backed tests in this package. Go runs
// packages concurrently, so it must not overlap with another package's test DB.
const gpuTestRedisDB = 14

// isNvidiaSmiAvailable checks if nvidia-smi command is available
// This is used by tests to skip tests that require nvidia-smi when it's not present
func isNvidiaSmiAvailable() bool {
	_, err := exec.LookPath("nvidia-smi")
	return err == nil
}

// isAmdSmiAvailable checks if amd-smi command is available
// This is used by tests to skip tests that require amd-smi when it's not present
func isAmdSmiAvailable() bool {
	_, err := exec.LookPath("amd-smi")
	return err == nil
}

// isAscendSmiAvailable checks that npu-smi can query the devices for the
// current user, rather than merely checking that its binary is on PATH.
func isAscendSmiAvailable() bool {
	return NewAscendProvider().IsAvailable()
}

// isAnyGPUProviderAvailable checks if any GPU provider is available
func isAnyGPUProviderAvailable() bool {
	return isNvidiaSmiAvailable() || isAmdSmiAvailable() || isAscendSmiAvailable()
}

// TestIsNvidiaSmiAvailable tests the helper function itself
func TestIsNvidiaSmiAvailable(t *testing.T) {
	available := isNvidiaSmiAvailable()
	t.Logf("nvidia-smi availability: %v", available)

	// This test just documents the current state, doesn't assert a specific value
	// since it depends on the test environment
}

// TestIsAmdSmiAvailable tests the helper function itself
func TestIsAmdSmiAvailable(t *testing.T) {
	available := isAmdSmiAvailable()
	t.Logf("amd-smi availability: %v", available)

	// This test just documents the current state, doesn't assert a specific value
	// since it depends on the test environment
}

func TestIsAscendSmiAvailable(t *testing.T) {
	available := isAscendSmiAvailable()
	t.Logf("npu-smi availability: %v", available)
}
