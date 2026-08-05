package gpu

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/russellb/canhazgpu/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsTerminalDevice(t *testing.T) {
	assert.True(t, isTerminalDevice("/dev/pts/3"))
	assert.True(t, isTerminalDevice("/dev/tty"))
	assert.True(t, isTerminalDevice("/dev/tty1"))

	// Warnings must never be written into these
	assert.False(t, isTerminalDevice("/dev/null"))
	assert.False(t, isTerminalDevice("/home/alice/train.log"))
	assert.False(t, isTerminalDevice("pipe:[12345]"))
	assert.False(t, isTerminalDevice("socket:[12345]"))
}

func TestWriteToProcessStderr_SkipsNonTerminals(t *testing.T) {
	// A process of our own whose stderr goes nowhere near a terminal
	logFile := filepath.Join(t.TempDir(), "job.log")
	file, err := os.Create(logFile)
	require.NoError(t, err)
	defer func() { _ = file.Close() }()

	victim := exec.Command("sleep", "30")
	victim.Stderr = file
	require.NoError(t, victim.Start())
	t.Cleanup(func() {
		_ = victim.Process.Kill()
		_ = victim.Wait()
	})

	err = writeToProcessStderr(victim.Process.Pid, "should not be written")
	require.Error(t, err)
	assert.True(t, isEmptyTargetError(err),
		"a job's log file is not a delivery failure, just not a terminal")

	content, readErr := os.ReadFile(logFile)
	require.NoError(t, readErr)
	assert.Empty(t, content, "the job's own output must not be polluted")
}

func TestWriteToProcessStderr_MissingProcess(t *testing.T) {
	err := writeToProcessStderr(1<<30, "nobody home")
	require.Error(t, err)
	assert.False(t, isEmptyTargetError(err), "a missing process is a real error")
}

func TestWriteToUserTerminals_UnknownUser(t *testing.T) {
	err := writeToUserTerminals("no-such-user-here", "hello")
	require.Error(t, err)
	assert.True(t, isEmptyTargetError(err))

	err = writeToUserTerminals("", "hello")
	require.Error(t, err)
	assert.True(t, isEmptyTargetError(err))
}

func TestMultiNotifier_LogChannel(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "violations.log")

	output, err := os.CreateTemp(t.TempDir(), "guard-output")
	require.NoError(t, err)
	defer func() { _ = output.Close() }()

	notifier := NewMultiNotifier([]string{ChannelLog}, logFile)
	notifier.Output = output

	violation := &types.Violation{
		GPUID: 2, PID: 4242, User: "bob", Kind: types.ViolationKindUnreserved, MemoryMB: 8452,
	}

	delivered := notifier.Notify(violation, "please reserve your GPUs")
	assert.Equal(t, []string{ChannelLog}, delivered)

	content, err := os.ReadFile(logFile)
	require.NoError(t, err)
	assert.Contains(t, string(content), "GPU 2 has no reservation")
	assert.Contains(t, string(content), "please reserve your GPUs")
	assert.Contains(t, string(content), types.ViolationKindUnreserved)
}

func TestMultiNotifier_UnknownChannelIsReported(t *testing.T) {
	output, err := os.CreateTemp(t.TempDir(), "guard-output")
	require.NoError(t, err)
	defer func() { _ = output.Close() }()

	notifier := NewMultiNotifier([]string{"carrier-pigeon"}, "")
	notifier.Output = output

	delivered := notifier.Notify(&types.Violation{GPUID: 0, PID: 1, User: "bob"}, "hello")
	assert.Empty(t, delivered)

	content, err := os.ReadFile(output.Name())
	require.NoError(t, err)
	assert.Contains(t, string(content), "unknown notification channel")
}
