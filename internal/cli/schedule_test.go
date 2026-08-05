package cli

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestFormatNowCell(t *testing.T) {
	// Compare plain text, without ANSI colors
	SetNoColor(true)
	t.Cleanup(func() { SetNoColor(false) })

	tests := []struct {
		name     string
		info     gpuNow
		expected string
	}{
		{
			name:     "Free GPU",
			info:     gpuNow{status: "AVAILABLE"},
			expected: "free",
		},
		{
			name:     "Free GPU with leftover memory",
			info:     gpuNow{status: "AVAILABLE", memoryMB: 45},
			expected: "free (45MB used)",
		},
		{
			name:     "Reserved and busy",
			info:     gpuNow{status: "IN_USE", user: "alice", kind: "manual", memoryMB: 8452},
			expected: "alice MANUAL 8452MB",
		},
		{
			name:     "Reserved by a booking and idle",
			info:     gpuNow{status: "IN_USE", user: "alice", kind: "manual", booked: true, idleFor: 4 * time.Minute, idleTimeout: 15 * time.Minute},
			expected: "alice MANUAL booked idle 4m",
		},
		{
			name:     "Reserved, idle for less than a minute",
			info:     gpuNow{status: "IN_USE", user: "bob", kind: "run", idleFor: 10 * time.Second, idleTimeout: 15 * time.Minute},
			expected: "bob RUN",
		},
		{
			name:     "Used without a reservation",
			info:     gpuNow{status: "UNRESERVED", user: "carol", memoryMB: 2048},
			expected: "carol UNRESERVED 2048MB",
		},
		{
			name:     "Unknown state",
			info:     gpuNow{},
			expected: "?",
		},
		{
			name:     "Error state",
			info:     gpuNow{status: "ERROR"},
			expected: "error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, formatNowCell(tt.info))
		})
	}
}
