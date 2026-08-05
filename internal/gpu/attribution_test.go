package gpu

import (
	"testing"

	"github.com/russellb/canhazgpu/internal/types"
	"github.com/stretchr/testify/assert"
)

func TestReservationAccount(t *testing.T) {
	assert.Equal(t, "alice", ReservationAccount(&types.GPUState{User: "Alice Smith", ActualUser: "alice"}),
		"the OS account wins over a custom display name")
	assert.Equal(t, "alice", ReservationAccount(&types.GPUState{User: "alice"}),
		"older reservations only have the display name")
	assert.Equal(t, "", ReservationAccount(&types.GPUState{}))
}

func TestIsReservationHolderActive(t *testing.T) {
	holder := &types.GPUState{User: "Alice Smith", ActualUser: "alice"}
	threshold := types.MemoryThresholdMB

	tests := []struct {
		name     string
		usage    *types.GPUUsage
		expected bool
	}{
		{
			name:     "No usage at all",
			usage:    nil,
			expected: false,
		},
		{
			name:     "Memory below the threshold",
			usage:    &types.GPUUsage{MemoryMB: threshold - 1},
			expected: false,
		},
		{
			name: "Holder's own process",
			usage: &types.GPUUsage{MemoryMB: 8452, Processes: []types.GPUProcessInfo{
				{PID: 1, User: "alice", MemoryMB: 8452},
			}},
			expected: true,
		},
		{
			name: "Only somebody else's process",
			usage: &types.GPUUsage{MemoryMB: 8452, Processes: []types.GPUProcessInfo{
				{PID: 1, User: "bob", MemoryMB: 8452},
			}},
			expected: false,
		},
		{
			name: "Holder alongside somebody else",
			usage: &types.GPUUsage{MemoryMB: 12000, Processes: []types.GPUProcessInfo{
				{PID: 1, User: "bob", MemoryMB: 4000},
				{PID: 2, User: "alice", MemoryMB: 8000},
			}},
			expected: true,
		},
		{
			name: "Unidentified owner gets the benefit of the doubt",
			usage: &types.GPUUsage{MemoryMB: 8452, Processes: []types.GPUProcessInfo{
				{PID: 1, User: "", MemoryMB: 8452},
			}},
			expected: true,
		},
		{
			name:     "Memory in use but no process list to attribute it to",
			usage:    &types.GPUUsage{MemoryMB: 8452},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, IsReservationHolderActive(holder, tt.usage, threshold))
		})
	}
}

func TestDetectForeignUsage(t *testing.T) {
	holder := &types.GPUState{User: "Alice Smith", ActualUser: "alice"}

	assert.Nil(t, DetectForeignUsage(&types.GPUState{}, &types.GPUUsage{MemoryMB: 8452}),
		"an unreserved GPU cannot have foreign usage")
	assert.Nil(t, DetectForeignUsage(holder, nil))
	assert.Nil(t, DetectForeignUsage(holder, &types.GPUUsage{MemoryMB: 8452, Processes: []types.GPUProcessInfo{
		{PID: 1, User: "alice", MemoryMB: 8452},
	}}), "the holder's own processes are not foreign")

	foreign := DetectForeignUsage(holder, &types.GPUUsage{MemoryMB: 12000, Processes: []types.GPUProcessInfo{
		{PID: 1, User: "alice", MemoryMB: 4000},
		{PID: 2, User: "bob", MemoryMB: 5000},
		{PID: 3, User: "bob", MemoryMB: 2000},
		{PID: 4, User: "carol", MemoryMB: 1000},
		{PID: 5, User: "", MemoryMB: 500},
	}})

	if assert.NotNil(t, foreign) {
		assert.Equal(t, []string{"bob", "carol"}, foreign.Users, "each foreign user is listed once")
		assert.Equal(t, 3, foreign.Processes)
		assert.Equal(t, 8000, foreign.MemoryMB, "only foreign processes count towards the memory")
	}
}
