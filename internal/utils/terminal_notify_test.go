package utils

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDetectTerminalNotifyMethod covers one representative env fingerprint per
// terminal of interest (plus a single unsupported/fallback case). Do not add
// extra cases for alternate env vars of the same terminal.
func TestDetectTerminalNotifyMethod(t *testing.T) {
	tests := []struct {
		name     string
		environ  []string
		expected TerminalNotifyMethod
	}{
		{
			name:     "WezTerm prefers OSC 777",
			environ:  []string{"TERM_PROGRAM=WezTerm"},
			expected: NotifyOSC777,
		},
		{
			name:     "ghostty prefers OSC 777",
			environ:  []string{"TERM_PROGRAM=ghostty"},
			expected: NotifyOSC777,
		},
		{
			name:     "rxvt prefers OSC 777",
			environ:  []string{"TERM=rxvt-unicode-256color"},
			expected: NotifyOSC777,
		},
		{
			name:     "foot prefers OSC 777",
			environ:  []string{"TERM=foot"},
			expected: NotifyOSC777,
		},
		{
			name:     "kitty prefers OSC 777",
			environ:  []string{"TERM_PROGRAM=kitty"},
			expected: NotifyOSC777,
		},
		{
			name:     "Warp prefers OSC 777",
			environ:  []string{"TERM_PROGRAM=WarpTerminal"},
			expected: NotifyOSC777,
		},
		{
			name:     "Windows Terminal prefers OSC 777",
			environ:  []string{"WT_SESSION=abcdef"},
			expected: NotifyOSC777,
		},
		{
			name:     "iTerm prefers OSC 9",
			environ:  []string{"TERM_PROGRAM=iTerm.app"},
			expected: NotifyOSC9,
		},
		{
			name:     "vscode prefers OSC 9",
			environ:  []string{"TERM_PROGRAM=vscode"},
			expected: NotifyOSC9,
		},
		{
			name:     "ConEmu prefers OSC 9",
			environ:  []string{"ConEmuPID=1234"},
			expected: NotifyOSC9,
		},
		{
			name:     "plain xterm falls back to bell",
			environ:  []string{"TERM=xterm-256color"},
			expected: NotifyBell,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, DetectTerminalNotifyMethod(tt.environ))
		})
	}
}

func TestNotifyTerminalAttentionTo(t *testing.T) {
	tests := []struct {
		name     string
		environ  []string
		contains string
	}{
		{
			name:     "OSC 777 sequence",
			environ:  []string{"TERM_PROGRAM=WezTerm"},
			contains: "\033]777;notify;canhazgpu;GPUs allocated\007",
		},
		{
			name:     "OSC 9 sequence",
			environ:  []string{"TERM_PROGRAM=iTerm.app"},
			contains: "\033]9;canhazgpu: GPUs allocated\007",
		},
		{
			name:     "bell fallback",
			environ:  []string{"TERM=xterm-256color"},
			contains: "\a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			NotifyTerminalAttentionTo(&buf, tt.environ, "canhazgpu", "GPUs allocated")
			assert.Equal(t, tt.contains, buf.String())
		})
	}
}

func TestSanitizeOSC(t *testing.T) {
	assert.Equal(t, "a,b", sanitizeOSC("a;b"))
	assert.False(t, strings.Contains(sanitizeOSC("hi\033there\007"), "\033"))
	assert.False(t, strings.Contains(sanitizeOSC("hi\033there\007"), "\007"))
}
