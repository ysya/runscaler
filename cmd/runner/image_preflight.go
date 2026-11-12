package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	dockerclient "github.com/moby/moby/client"
	"github.com/moby/moby/client/pkg/jsonmessage"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/ysya/runscaler/internal/config"
	"github.com/ysya/runscaler/internal/provider"
)

func runOptionalImagePreflight(cmd *cobra.Command, scaleSets []config.ScaleSetConfig, pullDocker, ensureTart func(context.Context, config.ScaleSetConfig) error) error {
	preflight, err := cmd.Flags().GetBool("preflight")
	if err != nil && cmd.Flags().Lookup("preflight") != nil {
		return err
	}
	if !preflight {
		return nil
	}
	fmt.Fprintln(cmd.OutOrStdout(), "\nPreparing runner images (downloads may take a while)...")
	if err := prepareRunnerImages(commandContext(cmd), scaleSets, pullDocker, ensureTart); err != nil {
		return fmt.Errorf("image preflight failed: %w", err)
	}
	return nil
}

// prepareRunnerImages visits each image once per Docker socket and platform,
// or once per Tart host. Both startup and explicit validation preflight use it.
func prepareRunnerImages(ctx context.Context, scaleSets []config.ScaleSetConfig, pullDocker, ensureTart func(context.Context, config.ScaleSetConfig) error) error {
	type dockerImage struct{ socket, image, platform string }
	dockerDone := make(map[dockerImage]bool)
	for _, ss := range scaleSets {
		if ss.IsTart() {
			continue
		}
		key := dockerImage{ss.Docker.Socket, ss.RunnerImage, ss.Docker.Platform}
		if dockerDone[key] {
			continue
		}
		if err := pullDocker(ctx, ss); err != nil {
			return err
		}
		dockerDone[key] = true
	}
	type tartImage struct{ home, image string }
	tartDone := make(map[tartImage]bool)
	for _, ss := range scaleSets {
		key := tartImage{ss.Tart.Home, ss.RunnerImage}
		if !ss.IsTart() || tartDone[key] {
			continue
		}
		if err := ensureTart(ctx, ss); err != nil {
			return err
		}
		tartDone[key] = true
	}
	return nil
}

func pullDockerRunnerImage(ctx context.Context, ss config.ScaleSetConfig, clients map[string]*dockerclient.Client, out io.Writer, logger *slog.Logger) error {
	logger.Info("Pulling runner image", slog.String("image", ss.RunnerImage), slog.String("platform", ss.Docker.Platform))
	pullCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	options := dockerclient.ImagePullOptions{}
	if ss.Docker.Platform != "" {
		parts := strings.SplitN(ss.Docker.Platform, "/", 3)
		platform := ocispec.Platform{OS: parts[0], Architecture: parts[1]}
		if len(parts) == 3 {
			platform.Variant = parts[2]
		}
		options.Platforms = []ocispec.Platform{platform}
	}
	pull, err := clients[ss.Docker.Socket].ImagePull(pullCtx, ss.RunnerImage, options)
	if err != nil {
		return fmt.Errorf("failed to pull runner image %s: %w", ss.RunnerImage, err)
	}
	defer pull.Close()
	fd := os.Stdout.Fd()
	if err := jsonmessage.DisplayJSONMessagesStream(pull, out, fd, term.IsTerminal(int(fd)), nil); err != nil {
		return fmt.Errorf("failed to pull runner image %s: %w", ss.RunnerImage, err)
	}
	return nil
}

func ensureTartRunnerImage(ctx context.Context, ss config.ScaleSetConfig, logger *slog.Logger, coordinator *provider.TartHostCoordinator) error {
	return provider.NewTartProviderWithCoordinator(ss, logger, coordinator).EnsureImage(ctx)
}
