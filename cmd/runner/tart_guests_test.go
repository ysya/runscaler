package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/ysya/runscaler/internal/capacity"
	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/provider"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"
)

func tartSet(name, image string, maxRunners int) config.ScaleSetConfig {
	ss := validScaleSet(name, 0)
	ss.Provider = "tart"
	ss.RunnerImage = image
	ss.MaxRunners = maxRunners
	return ss
}

func tartRuntimes(guests map[string]provider.TartGuest, sets ...config.ScaleSetConfig) []*scaleSetRuntime {
	runtimes := newScaleSetRuntimes(sets, runtimeBackends{tartHost: provider.NewTartHostCoordinator(2)})
	for _, rt := range runtimes {
		if guest, ok := guests[rt.config.ScaleSetName]; ok {
			rt.tart.guest, rt.tart.guestKnown = guest, true
		}
	}
	return runtimes
}

func TestDetectTartGuests_DetectsEachImageOnce(t *testing.T) {
	otherHome := tartSet("other-home", "macos-image", 2)
	otherHome.Tart.Home = "/Volumes/tart"
	runtimes := tartRuntimes(nil, tartSet("mac-a", "macos-image", 2), validScaleSet("docker", 0),
		tartSet("mac-b", "macos-image", 2), tartSet("linux", "linux-image", 4), otherHome)
	var calls []string
	detect := func(_ context.Context, home, image string) (provider.TartGuest, error) {
		calls = append(calls, home+"|"+image)
		if image == "linux-image" {
			return provider.TartGuestLinux, nil
		}
		return provider.TartGuestMacOS, nil
	}

	if err := detectTartGuests(context.Background(), runtimes, detect, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}

	if want := []string{"|macos-image", "|linux-image", "/Volumes/tart|macos-image"}; !slices.Equal(calls, want) {
		t.Fatalf("detections = %q, want %q", calls, want)
	}
	for i, want := range map[int]provider.TartGuest{0: provider.TartGuestMacOS, 2: provider.TartGuestMacOS, 3: provider.TartGuestLinux, 4: provider.TartGuestMacOS} {
		if rt := runtimes[i]; !rt.tart.guestKnown || rt.tart.guest != want {
			t.Errorf("%s: guest = %v (known %v), want %v", rt.config.ScaleSetName, rt.tart.guest, rt.tart.guestKnown, want)
		}
	}
}

func TestDetectTartGuests_ReturnsTheDetectionError(t *testing.T) {
	runtimes := tartRuntimes(nil, tartSet("mac", "macos-image", 2))
	detect := func(context.Context, string, string) (provider.TartGuest, error) {
		return provider.TartGuestMacOS, errors.New("clone failed")
	}
	err := detectTartGuests(context.Background(), runtimes, detect, slog.New(slog.DiscardHandler))
	if err == nil || !strings.Contains(err.Error(), "macos-image") || runtimes[0].tart.guestKnown {
		t.Fatalf("error = %v, guestKnown = %v", err, runtimes[0].tart.guestKnown)
	}
}

func TestApplyTartGuestLimits_CapsMacOSGuestsWithoutAnError(t *testing.T) {
	runtimes := tartRuntimes(map[string]provider.TartGuest{"mac": provider.TartGuestMacOS, "linux": provider.TartGuestLinux},
		tartSet("mac", "macos-image", 10), tartSet("linux", "linux-image", 4))

	if problems := applyTartGuestLimits(runtimes, slog.New(slog.DiscardHandler)); len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	if runtimes[0].maxRunners != 2 || runtimes[1].maxRunners != 4 {
		t.Fatalf("effective max-runners = %d (macOS), %d (Linux); want 2, 4", runtimes[0].maxRunners, runtimes[1].maxRunners)
	}
}

func TestApplyTartGuestLimits_RejectsWhatAppleCannotRun(t *testing.T) {
	withMin := func(name string, n int) config.ScaleSetConfig {
		ss := tartSet(name, "macos-image", 10)
		ss.MinRunners = n
		return ss
	}
	withPool := func(name string, n int) config.ScaleSetConfig {
		ss := tartSet(name, "macos-image", 10)
		ss.Tart.PoolSize = n
		return ss
	}
	macOS := map[string]provider.TartGuest{"a": provider.TartGuestMacOS, "b": provider.TartGuestMacOS}
	tests := []struct {
		name string
		sets []config.ScaleSetConfig
		want string // "" means no problem
	}{
		{name: "min-runners above two", sets: []config.ScaleSetConfig{withMin("a", 3)}, want: `scaleset[0] "a": min-runners is 3`},
		{name: "pool-size above two", sets: []config.ScaleSetConfig{withPool("a", 3)}, want: `scaleset[0] "a": tart pool-size is 3`},
		{name: "combined min-runners", sets: []config.ScaleSetConfig{withMin("a", 1), withMin("b", 2)}, want: "combined min-runners of macOS guest scale sets is 3"},
		{name: "combined pool-size", sets: []config.ScaleSetConfig{withPool("a", 2), withPool("b", 1)}, want: "combined tart pool-size of macOS guest scale sets is 3"},
		{name: "two warm macOS runners", sets: []config.ScaleSetConfig{withMin("a", 1), withMin("b", 1)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems := applyTartGuestLimits(tartRuntimes(macOS, tt.sets...), slog.New(slog.DiscardHandler))
			if tt.want == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %v, want none", problems)
				}
				return
			}
			if len(problems) != 1 || !strings.Contains(problems[0].Error(), tt.want) {
				t.Fatalf("problems = %v, want exactly one containing %q", problems, tt.want)
			}
		})
	}
}

func TestApplyTartGuestLimits_LeavesLinuxAndUninspectedGuestsAlone(t *testing.T) {
	linux := tartSet("linux", "linux-image", 6)
	linux.MinRunners, linux.Tart.PoolSize = 4, 3
	unknown := tartSet("unknown", "not-pulled", 10)
	unknown.MinRunners = 5
	runtimes := tartRuntimes(map[string]provider.TartGuest{"linux": provider.TartGuestLinux}, linux, unknown)

	if problems := applyTartGuestLimits(runtimes, slog.New(slog.DiscardHandler)); len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	if runtimes[0].maxRunners != 6 || runtimes[1].maxRunners != 10 {
		t.Fatalf("effective max-runners = %d, %d; want 6, 10", runtimes[0].maxRunners, runtimes[1].maxRunners)
	}
}

func TestEffectiveConcurrent(t *testing.T) {
	runtimes := tartRuntimes(map[string]provider.TartGuest{"mac": provider.TartGuestMacOS, "linux": provider.TartGuestLinux},
		tartSet("mac", "macos-image", 10), tartSet("linux", "linux-image", 4))
	applyTartGuestLimits(runtimes, slog.New(slog.DiscardHandler))
	if got := effectiveConcurrent(config.Config{}, runtimes); got != 6 {
		t.Errorf("automatic concurrent = %d, want 2 + 4", got)
	}
	if got := effectiveConcurrent(config.Config{Concurrent: 3}, runtimes); got != 3 {
		t.Errorf("explicit concurrent = %d, want 3", got)
	}
}

func TestRegisterCapacity_SharesOneBrokerAcrossRuntimes(t *testing.T) {
	runtimes := tartRuntimes(nil, tartSet("a", "image", 2), tartSet("b", "image", 2))
	registerCapacity(capacity.NewBroker(1), runtimes)

	lease, err := runtimes[0].capacity.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := runtimes[1].capacity.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the second scale set did not wait on the shared limit: %v", err)
	}
}

func TestReportTartGuests(t *testing.T) {
	present := func(_ context.Context, _, image string) (bool, error) { return image != "not-pulled", nil }
	detect := func(_ context.Context, _, image string) (provider.TartGuest, error) {
		if image == "not-pulled" {
			t.Errorf("detected %q, which is not local", image)
		}
		if image == "linux-image" {
			return provider.TartGuestLinux, nil
		}
		return provider.TartGuestMacOS, nil
	}
	logger := slog.New(slog.DiscardHandler)

	var out bytes.Buffer
	runtimes := tartRuntimes(nil, tartSet("linux", "linux-image", 4), tartSet("later", "not-pulled", 2))
	if err := reportTartGuests(context.Background(), &out, runtimes, present, detect, logger); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`✓ scaleset[0] "linux" Tart guest: linux`,
		`- scaleset[1] "later" Tart guest: detected at startup (image not pulled; use --preflight to check now)`,
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output %q lacks %q", out.String(), want)
		}
	}

	out.Reset()
	tooWarm := tartSet("mac", "macos-image", 2)
	tooWarm.MinRunners = 3
	err := reportTartGuests(context.Background(), &out, tartRuntimes(nil, tooWarm), present, detect, logger)
	if err == nil || !strings.Contains(out.String(), `✗ scaleset[0] "mac": min-runners is 3`) {
		t.Fatalf("error = %v, output %q", err, out.String())
	}
}
