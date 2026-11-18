//go:build !darwin

package provider

import (
	"context"
	"fmt"
	"os/exec"
)

func seatbeltProcessGroup(*exec.Cmd) {}
func seatbeltKill(context.Context, *seatbeltInstance) error {
	return fmt.Errorf("seatbelt requires macOS")
}
