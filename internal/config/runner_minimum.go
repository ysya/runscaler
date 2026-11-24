package config

import (
	"errors"
	"fmt"
	"strconv"
)

// RunnerMinimum is either a fixed idle floor or an automatic spare.
// Auto reserves no slots in the host-wide capacity broker.
type RunnerMinimum struct {
	Fixed int
	Auto  bool
}

func (m *RunnerMinimum) UnmarshalMapstructure(input any) error {
	if input == "auto" {
		*m = RunnerMinimum{Auto: true}
		return nil
	}
	switch input.(type) {
	case string, int, int64:
		n, err := strconv.ParseInt(fmt.Sprint(input), 0, strconv.IntSize)
		if err == nil {
			*m = RunnerMinimum{Fixed: int(n)}
			return nil
		}
	}
	return errors.New(`min-runners must be an integer or "auto"`)
}

// ConfigValue returns the TOML value for logs and validation output.
func (m RunnerMinimum) ConfigValue() any {
	if m.Auto {
		return "auto"
	}
	return m.Fixed
}
