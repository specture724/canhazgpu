package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/russellb/canhazgpu/internal/types"
	"github.com/stretchr/testify/assert"
)

// The supervisor is a fresh process: without the Redis settings on its command
// line it talks to the default instance and immediately loses the reservation
// it was spawned to hold.
func TestBuildSupervisorArgs_ForwardsRedisSettings(t *testing.T) {
	config := &types.Config{RedisHost: "redis.example", RedisPort: 6380, RedisDB: 15, HostSocket: "/run/canhazgpu/host.sock", HostPID: 99999}

	args := buildSupervisorArgs("/usr/local/bin/canhazgpu", config, "1,2", "alice", 4242, "2h", 30*time.Minute)
	line := strings.Join(args, " ")

	assert.Equal(t, "/usr/local/bin/canhazgpu", args[0])
	assert.Equal(t, "supervisor", args[1])
	assert.Contains(t, line, "--redis-host redis.example")
	assert.Contains(t, line, "--redis-port 6380")
	assert.Contains(t, line, "--redis-db 15")
	assert.Contains(t, line, "--gpus 1,2")
	assert.Contains(t, line, "--user alice")
	assert.Contains(t, line, "--pid 4242")
	assert.Contains(t, line, "--host-socket /run/canhazgpu/host.sock")
	assert.NotContains(t, line, "--pid 99999", "the supervisor must monitor the local PID, not the stored host PID")
	assert.Contains(t, line, "--timeout 2h")
	assert.Contains(t, line, "--idle-timeout 30m")

	// Without a timeout the flag is left out entirely
	args = buildSupervisorArgs("canhazgpu", config, "0", "bob", 1, "", 0)
	assert.NotContains(t, strings.Join(args, " "), "--timeout")
	assert.NotContains(t, strings.Join(args, " "), "--idle-timeout")

	// "0" also disables the timeout and leaves the flag off the command line
	args = buildSupervisorArgs("canhazgpu", config, "0", "bob", 1, "0", 30*time.Minute)
	assert.NotContains(t, strings.Join(args, " "), "--timeout")
	assert.Contains(t, strings.Join(args, " "), "--idle-timeout 30m")
}
