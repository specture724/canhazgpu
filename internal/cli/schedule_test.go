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

// A run reservation that started partway through the current slot has a window
// of just a few minutes. It still has to show up in that slot's cell.
func TestScheduleCellShowsPartialSlot(t *testing.T) {
	SetNoColor(true)
	t.Cleanup(func() { SetNoColor(false) })

	slot := time.Date(2026, 8, 14, 10, 0, 0, 0, time.Local)
	slotEnd := slot.Add(30 * time.Minute)

	// Started at 10:24, running until the end of the slot
	live := &scheduleEntry{
		user: "bob", gpuIDs: []int{0}, symbol: "a", kind: "run", openEnded: true,
		start: slot.Add(24 * time.Minute), end: slotEnd,
	}
	assert.Equal(t, "a", scheduleCell([]*scheduleEntry{live}, 0, slot, "alice"))
	assert.Equal(t, "·", scheduleCell([]*scheduleEntry{live}, 1, slot, "alice"), "other GPUs stay free")
	assert.Equal(t, "·", scheduleCell([]*scheduleEntry{live}, 0, slotEnd, "alice"), "and so does the next slot")

	// An aligned booking still fills exactly its own slots
	booking := &scheduleEntry{
		user: "bob", gpuIDs: []int{0}, symbol: "b", kind: "booking",
		start: slot, end: slotEnd,
	}
	assert.Equal(t, "b", scheduleCell([]*scheduleEntry{booking}, 0, slot, "alice"))
	assert.Equal(t, "·", scheduleCell([]*scheduleEntry{booking}, 0, slotEnd, "alice"),
		"a booking must not bleed into the slot where it ends")
}
