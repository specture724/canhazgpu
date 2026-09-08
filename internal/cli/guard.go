package cli

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/russellb/canhazgpu/internal/gpu"
	"github.com/russellb/canhazgpu/internal/redis_client"
	"github.com/russellb/canhazgpu/internal/types"
	"github.com/russellb/canhazgpu/internal/utils"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var guardCmd = &cobra.Command{
	Use:   "guard",
	Short: "Watch the GPUs and warn users who bypass the reservation system",
	Long: `Continuously monitor the GPUs and react to usage that bypasses reservations.

Two kinds of violation are detected:
- unreserved: a process runs on a GPU that nobody reserved
- foreign:    a process runs on a GPU somebody else reserved

Offenders are warned where they will actually see it: the warning is written
into the standard error of the offending process, so it appears in the terminal
running the job, and to any terminal that user has open. Warnings repeat on an
interval, and with --enforce the process is terminated once the warnings are
exhausted (SIGINT, then SIGTERM, then SIGKILL).

The guard is the only long running part of canhazgpu, so it also performs the
housekeeping other commands trigger on demand: scheduled bookings are activated
when their window starts, and expired, stale or idle reservations are released.
Disable this with --no-maintenance.

Each OS account may hold at most --max-tasks-per-user concurrent tasks (default
4). Excess run/reserve requests enter the queue until a slot opens. Set the
limit to 0 to disable it. The policy is shared through Redis and remains in
effect after guard exits; existing tasks continue running. Use --task-limit-hours
09:00-18:00 to apply the limit only during that daily local-time window (overnight
windows such as 22:00-06:00 are supported). Outside it, task admission is unlimited.
Omit the window or set it to an empty string for an all-day limit.

Privileges: warning other users' processes and terminating them requires root,
so the guard is normally run from systemd:

    [Unit]
    Description=canhazgpu GPU reservation guard
    After=network.target redis.service

    [Service]
    ExecStart=/usr/local/bin/canhazgpu guard --interval 15s
    Restart=always
    User=root

    [Install]
    WantedBy=multi-user.target

Only one guard runs at a time; a second one exits with an error.

False positives are avoided by a grace period (a process must be on the GPU for
--grace before it is reported), by requiring --confirmations consecutive scans,
and by the allow lists --exclude-users and --exclude-commands.

Example usage:
  canhazgpu guard                                   # warn only, every 15s
  canhazgpu guard --once                            # single scan, for cron
  canhazgpu guard --enforce --dry-run               # show what would be killed
  canhazgpu guard --enforce                         # warn, then terminate
  canhazgpu guard --enforce --max-warnings 5 --warn-interval 2m
  canhazgpu guard --exclude-users root,jenkins --channels process,tty,log`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runGuard(cmd.Context())
	},
}

func init() {
	defaults := gpu.DefaultGuardConfig()

	guardCmd.Flags().String("interval", utils.FormatDurationShort(defaults.Interval), "How often to scan the GPUs")
	guardCmd.Flags().Bool("once", false, "Run a single scan and exit (for cron)")
	guardCmd.Flags().String("grace", utils.FormatDurationShort(defaults.GracePeriod),
		"How long a process may use a GPU before it is reported")
	guardCmd.Flags().Int("confirmations", defaults.Confirmations, "Consecutive scans required before acting")
	guardCmd.Flags().String("warn-interval", utils.FormatDurationShort(defaults.WarnInterval),
		"Time between repeated warnings")
	guardCmd.Flags().Int("max-warnings", defaults.MaxWarnings, "Warnings delivered before enforcement starts")
	guardCmd.Flags().Bool("enforce", false, "Terminate offending processes once warnings are exhausted")
	guardCmd.Flags().Bool("dry-run", false, "With --enforce, report terminations without sending signals")
	guardCmd.Flags().String("kill-grace", utils.FormatDurationShort(defaults.KillGrace),
		"Wait between SIGINT, SIGTERM and SIGKILL")
	guardCmd.Flags().Int("max-kills-per-hour", defaults.MaxKillsPerHour,
		"Safety limit on terminations per hour (0 disables the limit)")
	guardCmd.Flags().StringSlice("exclude-users", defaults.ExcludeUsers, "Users that are never reported")
	guardCmd.Flags().StringSlice("exclude-commands", defaults.ExcludeCommands,
		"Process name fragments that are never reported")
	guardCmd.Flags().Int("min-memory", 0, "Ignore processes using less than this many MB (0 = no filter)")
	guardCmd.Flags().StringSlice("channels", []string{gpu.ChannelProcess, gpu.ChannelTTY, gpu.ChannelLog},
		"Warning channels: process, tty, log, wall")
	guardCmd.Flags().String("log-file", "", "Also append warnings to this file")
	guardCmd.Flags().Bool("no-maintenance", false, "Do not activate bookings or release expired reservations")
	guardCmd.Flags().Bool("notify-holder", defaults.NotifyHolder,
		"Also tell the reservation holder when somebody else uses their GPU")

	guardCmd.Flags().Int("max-tasks-per-user", defaults.MaxTasksPerUser,
		"Maximum concurrent tasks per OS account; excess requests queue (0 disables)")
	guardCmd.Flags().String("task-limit-hours", "",
		"Daily local-time window for the task limit, HH:MM-HH:MM (empty = all day)")

	rootCmd.AddCommand(guardCmd)
}

func runGuard(ctx context.Context) error {
	settings, err := guardSettingsFromConfig()
	if err != nil {
		return err
	}

	config := getConfig()
	client := redis_client.NewClient(config)
	defer func() {
		if err := client.Close(); err != nil {
			fmt.Printf("Warning: failed to close Redis client: %v\n", err)
		}
	}()

	if err := client.Ping(ctx); err != nil {
		return fmt.Errorf("failed to connect to Redis: %v", err)
	}

	// Fail early on an uninitialized pool rather than in the scan loop
	if _, err := client.GetGPUCount(ctx); err != nil {
		return err
	}

	engine := gpu.NewAllocationEngine(client, config)
	notifier := gpu.NewMultiNotifier(viper.GetStringSlice("guard.channels"), viper.GetString("guard.log-file"))
	guard := gpu.NewGuard(engine, client, config, settings, notifier)

	if viper.GetBool("guard.once") {
		pass, err := guard.RunOnce(ctx)
		if err != nil {
			return err
		}
		printGuardPass(pass)
		return nil
	}

	return guard.Run(ctx)
}

// guardSettingsFromConfig assembles the guard configuration from flags, the
// config file and environment variables
func guardSettingsFromConfig() (gpu.GuardConfig, error) {
	settings := gpu.DefaultGuardConfig()

	durations := []struct {
		key    string
		target *time.Duration
		// Zero is meaningful for everything except the scan interval: it means
		// "report immediately" or "escalate without waiting"
		mustBePositive bool
	}{
		{"guard.interval", &settings.Interval, true},
		{"guard.grace", &settings.GracePeriod, false},
		{"guard.warn-interval", &settings.WarnInterval, false},
		{"guard.kill-grace", &settings.KillGrace, false},
	}

	for _, duration := range durations {
		value := strings.TrimSpace(viper.GetString(duration.key))
		if value == "" {
			continue
		}

		name := strings.TrimPrefix(duration.key, "guard.")

		parsed, err := parseGuardDuration(value)
		if err != nil {
			return settings, fmt.Errorf("invalid %s: %v", name, err)
		}
		if parsed < 0 {
			return settings, fmt.Errorf("%s cannot be negative", name)
		}
		if parsed == 0 && duration.mustBePositive {
			return settings, fmt.Errorf("%s must be positive", name)
		}
		*duration.target = parsed
	}

	settings.Confirmations = viper.GetInt("guard.confirmations")
	if settings.Confirmations < 1 {
		settings.Confirmations = 1
	}
	settings.MaxWarnings = viper.GetInt("guard.max-warnings")
	if settings.MaxWarnings < 1 {
		settings.MaxWarnings = 1
	}

	settings.Enforce = viper.GetBool("guard.enforce")
	settings.DryRun = viper.GetBool("guard.dry-run")
	settings.MaxKillsPerHour = viper.GetInt("guard.max-kills-per-hour")
	settings.ExcludeUsers = viper.GetStringSlice("guard.exclude-users")
	settings.ExcludeCommands = viper.GetStringSlice("guard.exclude-commands")
	settings.MinMemoryMB = viper.GetInt("guard.min-memory")
	settings.Maintenance = !viper.GetBool("guard.no-maintenance")
	settings.NotifyHolder = viper.GetBool("guard.notify-holder")
	limit, err := strconv.Atoi(viper.GetString("guard.max-tasks-per-user"))
	if err != nil {
		return settings, fmt.Errorf("max-tasks-per-user must be an integer: %w", err)
	}
	settings.MaxTasksPerUser = limit
	if settings.MaxTasksPerUser < 0 {
		return settings, fmt.Errorf("max-tasks-per-user cannot be negative")
	}
	settings.TaskLimitHours = strings.TrimSpace(viper.GetString("guard.task-limit-hours"))
	if _, err := utils.InDailyTimeWindow(settings.TaskLimitHours, time.Now()); err != nil {
		return settings, fmt.Errorf("invalid task-limit-hours: %w", err)
	}

	return settings, nil
}

// parseGuardDuration parses a guard duration, accepting a bare "0" in addition
// to the usual "0s" for "do not wait"
func parseGuardDuration(value string) (time.Duration, error) {
	if value == "0" {
		return 0, nil
	}
	return utils.ParseDuration(value)
}

func printGuardPass(pass *gpu.GuardPass) {
	fmt.Printf("Checked %d GPU process(es): %d violation(s), %d warned, %d signalled, %d resolved\n",
		pass.Processes, len(pass.Violations), pass.Warned, pass.Signalled, pass.Resolved)

	for _, violation := range pass.Violations {
		status := fmt.Sprintf("warnings %d", violation.WarnCount)
		if len(violation.Signals) > 0 {
			status = fmt.Sprintf("warnings %d, sent %s", violation.WarnCount, strings.Join(violation.Signals, "+"))
		}
		if violation.Sightings < viper.GetInt("guard.confirmations") {
			status = "awaiting confirmation"
		}

		fmt.Printf("  %s (%dMB, %s) - %s\n",
			violation.Describe(), violation.MemoryMB,
			utils.FormatDurationShort(violation.Duration()), status)
	}
}

// guardViolationKindLabel renders a violation kind for display
func guardViolationKindLabel(kind string) string {
	switch kind {
	case types.ViolationKindForeign:
		return "FOREIGN"
	case types.ViolationKindUnreserved:
		return "UNRESERVED"
	default:
		return strings.ToUpper(kind)
	}
}
