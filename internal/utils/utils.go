package utils

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"
)

// GetUsernameFromUID converts a UID to username
func GetUsernameFromUID(uid int) (string, error) {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

// ParseDuration parses duration strings like "30m", "2h", "1d"
func ParseDuration(duration string) (time.Duration, error) {
	if duration == "" {
		return 8 * time.Hour, nil // Default 8 hours
	}

	// Handle decimal values like "0.5h"
	if strings.Contains(duration, ".") {
		if strings.HasSuffix(duration, "h") {
			hoursStr := strings.TrimSuffix(duration, "h")
			hours, err := strconv.ParseFloat(hoursStr, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid duration format: %s", duration)
			}
			return time.Duration(hours * float64(time.Hour)), nil
		}
		if strings.HasSuffix(duration, "d") {
			daysStr := strings.TrimSuffix(duration, "d")
			days, err := strconv.ParseFloat(daysStr, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid duration format: %s", duration)
			}
			return time.Duration(days * 24 * float64(time.Hour)), nil
		}
	}

	// Handle integer values
	if strings.HasSuffix(duration, "s") {
		secondsStr := strings.TrimSuffix(duration, "s")
		seconds, err := strconv.Atoi(secondsStr)
		if err != nil {
			return 0, fmt.Errorf("invalid duration format: %s", duration)
		}
		return time.Duration(seconds) * time.Second, nil
	}

	if strings.HasSuffix(duration, "m") {
		minutesStr := strings.TrimSuffix(duration, "m")
		minutes, err := strconv.Atoi(minutesStr)
		if err != nil {
			return 0, fmt.Errorf("invalid duration format: %s", duration)
		}
		return time.Duration(minutes) * time.Minute, nil
	}

	if strings.HasSuffix(duration, "h") {
		hoursStr := strings.TrimSuffix(duration, "h")
		hours, err := strconv.Atoi(hoursStr)
		if err != nil {
			return 0, fmt.Errorf("invalid duration format: %s", duration)
		}
		return time.Duration(hours) * time.Hour, nil
	}

	if strings.HasSuffix(duration, "d") {
		daysStr := strings.TrimSuffix(duration, "d")
		days, err := strconv.Atoi(daysStr)
		if err != nil {
			return 0, fmt.Errorf("invalid duration format: %s", duration)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}

	return 0, fmt.Errorf("invalid duration format: %s (use formats like 30s, 30m, 2h, 1d)", duration)
}

// ParseTimeSpec parses a wall clock time specification into an absolute time,
// interpreted in the local timezone. See ParseTimeSpecFrom for the accepted
// formats.
func ParseTimeSpec(spec string, now time.Time) (time.Time, error) {
	return ParseTimeSpecFrom(spec, now)
}

// ParseTimeSpecFrom parses a wall clock time specification relative to ref.
//
// Accepted formats:
//
//	14:00               clock time on ref's date, rolled to the next day if it
//	                    would otherwise be in the past
//	14:00:30            same, with seconds
//	tomorrow 14:00      clock time on the day after ref (also "today 14:00")
//	2026-08-04 14:00    explicit date and time ('T' also accepted as separator)
//	2026-08-04          midnight on that date
//	+2h                 relative to ref, using the usual duration formats
//	now                 ref itself
func ParseTimeSpecFrom(spec string, ref time.Time) (time.Time, error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty time specification")
	}

	if strings.EqualFold(s, "now") {
		return ref, nil
	}

	// Relative offsets: +30m, +2h, ...
	if strings.HasPrefix(s, "+") {
		d, err := ParseDuration(strings.TrimPrefix(s, "+"))
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid relative time %q: %v", spec, err)
		}
		return ref.Add(d), nil
	}

	// Day keywords pin the date explicitly, so no rolling forward
	dayOffset := 0
	rollForward := true
	for _, keyword := range []struct {
		prefix string
		offset int
	}{{"today", 0}, {"tomorrow", 1}} {
		if len(s) > len(keyword.prefix) && strings.EqualFold(s[:len(keyword.prefix)], keyword.prefix) &&
			(s[len(keyword.prefix)] == ' ' || s[len(keyword.prefix)] == 'T' || s[len(keyword.prefix)] == '@') {
			dayOffset = keyword.offset
			rollForward = false
			s = strings.TrimSpace(s[len(keyword.prefix)+1:])
			break
		}
	}

	// Explicit date and time
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02T15:04"} {
		if t, err := time.ParseInLocation(layout, s, ref.Location()); err == nil {
			return t, nil
		}
	}

	// Bare clock time on ref's date (optionally shifted by a day keyword)
	for _, layout := range []string{"15:04:05", "15:04"} {
		if t, err := time.ParseInLocation(layout, s, ref.Location()); err == nil {
			result := time.Date(ref.Year(), ref.Month(), ref.Day(), t.Hour(), t.Minute(), t.Second(), 0, ref.Location())
			if dayOffset != 0 {
				result = result.AddDate(0, 0, dayOffset)
			}
			if rollForward && result.Before(ref) {
				result = result.AddDate(0, 0, 1)
			}
			return result, nil
		}
	}

	// Bare date means midnight
	if t, err := time.ParseInLocation("2006-01-02", s, ref.Location()); err == nil {
		return t, nil
	}

	return time.Time{}, fmt.Errorf("invalid time %q (use formats like 14:00, 'tomorrow 09:30', 2026-08-04T14:00 or +2h)", spec)
}

// ParseDaySpec parses a calendar day specification and returns midnight at the
// start of that day. Accepted formats are "2026-08-04", "today", "tomorrow",
// "yesterday" and relative offsets like "+2d" or "-1d".
func ParseDaySpec(spec string, now time.Time) (time.Time, error) {
	s := strings.TrimSpace(spec)
	midnight := func(t time.Time) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	}

	switch {
	case s == "" || strings.EqualFold(s, "today"):
		return midnight(now), nil
	case strings.EqualFold(s, "tomorrow"):
		return midnight(now).AddDate(0, 0, 1), nil
	case strings.EqualFold(s, "yesterday"):
		return midnight(now).AddDate(0, 0, -1), nil
	}

	if strings.HasPrefix(s, "+") || strings.HasPrefix(s, "-") {
		negative := strings.HasPrefix(s, "-")
		d, err := ParseDuration(strings.TrimLeft(s, "+-"))
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid date %q: %v", spec, err)
		}
		if negative {
			d = -d
		}
		return midnight(now.Add(d)), nil
	}

	t, err := time.ParseInLocation("2006-01-02", s, now.Location())
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date %q (use YYYY-MM-DD, today, tomorrow or +1d)", spec)
	}
	return t, nil
}

// FormatDuration formats a duration into human readable format
func FormatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("0h 0m %ds", int(d.Seconds()))
	}

	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60

	return fmt.Sprintf("%dh %dm %ds", hours, minutes, seconds)
}

// FormatDurationShort formats a duration compactly, e.g. "45s", "15m", "1h30m"
func FormatDurationShort(d time.Duration) string {
	if d < 0 {
		return "0s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}

	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	if minutes == 0 {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dh%dm", hours, minutes)
}

// FormatTime formats a time.Time into relative format like "2h 30m 15s ago"
func FormatTimeAgo(t time.Time) string {
	if t.IsZero() {
		return "never"
	}

	d := time.Since(t)
	if d < 0 {
		return "in the future"
	}

	return FormatDuration(d) + " ago"
}

// FormatTimeUntil formats a time.Time into relative format like "in 2h 30m 15s"
func FormatTimeUntil(t time.Time) string {
	if t.IsZero() {
		return "never"
	}

	d := time.Until(t)
	if d < 0 {
		return "expired"
	}

	return "in " + FormatDuration(d)
}

// TruncateString truncates a string to maxLen characters
func TruncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// FormatUserList formats a list of users for display
func FormatUserList(users []string, maxUsers int) string {
	if len(users) == 0 {
		return "unknown"
	}

	if len(users) == 1 {
		return users[0]
	}

	if len(users) <= maxUsers {
		if len(users) == 2 {
			return users[0] + " and " + users[1]
		}
		return strings.Join(users[:len(users)-1], ", ") + " and " + users[len(users)-1]
	}

	displayed := users[:maxUsers]
	remaining := len(users) - maxUsers
	return strings.Join(displayed, ", ") + fmt.Sprintf(" and %d more", remaining)
}

// FormatProcessList formats a list of processes for display
func FormatProcessList(processes []string, maxProcesses int) string {
	if len(processes) == 0 {
		return ""
	}

	if len(processes) <= maxProcesses {
		return strings.Join(processes, ", ")
	}

	displayed := processes[:maxProcesses]
	remaining := len(processes) - maxProcesses
	return strings.Join(displayed, ", ") + fmt.Sprintf(" and %d more", remaining)
}

// ExecuteRemoteCommand executes a command on a remote host via SSH
// Returns stdout, stderr, and error
func ExecuteRemoteCommand(ctx context.Context, host string, command string) (string, string, error) {
	// Build SSH command
	// Use -o BatchMode=yes to prevent interactive prompts
	// Use -o ConnectTimeout=10 to timeout connection attempts
	sshArgs := []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=accept-new",
		host,
		command,
	}

	cmd := exec.CommandContext(ctx, "ssh", sshArgs...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	return stdout.String(), stderr.String(), err
}

// ExecuteRemoteCanHazGPU executes a canhazgpu command on a remote host
// Automatically adds the full path to canhazgpu if needed
func ExecuteRemoteCanHazGPU(ctx context.Context, host string, args []string) (string, string, error) {
	// Build the command - try to use canhazgpu from PATH
	// The remote host should have canhazgpu installed
	command := "canhazgpu " + strings.Join(args, " ")
	return ExecuteRemoteCommand(ctx, host, command)
}
