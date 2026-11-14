package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// TartGuest is the guest operating system of a Tart image. The zero value is
// macOS, so an unknown guest is always held to Apple's limit.
type TartGuest int

const (
	TartGuestMacOS TartGuest = iota
	TartGuestLinux
)

// String returns "macos" or "linux".
func (g TartGuest) String() string {
	if g == TartGuestLinux {
		return "linux"
	}
	return "macos"
}

// DetectTartGuest reads the guest OS from a temporary clone of image.
// OCI image references have no local config.json until Tart clones them.
func DetectTartGuest(ctx context.Context, runner CommandRunner, home, image string) (TartGuest, error) {
	release, err := AcquireTartInspectionLock(ctx, home)
	if err != nil {
		return TartGuestMacOS, err
	}
	defer release()
	name := KeepVMName()
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		_, _ = runner.Run(cleanupCtx, "tart", "delete", name)
	}()
	// Tart may publish the clone before a later prune or cancellation fails.
	if _, err := runner.Run(ctx, "tart", "clone", image, name); err != nil {
		return TartGuestMacOS, fmt.Errorf("clone Tart image %q for guest detection: %w", image, err)
	}
	if err := ctx.Err(); err != nil {
		return TartGuestMacOS, err
	}
	cfg, err := readTartVMConfig(home, name)
	if err != nil {
		return TartGuestMacOS, fmt.Errorf("detect guest of Tart image %q: %w", image, err)
	}
	guest, err := tartGuestFromConfig(cfg)
	if err != nil {
		return TartGuestMacOS, fmt.Errorf("detect guest of Tart image %q: %w", image, err)
	}
	return guest, nil
}

// TartImagePresent reports whether Tart already has image locally.
func TartImagePresent(ctx context.Context, runner CommandRunner, image string) (bool, error) {
	out, err := runner.Run(ctx, "tart", "list", "--format", "json")
	if err != nil {
		return false, err
	}
	names, err := tartListNames(out)
	if err != nil {
		return false, err
	}
	return names[image], nil
}

// TartHomeDir resolves the directory tart keeps VMs under: home, then
// $TART_HOME, then ~/.tart.
func TartHomeDir(home string) (string, error) {
	if home != "" {
		return home, nil
	}
	if env := os.Getenv("TART_HOME"); env != "" {
		return env, nil
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home dir: %w", err)
	}
	return filepath.Join(userHome, ".tart"), nil
}

// readTartVMConfig decodes the config.json of the local VM name. home is
// resolved as in TartHomeDir.
func readTartVMConfig(home, name string) (map[string]any, error) {
	tartHome, err := TartHomeDir(home)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(tartHome, "vms", name, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("read VM config: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse VM config: %w", err)
	}
	if cfg == nil {
		return nil, fmt.Errorf("parse VM config: expected an object")
	}
	return cfg, nil
}

// tartGuestFromConfig reads the guest OS from a VM's decoded config.json.
// Tart writes "os" as "darwin" or "linux" and decodes a config without the
// key as darwin, so a missing key is macOS. Any other value is an error.
func tartGuestFromConfig(cfg map[string]any) (TartGuest, error) {
	value, ok := cfg["os"]
	if !ok {
		return TartGuestMacOS, nil
	}
	name, ok := value.(string)
	if !ok {
		return TartGuestMacOS, fmt.Errorf(`VM config "os" is not a string: %v`, value)
	}
	switch name {
	case "darwin":
		return TartGuestMacOS, nil
	case "linux":
		return TartGuestLinux, nil
	default:
		return TartGuestMacOS, fmt.Errorf("unknown guest OS %q in VM config", name)
	}
}
