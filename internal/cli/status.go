package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/russellb/canhazgpu/internal/gpu"
	"github.com/russellb/canhazgpu/internal/redis_client"
	"github.com/russellb/canhazgpu/internal/types"
	"github.com/russellb/canhazgpu/internal/utils"
	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show current GPU allocation status",
	Long: `Show the current status of all GPUs including:
- Which GPUs are available
- Which GPUs are reserved and by whom
- GPU usage validation via nvidia-smi
- Unreserved usage detection

Remote host status:
- Use --remote/-r <address> to check status on a specific remote host
- Use --all to check status on all configured remote hosts
- Configure remote hosts in ~/.canhazgpu.yaml under remote_hosts

Summary mode:
- Use --summary or -s to show a condensed summary
- Works with local, --remote, or --all modes`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runStatus(cmd.Context())
	},
}

var (
	jsonOutput   bool
	showAll      bool
	remoteName   string
	showSummary  bool
	noColorFlag  bool
	verboseCount int
	noSchedule   bool
)

func init() {
	statusCmd.Flags().BoolVarP(&jsonOutput, "json", "j", false, "Output status as JSON array")
	statusCmd.Flags().BoolVar(&showAll, "all", false, "Show status for all configured remote hosts")
	statusCmd.Flags().StringVarP(&remoteName, "remote", "r", "", "Show status for a specific remote host")
	statusCmd.Flags().BoolVarP(&showSummary, "summary", "s", false, "Show summary with GPU counts and availability")
	statusCmd.Flags().BoolVar(&noColorFlag, "no-color", false, "Disable colored output")
	statusCmd.Flags().BoolVar(&noSchedule, "no-schedule", false, "Do not print today's schedule after the status table")
	statusCmd.Flags().CountVarP(&verboseCount, "verbose", "v",
		"Show more process detail in DETAILS: -v adds process names (max 2), -vv shows all")
	rootCmd.AddCommand(statusCmd)
}

func runStatus(ctx context.Context) error {
	config := getConfig()

	// Set color mode
	SetNoColor(noColorFlag)

	// Validate flags
	if showAll && remoteName != "" {
		return fmt.Errorf("cannot use --all and --remote together")
	}

	// Determine execution mode
	if showAll {
		return runStatusAllHosts(ctx, config)
	} else if remoteName != "" {
		return runStatusRemoteHost(ctx, remoteName)
	} else {
		return runStatusLocal(ctx, config)
	}
}

func runStatusLocal(ctx context.Context, config *types.Config) error {
	client := redis_client.NewClient(config)
	defer func() {
		if err := client.Close(); err != nil {
			fmt.Printf("Warning: failed to close Redis client: %v\n", err)
		}
	}()

	// Test Redis connection
	if err := client.Ping(ctx); err != nil {
		return fmt.Errorf("failed to connect to Redis: %v", err)
	}

	// Create allocation engine and get status
	engine := gpu.NewAllocationEngine(client, config)

	// Clean up expired reservations first
	if err := engine.CleanupExpiredReservations(ctx); err != nil {
		fmt.Printf("Warning: Failed to cleanup expired reservations: %v\n", err)
	}

	statuses, err := engine.GetGPUStatus(ctx)
	if err != nil {
		return fmt.Errorf("failed to get GPU status: %v", err)
	}

	// Display status in requested format
	if showSummary {
		displaySingleHostSummary("localhost", statuses)
	} else if jsonOutput {
		return displayGPUStatusJSON(statuses)
	} else {
		displayGPUStatusTable(statuses)

		// Default: append today's booking schedule below the status table so
		// one command shows both what is happening now and what is booked
		if !noSchedule {
			if err := runSchedule(ctx, scheduleOptions{date: "today", days: 1}); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to show schedule: %v\n", err)
			}
		}
	}

	return nil
}

func runStatusRemoteHost(ctx context.Context, host string) error {
	statuses, err := getRemoteStatus(ctx, host)
	if err != nil {
		return fmt.Errorf("failed to get status from %s: %v", host, err)
	}

	if showSummary {
		displaySingleHostSummary(host, statuses)
	} else if jsonOutput {
		return displayGPUStatusJSON(statuses)
	} else {
		fmt.Printf("Status for %s:\n", host)
		displayGPUStatusTable(statuses)
	}

	return nil
}

func runStatusAllHosts(ctx context.Context, config *types.Config) error {
	// Check if localhost Redis is available
	localhostAvail := checkLocalhostAvailable(ctx, config)

	// If localhost is not available and no remote hosts, fail
	if !localhostAvail && len(config.RemoteHosts) == 0 {
		return fmt.Errorf("failed to connect to Redis and no remote hosts configured")
	}

	// Warn if localhost is not available but we have remote hosts
	if !localhostAvail {
		fmt.Println("Warning: Redis not available locally, showing remote hosts only")
	}

	// JSON mode needs to collect all results first
	if jsonOutput {
		return runStatusAllHostsJSON(ctx, config, localhostAvail)
	}

	// For summary mode, collect all results first then display in table
	if showSummary {
		return runStatusAllHostsSummary(ctx, config, localhostAvail)
	}

	// Fetch all host statuses in parallel
	results := getAllHostStatuses(ctx, config, localhostAvail)

	// Display results in order
	for _, result := range results {
		fmt.Println()
		if result.err != nil {
			fmt.Printf("┌─ %s ─┐\n", FormatHost(result.host))
			fmt.Printf("│ %s\n", FormatDim(fmt.Sprintf("ERROR: %v", result.err)))
			fmt.Println("└────────────┘")
		} else {
			fmt.Printf("┌─ %s ─┐\n", FormatHost(result.host))
			displayGPUStatusTable(result.statuses)
		}
	}

	return nil
}

// runStatusAllHostsSummary collects all results then displays summary table
func runStatusAllHostsSummary(ctx context.Context, config *types.Config, localhostAvail bool) error {
	// Fetch all host statuses in parallel
	results := getAllHostStatuses(ctx, config, localhostAvail)

	// Create table
	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	t.SetStyle(table.StyleLight)
	t.Style().Options.SeparateRows = false
	t.Style().Options.DrawBorder = false

	// Set header
	t.AppendHeader(table.Row{
		FormatHeader("HOST"),
		FormatHeader("TOTAL"),
		FormatHeader("GPU MODELS"),
		FormatHeader("AVAILABLE"),
		FormatHeader("IN USE"),
	})

	// Add rows for all hosts
	for _, result := range results {
		if result.err != nil {
			t.AppendRow(table.Row{
				FormatHost(result.host),
				FormatDim("ERR"),
				FormatDim(fmt.Sprintf("ERROR: %v", result.err)),
				FormatDim("-"),
				FormatDim("-"),
			})
		} else {
			addSummaryRow(t, result.host, result.statuses)
		}
	}

	fmt.Println()
	t.Render()
	fmt.Println()

	return nil
}

// runStatusAllHostsJSON collects all results then outputs JSON
func runStatusAllHostsJSON(ctx context.Context, config *types.Config, localhostAvail bool) error {
	// Fetch all host statuses in parallel
	results := getAllHostStatuses(ctx, config, localhostAvail)

	// Output all statuses as JSON
	allStatuses := make(map[string]any)
	for _, result := range results {
		if result.err != nil {
			allStatuses[result.host] = map[string]string{"error": result.err.Error()}
		} else {
			allStatuses[result.host] = result.statuses
		}
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(allStatuses)
}

func getLocalStatus(ctx context.Context, config *types.Config) ([]gpu.GPUStatusInfo, error) {
	client := redis_client.NewClient(config)
	defer func() {
		_ = client.Close()
	}()

	if err := client.Ping(ctx); err != nil {
		return nil, fmt.Errorf("failed to connect to Redis: %v", err)
	}

	engine := gpu.NewAllocationEngine(client, config)

	// Cleanup expired reservations
	_ = engine.CleanupExpiredReservations(ctx)

	return engine.GetGPUStatus(ctx)
}

// hostResult holds the status result for a single host
type hostResult struct {
	host     string
	statuses []gpu.GPUStatusInfo
	err      error
}

// checkLocalhostAvailable tests if local Redis is available
func checkLocalhostAvailable(ctx context.Context, config *types.Config) bool {
	client := redis_client.NewClient(config)
	defer func() {
		_ = client.Close()
	}()
	return client.Ping(ctx) == nil
}

// getAllHostStatuses fetches status from localhost and all remote hosts in parallel
// If includeLocalhost is false, localhost is skipped
func getAllHostStatuses(ctx context.Context, config *types.Config, includeLocalhost bool) []hostResult {
	// Calculate total hosts
	totalHosts := len(config.RemoteHosts)
	if includeLocalhost {
		totalHosts++
	}

	if totalHosts == 0 {
		return nil
	}

	results := make([]hostResult, totalHosts)

	var wg sync.WaitGroup
	wg.Add(totalHosts)

	resultIndex := 0

	// Fetch localhost status in parallel (if included)
	if includeLocalhost {
		go func() {
			defer wg.Done()
			statuses, err := getLocalStatus(ctx, config)
			results[0] = hostResult{
				host:     "localhost",
				statuses: statuses,
				err:      err,
			}
		}()
		resultIndex = 1
	}

	// Fetch each remote host status in parallel
	for i, host := range config.RemoteHosts {
		go func(index int, h string) {
			defer wg.Done()
			statuses, err := getRemoteStatus(ctx, h)
			results[index] = hostResult{
				host:     h,
				statuses: statuses,
				err:      err,
			}
		}(resultIndex+i, host)
	}

	wg.Wait()
	return results
}

func getRemoteStatus(ctx context.Context, host string) ([]gpu.GPUStatusInfo, error) {
	// Execute remote status command with JSON output
	stdout, stderr, err := utils.ExecuteRemoteCanHazGPU(ctx, host, []string{"status", "--json"})
	if err != nil {
		if stderr != "" {
			return nil, fmt.Errorf("%v: %s", err, stderr)
		}
		return nil, err
	}

	// Parse JSON output
	var statuses []JSONGPUStatus
	if err := json.Unmarshal([]byte(stdout), &statuses); err != nil {
		return nil, fmt.Errorf("failed to parse JSON output: %v", err)
	}

	// Convert to GPUStatusInfo
	result := make([]gpu.GPUStatusInfo, len(statuses))
	for i, s := range statuses {
		result[i] = convertJSONToStatusInfo(s)
	}

	// Check if any GPU is missing model info (older canhazgpu version)
	needsGPUModel := false
	for _, s := range result {
		if s.GPUModel == "" {
			needsGPUModel = true
			break
		}
	}

	// If GPU model is missing, try to get it directly via nvidia-smi or amd-smi
	if needsGPUModel {
		gpuModel := getRemoteGPUModel(ctx, host)
		if gpuModel != "" {
			// Apply the model to all GPUs (assumes homogeneous system)
			for i := range result {
				if result[i].GPUModel == "" {
					result[i].GPUModel = gpuModel
				}
			}
		}
	}

	return result, nil
}

// getRemoteGPUModel tries to detect GPU model directly via nvidia-smi or amd-smi
func getRemoteGPUModel(ctx context.Context, host string) string {
	// Try nvidia-smi first
	stdout, _, err := utils.ExecuteRemoteCommand(ctx, host,
		"nvidia-smi --query-gpu=gpu_name --format=csv,noheader,nounits | head -1")
	if err == nil && stdout != "" {
		return strings.TrimSpace(stdout)
	}

	// Try amd-smi
	stdout, _, err = utils.ExecuteRemoteCommand(ctx, host,
		"amd-smi list --json 2>/dev/null | jq -r '.[0].name' 2>/dev/null")
	if err == nil && stdout != "" && stdout != "null" {
		return strings.TrimSpace(stdout)
	}

	return ""
}

func convertJSONToStatusInfo(j JSONGPUStatus) gpu.GPUStatusInfo {
	status := gpu.GPUStatusInfo{
		GPUID:  j.GPUID,
		Status: j.Status,
		User:   j.User,
		Note:   j.Note,
	}

	// Parse duration if present
	if j.Duration != "" {
		// Duration is formatted as "Xh Ym Zs", we'll store it as-is for display
		// but we need a time.Duration for the struct
		// For remote status, we don't have the exact duration, so we'll estimate
		status.Duration = 0 // Will use details field instead
	}

	status.ReservationType = strings.ToLower(j.ReservationType)
	status.ValidationInfo = j.ValidationInfo
	status.ProcessInfo = j.ProcessInfo
	status.UnreservedUsers = j.UnreservedUsers
	status.Error = j.Error
	status.BookingID = j.BookingID
	status.ForeignUsers = j.ForeignUsers
	status.ForeignProcesses = j.ForeignProcesses
	status.ForeignMemoryMB = j.ForeignMemoryMB
	status.Processes = j.Processes
	status.UtilizationPercent = j.UtilizationPercent

	if j.LastReleased != nil {
		status.LastReleased = *j.LastReleased
	}
	if j.LastHeartbeat != nil {
		status.LastHeartbeat = *j.LastHeartbeat
	}
	if j.ExpiryTime != nil {
		status.ExpiryTime = *j.ExpiryTime
	}

	if j.ModelInfo != nil {
		status.ModelInfo = &gpu.ModelInfo{
			Provider: j.ModelInfo.Provider,
			Model:    j.ModelInfo.Model,
		}
	}

	status.GPUModel = j.GPUModel

	return status
}

func displaySingleHostSummary(host string, statuses []gpu.GPUStatusInfo) {
	// Create table
	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	t.SetStyle(table.StyleLight)
	t.Style().Options.SeparateRows = false
	t.Style().Options.DrawBorder = false

	// Set header
	t.AppendHeader(table.Row{
		FormatHeader("HOST"),
		FormatHeader("TOTAL"),
		FormatHeader("GPU MODELS"),
		FormatHeader("AVAILABLE"),
		FormatHeader("IN USE"),
	})

	// Add single row
	addSummaryRow(t, host, statuses)

	fmt.Println()
	t.Render()
	fmt.Println()
}

func addSummaryRow(t table.Writer, host string, statuses []gpu.GPUStatusInfo) {
	totalGPUs := len(statuses)
	availableCount := 0
	inUseCount := 0

	// Count by GPU model type
	modelCounts := make(map[string]int)

	for _, status := range statuses {
		// Get GPU model from provider info
		gpuModel := ""
		if status.GPUModel != "" {
			gpuModel = status.GPUModel
		}

		if gpuModel != "" {
			modelCounts[gpuModel]++
		}

		switch status.Status {
		case "AVAILABLE":
			availableCount++
		case "IN_USE", "UNRESERVED":
			// Combine reserved and unreserved usage into IN_USE
			inUseCount++
		}
	}

	// Build GPU models string
	var modelsStr string
	if len(modelCounts) == 0 {
		modelsStr = "-"
	} else if len(modelCounts) == 1 {
		for model := range modelCounts {
			modelsStr = model
		}
	} else {
		models := make([]string, 0, len(modelCounts))
		for model := range modelCounts {
			models = append(models, model)
		}
		sort.Strings(models)
		var parts []string
		for _, model := range models {
			parts = append(parts, fmt.Sprintf("%d %s", modelCounts[model], model))
		}
		modelsStr = strings.Join(parts, ", ")
	}

	// Format available column: show checkmark if > 0, X if 0
	var availStr string
	if availableCount > 0 {
		availStr = fmt.Sprintf("%s %d", colorSuccess.Sprint("✓"), availableCount)
	} else {
		availStr = fmt.Sprintf("%s %d", colorError.Sprint("✗"), availableCount)
	}

	// Format in-use column: just the number, no symbol
	inUseStr := fmt.Sprintf("%d", inUseCount)

	t.AppendRow(table.Row{
		FormatHost(host),
		FormatMetric(totalGPUs),
		FormatDim(modelsStr),
		availStr,
		inUseStr,
	})
}

func displayGPUStatusTable(statuses []gpu.GPUStatusInfo) {
	// Check if any GPU has model information
	hasModels := false
	for _, status := range statuses {
		if status.ModelInfo != nil && status.ModelInfo.Model != "" {
			hasModels = true
			break
		}
	}

	// Create table
	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)

	// Set style
	t.SetStyle(table.StyleLight)
	t.Style().Options.SeparateRows = false
	t.Style().Options.DrawBorder = false

	// Set header
	if hasModels {
		t.AppendHeader(table.Row{
			FormatHeader("GPU"), FormatHeader("STATUS"), FormatHeader("USER"),
			FormatHeader("DURATION"), FormatHeader("TYPE"), FormatHeader("DETAILS"),
			FormatHeader("MEMORY"), FormatHeader("MODEL"), FormatHeader("NOTE"),
			FormatHeader("UTIL"),
		})
	} else {
		t.AppendHeader(table.Row{
			FormatHeader("GPU"), FormatHeader("STATUS"), FormatHeader("USER"),
			FormatHeader("DURATION"), FormatHeader("TYPE"), FormatHeader("DETAILS"),
			FormatHeader("MEMORY"), FormatHeader("NOTE"), FormatHeader("UTIL"),
		})
	}

	// Add rows
	for _, status := range statuses {
		addGPUStatusRow(t, status, hasModels)
	}

	t.Render()
}

func addGPUStatusRow(t table.Writer, status gpu.GPUStatusInfo, includeModel bool) {
	gpuID := fmt.Sprintf("%d", status.GPUID)

	switch status.Status {
	case "AVAILABLE":
		var details string
		if status.LastReleased.IsZero() {
			details = "never used"
		} else {
			details = fmt.Sprintf("free for %s", utils.FormatDuration(time.Since(status.LastReleased)))
		}

		// Clean validation info
		validation := strings.TrimSpace(strings.Trim(status.ValidationInfo, "[]"))
		validation = strings.TrimPrefix(validation, "validated: ")

		// Set model info
		model := "-"
		if status.ModelInfo != nil && status.ModelInfo.Model != "" {
			model = status.ModelInfo.Model
		}

		if includeModel {
			t.AppendRow(table.Row{
				gpuID, FormatStatus("AVAILABLE"), FormatDim("-"), FormatDim("-"), FormatDim("-"),
				details, FormatDim(validation), model, FormatDim("-"), formatUtilization(status.UtilizationPercent),
			})
		} else {
			t.AppendRow(table.Row{
				gpuID, FormatStatus("AVAILABLE"), FormatDim("-"), FormatDim("-"), FormatDim("-"),
				details, FormatDim(validation), FormatDim("-"), formatUtilization(status.UtilizationPercent),
			})
		}

	case "IN_USE":
		user := status.User
		duration := utils.FormatDuration(status.Duration)
		reservationType := strings.ToUpper(status.ReservationType)

		// A reserved GPU that somebody else is using looks like a perfectly
		// normal reservation unless it is called out
		statusCell := FormatStatus("IN_USE")
		if status.HasForeignUsage() {
			statusCell = FormatStatus("FOREIGN")
		}

		var details string
		switch status.ReservationType {
		case "run":
			if !status.LastHeartbeat.IsZero() {
				details = fmt.Sprintf("heartbeat %s", utils.FormatTimeAgo(status.LastHeartbeat))
			} else {
				details = "active"
			}
			if idle := formatIdleStatus(status); idle != "" {
				details += ", " + idle
			}
		case "manual":
			if !status.ExpiryTime.IsZero() {
				details = fmt.Sprintf("expires %s", utils.FormatTimeUntil(status.ExpiryTime))
			} else {
				details = "manual reservation"
			}
			if idle := formatIdleStatus(status); idle != "" {
				details += ", " + idle
			}
			if status.BookingID != "" {
				details += ", booked"
			}
		}

		if foreign := formatForeignUsage(status); foreign != "" {
			details += ", " + foreign
		}
		if processes := formatProcessRuntime(status.Processes, verboseCount); processes != "" {
			details += ", " + processes
		}

		// Clean validation info
		validation := strings.TrimSpace(strings.Trim(status.ValidationInfo, "[]"))
		validation = strings.TrimPrefix(validation, "validated: ")

		// Set model info
		model := "-"
		if status.ModelInfo != nil && status.ModelInfo.Model != "" {
			model = status.ModelInfo.Model
		}

		// Format note
		note := "-"
		if status.Note != "" {
			note = status.Note
		}

		if includeModel {
			t.AppendRow(table.Row{
				gpuID, statusCell, user, duration, reservationType, details, FormatDim(validation), model, note,
				formatUtilization(status.UtilizationPercent),
			})
		} else {
			t.AppendRow(table.Row{
				gpuID, statusCell, user, duration, reservationType, details, FormatDim(validation), note,
				formatUtilization(status.UtilizationPercent),
			})
		}

	case "UNRESERVED":
		userList := utils.FormatUserList(status.UnreservedUsers, 2)
		details := status.ProcessInfo
		if formatted := formatUnreservedDetails(status, verboseCount); formatted != "" {
			details = formatted
		}

		// Set model info
		model := "-"
		if status.ModelInfo != nil && status.ModelInfo.Model != "" {
			model = status.ModelInfo.Model
		}

		// Clean validation info
		validation := strings.TrimSpace(strings.Trim(status.ValidationInfo, "[]"))
		validation = strings.TrimPrefix(validation, "validated: ")

		if includeModel {
			t.AppendRow(table.Row{
				gpuID, FormatStatus("UNRESERVED"), userList, FormatDim("-"), FormatDim("-"),
				details, FormatDim(validation), model, FormatDim("-"), formatUtilization(status.UtilizationPercent),
			})
		} else {
			t.AppendRow(table.Row{
				gpuID, FormatStatus("UNRESERVED"), userList, FormatDim("-"), FormatDim("-"),
				details, FormatDim(validation), FormatDim("-"), formatUtilization(status.UtilizationPercent),
			})
		}

	case "ERROR":
		if includeModel {
			t.AppendRow(table.Row{
				gpuID, FormatStatus("ERROR"), FormatDim("-"), FormatDim("-"), FormatDim("-"),
				status.Error, FormatDim("-"), FormatDim("-"), FormatDim("-"), FormatDim("-"),
			})
		} else {
			t.AppendRow(table.Row{
				gpuID, FormatStatus("ERROR"), FormatDim("-"), FormatDim("-"), FormatDim("-"),
				status.Error, FormatDim("-"), FormatDim("-"), FormatDim("-"),
			})
		}

	default:
		if includeModel {
			t.AppendRow(table.Row{
				gpuID, "UNKNOWN", FormatDim("-"), FormatDim("-"), FormatDim("-"),
				"unknown status", FormatDim("-"), FormatDim("-"), FormatDim("-"), FormatDim("-"),
			})
		} else {
			t.AppendRow(table.Row{
				gpuID, "UNKNOWN", FormatDim("-"), FormatDim("-"), FormatDim("-"),
				"unknown status", FormatDim("-"), FormatDim("-"), FormatDim("-"),
			})
		}
	}
}

// formatUtilization renders a GPU utilization percentage for the UTIL column.
func formatUtilization(percent int) string {
	return fmt.Sprintf("%d%%", percent)
}

// formatProcessRuntime describes the processes currently using a GPU. The
// default shows only PID and elapsed time (e.g. "PID 123 (2h3m)"); -v adds
// process names for at most two processes, and -vv shows every process with
// its name.
func formatProcessRuntime(processes []types.GPUProcessInfo, verbose int) string {
	const (
		maxProcesses      = 3 // default cap, PID + time only
		maxNamedProcesses = 2 // cap with process names (-v)
	)
	if len(processes) == 0 {
		return ""
	}

	includeNames := verbose >= 1
	limit := maxProcesses
	if includeNames {
		limit = maxNamedProcesses
	}
	if verbose >= 2 {
		limit = len(processes) // -vv: show everything
	}

	parts := make([]string, 0, len(processes))
	for _, proc := range processes {
		if len(parts) >= limit {
			break
		}

		desc := fmt.Sprintf("PID %d", proc.PID)
		var detail []string
		if includeNames && proc.ProcessName != "" {
			detail = append(detail, proc.ProcessName)
		}
		if proc.ElapsedSeconds > 0 {
			detail = append(detail, utils.FormatDurationShort(time.Duration(proc.ElapsedSeconds)*time.Second))
		}
		if len(detail) > 0 {
			desc += " (" + strings.Join(detail, ", ") + ")"
		}
		parts = append(parts, desc)
	}
	if len(processes) > limit {
		parts = append(parts, fmt.Sprintf("+%d more", len(processes)-limit))
	}

	return "processes: " + strings.Join(parts, ", ")
}

// formatUnreservedDetails describes the processes behind unreserved usage, e.g.
// "used by PID 123 (2h3m), PID 456 (5m)", honoring the -v/-vv verbosity levels.
// Memory usage lives in the MEMORY column. Returns an empty string when no
// process list is available (older remote hosts), so callers can fall back to
// ProcessInfo.
func formatUnreservedDetails(status gpu.GPUStatusInfo, verbose int) string {
	if len(status.Processes) == 0 {
		return ""
	}

	processes := strings.TrimPrefix(formatProcessRuntime(status.Processes, verbose), "processes: ")
	return "used by " + processes
}

// formatIdleStatus describes how much of a reservation's idle grace period is
// left, and is empty when the reservation has no idle timeout or is in use
func formatIdleStatus(status gpu.GPUStatusInfo) string {
	// Below a minute the countdown is noise: the reservation was just made or
	// the GPU was in use moments ago
	if status.IdleTimeout <= 0 || status.IdleFor < time.Minute {
		return ""
	}

	remaining := status.IdleTimeout - status.IdleFor
	if remaining <= 0 {
		return "idle, releasing"
	}

	return fmt.Sprintf("idle %s, released in %s",
		utils.FormatDurationShort(status.IdleFor), utils.FormatDurationShort(remaining))
}

// formatForeignUsage describes usage by somebody other than the reservation
// holder, and is empty when there is none
func formatForeignUsage(status gpu.GPUStatusInfo) string {
	if !status.HasForeignUsage() {
		return ""
	}

	// The memory figure lives in the MEMORY column; DETAILS only says who
	return fmt.Sprintf("used by %s", utils.FormatUserList(status.ForeignUsers, 2))
}

// JSONGPUStatus represents a GPU status for JSON output
type JSONGPUStatus struct {
	GPUID           int            `json:"gpu_id"`
	Status          string         `json:"status"`
	User            string         `json:"user,omitempty"`
	Duration        string         `json:"duration,omitempty"`
	ReservationType string         `json:"type,omitempty"`
	Note            string         `json:"note,omitempty"`
	Details         string         `json:"details,omitempty"`
	ValidationInfo  string         `json:"validation,omitempty"`
	ModelInfo       *JSONModelInfo `json:"model,omitempty"`
	GPUModel        string         `json:"gpu_model,omitempty"`
	LastReleased    *time.Time     `json:"last_released,omitempty"`
	LastHeartbeat   *time.Time     `json:"last_heartbeat,omitempty"`
	ExpiryTime      *time.Time     `json:"expiry_time,omitempty"`
	UnreservedUsers []string       `json:"unreserved_users,omitempty"`
	ProcessInfo     string         `json:"process_info,omitempty"`
	Error           string         `json:"error,omitempty"`
	IdleTimeout     string         `json:"idle_timeout,omitempty"`
	IdleFor         string         `json:"idle_for,omitempty"`
	BookingID       string         `json:"booking_id,omitempty"`

	// Usage by somebody other than the reservation holder. The status stays
	// IN_USE so existing consumers keep working; these fields carry the detail.
	ForeignUsers     []string `json:"foreign_users,omitempty"`
	ForeignProcesses int      `json:"foreign_processes,omitempty"`
	ForeignMemoryMB  int      `json:"foreign_memory_mb,omitempty"`

	// Processes currently using this GPU, with owner and elapsed time
	Processes []types.GPUProcessInfo `json:"processes,omitempty"`

	UtilizationPercent int `json:"utilization_percent,omitempty"` // GPU utilization (0-100)
}

// JSONModelInfo represents model information for JSON output
type JSONModelInfo struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model"`
}

func displayGPUStatusJSON(statuses []gpu.GPUStatusInfo) error {
	jsonStatuses := make([]JSONGPUStatus, len(statuses))

	for i, status := range statuses {
		jsonStatus := JSONGPUStatus{
			GPUID:  status.GPUID,
			Status: status.Status,
		}

		// Add optional fields based on status
		if status.User != "" {
			jsonStatus.User = status.User
		}

		if status.Duration > 0 {
			jsonStatus.Duration = utils.FormatDuration(status.Duration)
		}

		if status.ReservationType != "" {
			jsonStatus.ReservationType = strings.ToUpper(status.ReservationType)
		}

		if status.Note != "" {
			jsonStatus.Note = status.Note
		}

		// Add details based on status type
		switch status.Status {
		case "AVAILABLE":
			if status.LastReleased.IsZero() {
				jsonStatus.Details = "never used"
			} else {
				jsonStatus.Details = fmt.Sprintf("free for %s", utils.FormatDuration(time.Since(status.LastReleased)))
				jsonStatus.LastReleased = &status.LastReleased
			}

		case "IN_USE":
			switch status.ReservationType {
			case "run":
				if !status.LastHeartbeat.IsZero() {
					jsonStatus.Details = fmt.Sprintf("heartbeat %s", utils.FormatTimeAgo(status.LastHeartbeat))
					jsonStatus.LastHeartbeat = &status.LastHeartbeat
				} else {
					jsonStatus.Details = "active"
				}
				if status.IdleTimeout > 0 {
					jsonStatus.IdleTimeout = utils.FormatDurationShort(status.IdleTimeout)
					if status.IdleFor > 0 {
						jsonStatus.IdleFor = utils.FormatDurationShort(status.IdleFor)
					}
				}
			case "manual":
				if !status.ExpiryTime.IsZero() {
					jsonStatus.Details = fmt.Sprintf("expires %s", utils.FormatTimeUntil(status.ExpiryTime))
					jsonStatus.ExpiryTime = &status.ExpiryTime
				} else {
					jsonStatus.Details = "manual reservation"
				}
				if status.IdleTimeout > 0 {
					jsonStatus.IdleTimeout = utils.FormatDurationShort(status.IdleTimeout)
					if status.IdleFor > 0 {
						jsonStatus.IdleFor = utils.FormatDurationShort(status.IdleFor)
					}
				}
				jsonStatus.BookingID = status.BookingID
			}

			if status.HasForeignUsage() {
				jsonStatus.ForeignUsers = status.ForeignUsers
				jsonStatus.ForeignProcesses = status.ForeignProcesses
				jsonStatus.ForeignMemoryMB = status.ForeignMemoryMB
				jsonStatus.Details += ", " + formatForeignUsage(status)
			}
			if processes := formatProcessRuntime(status.Processes, verboseCount); processes != "" {
				jsonStatus.Details += ", " + processes
			}

		case "UNRESERVED":
			jsonStatus.Details = "WITHOUT RESERVATION"
			if len(status.UnreservedUsers) > 0 {
				jsonStatus.UnreservedUsers = status.UnreservedUsers
			}
			if formatted := formatUnreservedDetails(status, verboseCount); formatted != "" {
				jsonStatus.ProcessInfo = formatted
			} else if status.ProcessInfo != "" {
				jsonStatus.ProcessInfo = status.ProcessInfo
			}

		case "ERROR":
			if status.Error != "" {
				jsonStatus.Error = status.Error
				jsonStatus.Details = status.Error
			}

		default:
			jsonStatus.Details = "unknown status"
		}

		// Clean and add validation info
		if status.ValidationInfo != "" {
			validation := strings.TrimSpace(strings.Trim(status.ValidationInfo, "[]"))
			validation = strings.TrimPrefix(validation, "validated: ")
			jsonStatus.ValidationInfo = validation
		}

		// Add model info if present
		if status.ModelInfo != nil && status.ModelInfo.Model != "" {
			jsonStatus.ModelInfo = &JSONModelInfo{
				Provider: status.ModelInfo.Provider,
				Model:    status.ModelInfo.Model,
			}
		}

		// Add GPU model if present
		if status.GPUModel != "" {
			jsonStatus.GPUModel = status.GPUModel
		}

		// Add process details if present
		if len(status.Processes) > 0 {
			jsonStatus.Processes = status.Processes
		}

		// Add GPU utilization if present
		if status.UtilizationPercent > 0 {
			jsonStatus.UtilizationPercent = status.UtilizationPercent
		}

		jsonStatuses[i] = jsonStatus
	}

	// Output as pretty-printed JSON
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(jsonStatuses)
}
