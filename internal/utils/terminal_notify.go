package utils

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// TerminalNotifyMethod is how we ask the terminal to get the user's attention.
type TerminalNotifyMethod string

const (
	NotifyOSC777 TerminalNotifyMethod = "osc777"
	NotifyOSC9   TerminalNotifyMethod = "osc9"
	NotifyBell   TerminalNotifyMethod = "bell"
)

// DetectTerminalNotifyMethod naively picks a desktop-notification escape
// sequence from environment variables. OSC 777 (rxvt-unicode style) is
// preferred when the terminal looks like one that supports it; otherwise OSC 9
// (iTerm2 style); otherwise a plain BEL.
func DetectTerminalNotifyMethod(environ []string) TerminalNotifyMethod {
	env := environMap(environ)

	if supportsOSC777(env) {
		return NotifyOSC777
	}
	if supportsOSC9(env) {
		return NotifyOSC9
	}
	return NotifyBell
}

// NotifyTerminalAttention writes a terminal notification to stdout.
// Used when a queued GPU request is finally allocated.
func NotifyTerminalAttention(title, body string) {
	NotifyTerminalAttentionTo(os.Stdout, os.Environ(), title, body)
}

// NotifyTerminalAttentionTo is the testable form of NotifyTerminalAttention.
// A nil writer is a no-op.
func NotifyTerminalAttentionTo(w io.Writer, environ []string, title, body string) {
	if w == nil {
		return
	}
	if title == "" {
		title = "canhazgpu"
	}
	if body == "" {
		body = "GPUs allocated"
	}

	switch DetectTerminalNotifyMethod(environ) {
	case NotifyOSC777:
		// OSC 777 ; notify ; title ; body BEL (rxvt-unicode / WezTerm / Ghostty)
		fmt.Fprintf(w, "\033]777;notify;%s;%s\007", sanitizeOSC(title), sanitizeOSC(body))
	case NotifyOSC9:
		// OSC 9 ; message BEL (iTerm2)
		fmt.Fprintf(w, "\033]9;%s\007", sanitizeOSC(fmt.Sprintf("%s: %s", title, body)))
	default:
		fmt.Fprint(w, "\a")
	}
}

func environMap(environ []string) map[string]string {
	out := make(map[string]string, len(environ))
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		out[key] = value
	}
	return out
}

func envLookup(env map[string]string, key string) string {
	return env[key]
}

func envHas(env map[string]string, key string) bool {
	_, ok := env[key]
	return ok
}

func supportsOSC777(env map[string]string) bool {
	termProgram := strings.ToLower(envLookup(env, "TERM_PROGRAM"))
	switch termProgram {
	case "wezterm", "ghostty", "foot", "kitty", "warpterminal":
		return true
	}

	if envHas(env, "WEZTERM_EXECUTABLE") || envHas(env, "WEZTERM_PANE") {
		return true
	}
	if envHas(env, "GHOSTTY_RESOURCES_DIR") || envHas(env, "GHOSTTY_BINDIR") {
		return true
	}
	if envHas(env, "KITTY_WINDOW_ID") {
		return true
	}
	if envHas(env, "WT_SESSION") {
		return true
	}

	term := strings.ToLower(envLookup(env, "TERM"))
	switch {
	case strings.Contains(term, "rxvt"),
		strings.Contains(term, "urxvt"),
		strings.HasPrefix(term, "foot"):
		return true
	}

	return false
}

func supportsOSC9(env map[string]string) bool {
	termProgram := strings.ToLower(envLookup(env, "TERM_PROGRAM"))
	switch termProgram {
	case "iterm.app", "vscode", "vscode-insiders", "cursor", "hyper", "apple_terminal":
		return true
	}

	if envHas(env, "ITERM_SESSION_ID") || envHas(env, "ITERM_PROFILE") {
		return true
	}
	if envHas(env, "VSCODE_INJECTION") {
		return true
	}
	// ConEmu accepts OSC 9-style notifications
	if envHas(env, "ConEmuPID") {
		return true
	}

	return false
}

// sanitizeOSC strips characters that would break OSC parameter parsing.
func sanitizeOSC(s string) string {
	s = strings.ReplaceAll(s, "\033", "")
	s = strings.ReplaceAll(s, "\007", "")
	s = strings.ReplaceAll(s, ";", ",")
	return s
}
