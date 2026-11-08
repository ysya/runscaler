package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/actions/scaleset"
	"github.com/moby/moby/api/types/volume"
	dockerclient "github.com/moby/moby/client"

	"github.com/ysya/runscaler/internal/config"
)

type doctorProbeClient struct {
	fakeScaleSetAPI
	err     error
	listErr error
}

func (c *doctorProbeClient) ListRunnerScaleSets(ctx context.Context, groupID int) ([]scaleset.RunnerScaleSet, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("listing has no deadline")
	}
	if c.listErr != nil {
		return nil, c.listErr
	}
	return c.fakeScaleSetAPI.ListRunnerScaleSets(ctx, groupID)
}

func (c *doctorProbeClient) GetRunnerScaleSet(ctx context.Context, groupID int, name string) (*scaleset.RunnerScaleSet, error) {
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("probe has no deadline")
	}
	if c.err != nil {
		return nil, c.err
	}
	return c.fakeScaleSetAPI.GetRunnerScaleSet(ctx, groupID, name)
}

func TestDoctorProbesEveryScaleSetAfterFailureAndRedactsTokens(t *testing.T) {
	cfg := config.Config{ScaleSets: []config.ScaleSetConfig{
		{ScaleSetName: "broken", RegistrationURL: "https://github.com/example-a", Token: "synthetic-token-a"},
		{ScaleSetName: "working", RegistrationURL: "https://github.com/example-b", Token: "synthetic-token-b", RunnerGroup: "builders"},
	}}
	working := &doctorProbeClient{fakeScaleSetAPI: fakeScaleSetAPI{
		groups:   map[string]int{"builders": 42},
		existing: map[int]*scaleset.RunnerScaleSet{42: {ID: 7, Name: "working"}},
	}}
	var checked []string
	var out bytes.Buffer
	err := checkScalesetAPIWith(context.Background(), &cfg, &out, func(ss config.ScaleSetConfig) (scaleSetAPI, error) {
		checked = append(checked, ss.ScaleSetName)
		if ss.ScaleSetName == "broken" {
			return &doctorProbeClient{err: errors.New("request rejected for synthetic-token-a")}, nil
		}
		return working, nil
	})
	if err == nil {
		t.Fatal("failed API probe must make diagnosis fail")
	}
	if strings.Join(checked, ",") != "broken,working" {
		t.Fatalf("checked %v, want both scale sets despite first failure", checked)
	}
	if len(working.lookedUp) != 1 || working.lookedUp[0] != 42 {
		t.Fatalf("queried groups %v, want configured group 42", working.lookedUp)
	}
	if !strings.Contains(out.String(), "working") || !strings.Contains(out.String(), "reachable") {
		t.Fatalf("successful API probe missing from report: %s", &out)
	}
	if strings.Contains(out.String(), "synthetic-token-a") || strings.Contains(err.Error(), "synthetic-token-a") {
		t.Fatalf("configured token leaked in diagnostic output or returned error")
	}
}

func TestDoctorMissingScaleSetIsReadOnlyAndNotFailure(t *testing.T) {
	cfg := config.Config{ScaleSets: []config.ScaleSetConfig{{ScaleSetName: "new-set", RegistrationURL: "https://github.com/example", Token: "synthetic-token"}}}
	client := &doctorProbeClient{}
	var out bytes.Buffer
	err := checkScalesetAPIWith(context.Background(), &cfg, &out, func(config.ScaleSetConfig) (scaleSetAPI, error) {
		return client, nil
	})
	if err != nil {
		t.Fatalf("a not-yet-registered scale set is valid: %v", err)
	}
	if len(client.lookedUp) != 1 || !strings.Contains(out.String(), "not registered") {
		t.Fatalf("missing registration was not actually probed or reported: %s", &out)
	}
	if len(client.created) != 0 || len(client.updated) != 0 || len(client.deleted) != 0 {
		t.Fatal("diagnosis must never change scale-set registration")
	}
}

func TestDoctorAPIProbeHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cfg := config.Config{ScaleSets: []config.ScaleSetConfig{{ScaleSetName: "cancelled", RegistrationURL: "https://github.com/example", Token: "synthetic-token"}}}
	var out bytes.Buffer
	err := checkScalesetAPIWith(ctx, &cfg, &out, func(config.ScaleSetConfig) (scaleSetAPI, error) {
		t.Fatal("cancelled diagnosis must not start another API probe")
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context cancellation", err)
	}
}

func TestDoctorSummaryDoesNotClaimCleanWhenAPIProbeFails(t *testing.T) {
	var out bytes.Buffer
	probeErr := errors.New("GitHub API probe failed")
	if err := reportDoctorSummary(&out, 0, false, probeErr); !errors.Is(err, probeErr) {
		t.Fatalf("got %v, want API probe failure", err)
	}
	if strings.Contains(out.String(), "All clean") {
		t.Fatalf("failed diagnosis reported success: %s", &out)
	}
}

// Rejecting unsafe URLs must happen before client creation, which can read
// App keys, and before any credential-bearing HTTP request can be issued.
func TestDoctorRejectsUnsafeRegistrationURLsBeforeClientCreation(t *testing.T) {
	for _, registrationURL := range []string{
		"",
		"not-a-url",
		"https://",
		"http://github.com/example",
		"ftp://github.com/example",
		"//github.com/example",
		"/example",
		"https:///example",
		"https://synthetic-user:synthetic-url-secret@github.com/example",
		"https://github.com/%synthetic-url-secret",
		"https://github.com:invalid/example",
	} {
		t.Run(registrationURL, func(t *testing.T) {
			cfg := config.Config{ScaleSets: []config.ScaleSetConfig{
				{ScaleSetName: "unsafe", RegistrationURL: registrationURL, Token: "synthetic-pat"},
				{ScaleSetName: "safe", RegistrationURL: "https://github.com/example", Token: "synthetic-pat"},
			}}
			var output bytes.Buffer
			var checked []string
			err := checkScalesetAPIWith(context.Background(), &cfg, &output, func(ss config.ScaleSetConfig) (scaleSetAPI, error) {
				checked = append(checked, ss.ScaleSetName)
				return &doctorProbeClient{}, nil
			})
			if strings.Join(checked, ",") != "safe" {
				t.Fatalf("client factory called for %v; unsafe URL must be rejected before client creation", checked)
			}
			if err == nil || !strings.Contains(output.String(), "GitHub API [unsafe]") || !strings.Contains(output.String(), "GitHub API [safe]: reachable") {
				t.Fatalf("invalid URL must fail diagnosis and preserve later probes: %s; %v", &output, err)
			}
			for _, sensitive := range []string{"synthetic-user", "synthetic-url-secret", "synthetic-pat"} {
				if strings.Contains(output.String(), sensitive) || strings.Contains(err.Error(), sensitive) {
					t.Fatal("unsafe registration URL or credentials leaked in diagnosis")
				}
			}
		})
	}
}

// Dropping App support in the probe must prevent this real SDK request
// from reaching the local App authentication API over verified TLS.
func TestDoctorSDKFactoryUsesGitHubApp(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("RUNNER_DOCTOR_TEST_APP_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})))
	requests := make(chan string, 8)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.URL.Path
		if len(strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")) != 3 {
			t.Error("App authentication did not use a JWT")
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "synthetic-installation-token eyJ.synthetic.jwt -----BEGIN RSA PRIVATE KEY-----")
	}))
	defer server.Close()
	cfg := config.Config{ScaleSets: []config.ScaleSetConfig{{
		ScaleSetName: "app-set", RegistrationURL: server.URL + "/example",
		GitHubApp: &config.GitHubAppConfig{ClientID: "Iv1.synthetic", InstallationID: 123, PrivateKeyEnv: "RUNNER_DOCTOR_TEST_APP_KEY"},
	}}}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	var output bytes.Buffer
	probeErr := checkScalesetAPIWith(context.Background(), &cfg, &output, func(ss config.ScaleSetConfig) (scaleSetAPI, error) {
		return ss.NewScalesetClient(nil, scaleset.WithHTTPClient(scaleset.NewHTTPClient(scaleset.HTTPClientConfig{RootCAs: roots})))
	})
	select {
	case path := <-requests:
		if path != "/api/v3/app/installations/123/access_tokens" {
			t.Fatalf("unexpected App auth path %q", path)
		}
	default:
		t.Fatal("doctor did not invoke the real App authentication factory")
	}
	if probeErr == nil {
		t.Fatal("synthetic API rejection must fail diagnosis")
	}
	if !strings.Contains(output.String(), "HTTP 403") || !strings.Contains(probeErr.Error(), "HTTP 403") {
		t.Fatal("real SDK response did not retain its safe HTTP status category")
	}
	for _, secret := range []string{"synthetic-installation-token", "eyJ.synthetic.jwt", "-----BEGIN RSA PRIVATE KEY-----"} {
		if strings.Contains(output.String(), secret) || strings.Contains(probeErr.Error(), secret) {
			t.Fatal("SDK response secret leaked")
		}
	}
}

func TestDoctorAPIErrorMessagesUseSafeCategories(t *testing.T) {
	secret := "synthetic-installation-token eyJ.synthetic.jwt -----BEGIN RSA PRIVATE KEY-----"
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"backend response", errors.New(secret), "request failed"},
		{"factory", errors.New(secret), "request failed"},
		{"deadline", fmt.Errorf("%s: %w", secret, context.DeadlineExceeded), "timed out"},
		{"cancelled", fmt.Errorf("%s: %w", secret, context.Canceled), "cancelled"},
		{"DNS", &net.DNSError{Err: secret, Name: secret}, "DNS lookup failed"},
		{"TLS", x509.UnknownAuthorityError{}, "TLS verification failed"},
		{"HTTP", errors.New(`request failed(status="403 Forbidden"): ` + secret), "HTTP 403"},
		{"rate limit", fmt.Errorf("%s: %w", secret, &scaleset.RateLimitError{StatusCode: 403, Kind: scaleset.RateLimitSecondary, RetryAfter: 90 * time.Second}), "HTTP 403 rate limited (secondary); retry after 1m30s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{ScaleSets: []config.ScaleSetConfig{{ScaleSetName: "builders", RegistrationURL: "https://github.com/example", Token: "synthetic-pat"}}}
			var out bytes.Buffer
			err := checkScalesetAPIWith(context.Background(), &cfg, &out, func(config.ScaleSetConfig) (scaleSetAPI, error) {
				if tc.name == "factory" {
					return nil, tc.err
				}
				return &doctorProbeClient{err: tc.err}, nil
			})
			if err == nil {
				t.Fatal("failed request must return an error")
			}
			if !strings.Contains(out.String(), tc.want) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("missing safe category %q: %s; %v", tc.want, &out, err)
			}
			for _, sensitive := range []string{"synthetic-installation-token", "eyJ.synthetic.jwt", "-----BEGIN RSA PRIVATE KEY-----"} {
				if strings.Contains(out.String(), sensitive) || strings.Contains(err.Error(), sensitive) {
					t.Fatal("raw SDK error leaked into report or returned error")
				}
			}
		})
	}
}

// A 429 whose Retry-After outlasts the probe deadline comes back from the
// real SDK at once, as a RateLimitError that doctor must name without
// response text.
func TestDoctorReportsRateLimitsFromTheRealSDK(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, "synthetic-rate-limit-body ghp_syntheticsecret")
	}))
	defer server.Close()
	cfg := config.Config{ScaleSets: []config.ScaleSetConfig{{
		ScaleSetName: "limited", RegistrationURL: server.URL + "/example", Token: "synthetic-pat",
	}}}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	var out bytes.Buffer
	err := checkScalesetAPIWith(context.Background(), &cfg, &out, func(ss config.ScaleSetConfig) (scaleSetAPI, error) {
		return ss.NewScalesetClient(nil, scaleset.WithHTTPClient(scaleset.NewHTTPClient(scaleset.HTTPClientConfig{RootCAs: roots})))
	})
	if err == nil {
		t.Fatal("a rate-limited probe passed")
	}
	want := "HTTP 429 rate limited (secondary); retry after 1m0s"
	if !strings.Contains(out.String(), want) || !strings.Contains(err.Error(), want) {
		t.Fatalf("missing %q:\n%s\n%v", want, &out, err)
	}
	for _, secret := range []string{"synthetic-rate-limit-body", "ghp_syntheticsecret", "synthetic-pat"} {
		if strings.Contains(out.String(), secret) || strings.Contains(err.Error(), secret) {
			t.Fatal("rate-limited response text leaked")
		}
	}
}

func TestDoctorListsScaleSetsThisConfigDoesNotRun(t *testing.T) {
	example := &doctorProbeClient{fakeScaleSetAPI: fakeScaleSetAPI{
		groups: map[string]int{"builders": 42},
		scaleSets: map[int][]scaleset.RunnerScaleSet{
			1:  {{Name: "linux"}, {Name: "old-linux"}, {Name: "MAC"}, {Name: "evil\nname"}, {Name: "elsewhere"}},
			42: {{Name: "builders-set"}},
		},
	}}
	other := &doctorProbeClient{fakeScaleSetAPI: fakeScaleSetAPI{
		scaleSets: map[int][]scaleset.RunnerScaleSet{1: {{Name: "elsewhere"}}},
	}}
	cfg := config.Config{ScaleSets: []config.ScaleSetConfig{
		{ScaleSetName: "linux", RegistrationURL: "https://github.com/example", Token: "synthetic-pat"},
		{ScaleSetName: "mac", RegistrationURL: "https://github.com/example", Token: "synthetic-pat"},
		{ScaleSetName: "builders-set", RegistrationURL: "https://github.com/example", RunnerGroup: "builders", Token: "synthetic-pat"},
		{ScaleSetName: "elsewhere", RegistrationURL: "https://github.com/other", Token: "synthetic-pat"},
	}}
	var out bytes.Buffer
	err := checkScalesetAPIWith(context.Background(), &cfg, &out, func(ss config.ScaleSetConfig) (scaleSetAPI, error) {
		if ss.RegistrationURL == "https://github.com/other" {
			return other, nil
		}
		return example, nil
	})
	if err != nil {
		t.Fatalf("scale sets this config does not run must not fail diagnosis: %v\n%s", err, &out)
	}
	if !slices.Equal(example.listed, []int{1, 42}) || !slices.Equal(other.listed, []int{1}) {
		t.Fatalf("listed runner groups %v and %v, want each group once", example.listed, other.listed)
	}
	report := out.String()
	want := "  ⚠ GitHub API [linux]: its runner group has 3 scale set(s) not in this config\n" +
		"      \"elsewhere\"\n      \"evil\\nname\"\n      \"old-linux\"\n"
	if !strings.Contains(report, want) {
		t.Fatalf("missing the scale sets this config does not run:\n%s", report)
	}
	if strings.Contains(report, `"MAC"`) || strings.Contains(report, "[builders-set]: its runner group") || strings.Contains(report, "[elsewhere]: its runner group") {
		t.Fatalf("reported a scale set this config runs:\n%s", report)
	}
}

func TestDoctorBoundsTheListOfScaleSetsThisConfigDoesNotRun(t *testing.T) {
	var registered []scaleset.RunnerScaleSet
	for i := range 25 {
		registered = append(registered, scaleset.RunnerScaleSet{Name: fmt.Sprintf("other-%02d", i)})
	}
	client := &doctorProbeClient{fakeScaleSetAPI: fakeScaleSetAPI{scaleSets: map[int][]scaleset.RunnerScaleSet{1: registered}}}
	cfg := config.Config{ScaleSets: []config.ScaleSetConfig{{ScaleSetName: "linux", RegistrationURL: "https://github.com/example", Token: "synthetic-pat"}}}
	var out bytes.Buffer
	if err := checkScalesetAPIWith(context.Background(), &cfg, &out, func(config.ScaleSetConfig) (scaleSetAPI, error) { return client, nil }); err != nil {
		t.Fatal(err)
	}
	report := out.String()
	if !strings.Contains(report, "has 25 scale set(s)") || !strings.Contains(report, `"other-19"`) || strings.Contains(report, `"other-20"`) || !strings.Contains(report, "… and 5 more") {
		t.Fatalf("the list is not bounded to 20 names:\n%s", report)
	}
}

func TestDoctorListingFailureFailsDiagnosis(t *testing.T) {
	limited := &scaleset.RateLimitError{StatusCode: 403, Kind: scaleset.RateLimitSecondary}
	client := &doctorProbeClient{listErr: fmt.Errorf("synthetic-installation-token: %w", limited)}
	cfg := config.Config{ScaleSets: []config.ScaleSetConfig{{ScaleSetName: "linux", RegistrationURL: "https://github.com/example", Token: "synthetic-pat"}}}
	var out bytes.Buffer
	err := checkScalesetAPIWith(context.Background(), &cfg, &out, func(config.ScaleSetConfig) (scaleSetAPI, error) { return client, nil })
	if err == nil || !strings.Contains(out.String(), "✗ GitHub API [linux]: listing its runner group failed: HTTP 403 rate limited (secondary)") || !strings.Contains(err.Error(), "HTTP 403 rate limited") {
		t.Fatalf("a failed listing did not fail diagnosis with its safe category: %v\n%s", err, &out)
	}
	if strings.Contains(out.String(), "synthetic-installation-token") || strings.Contains(err.Error(), "synthetic-installation-token") {
		t.Fatal("raw listing error leaked")
	}
}

// A configured scale set whose own probe failed is still in this config.
func TestDoctorDoesNotListConfiguredScaleSetsWhoseProbeFailed(t *testing.T) {
	working := &doctorProbeClient{fakeScaleSetAPI: fakeScaleSetAPI{scaleSets: map[int][]scaleset.RunnerScaleSet{1: {{Name: "broken"}, {Name: "linux"}}}}}
	cfg := config.Config{ScaleSets: []config.ScaleSetConfig{
		{ScaleSetName: "broken", RegistrationURL: "https://github.com/example", Token: "synthetic-pat"},
		{ScaleSetName: "linux", RegistrationURL: "https://github.com/example", Token: "synthetic-pat"},
	}}
	var out bytes.Buffer
	err := checkScalesetAPIWith(context.Background(), &cfg, &out, func(ss config.ScaleSetConfig) (scaleSetAPI, error) {
		if ss.ScaleSetName == "broken" {
			return &doctorProbeClient{err: errors.New("synthetic failure")}, nil
		}
		return working, nil
	})
	if err == nil {
		t.Fatal("the failed probe must still fail diagnosis")
	}
	if !slices.Equal(working.listed, []int{1}) || strings.Contains(out.String(), "not in this config") {
		t.Fatalf("listed %v; reported a configured scale set:\n%s", working.listed, &out)
	}
}

type blockingDoctorClient struct {
	fakeScaleSetAPI
	started chan struct{}
}

func (c *blockingDoctorClient) GetRunnerScaleSet(ctx context.Context, _ int, _ string) (*scaleset.RunnerScaleSet, error) {
	if c.started != nil {
		close(c.started)
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestDoctorProbeDeadlineEndsBlockedRequestAndContinues(t *testing.T) {
	cfg := config.Config{ScaleSets: []config.ScaleSetConfig{
		{ScaleSetName: "blocked", RegistrationURL: "https://github.com/example", Token: "synthetic-pat"},
		{ScaleSetName: "next", RegistrationURL: "https://github.com/example", Token: "synthetic-pat"},
	}}
	var out bytes.Buffer
	// Bound the test as well: a regression that drops the per-probe timeout
	// must fail instead of hanging the test process indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	started := time.Now()
	err := checkScalesetAPIWith(ctx, &cfg, &out, func(ss config.ScaleSetConfig) (scaleSetAPI, error) {
		if ss.ScaleSetName == "blocked" {
			return &blockingDoctorClient{}, nil
		}
		return &doctorProbeClient{}, nil
	})
	if elapsed := time.Since(started); elapsed > 7*time.Second {
		t.Fatalf("probe deadline did not stop request promptly: %v", elapsed)
	}
	if err == nil || !strings.Contains(out.String(), "timed out") || !strings.Contains(out.String(), "GitHub API [next]: reachable") {
		t.Fatalf("timeout hid later result: %s; %v", &out, err)
	}
}

func TestDoctorCommandCancellationStopsInFlightProbe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	go func() { <-started; cancel() }()
	cfg := config.Config{ScaleSets: []config.ScaleSetConfig{{ScaleSetName: "blocked", RegistrationURL: "https://github.com/example", Token: "synthetic-pat"}}}
	var out bytes.Buffer
	err := checkScalesetAPIWith(ctx, &cfg, &out, func(config.ScaleSetConfig) (scaleSetAPI, error) { return &blockingDoctorClient{started: started}, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("in-flight cancellation was not propagated: %v", err)
	}
	if !strings.Contains(out.String(), "cancelled") {
		t.Fatalf("cancelled probe missing from report: %s", &out)
	}
}

func TestDoctorSummaryExitBehavior(t *testing.T) {
	apiErr := errors.New("API diagnosis failed")
	for _, tc := range []struct {
		name    string
		orphans int
		fix     bool
		apiErr  error
		wantErr bool
		want    string
	}{
		{"clean", 0, false, nil, false, "All clean"},
		{"API failure", 0, false, apiErr, true, "API diagnosis failed"},
		{"orphans", 2, false, nil, true, "Found 2"},
		{"failed cleanup", 1, true, nil, true, "Cleanup incomplete"},
		{"API failure after cleanup", 0, true, apiErr, true, "API diagnosis failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := reportDoctorSummary(&out, tc.orphans, tc.fix, tc.apiErr)
			if (err != nil) != tc.wantErr || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("summary %q; error %v", &out, err)
			}
			if tc.apiErr != nil && !errors.Is(err, tc.apiErr) {
				t.Fatal("summary lost API failure")
			}
		})
	}
}

// TestCheckDockerVolume_SharedVolumeIsNotOrphanedByExistence pins the core
// behavior change: the shared volume carries handoff data between jobs of
// one workflow run, so its mere existence must never be treated as a
// reason to remove it — not even under --fix.
func TestCheckDockerVolume_SharedVolumeIsNotOrphanedByExistence(t *testing.T) {
	md := &orphanFake{volumes: []string{"runner-shared"}}

	// fix=true must still not remove it: the volume is designed to outlive
	// the process, so its mere existence says nothing about whether it is
	// still needed.
	if _, err := checkDockerVolume(context.Background(), md, []string{"runner-shared"}); err != nil {
		t.Fatalf("checkDockerVolume: %v", err)
	}
	if len(md.volumesRemoved) != 0 {
		t.Errorf("doctor --fix removed %v; the shared volume carries handoff "+
			"data between jobs of one workflow run", md.volumesRemoved)
	}
}

// TestCheckDockerVolumeReportsCurrentName covers both names
// The shared volume is reported and never removed; its presence is not
// an unresolved issue — it is expected steady state, not a problem.
func TestCheckDockerVolumeReportsCurrentName(t *testing.T) {
	md := &orphanFake{volumes: []string{"runner-shared"}}

	issues, err := checkDockerVolume(context.Background(), md, []string{"runner-shared"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if issues != 0 {
		t.Errorf("issues = %d, want 0: an existing shared volume is expected state, not a problem", issues)
	}
	if len(md.volumesRemoved) != 0 {
		t.Errorf("expected no volumes removed, got %v", md.volumesRemoved)
	}
}

// TestCheckDockerVolumeReportsWithoutFix asserts fix=false behaves
// identically to fix=true (see TestCheckDockerVolumeReportsCurrentName):
// this check no longer branches on fix at all.
func TestCheckDockerVolumeReportsWithoutFix(t *testing.T) {
	md := &orphanFake{volumes: []string{"runner-shared"}}

	issues, err := checkDockerVolume(context.Background(), md, []string{"runner-shared"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if issues != 0 {
		t.Errorf("issues = %d, want 0", issues)
	}
	if len(md.volumesRemoved) != 0 {
		t.Errorf("VolumeRemove must never be called by this check, got %v", md.volumesRemoved)
	}
}

func TestCheckDockerVolumeNoneFound(t *testing.T) {
	md := &orphanFake{}

	issues, err := checkDockerVolume(context.Background(), md, []string{"runner-shared"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if issues != 0 {
		t.Errorf("issues = %d, want 0 when no volumes exist", issues)
	}
}

func TestCheckDockerVolumeSurfacesInspectFailure(t *testing.T) {
	md := &orphanFake{volumeInspectErr: fmt.Errorf("permission denied")}

	if _, err := checkDockerVolume(context.Background(), md, []string{"runner-shared"}); err == nil {
		t.Fatal("expected volume inspection failure to be returned")
	}
}

// TestVolumeSizeText pins "size not reported" for the cases doctor actually
// sees from a plain VolumeInspect call (UsageData is only populated by the
// disk-usage endpoint, so it is nil in practice) as well as the driver's
// explicit "-1 means unavailable" sentinel, and confirms a real size is
// formatted through bytesize.Format.
func TestVolumeSizeText(t *testing.T) {
	cases := []struct {
		name  string
		usage *volume.UsageData
		want  string
	}{
		{"nil usage data (plain inspect never returns it)", nil, "size not reported"},
		{"driver reports size unavailable (-1)", &volume.UsageData{Size: -1}, "size not reported"},
		{"known size", &volume.UsageData{Size: 2 * 1024 * 1024}, "2.0 MiB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := dockerclient.VolumeInspectResult{Volume: volume.Volume{UsageData: tc.usage}}
			if got := volumeSizeText(result); got != tc.want {
				t.Errorf("volumeSizeText() = %q, want %q", got, tc.want)
			}
		})
	}
}
