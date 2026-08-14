package cli

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/russellb/canhazgpu/internal/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsProcessRunning_Zombie(t *testing.T) {
	// A zombie still answers signal 0 but is finished for our purposes
	cmd := exec.Command("sh", "-c", "exit 0")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid
	defer func() { _, _ = cmd.Process.Wait() }() // reap the zombie at the end

	deadline := time.Now().Add(5 * time.Second)
	zombie := false
	for time.Now().Before(deadline) {
		if utils.ProcessIsZombie(pid) {
			zombie = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.True(t, zombie, "test child did not become a zombie")

	// The zombie still exists as far as signal 0 is concerned...
	require.NoError(t, syscall.Kill(pid, 0))
	// ...but the supervisor must treat it as finished and release the GPU
	assert.False(t, isProcessRunning(pid))
}

func TestProcessState_CurrentProcess(t *testing.T) {
	assert.False(t, utils.ProcessIsZombie(os.Getpid()), "the test process itself is not a zombie")
}
