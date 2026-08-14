package cli

import (
	"strings"
	"testing"

	"github.com/russellb/canhazgpu/internal/types"
	"github.com/stretchr/testify/assert"
)

// The supervisor is a fresh process: without the Redis settings on its command
// line it talks to the default instance and immediately loses the reservation
// it was spawned to hold.
func TestBuildSupervisorArgs_ForwardsRedisSettings(t *testing.T) {
	config := &types.Config{RedisHost: "redis.example", RedisPort: 6380, RedisDB: 15}

	args := buildSupervisorArgs("/usr/local/bin/canhazgpu", config, "1,2", "alice", 4242, "2h")
	line := strings.Join(args, " ")

	assert.Equal(t, "/usr/local/bin/canhazgpu", args[0])
	assert.Equal(t, "supervisor", args[1])
	assert.Contains(t, line, "--redis-host redis.example")
	assert.Contains(t, line, "--redis-port 6380")
	assert.Contains(t, line, "--redis-db 15")
	assert.Contains(t, line, "--gpus 1,2")
	assert.Contains(t, line, "--user alice")
	assert.Contains(t, line, "--pid 4242")
	assert.Contains(t, line, "--timeout 2h")

	// Without a timeout the flag is left out entirely
	args = buildSupervisorArgs("canhazgpu", config, "0", "bob", 1, "")
	assert.NotContains(t, strings.Join(args, " "), "--timeout")
}
