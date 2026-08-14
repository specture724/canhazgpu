package gpu

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/russellb/canhazgpu/internal/types"
)

// Notification channels understood by the guard
const (
	// ChannelProcess writes the warning straight into the offending process's
	// standard error, so it appears in the terminal running the job
	ChannelProcess = "process"
	// ChannelTTY writes to every terminal the offending user has open
	ChannelTTY = "tty"
	// ChannelLog writes to the guard's own log (stderr, journald under systemd)
	ChannelLog = "log"
	// ChannelWall broadcasts to all logged in users
	ChannelWall = "wall"
)

// Notifier delivers a warning about a violation and reports which channels
// actually reached somebody
type Notifier interface {
	// Notify warns the owner of the offending process
	Notify(violation *types.Violation, message string) []string
	// NotifyUser warns a user who is not tied to a specific process, used to
	// tell a reservation holder that somebody else is on their GPU
	NotifyUser(username string, message string) []string
}

// MultiNotifier delivers warnings over the configured channels, in order, and
// records what worked. Channels that need privileges we do not have are
// reported once rather than on every warning.
type MultiNotifier struct {
	Channels []string
	LogFile  string
	Output   *os.File // Where the log channel writes, defaults to stderr

	warnedAboutPrivileges bool
}

func NewMultiNotifier(channels []string, logFile string) *MultiNotifier {
	return &MultiNotifier{
		Channels: channels,
		LogFile:  logFile,
		Output:   os.Stderr,
	}
}

func (n *MultiNotifier) Notify(violation *types.Violation, message string) []string {
	var delivered []string

	for _, channel := range n.Channels {
		var err error

		switch channel {
		case ChannelProcess:
			err = writeToProcessStderr(violation.PID, message)
		case ChannelTTY:
			err = writeToUserTerminals(violation.User, message)
		case ChannelLog:
			err = n.writeLog(violation, message)
		case ChannelWall:
			err = exec.Command("wall", message).Run()
		default:
			err = fmt.Errorf("unknown notification channel %q", channel)
		}

		if err == nil {
			delivered = append(delivered, channel)
			continue
		}

		// Reaching another user's process is best effort. Root normally can,
		// but a restricted /proc or a container can deny it too, so report the
		// limitation once instead of once per process
		if os.IsPermission(err) {
			n.reportPrivilegeLimitation(channel)
			continue
		}

		// Channels regularly have nothing to deliver to (a batch job has no
		// terminal), so only unexpected failures are worth reporting
		if !isEmptyTargetError(err) {
			fmt.Fprintf(n.Output, "canhazgpu guard: %s notification failed for PID %d: %v\n",
				channel, violation.PID, err)
		}
	}

	return delivered
}

// NotifyUser warns a user directly. Only the terminal and log channels apply:
// there is no specific process to write to.
func (n *MultiNotifier) NotifyUser(username string, message string) []string {
	var delivered []string

	for _, channel := range n.Channels {
		var err error

		switch channel {
		case ChannelTTY, ChannelProcess:
			// Both process based and terminal based warnings land in the user's
			// terminals here
			if contains(delivered, ChannelTTY) {
				continue
			}
			err = writeToUserTerminals(username, message)
			channel = ChannelTTY
		case ChannelLog:
			line := fmt.Sprintf("%s [notice] %s: %s\n", time.Now().Format(time.RFC3339), username, message)
			_, err = n.Output.WriteString(line)
		default:
			continue
		}

		if err == nil {
			delivered = append(delivered, channel)
			continue
		}
		if os.IsPermission(err) {
			n.reportPrivilegeLimitation(channel)
		}
	}

	return delivered
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func (n *MultiNotifier) reportPrivilegeLimitation(channel string) {
	if n.warnedAboutPrivileges {
		return
	}
	n.warnedAboutPrivileges = true
	hint := "run the guard as root to warn other users directly"
	if os.Geteuid() == 0 {
		hint = "even as root the kernel denies access to those processes - the other channels still apply"
	}
	fmt.Fprintf(n.Output,
		"canhazgpu guard: permission denied on the %q channel - %s\n", channel, hint)
}

// writeLog records the warning in the guard's own output and, if configured, in
// a log file
func (n *MultiNotifier) writeLog(violation *types.Violation, message string) error {
	line := fmt.Sprintf("%s [%s] %s | %s\n",
		time.Now().Format(time.RFC3339), violation.Kind, violation.Describe(), message)

	if _, err := n.Output.WriteString(line); err != nil {
		return err
	}

	if n.LogFile == "" {
		return nil
	}

	file, err := os.OpenFile(n.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); err != nil {
			fmt.Fprintf(n.Output, "canhazgpu guard: failed to close log file: %v\n", err)
		}
	}()

	_, err = file.WriteString(line)
	return err
}

// writeToProcessStderr writes a message into a running process's standard
// error. Only terminals are written to: stderr redirected to a file or pipe
// belongs to the job, and /dev/null would swallow the warning while looking
// like a successful delivery.
func writeToProcessStderr(pid int, message string) error {
	link := fmt.Sprintf("/proc/%d/fd/2", pid)

	target, err := os.Readlink(link)
	if err != nil {
		return err
	}
	if !isTerminalDevice(target) {
		return errEmptyTarget{fmt.Errorf("stderr of PID %d is not a terminal (%s)", pid, target)}
	}

	return appendToTerminal(link, message)
}

// isTerminalDevice reports whether a path refers to a terminal
func isTerminalDevice(path string) bool {
	return strings.HasPrefix(path, "/dev/pts/") || strings.HasPrefix(path, "/dev/tty")
}

// writeToUserTerminals writes a message to every pseudo terminal owned by the
// given user, which reaches their interactive shells
func writeToUserTerminals(username string, message string) error {
	if username == "" {
		return errEmptyTarget{fmt.Errorf("no user to notify")}
	}

	account, err := user.Lookup(username)
	if err != nil {
		return errEmptyTarget{fmt.Errorf("unknown user %q", username)}
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return err
	}

	terminals, err := filepath.Glob("/dev/pts/*")
	if err != nil {
		return err
	}

	var lastErr error
	written := 0

	for _, terminal := range terminals {
		info, err := os.Stat(terminal)
		if err != nil {
			continue
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != uid {
			continue
		}

		if err := appendToTerminal(terminal, message); err != nil {
			lastErr = err
			continue
		}
		written++
	}

	if written > 0 {
		return nil
	}
	if lastErr != nil {
		return lastErr
	}

	return errEmptyTarget{fmt.Errorf("user %s has no terminal open", username)}
}

// appendToTerminal writes a message to a terminal device, framed so it stands
// out in whatever output is already scrolling past
func appendToTerminal(device string, message string) error {
	file, err := os.OpenFile(device, os.O_WRONLY|syscall.O_NOCTTY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	_, err = file.WriteString("\r\n" + message + "\r\n")
	return err
}

// errEmptyTarget marks the case where a channel simply had nobody to deliver to,
// which is normal and should not be logged as a failure
type errEmptyTarget struct{ error }

func isEmptyTargetError(err error) bool {
	_, ok := err.(errEmptyTarget)
	return ok
}
