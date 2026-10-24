package a2a

import (
	"strconv"
	"strings"
)

// parseFloat parses a decimal string, tolerating whitespace.
func parseFloat(s string) (float64, error) {
	return strconv.ParseFloat(strings.TrimSpace(s), 64)
}
