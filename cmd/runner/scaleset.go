package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"regexp"
	"time"

	"github.com/actions/scaleset"
	"github.com/spf13/cobra"

	"github.com/ysya/runscaler/internal/config"
	runnerlock "github.com/ysya/runscaler/internal/lock"
)

// scaleSetAPI is the part of the scaleset client that finds, lists,
// registers and removes runner scale sets.
type scaleSetAPI interface {
	GetRunnerGroupByName(ctx context.Context, runnerGroup string) (*scaleset.RunnerGroup, error)
	GetRunnerScaleSet(ctx context.Context, runnerGroupID int, runnerScaleSetName string) (*scaleset.RunnerScaleSet, error)
	ListRunnerScaleSets(ctx context.Context, runnerGroupID int) ([]scaleset.RunnerScaleSet, error)
	CreateRunnerScaleSet(ctx context.Context, runnerScaleSet *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	UpdateRunnerScaleSet(ctx context.Context, runnerScaleSetID int, runnerScaleSet *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	DeleteRunnerScaleSet(ctx context.Context, runnerScaleSetID int) error
}

// apiProbeTimeout bounds each read-only GitHub probe made by `runner doctor`
// and `runner validate`, so one unreachable registration cannot stall the
// rest.
const apiProbeTimeout = 5 * time.Second

// newScaleSetClient builds the real client for a read-only probe.
func newScaleSetClient(ss config.ScaleSetConfig) (scaleSetAPI, error) {
	return ss.NewScalesetClient(nil)
}

// scaleSetProbe is what a read-only probe learned about one scale set: the
// client and runner group it used, and whether the scale set is registered.
type scaleSetProbe struct {
	client        scaleSetAPI
	runnerGroupID int
	registered    bool
}

// probeScaleSet checks, read-only, that GitHub accepts ss's credentials for
// its configured runner group, and reports whether its scale set is
// registered. Its errors may carry SDK text; print them through
// safeAPIError.
func probeScaleSet(ctx context.Context, ss config.ScaleSetConfig, newClient func(config.ScaleSetConfig) (scaleSetAPI, error)) (scaleSetProbe, error) {
	// Match ScaleSetConfig.Validate's URL boundary before the factory can
	// read App credentials or the SDK can issue an authenticated request.
	registrationURL, err := url.ParseRequestURI(ss.RegistrationURL)
	if err != nil || registrationURL.Scheme != "https" || registrationURL.Host == "" || registrationURL.User != nil {
		return scaleSetProbe{}, errors.New("invalid registration URL")
	}
	if ss.Token == "" && ss.GitHubApp == nil {
		return scaleSetProbe{}, fmt.Errorf("registration URL and credentials are required")
	}
	client, err := newClient(ss)
	if err != nil {
		return scaleSetProbe{}, err
	}
	groupID, err := resolveRunnerGroupID(ctx, client, ss.RunnerGroup)
	if err != nil {
		return scaleSetProbe{}, err
	}
	registered, err := client.GetRunnerScaleSet(ctx, groupID, ss.ScaleSetName)
	if err != nil {
		return scaleSetProbe{}, err
	}
	return scaleSetProbe{client: client, runnerGroupID: groupID, registered: registered != nil}, nil
}

// The SDK wraps response bodies, URLs and authentication failures in error
// strings. Never return or print those strings: App JWTs and exchanged tokens
// are not available for configured-value redaction. Only fixed categories and
// a bounded numeric HTTP status may cross the diagnostic output boundary.
var apiHTTPStatus = regexp.MustCompile(`\(status="([1-5][0-9]{2})(?: |")`)

func safeAPIError(err error) error {
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("cancelled: %w", context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("timed out: %w", context.DeadlineExceeded)
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return errors.New("request timed out")
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return errors.New("DNS lookup failed")
	}
	var verificationErr *tls.CertificateVerificationError
	var authorityErr x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var certificateErr x509.CertificateInvalidError
	var recordErr tls.RecordHeaderError
	if errors.As(err, &verificationErr) || errors.As(err, &authorityErr) || errors.As(err, &hostnameErr) || errors.As(err, &certificateErr) || errors.As(err, &recordErr) {
		return errors.New("TLS verification failed")
	}
	var rateLimited *scaleset.RateLimitError
	if errors.As(err, &rateLimited) {
		return rateLimitCategory(rateLimited)
	}
	// SDK v0.5.0 types only some statuses (400, 401, 404, 409 and rate
	// limits); others, such as a plain 403 or a 5xx, appear only in its
	// formatted error. Copy just the three validated digits, never arbitrary
	// response text.
	if match := apiHTTPStatus.FindStringSubmatch(err.Error()); len(match) == 2 {
		return fmt.Errorf("HTTP %s request failed", match[1])
	}
	return errors.New("request failed")
}

// rateLimitCategory names a rate limit by its status, its kind and the wait
// GitHub asked for, none of which carries response text.
func rateLimitCategory(rl *scaleset.RateLimitError) error {
	category := fmt.Sprintf("HTTP %d rate limited (%s)", rl.StatusCode, rl.Kind)
	if rl.RetryAfter > 0 {
		category += "; retry after " + rl.RetryAfter.Round(time.Second).String()
	}
	return errors.New(category)
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

	client, err := ss.NewScalesetClient(slog.Default())
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
