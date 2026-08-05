package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/russellb/canhazgpu/internal/redis_client"
	"github.com/russellb/canhazgpu/internal/types"
	"github.com/russellb/canhazgpu/internal/utils"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var violationsCmd = &cobra.Command{
	Use:   "violations",
	Short: "Show GPU usage that bypassed the reservation system",
	Long: `List GPU usage that bypasses reservations, as recorded by 'canhazgpu guard'.

Two kinds of violation are reported:
- UNRESERVED: a process on a GPU that nobody reserved
- FOREIGN:    a process on a GPU somebody else reserved

Without --history the currently open violations are shown, including how many
warnings have been delivered and which termination signals were sent. With
--history, violations that have already been resolved are listed instead.

Violations are recorded by the guard, so this command is only useful on a host
where 'canhazgpu guard' runs (or after 'canhazgpu guard --once').

Example usage:
  canhazgpu violations
  canhazgpu violations --json
  canhazgpu violations --history --days 7`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runViolations(cmd.Context(),
			viper.GetBool("violations.json"),
			viper.GetBool("violations.history"),
			viper.GetInt("violations.days"))
	},
}

func init() {
	violationsCmd.Flags().Bool("json", false, "Output as JSON")
	violationsCmd.Flags().Bool("history", false, "Show resolved violations instead of open ones")
	violationsCmd.Flags().Int("days", 7, "How many days of history to include")

	rootCmd.AddCommand(violationsCmd)
}

func runViolations(ctx context.Context, jsonOut bool, history bool, days int) error {
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

	violations, err := loadViolations(ctx, client, history, days)
	if err != nil {
		return err
	}

	if jsonOut {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if violations == nil {
			violations = []*types.Violation{}
		}
		return encoder.Encode(violations)
	}

	printViolationsTable(violations, history)
	return nil
}

func loadViolations(ctx context.Context, client *redis_client.Client, history bool, days int) ([]*types.Violation, error) {
	if !history {
		return client.GetAllViolations(ctx)
	}

	if days <= 0 {
		days = 7
	}
	end := time.Now()
	start := end.AddDate(0, 0, -days)

	violations, err := client.GetViolationHistory(ctx, start, end)
	if err != nil {
		return nil, err
	}

	// Most recent first reads better for history
	sort.SliceStable(violations, func(i, j int) bool {
		return violations[i].EndTime.ToTime().After(violations[j].EndTime.ToTime())
	})

	return violations, nil
}

func printViolationsTable(violations []*types.Violation, history bool) {
	if len(violations) == 0 {
		if history {
			fmt.Println("No violations recorded in that period.")
		} else {
			fmt.Println("No GPU usage bypassing reservations right now.")
		}
		return
	}

	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	t.SetStyle(table.StyleLight)
	t.Style().Options.SeparateRows = false
	t.Style().Options.DrawBorder = false

	header := table.Row{
		FormatHeader("GPU"), FormatHeader("KIND"), FormatHeader("USER"),
		FormatHeader("PID"), FormatHeader("MEMORY"), FormatHeader("DURATION"),
		FormatHeader("COMMAND"),
	}
	if history {
		header = append(header, FormatHeader("RESOLUTION"))
	} else {
		header = append(header, FormatHeader("ACTION"))
	}
	t.AppendHeader(header)

	for _, violation := range violations {
		user := violation.User
		if violation.Kind == types.ViolationKindForeign && violation.Holder != "" {
			user = fmt.Sprintf("%s (holder: %s)", violation.User, violation.Holder)
		}

		row := table.Row{
			fmt.Sprintf("%d", violation.GPUID),
			colorUnreserved.Sprint(guardViolationKindLabel(violation.Kind)),
			user,
			fmt.Sprintf("%d", violation.PID),
			fmt.Sprintf("%dMB", violation.MemoryMB),
			utils.FormatDurationShort(violation.Duration()),
			FormatDim(utils.TruncateString(violation.Command, 24)),
		}

		if history {
			row = append(row, FormatDim(violation.Resolution))
		} else {
			row = append(row, formatViolationAction(violation))
		}

		t.AppendRow(row)
	}

	fmt.Println()
	t.Render()
	fmt.Println()
}

// formatViolationAction summarises what the guard has done about a violation
func formatViolationAction(violation *types.Violation) string {
	var parts []string

	if violation.WarnCount > 0 {
		parts = append(parts, fmt.Sprintf("%d warning(s)", violation.WarnCount))
	}
	if len(violation.Signals) > 0 {
		parts = append(parts, strings.Join(violation.Signals, "+"))
	}
	if len(parts) == 0 {
		return FormatDim("watching")
	}

	return strings.Join(parts, ", ")
}
