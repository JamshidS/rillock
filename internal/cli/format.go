package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// formatAge shows a duration the way kubectl does: 45s, 3m, 5h, 2d.
func formatAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(0, int(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// formatCost shows micro-dollars as dollars: 12000 → "$0.012".
func formatCost(microUSD int64) string {
	s := fmt.Sprintf("%.6f", float64(microUSD)/1_000_000)
	s = strings.TrimRight(s, "0")
	if strings.HasSuffix(s, ".") {
		s += "00"
	}
	return "$" + s
}

// compactJSON prints a JSON value on one line.
func compactJSON(raw json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return string(raw)
	}
	return b.String()
}

// lowerKind turns "Tool" into "tool" for kubectl-style output (tool/orders_get).
func lowerKind(kind string) string {
	return strings.ToLower(kind)
}
