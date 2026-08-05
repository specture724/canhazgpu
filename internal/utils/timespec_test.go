package utils

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTimeSpecFrom(t *testing.T) {
	ref := time.Date(2026, 8, 3, 13, 42, 0, 0, time.Local)

	tests := []struct {
		name     string
		input    string
		expected time.Time
		wantErr  bool
	}{
		{
			name:     "Clock time later today",
			input:    "14:00",
			expected: time.Date(2026, 8, 3, 14, 0, 0, 0, time.Local),
		},
		{
			name:     "Clock time already passed rolls to tomorrow",
			input:    "09:00",
			expected: time.Date(2026, 8, 4, 9, 0, 0, 0, time.Local),
		},
		{
			name:     "Clock time with seconds",
			input:    "14:00:30",
			expected: time.Date(2026, 8, 3, 14, 0, 30, 0, time.Local),
		},
		{
			name:     "Tomorrow keyword pins the date",
			input:    "tomorrow 09:00",
			expected: time.Date(2026, 8, 4, 9, 0, 0, 0, time.Local),
		},
		{
			name:     "Today keyword does not roll forward",
			input:    "today 09:00",
			expected: time.Date(2026, 8, 3, 9, 0, 0, 0, time.Local),
		},
		{
			name:     "Explicit date and time with space",
			input:    "2026-08-05 09:30",
			expected: time.Date(2026, 8, 5, 9, 30, 0, 0, time.Local),
		},
		{
			name:     "Explicit date and time with T separator",
			input:    "2026-08-05T09:30",
			expected: time.Date(2026, 8, 5, 9, 30, 0, 0, time.Local),
		},
		{
			name:     "Bare date means midnight",
			input:    "2026-08-05",
			expected: time.Date(2026, 8, 5, 0, 0, 0, 0, time.Local),
		},
		{
			name:     "Relative offset",
			input:    "+2h",
			expected: ref.Add(2 * time.Hour),
		},
		{
			name:     "now",
			input:    "now",
			expected: ref,
		},
		{
			name:    "Empty input",
			input:   "",
			wantErr: true,
		},
		{
			name:    "Garbage input",
			input:   "half past two",
			wantErr: true,
		},
		{
			name:    "Out of range clock time",
			input:   "25:00",
			wantErr: true,
		},
		{
			name:    "Invalid relative offset",
			input:   "+2weeks",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseTimeSpecFrom(tt.input, ref)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.True(t, tt.expected.Equal(result),
				"expected %s, got %s", tt.expected, result)
		})
	}
}

// An end time is parsed relative to the start time, so "16:00" after a start of
// "14:00" stays on the same day while an earlier clock time moves to the next
func TestParseTimeSpecFrom_EndRelativeToStart(t *testing.T) {
	start := time.Date(2026, 8, 3, 22, 0, 0, 0, time.Local)

	end, err := ParseTimeSpecFrom("23:30", start)
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 8, 3, 23, 30, 0, 0, time.Local), end)

	end, err = ParseTimeSpecFrom("02:00", start)
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 8, 4, 2, 0, 0, 0, time.Local), end)
}

func TestParseDaySpec(t *testing.T) {
	now := time.Date(2026, 8, 3, 13, 42, 0, 0, time.Local)

	tests := []struct {
		name     string
		input    string
		expected time.Time
		wantErr  bool
	}{
		{name: "Empty means today", input: "", expected: time.Date(2026, 8, 3, 0, 0, 0, 0, time.Local)},
		{name: "today", input: "today", expected: time.Date(2026, 8, 3, 0, 0, 0, 0, time.Local)},
		{name: "tomorrow", input: "tomorrow", expected: time.Date(2026, 8, 4, 0, 0, 0, 0, time.Local)},
		{name: "yesterday", input: "yesterday", expected: time.Date(2026, 8, 2, 0, 0, 0, 0, time.Local)},
		{name: "Explicit date", input: "2026-12-24", expected: time.Date(2026, 12, 24, 0, 0, 0, 0, time.Local)},
		{name: "Positive offset", input: "+2d", expected: time.Date(2026, 8, 5, 0, 0, 0, 0, time.Local)},
		{name: "Negative offset", input: "-1d", expected: time.Date(2026, 8, 2, 0, 0, 0, 0, time.Local)},
		{name: "Garbage", input: "next week", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseDaySpec(tt.input, now)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestFormatDurationShort(t *testing.T) {
	tests := []struct {
		input    time.Duration
		expected string
	}{
		{45 * time.Second, "45s"},
		{15 * time.Minute, "15m"},
		{90 * time.Minute, "1h30m"},
		{2 * time.Hour, "2h"},
		{-5 * time.Minute, "0s"},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.expected, FormatDurationShort(tt.input))
	}
}
