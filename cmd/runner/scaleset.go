package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/actions/scaleset"
	"github.com/spf13/cobra"

	"github.com/ysya/runscaler/internal/config"
	runnerlock "github.com/ysya/runscaler/internal/lock"
)

// scaleSetAPI is the part of the scaleset client that finds, registers and
// removes a runner scale set.
type scaleSetAPI interface {
	GetRunnerGroupByName(ctx context.Context, runnerGroup string) (*scaleset.RunnerGroup, error)
	GetRunnerScaleSet(ctx context.Context, runnerGroupID int, runnerScaleSetName string) (*scaleset.RunnerScaleSet, error)
	CreateRunnerScaleSet(ctx context.Context, runnerScaleSet *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	UpdateRunnerScaleSet(ctx context.Context, runnerScaleSetID int, runnerScaleSet *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	DeleteRunnerScaleSet(ctx context.Context, runnerScaleSetID int) error
}

// resolveRunnerGroupID maps the configured runner group to its ID; the
// default group is always 1.
func resolveRunnerGroupID(ctx context.Context, api scaleSetAPI, group string) (int, error) {
	switch group {
	case scaleset.DefaultRunnerGroup, "":
		return 1, nil
	}
	runnerGroup, err := api.GetRunnerGroupByName(ctx, group)
	if err != nil {
		return 0, fmt.Errorf("failed to get runner group: %w", err)
	}
	return runnerGroup.ID, nil
}

// ensureScaleSet returns ss's runner scale set, registering it when GitHub
// has none and updating it to the configured labels and settings otherwise.
// runner never removes it on exit: jobs queued while runner is down stay
// assigned to it and are picked up once the listener reconnects, whereas a
// scale set deleted and re-created on restart leaves them unassigned for
// good. Retiring a scale set is an explicit `runner scaleset delete`.
func ensureScaleSet(ctx context.Context, api scaleSetAPI, ss config.ScaleSetConfig, logger *slog.Logger) (*scaleset.RunnerScaleSet, error) {
	runnerGroupID, err := resolveRunnerGroupID(ctx, api, ss.RunnerGroup)
	if err != nil {
		return nil, err
	}
	desired := &scaleset.RunnerScaleSet{
		Name:          ss.ScaleSetName,
		RunnerGroupID: runnerGroupID,
		Labels:        config.BuildLabels(ss.ScaleSetName, ss.Labels),
		RunnerSetting: scaleset.RunnerSetting{
			DisableUpdate: ss.IsUpdateDisabled(),
		},
	}

	existing, err := api.GetRunnerScaleSet(ctx, runnerGroupID, ss.ScaleSetName)
	if err != nil {
		return nil, fmt.Errorf("failed to get runner scale set: %w", err)
	}
	if existing == nil {
		created, err := api.CreateRunnerScaleSet(ctx, desired)
		if err != nil {
			return nil, fmt.Errorf("failed to create runner scale set: %w", err)
		}
		logger.Info("Scale set created",
			slog.Int("scaleSetID", created.ID),
			slog.String("name", created.Name),
		)
		return created, nil
	}

	updated, err := api.UpdateRunnerScaleSet(ctx, existing.ID, desired)
	if err != nil {
		return nil, fmt.Errorf("failed to update runner scale set: %w", err)
	}
	logger.Info("Scale set reused",
		slog.Int("scaleSetID", updated.ID),
		slog.String("name", updated.Name),
	)
	return updated, nil
}

// deleteScaleSet removes ss's runner scale set from GitHub. found is false
// when GitHub has none registered under that name.
func deleteScaleSet(ctx context.Context, api scaleSetAPI, ss config.ScaleSetConfig) (id int, found bool, err error) {
	runnerGroupID, err := resolveRunnerGroupID(ctx, api, ss.RunnerGroup)
	if err != nil {
		return 0, false, err
	}
	existing, err := api.GetRunnerScaleSet(ctx, runnerGroupID, ss.ScaleSetName)
	if err != nil {
		return 0, false, fmt.Errorf("failed to get runner scale set: %w", err)
	}
	if existing == nil {
		return 0, false, nil
	}
	if err := api.DeleteRunnerScaleSet(ctx, existing.ID); err != nil {
		return 0, false, fmt.Errorf("failed to delete runner scale set %d: %w", existing.ID, err)
	}
	return existing.ID, true, nil
}

var scalesetCmd = &cobra.Command{
	Use:   "scaleset",
	Short: "Manage the runner scale sets registered on GitHub",
}

var scalesetDeleteCmd = &cobra.Command{
	Use:   "delete NAME",
	Short: "Remove a runner scale set from GitHub",
	Long: `runner keeps each scale set registered across restarts, so jobs queued
while it is down are picked up when it comes back. When you retire a
[[scaleset]], remove its scale set from GitHub with this command. NAME is the
scale set's name in the config. Stop runner first.`,
	Example: `  runner scaleset delete macos-runners`,
	Args:    cobra.ExactArgs(1),
	RunE:    runScalesetDelete,
}

func init() {
	scalesetCmd.AddCommand(scalesetDeleteCmd)
	cmd.AddCommand(scalesetCmd)
}

func runScalesetDelete(c *cobra.Command, args []string) error {
	cfg, err := loadConfig(c)
	if err != nil {
		return err
	}
	name := args[0]
	var ss *config.ScaleSetConfig
	for _, candidate := range cfg.ResolveScaleSets() {
		if candidate.ScaleSetName == name {
			ss = &candidate
			break
		}
	}
	if ss == nil {
		return fmt.Errorf("no scale set named %q in the config", name)
	}

	// Deleting the scale set a running listener holds would strand its jobs.
	release, err := runnerlock.Acquire(runnerlock.DefaultPath, runnerlock.Info{
		PID:       os.Getpid(),
		StartedAt: time.Now(),
	})
	if err != nil {
		if errors.Is(err, runnerlock.ErrAlreadyRunning) {
			return fmt.Errorf("stop runner before deleting a scale set it listens on: %w", err)
		}
		return err
	}
	defer release()

	client, err := config.NewScalesetClient(ss.RegistrationURL, ss.Token, slog.Default())
	if err != nil {
		return fmt.Errorf("failed to create scaleset client: %w", err)
	}
	id, found, err := deleteScaleSet(c.Context(), client, *ss)
	if err != nil {
		return err
	}
	if !found {
		fmt.Fprintf(c.OutOrStdout(), "No runner scale set named %q is registered on GitHub.\n", name)
		return nil
	}
	fmt.Fprintf(c.OutOrStdout(), "Deleted runner scale set %q (ID %d).\n", name, id)
	return nil
}
