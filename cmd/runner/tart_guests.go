package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/ysya/runscaler/internal/capacity"
	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/provider"
)

type tartGuestDetector func(context.Context, string, string) (provider.TartGuest, error)

func detectTartGuests(ctx context.Context, runtimes []*scaleSetRuntime, detect tartGuestDetector, logger *slog.Logger) error {
	type imageKey struct{ home, image string }
	guests := make(map[imageKey]provider.TartGuest)
	for _, rt := range runtimes {
		if rt.tart == nil {
			continue
		}
		key := imageKey{rt.config.Tart.Home, rt.config.RunnerImage}
		guest, ok := guests[key]
		if !ok {
			var err error
			guest, err = detect(ctx, key.home, key.image)
			if err != nil {
				return fmt.Errorf("detect guest OS of Tart image %q: %w", key.image, err)
			}
			guests[key] = guest
			logger.Info("Detected Tart guest", slog.String("image", key.image), slog.String("os", guest.String()))
		}
		rt.tart.guest, rt.tart.guestKnown = guest, true
	}
	return nil
}

func applyTartGuestLimits(runtimes []*scaleSetRuntime, logger *slog.Logger) []error {
	var problems []error
	limit := config.MaxMacOSVMsPerHost
	totalMin, totalPool := 0, 0
	invalidMin, invalidPool := false, false
	for _, rt := range runtimes {
		if rt.tart == nil || !rt.tart.guestKnown || rt.tart.guest != provider.TartGuestMacOS {
			continue
		}
		ss := rt.config
		rt.maxRunners = min(ss.MaxRunners, limit)
		if rt.maxRunners < ss.MaxRunners {
			logger.Info("Apple allows two macOS VMs per host; capping max-runners",
				slog.String("scaleSet", ss.ScaleSetName), slog.Int("configured", ss.MaxRunners), slog.Int("effective", rt.maxRunners))
		}
		if ss.MinRunners > limit {
			invalidMin = true
			problems = append(problems, fmt.Errorf("scaleset[%d] %q: min-runners is %d, but its image is a macOS guest and Apple allows %d macOS VMs per host", rt.index, ss.ScaleSetName, ss.MinRunners, limit))
		}
		if ss.Tart.PoolSize > limit {
			invalidPool = true
			problems = append(problems, fmt.Errorf("scaleset[%d] %q: tart pool-size is %d, but its image is a macOS guest and Apple allows %d macOS VMs per host", rt.index, ss.ScaleSetName, ss.Tart.PoolSize, limit))
		}
		totalMin += ss.MinRunners
		totalPool += ss.Tart.PoolSize
	}
	if totalMin > limit && !invalidMin {
		problems = append(problems, fmt.Errorf("combined min-runners of macOS guest scale sets is %d, exceeding Apple's limit of %d macOS VMs per host", totalMin, limit))
	}
	if totalPool > limit && !invalidPool {
		problems = append(problems, fmt.Errorf("combined tart pool-size of macOS guest scale sets is %d, exceeding Apple's limit of %d macOS VMs per host", totalPool, limit))
	}
	return problems
}

func effectiveConcurrent(cfg config.Config, runtimes []*scaleSetRuntime) int {
	if cfg.Concurrent > 0 {
		return cfg.Concurrent
	}
	total := 0
	for _, rt := range runtimes {
		total += rt.maxRunners
	}
	return total
}

func registerCapacity(broker *capacity.Broker, runtimes []*scaleSetRuntime) {
	for _, rt := range runtimes {
		rt.capacity = broker.Register(fmt.Sprintf("%d:%s", rt.index, rt.config.ScaleSetName), rt.config.MinRunners)
	}
}

// reportTartGuests inspects local images without constructing a provider or pool.
func reportTartGuests(ctx context.Context, out io.Writer, runtimes []*scaleSetRuntime,
	present func(context.Context, string, string) (bool, error), detect tartGuestDetector, logger *slog.Logger) error {
	var local []*scaleSetRuntime
	for _, rt := range runtimes {
		if rt.tart == nil {
			continue
		}
		found, err := present(ctx, rt.config.Tart.Home, rt.config.RunnerImage)
		if err != nil {
			fmt.Fprintf(out, "  ✗ scaleset[%d] %q Tart image check: %s\n", rt.index, rt.config.ScaleSetName, err)
			return errors.New("validation failed")
		}
		if found {
			local = append(local, rt)
		}
	}
	if err := detectTartGuests(ctx, local, detect, logger); err != nil {
		fmt.Fprintf(out, "  ✗ %s\n", err)
		return errors.New("validation failed")
	}
	for _, rt := range runtimes {
		if rt.tart == nil {
			continue
		}
		if rt.tart.guestKnown {
			fmt.Fprintf(out, "  ✓ scaleset[%d] %q Tart guest: %s\n", rt.index, rt.config.ScaleSetName, rt.tart.guest)
		} else {
			fmt.Fprintf(out, "  - scaleset[%d] %q Tart guest: detected at startup (image not pulled; use --preflight to check now)\n", rt.index, rt.config.ScaleSetName)
		}
	}
	problems := applyTartGuestLimits(runtimes, logger)
	for _, err := range problems {
		fmt.Fprintf(out, "  ✗ %s\n", err)
	}
	if len(problems) > 0 {
		return errors.New("validation failed")
	}
	return nil
}
