package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/russellb/canhazgpu/internal/gpu"
	"github.com/russellb/canhazgpu/internal/redis_client"
	"github.com/russellb/canhazgpu/internal/types"
	"github.com/russellb/canhazgpu/internal/utils"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var scheduleCmd = &cobra.Command{
	Use:   "schedule",
	Short: "Show the GPU booking schedule for a day",
	Long: `Display who has which GPUs when, like a meeting room booking sheet.

Each row is a GPU and each pair of characters is one hour, so every character
covers 30 minutes. The grid shows both reservations that are active right now
and bookings made for later with 'canhazgpu reserve --start'.

Bookings are created by the reserve command:
  canhazgpu reserve --start 14:00 --end 16:00 --gpus 2
  canhazgpu reserve --start 'tomorrow 09:00' --duration 4h --gpus 8

A booking holds specific GPUs for its window: those GPUs are not handed out to
reservations that would still be holding them when the booking starts, and when
the window arrives the booking becomes a normal manual reservation that expires
at the end of the window.

Example usage:
  canhazgpu schedule                        # today
  canhazgpu schedule --date tomorrow
  canhazgpu schedule --date 2026-08-04 --days 3
  canhazgpu schedule --json
  canhazgpu schedule --cancel 3f9ac21b      # cancel one of your bookings`,
	RunE: func(cmd *cobra.Command, args []string) error {
		opts := scheduleOptions{
			date:      viper.GetString("schedule.date"),
			days:      viper.GetInt("schedule.days"),
			json:      viper.GetBool("schedule.json"),
			cancelRef: viper.GetString("schedule.cancel"),
			force:     viper.GetBool("schedule.force"),
		}

		SetNoColor(viper.GetBool("schedule.no-color"))

		return runSchedule(cmd.Context(), opts)
	},
}

type scheduleOptions struct {
	date      string
	days      int
	json      bool
	cancelRef string
	force     bool
}

func init() {
	scheduleCmd.Flags().String("date", "today", "Day to display (YYYY-MM-DD, today, tomorrow, +1d)")
	scheduleCmd.Flags().Int("days", 1, "Number of days to display starting at --date")
	scheduleCmd.Flags().Bool("json", false, "Output the schedule as JSON")
	scheduleCmd.Flags().String("cancel", "", "Cancel a booking by (abbreviated) ID")
	scheduleCmd.Flags().Bool("force", false, "Allow cancelling a booking that belongs to someone else")
	scheduleCmd.Flags().Bool("no-color", false, "Disable colored output")

	rootCmd.AddCommand(scheduleCmd)
}

func runSchedule(ctx context.Context, opts scheduleOptions) error {
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

	engine := gpu.NewAllocationEngine(client, config)

	if opts.cancelRef != "" {
		return runScheduleCancel(ctx, engine, opts)
	}

	// Activate bookings that just started and drop expired reservations, so the
	// schedule reflects reality
	if err := engine.CleanupExpiredReservations(ctx); err != nil {
		fmt.Printf("Warning: failed to cleanup expired reservations: %v\n", err)
	}

	gpuCount, err := client.GetGPUCount(ctx)
	if err != nil {
		return err
	}

	now := time.Now()

	firstDay, err := utils.ParseDaySpec(opts.date, now)
	if err != nil {
		return err
	}

	days := opts.days
	if days <= 0 {
		days = 1
	}

	reservations, err := currentReservations(ctx, client, gpuCount, now)
	if err != nil {
		return err
	}

	bookings, err := engine.ListBookings(ctx, firstDay, firstDay.AddDate(0, 0, days))
	if err != nil {
		return err
	}

	if opts.json {
		return printScheduleJSON(firstDay, days, gpuCount, bookings, reservations)
	}

	// What each GPU is doing at this moment, shown in the NOW column
	nowStatus := currentGPUUsage(ctx, engine, client, gpuCount)

	for day := 0; day < days; day++ {
		dayStart := firstDay.AddDate(0, 0, day)
		if day > 0 {
			fmt.Println()
		}
		printScheduleDay(dayStart, gpuCount, bookings, reservations, nowStatus, now)
	}

	return nil
}

func runScheduleCancel(ctx context.Context, engine *gpu.AllocationEngine, opts scheduleOptions) error {
	wasActive := false
	if booking, err := engine.FindBooking(ctx, opts.cancelRef); err == nil {
		wasActive = booking.Status == types.BookingStatusActive
	}

	booking, err := engine.CancelBooking(ctx, opts.cancelRef, getCurrentUser(), opts.force)
	if err != nil {
		return err
	}

	fmt.Printf("Cancelled booking %s: %s, GPU %s, %s\n",
		booking.ShortID(), booking.User, formatGPUList(booking.GPUIDs),
		gpu.FormatBookingWindow(booking.StartTime.ToTime(), booking.EndTime.ToTime(), time.Now()))

	if wasActive {
		fmt.Printf("The booking was active: GPU %s released\n", formatGPUList(booking.GPUIDs))
	}

	return nil
}

// scheduleEntry is one occupied stretch of time on one or more GPUs, coming
// either from a booking or from a reservation that is active right now
type scheduleEntry struct {
	user      string
	gpuIDs    []int
	start     time.Time
	end       time.Time
	openEnded bool // Run-type reservations have no known end time
	note      string
	bookingID string
	status    string // Booking status, empty for plain reservations
	kind      string // "booking" or "manual"/"run"
	symbol    string
}

// covers reports whether this entry occupies the given GPU at a point in time,
// treating the window as [start, end)
func (e *scheduleEntry) covers(gpuID int, at time.Time) bool {
	if at.Before(e.start) || !at.Before(e.end) {
		return false
	}
	for _, id := range e.gpuIDs {
		if id == gpuID {
			return true
		}
	}
	return false
}

// currentReservations reads the live GPU reservations straight from Redis.
// Reservations created by a booking are skipped: the booking itself already
// describes that window.
func currentReservations(ctx context.Context, client *redis_client.Client, gpuCount int, now time.Time) ([]*scheduleEntry, error) {
	var entries []*scheduleEntry

	for gpuID := 0; gpuID < gpuCount; gpuID++ {
		state, err := client.GetGPUState(ctx, gpuID)
		if err != nil || state.User == "" || state.BookingID != "" {
			continue
		}

		entry := &scheduleEntry{
			user:   state.User,
			gpuIDs: []int{gpuID},
			start:  state.StartTime.ToTime(),
			note:   state.Note,
			kind:   state.Type,
		}

		switch {
		case !state.ExpiryTime.ToTime().IsZero():
			entry.end = state.ExpiryTime.ToTime()
		default:
			// Run-type reservations end when the process does - show them up to
			// the end of the slot that is currently in progress
			entry.end = now.Truncate(types.BookingSlotDuration).Add(types.BookingSlotDuration)
			entry.openEnded = true
		}

		if entry.start.IsZero() {
			entry.start = now
		}

		entries = append(entries, entry)
	}

	return entries, nil
}

// gpuNow describes what a GPU is doing at this very moment, for the NOW column
// of the schedule grid
type gpuNow struct {
	status      string // "AVAILABLE", "IN_USE", "UNRESERVED" or "" when unknown
	user        string
	kind        string // Reservation type, for reserved GPUs
	booked      bool   // Reservation comes from a scheduled booking
	memoryMB    int
	idleFor     time.Duration
	idleTimeout time.Duration
}

// currentGPUUsage collects the live state of every GPU. Full validation needs
// the GPU provider; when that is unavailable the reservation records alone still
// tell us who holds what.
func currentGPUUsage(ctx context.Context, engine *gpu.AllocationEngine, client *redis_client.Client, gpuCount int) []gpuNow {
	result := make([]gpuNow, gpuCount)

	if statuses, err := engine.GetGPUStatus(ctx); err == nil {
		for _, status := range statuses {
			if status.GPUID < 0 || status.GPUID >= gpuCount {
				continue
			}
			result[status.GPUID] = gpuNow{
				status:      status.Status,
				user:        status.User,
				kind:        status.ReservationType,
				booked:      status.BookingID != "",
				memoryMB:    status.MemoryMB,
				idleFor:     status.IdleFor,
				idleTimeout: status.IdleTimeout,
			}
			if status.Status == "UNRESERVED" {
				result[status.GPUID].user = utils.FormatUserList(status.UnreservedUsers, 2)
			}
		}
		return result
	}

	// Fall back to reservation records only
	for gpuID := 0; gpuID < gpuCount; gpuID++ {
		state, err := client.GetGPUState(ctx, gpuID)
		if err != nil {
			continue
		}
		if state.User == "" {
			result[gpuID] = gpuNow{status: "AVAILABLE"}
			continue
		}
		result[gpuID] = gpuNow{
			status: "IN_USE",
			user:   state.User,
			kind:   state.Type,
			booked: state.BookingID != "",
		}
	}

	return result
}

// formatNowCell renders one GPU's current state for the NOW column
func formatNowCell(info gpuNow) string {
	switch info.status {
	case "AVAILABLE":
		if info.memoryMB > 0 {
			return FormatDim(fmt.Sprintf("free (%dMB used)", info.memoryMB))
		}
		return colorSuccess.Sprint("free")

	case "IN_USE":
		what := colorInUse.Sprintf("%s %s", info.user, strings.ToUpper(info.kind))
		if info.booked {
			what += FormatDim(" booked")
		}

		switch {
		case info.memoryMB > 0:
			return what + FormatDim(fmt.Sprintf(" %dMB", info.memoryMB))
		case info.idleTimeout > 0 && info.idleFor >= time.Minute:
			return what + FormatDim(fmt.Sprintf(" idle %s", utils.FormatDurationShort(info.idleFor)))
		default:
			return what
		}

	case "UNRESERVED":
		return colorUnreserved.Sprintf("%s UNRESERVED", info.user) +
			FormatDim(fmt.Sprintf(" %dMB", info.memoryMB))

	case "":
		return FormatDim("?")

	default:
		return FormatDim(strings.ToLower(info.status))
	}
}

// bookingEntries converts bookings into schedule entries
func bookingEntries(bookings []*types.Booking) []*scheduleEntry {
	var entries []*scheduleEntry

	for _, booking := range bookings {
		entries = append(entries, &scheduleEntry{
			user:      booking.User,
			gpuIDs:    booking.GPUIDs,
			start:     booking.StartTime.ToTime(),
			end:       booking.EndTime.ToTime(),
			note:      booking.Note,
			bookingID: booking.ShortID(),
			status:    booking.Status,
			kind:      "booking",
		})
	}

	return entries
}

func printScheduleDay(dayStart time.Time, gpuCount int, bookings []*types.Booking, reservations []*scheduleEntry, nowStatus []gpuNow, now time.Time) {
	dayEnd := dayStart.AddDate(0, 0, 1)

	// "What is happening right now" only makes sense on the day that contains now
	isToday := !now.Before(dayStart) && now.Before(dayEnd)

	// Collect everything that touches this day, bookings first so their symbols
	// are assigned in start order
	var entries []*scheduleEntry
	for _, entry := range bookingEntries(bookings) {
		if entry.start.Before(dayEnd) && entry.end.After(dayStart) {
			entries = append(entries, entry)
		}
	}
	for _, entry := range reservations {
		if entry.start.Before(dayEnd) && entry.end.After(dayStart) {
			entries = append(entries, entry)
		}
	}

	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].start.Before(entries[j].start)
	})

	currentUser := getCurrentUser()
	symbols := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	for i, entry := range entries {
		if i < len(symbols) {
			entry.symbol = string(symbols[i])
		} else {
			entry.symbol = "*"
		}
	}

	slotsPerHour := int(time.Hour / types.BookingSlotDuration)
	labelWidth := 6

	fmt.Printf("\n%s %s\n\n",
		FormatHeader("GPU Booking Schedule"),
		FormatDim("- "+dayStart.Format("2006-01-02 (Mon)")))

	// The NOW column sits a few columns clear of the grid
	nowGap := strings.Repeat(" ", 4)
	showNow := isToday && len(nowStatus) >= gpuCount

	// Hour header
	var header strings.Builder
	header.WriteString(strings.Repeat(" ", labelWidth))
	for hour := 0; hour < 24; hour++ {
		if hour > 0 {
			header.WriteString(" ")
		}
		header.WriteString(fmt.Sprintf("%02d", hour))
	}
	if showNow {
		header.WriteString(nowGap + "NOW")
	}
	fmt.Println(FormatDim(header.String()))

	// One row per GPU
	for gpuID := 0; gpuID < gpuCount; gpuID++ {
		var row strings.Builder
		row.WriteString(fmt.Sprintf("%-*s", labelWidth, fmt.Sprintf("GPU%d", gpuID)))

		for hour := 0; hour < 24; hour++ {
			if hour > 0 {
				row.WriteString(" ")
			}
			for slot := 0; slot < slotsPerHour; slot++ {
				slotStart := dayStart.Add(time.Duration(hour)*time.Hour + time.Duration(slot)*types.BookingSlotDuration)
				row.WriteString(scheduleCell(entries, gpuID, slotStart, currentUser))
			}
		}

		if showNow {
			row.WriteString(nowGap + formatNowCell(nowStatus[gpuID]))
		}

		fmt.Println(row.String())
	}

	// Marker for the current time, when the day being shown is today
	if isToday {
		column := labelWidth + now.Hour()*(slotsPerHour+1)
		if now.Minute() >= 30 {
			column++
		}
		fmt.Printf("%s%s\n", strings.Repeat(" ", column),
			colorWarning.Sprintf("^ now %s", now.Format("15:04")))
	}

	printScheduleLegend(entries, currentUser, now)
}

// scheduleCell renders one 30 minute slot of one GPU
func scheduleCell(entries []*scheduleEntry, gpuID int, slotStart time.Time, currentUser string) string {
	// The middle of the slot decides what it shows, so a booking from 14:00 to
	// 16:00 fills exactly four cells
	at := slotStart.Add(types.BookingSlotDuration / 2)

	var covering []*scheduleEntry
	for _, entry := range entries {
		if entry.covers(gpuID, at) {
			covering = append(covering, entry)
		}
	}

	switch len(covering) {
	case 0:
		return FormatDim("·")
	case 1:
		entry := covering[0]
		if entry.user == currentUser {
			return colorSuccess.Sprint(entry.symbol)
		}
		return colorInUse.Sprint(entry.symbol)
	default:
		// Overlapping entries: a booking about to preempt a reservation, or
		// unreserved usage on a booked GPU
		return colorUnreserved.Sprint("!")
	}
}

func printScheduleLegend(entries []*scheduleEntry, currentUser string, now time.Time) {
	fmt.Println()

	if len(entries) == 0 {
		fmt.Println(FormatDim("  No bookings or reservations for this day."))
		return
	}

	for _, entry := range entries {
		symbol := colorInUse.Sprint(entry.symbol)
		if entry.user == currentUser {
			symbol = colorSuccess.Sprint(entry.symbol)
		}

		window := gpu.FormatBookingWindow(entry.start, entry.end, now)
		if entry.openEnded {
			window = fmt.Sprintf("since %s", entry.start.Format("15:04"))
		}

		what := fmt.Sprintf("%s reservation", entry.kind)
		if entry.kind == "booking" {
			what = fmt.Sprintf("booking %s (%s)", entry.bookingID, entry.status)
		}

		line := fmt.Sprintf("  %s  %-12s %-21s GPU %-9s %s",
			symbol, utils.TruncateString(entry.user, 12), window,
			formatGPUList(entry.gpuIDs), FormatDim(what))

		if entry.note != "" {
			line += FormatDim(fmt.Sprintf("  note: %s", utils.TruncateString(entry.note, 40)))
		}

		fmt.Println(line)
	}

	fmt.Printf("  %s  %s\n", FormatDim("·"), FormatDim("free"))
}

// JSONBooking is a booking as reported by 'schedule --json'
type JSONBooking struct {
	ID                 string    `json:"id"`
	ShortID            string    `json:"short_id"`
	User               string    `json:"user"`
	ActualUser         string    `json:"actual_user,omitempty"`
	GPUIDs             []int     `json:"gpu_ids"`
	Start              time.Time `json:"start"`
	End                time.Time `json:"end"`
	Status             string    `json:"status"`
	Note               string    `json:"note,omitempty"`
	IdleTimeoutSeconds int64     `json:"idle_timeout_seconds,omitempty"`
}

// JSONScheduleReservation is a currently active reservation shown in the schedule
type JSONScheduleReservation struct {
	GPUID     int        `json:"gpu_id"`
	User      string     `json:"user"`
	Type      string     `json:"type"`
	Start     time.Time  `json:"start"`
	End       *time.Time `json:"end,omitempty"`
	Note      string     `json:"note,omitempty"`
	OpenEnded bool       `json:"open_ended,omitempty"`
}

// JSONScheduleDay holds one day of the schedule
type JSONScheduleDay struct {
	Date         string                    `json:"date"`
	Bookings     []JSONBooking             `json:"bookings"`
	Reservations []JSONScheduleReservation `json:"reservations"`
}

func printScheduleJSON(firstDay time.Time, days int, gpuCount int, bookings []*types.Booking, reservations []*scheduleEntry) error {
	output := struct {
		GPUCount int               `json:"gpu_count"`
		From     time.Time         `json:"from"`
		To       time.Time         `json:"to"`
		Days     []JSONScheduleDay `json:"days"`
	}{
		GPUCount: gpuCount,
		From:     firstDay,
		To:       firstDay.AddDate(0, 0, days),
	}

	for day := 0; day < days; day++ {
		dayStart := firstDay.AddDate(0, 0, day)
		dayEnd := dayStart.AddDate(0, 0, 1)

		jsonDay := JSONScheduleDay{
			Date:         dayStart.Format("2006-01-02"),
			Bookings:     []JSONBooking{},
			Reservations: []JSONScheduleReservation{},
		}

		for _, booking := range bookings {
			if !booking.Overlaps(dayStart, dayEnd) {
				continue
			}
			jsonDay.Bookings = append(jsonDay.Bookings, JSONBooking{
				ID:                 booking.ID,
				ShortID:            booking.ShortID(),
				User:               booking.User,
				ActualUser:         booking.ActualUser,
				GPUIDs:             booking.GPUIDs,
				Start:              booking.StartTime.ToTime(),
				End:                booking.EndTime.ToTime(),
				Status:             booking.Status,
				Note:               booking.Note,
				IdleTimeoutSeconds: int64(booking.IdleTimeout.Seconds()),
			})
		}

		for _, entry := range reservations {
			if !entry.start.Before(dayEnd) || !entry.end.After(dayStart) {
				continue
			}
			reservation := JSONScheduleReservation{
				GPUID:     entry.gpuIDs[0],
				User:      entry.user,
				Type:      entry.kind,
				Start:     entry.start,
				Note:      entry.note,
				OpenEnded: entry.openEnded,
			}
			if !entry.openEnded {
				end := entry.end
				reservation.End = &end
			}
			jsonDay.Reservations = append(jsonDay.Reservations, reservation)
		}

		output.Days = append(output.Days, jsonDay)
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(output)
}

// formatGPUList renders GPU IDs compactly, e.g. "0,2,3"
func formatGPUList(gpuIDs []int) string {
	parts := make([]string, len(gpuIDs))
	for i, gpuID := range gpuIDs {
		parts[i] = fmt.Sprintf("%d", gpuID)
	}
	return strings.Join(parts, ",")
}
