package cli

import (
	"bytes"
	"testing"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/russellb/canhazgpu/internal/gpu"
	"github.com/stretchr/testify/assert"
)

func TestFormatForeignUsage(t *testing.T) {
	assert.Empty(t, formatForeignUsage(gpu.GPUStatusInfo{Status: "IN_USE", User: "alice"}),
		"a reservation used by its owner says nothing extra")

	assert.Equal(t, "used by bob", formatForeignUsage(gpu.GPUStatusInfo{
		ForeignUsers: []string{"bob"}, ForeignProcesses: 1, ForeignMemoryMB: 8452,
	}))

	assert.Equal(t, "used by bob and carol", formatForeignUsage(gpu.GPUStatusInfo{
		ForeignUsers: []string{"bob", "carol"}, ForeignProcesses: 3, ForeignMemoryMB: 9000,
	}))

	// Some drivers do not report per-process memory; the format is the same
	assert.Equal(t, "used by bob", formatForeignUsage(gpu.GPUStatusInfo{
		ForeignUsers: []string{"bob"}, ForeignProcesses: 1,
	}))
}

func TestAddGPUStatusRow_MarksForeignUsage(t *testing.T) {
	SetNoColor(true)
	t.Cleanup(func() { SetNoColor(false) })

	render := func(status gpu.GPUStatusInfo) string {
		var buf bytes.Buffer
		tbl := table.NewWriter()
		tbl.SetOutputMirror(&buf)
		tbl.AppendHeader(table.Row{"GPU", "STATUS", "USER", "DURATION", "TYPE", "DETAILS", "MEMORY", "NOTE", "UTIL"})
		addGPUStatusRow(tbl, status, false)
		tbl.Render()
		return buf.String()
	}

	reserved := gpu.GPUStatusInfo{
		GPUID:           2,
		Status:          "IN_USE",
		User:            "alice",
		ReservationType: "manual",
		Duration:        time.Hour,
		ExpiryTime:      time.Now().Add(time.Hour),
		ValidationInfo:  "[validated: 8452MB, 1 processes]",
	}

	output := render(reserved)
	assert.Contains(t, output, "IN_USE")
	assert.NotContains(t, output, "FOREIGN")

	squatted := reserved
	squatted.ForeignUsers = []string{"bob"}
	squatted.ForeignProcesses = 1
	squatted.ForeignMemoryMB = 8452

	output = render(squatted)
	assert.Contains(t, output, "FOREIGN", "a squatted reservation must stand out from a normal one")
	assert.Contains(t, output, "used by bob", "the details name who is actually on the GPU")
	assert.Contains(t, output, "alice", "the reservation holder is still shown")
}
