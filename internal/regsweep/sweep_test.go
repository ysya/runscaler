package regsweep

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/actions/scaleset"
)

const (
	testOrg        = "acme"
	testGroup      = 7
	testScaleSetID = 11
	testToken      = "test-pat-value"
)

// fakeGitHub serves the REST runner-group list from pages of runners and
// records each request it serves.
type fakeGitHub struct {
	t        *testing.T
	mu       sync.Mutex
	pages    [][]Runner
	requests []string
	onList   func() // runs while the list request is being served
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	want := fmt.Sprintf("/orgs/%s/actions/runner-groups/%d/runners", testOrg, testGroup)
	if r.URL.Path != want {
		f.t.Errorf("request path = %q, want %q", r.URL.Path, want)
		http.NotFound(w, r)
		return
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
		f.t.Errorf("Authorization = %q", got)
	}
	if got := r.URL.Query().Get("per_page"); got != "100" {
		f.t.Errorf("per_page = %q, want 100", got)
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	f.mu.Lock()
	f.requests = append(f.requests, r.URL.RawQuery)
	pages := f.pages
	onList := f.onList
	f.mu.Unlock()
	if onList != nil {
		onList()
	}
	total := 0
	for _, p := range pages {
		total += len(p)
	}
	var runners []Runner
	if page >= 1 && page <= len(pages) {
		runners = pages[page-1]
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"total_count": total, "runners": runners})
}

func (f *fakeGitHub) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// fakeScaleSet is the scale set API: GetRunner answers from refs (absent IDs
// are not found), RemoveRunner records the IDs it removed.
type fakeScaleSet struct {
	mu        sync.Mutex
	refs      map[int]*scaleset.RunnerReference
	getErr    map[int]error
	removeErr map[int64]error
	lookups   []int
	removed   []int64
}

func (f *fakeScaleSet) GetRunner(_ context.Context, id int) (*scaleset.RunnerReference, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups = append(f.lookups, id)
	if err := f.getErr[id]; err != nil {
		return nil, err
	}
	ref, ok := f.refs[id]
	if !ok {
		return nil, fmt.Errorf("request GET failed: %w", scaleset.NotFoundError)
	}
	return ref, nil
}

func (f *fakeScaleSet) RemoveRunner(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.removeErr[id]; err != nil {
		return err
	}
	f.removed = append(f.removed, id)
	delete(f.refs, int(id))
	return nil
}

func (f *fakeScaleSet) removedIDs() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := slices.Clone(f.removed)
	slices.Sort(out)
	return out
}

type fakeLocal struct {
	mu    sync.Mutex
	names map[string]bool
}

func (l *fakeLocal) HasRunner(name string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.names[name]
}

func (l *fakeLocal) add(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.names == nil {
		l.names = map[string]bool{}
	}
	l.names[name] = true
}

// leaked is the live shape of a leaked JIT registration: offline, not busy,
// no labels, named like a runner this process creates.
func leaked(id int64) Runner {
	return Runner{ID: id, Name: fmt.Sprintf("runner-%08x", 0xa0000000+id), Status: "offline"}
}

// ours registers id as a runner of this scale set in the scale set API.
func (f *fakeScaleSet) ours(ids ...int64) *fakeScaleSet {
	if f.refs == nil {
		f.refs = map[int]*scaleset.RunnerReference{}
	}
	for _, id := range ids {
		f.refs[int(id)] = &scaleset.RunnerReference{ID: int(id), RunnerScaleSetID: testScaleSetID}
	}
	return f
}

type env struct {
	gh      *fakeGitHub
	api     *fakeScaleSet
	local   *fakeLocal
	sweeper *Sweeper
	logs    *bytes.Buffer
}

func newEnv(t *testing.T, mode Mode, pages ...[]Runner) *env {
	t.Helper()
	return newEnvWith(t, mode, &fakeGitHub{t: t, pages: pages}, nil)
}

// newEnvWith builds an env around gh and, when api is non-nil, that scale
// set API instead of a fresh fakeScaleSet.
func newEnvWith(t *testing.T, mode Mode, gh *fakeGitHub, api ScaleSetAPI) *env {
	t.Helper()
	srv := httptest.NewServer(gh)
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	resolve := func(context.Context) (int, error) { return testGroup, nil }
	lister := NewGroupLister(base, testOrg, resolve, StaticToken(testToken), srv.Client())
	e := &env{gh: gh, api: &fakeScaleSet{}, local: &fakeLocal{}, logs: &bytes.Buffer{}}
	if api == nil {
		api = e.api
	}
	logger := slog.New(slog.NewTextHandler(e.logs, nil))
	s, err := New(Config{Mode: mode, ScaleSetID: testScaleSetID, Interval: 10 * time.Minute}, lister, api, e.local, logger)
	if err != nil {
		t.Fatal(err)
	}
	e.sweeper = s
	return e
}

func assertRemoved(t *testing.T, e *env, want ...int64) {
	t.Helper()
	got := e.api.removedIDs()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("removed %v, want %v\nlogs:\n%s", got, want, e.logs)
	}
}

func TestSweepKeepsOnlineRunner(t *testing.T) {
	online := leaked(2)
	online.Status = "online"
	e := newEnv(t, ModeRemove, []Runner{leaked(1), online})
	e.api.ours(1, 2)
	e.sweeper.Sweep(context.Background())
	assertRemoved(t, e, 1)
}

func TestSweepKeepsBusyRunner(t *testing.T) {
	busy := leaked(2)
	busy.Busy = true
	e := newEnv(t, ModeRemove, []Runner{leaked(1), busy})
	e.api.ours(1, 2)
	e.sweeper.Sweep(context.Background())
	assertRemoved(t, e, 1)
}

func TestSweepKeepsForeignName(t *testing.T) {
	names := []string{
		"runner-keep-0000abcd", // the cache prune's mark-used clone
		"runner-ABCDEF12",      // upper-case hex is not what NewRunnerName makes
		"runner-1234567",       // too short
		"runner-123456789",     // too long
		"runner-1234567g",      // not hex
		"pool-1-1700000000000", // warm-pool VM name
		"xrunner-12345678",
		"runner-12345678x",
		"other-12345678",
		"mac-mini-01",
	}
	runners := []Runner{leaked(1)}
	ids := []int64{1}
	for i, name := range names {
		id := int64(100 + i)
		runners = append(runners, Runner{ID: id, Name: name, Status: "offline"})
		ids = append(ids, id)
	}
	e := newEnv(t, ModeRemove, runners)
	e.api.ours(ids...)
	e.sweeper.Sweep(context.Background())
	assertRemoved(t, e, 1)
}

func TestSweepKeepsLocalRunner(t *testing.T) {
	local := leaked(2)
	e := newEnv(t, ModeRemove, []Runner{leaked(1), local})
	e.api.ours(1, 2)
	e.local.add(local.Name)
	e.sweeper.Sweep(context.Background())
	assertRemoved(t, e, 1)
}

// A runner that became local while the list was in flight must be kept: the
// local check reads state after the list returns, not before it.
func TestSweepReadsLocalStateAfterTheList(t *testing.T) {
	booting := leaked(2)
	e := newEnv(t, ModeRemove, []Runner{leaked(1), booting})
	e.api.ours(1, 2)
	e.gh.onList = func() { e.local.add(booting.Name) }
	e.sweeper.Sweep(context.Background())
	assertRemoved(t, e, 1)
}

func TestSweepKeepsRunnerOfAnotherScaleSet(t *testing.T) {
	e := newEnv(t, ModeRemove, []Runner{leaked(1), leaked(2), leaked(3)})
	e.api.ours(1)
	e.api.refs[2] = &scaleset.RunnerReference{ID: 2, RunnerScaleSetID: testScaleSetID + 1}
	e.api.refs[3] = &scaleset.RunnerReference{ID: 3} // no scale set
	e.sweeper.Sweep(context.Background())
	assertRemoved(t, e, 1)
	if !strings.Contains(e.logs.String(), "runnerScaleSetID=12") {
		t.Fatalf("the kept runner's scale set is not logged:\n%s", e.logs)
	}
}

func TestSweepLogModeRemovesNothing(t *testing.T) {
	e := newEnv(t, ModeLog, []Runner{leaked(1985), leaked(1988)})
	e.api.ours(1985, 1988)
	res := e.sweeper.Sweep(context.Background())
	assertRemoved(t, e)
	if res.Candidates != 2 || res.Removed != 0 {
		t.Fatalf("result = %+v, want 2 candidates and 0 removed", res)
	}
	logs := e.logs.String()
	for _, r := range []Runner{leaked(1985), leaked(1988)} {
		if !strings.Contains(logs, "runnerID="+strconv.FormatInt(r.ID, 10)) || !strings.Contains(logs, "name="+r.Name) {
			t.Fatalf("log mode does not name runner %d %s:\n%s", r.ID, r.Name, logs)
		}
	}
	if !strings.Contains(logs, "candidates=2") || !strings.Contains(logs, "removed=0") {
		t.Fatalf("summary line missing:\n%s", logs)
	}
}

func TestSweepRemoveModeRemovesExactlyTheLeaked(t *testing.T) {
	online := leaked(10)
	online.Status = "online"
	busy := leaked(11)
	busy.Busy = true
	local := leaked(12)
	foreignName := Runner{ID: 13, Name: "mac-mini-01", Status: "offline"}
	otherSet := leaked(14)
	e := newEnv(t, ModeRemove, []Runner{leaked(1), online, busy, local, foreignName, otherSet, leaked(2), leaked(3)})
	e.api.ours(1, 2, 3, 10, 11, 12, 13)
	e.api.refs[14] = &scaleset.RunnerReference{ID: 14, RunnerScaleSetID: 99}
	e.local.add(local.Name)
	res := e.sweeper.Sweep(context.Background())
	assertRemoved(t, e, 1, 2, 3)
	if res.Listed != 8 || res.Candidates != 3 || res.Removed != 3 || res.Errors != 0 {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(e.logs.String(), "listed=8 candidates=3 removed=3") {
		t.Fatalf("summary line missing:\n%s", e.logs)
	}
}

func TestSweepReadsEveryPage(t *testing.T) {
	var page1 []Runner
	for i := range 100 {
		r := leaked(int64(1000 + i))
		r.Status = "online"
		page1 = append(page1, r)
	}
	e := newEnv(t, ModeRemove, page1, []Runner{leaked(5)})
	e.api.ours(5)
	res := e.sweeper.Sweep(context.Background())
	assertRemoved(t, e, 5)
	if res.Listed != 101 || e.gh.requestCount() != 2 {
		t.Fatalf("listed %d runners in %d requests, want 101 in 2", res.Listed, e.gh.requestCount())
	}
}

func TestSweepCapsRemovalsPerSweep(t *testing.T) {
	var runners []Runner
	for i := range 25 {
		runners = append(runners, leaked(int64(i+1)))
	}
	e := newEnv(t, ModeRemove, runners)
	for i := range 25 {
		e.api.ours(int64(i + 1))
	}
	res := e.sweeper.Sweep(context.Background())
	if res.Removed != DefaultMaxRemovals || res.Deferred != 25-DefaultMaxRemovals {
		t.Fatalf("result = %+v, want %d removed and %d deferred", res, DefaultMaxRemovals, 25-DefaultMaxRemovals)
	}
	// The next sweep takes the rest.
	res = e.sweeper.Sweep(context.Background())
	if res.Removed != 25-DefaultMaxRemovals || len(e.api.removedIDs()) != 25 {
		t.Fatalf("second sweep = %+v, removed %d in all", res, len(e.api.removedIDs()))
	}
}

func TestSweepLeavesRefusedRemovalForNextSweep(t *testing.T) {
	e := newEnv(t, ModeRemove, []Runner{leaked(1), leaked(2)})
	e.api.ours(1, 2)
	e.api.removeErr = map[int64]error{2: fmt.Errorf("request DELETE failed: %w", scaleset.JobStillRunningError)}
	res := e.sweeper.Sweep(context.Background())
	assertRemoved(t, e, 1)
	if res.Errors != 1 {
		t.Fatalf("result = %+v, want 1 error", res)
	}
	delete(e.api.removeErr, 2)
	e.gh.pages = [][]Runner{{leaked(2)}}
	e.sweeper.Sweep(context.Background())
	assertRemoved(t, e, 1, 2)
}

func TestSweepCountsRunnerGoneBeforeLookupAsGone(t *testing.T) {
	e := newEnv(t, ModeRemove, []Runner{leaked(1), leaked(2)})
	e.api.ours(1) // 2 is no longer known to the scale set API
	res := e.sweeper.Sweep(context.Background())
	assertRemoved(t, e, 1)
	if res.Gone != 1 || res.Errors != 0 {
		t.Fatalf("result = %+v, want 1 gone and no error", res)
	}
}

func TestSweepListErrorRemovesNothingAndLeaksNoSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "secret-response-body", http.StatusInternalServerError)
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	lister := NewGroupLister(base, testOrg, func(context.Context) (int, error) { return testGroup, nil }, StaticToken(testToken), srv.Client())
	logs := &bytes.Buffer{}
	api := (&fakeScaleSet{}).ours(1)
	s, err := New(Config{Mode: ModeRemove, ScaleSetID: testScaleSetID, Interval: time.Minute}, lister, api, &fakeLocal{}, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	res := s.Sweep(context.Background())
	if res.ListErr == nil || res.Errors != 1 || len(api.removedIDs()) != 0 {
		t.Fatalf("result = %+v, removed %v", res, api.removedIDs())
	}
	for _, secret := range []string{"secret-response-body", testToken} {
		if strings.Contains(logs.String(), secret) || strings.Contains(res.ListErr.Error(), secret) {
			t.Fatalf("list error exposes %q: %v\n%s", secret, res.ListErr, logs)
		}
	}
	if !strings.Contains(res.ListErr.Error(), "HTTP 500") {
		t.Fatalf("list error = %v, want the status", res.ListErr)
	}
}

func TestGroupListerRetriesFailedGroupLookup(t *testing.T) {
	gh := &fakeGitHub{t: t, pages: [][]Runner{{leaked(1)}}}
	srv := httptest.NewServer(gh)
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	calls := 0
	resolve := func(context.Context) (int, error) {
		calls++
		if calls == 1 {
			return 0, fmt.Errorf("transient")
		}
		return testGroup, nil
	}
	l := NewGroupLister(base, testOrg, resolve, StaticToken(testToken), srv.Client())
	if _, err := l.ListRunners(context.Background()); err == nil {
		t.Fatal("first list succeeded without a group")
	}
	for range 2 {
		if got, err := l.ListRunners(context.Background()); err != nil || len(got) != 1 {
			t.Fatalf("list = %v, %v", got, err)
		}
	}
	if calls != 2 {
		t.Fatalf("group resolved %d times, want 2 (one failure, then cached)", calls)
	}
}

func TestNewRefusesUnsafeConfig(t *testing.T) {
	l, api, local, logger := &GroupLister{}, &fakeScaleSet{}, &fakeLocal{}, slog.New(slog.DiscardHandler)
	for name, cfg := range map[string]Config{
		"no scale set": {Mode: ModeRemove, Interval: time.Minute},
		"no interval":  {Mode: ModeRemove, ScaleSetID: 1},
		"off":          {Mode: "off", ScaleSetID: 1, Interval: time.Minute},
	} {
		if _, err := New(cfg, l, api, local, logger); err == nil {
			t.Errorf("%s: New accepted %+v", name, cfg)
		}
	}
}

func TestAPIBaseURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/acme":          "https://api.github.com",
		"https://www.github.com/acme":      "https://api.github.com",
		"https://acme.ghe.com/acme":        "https://api.acme.ghe.com",
		"https://ghes.example.com/acme":    "https://ghes.example.com/api/v3",
		"https://ghes.example.com/acme/":   "https://ghes.example.com/api/v3",
		"https://github.localhost/acme":    "https://api.github.localhost",
		"https://GitHub.com/acme/":         "https://api.GitHub.com",
		"http://ghes.internal:8443/acme":   "http://ghes.internal:8443/api/v3",
		"https://ghes.example.com/acme/re": "https://ghes.example.com/api/v3",
	} {
		got, err := APIBaseURL(in)
		if err != nil || got.String() != want {
			t.Errorf("APIBaseURL(%q) = %v, %v; want %s", in, got, err, want)
		}
	}
}

// Runners of another scale set that stay in the list use up the lookups of
// a sweep; the next sweep starts after them and reaches the leaked one.
func TestSweepReachesLaterRunnerPastForeignLookups(t *testing.T) {
	var runners []Runner
	for i := range DefaultMaxLookups {
		runners = append(runners, leaked(int64(i+1)))
	}
	mine := leaked(int64(DefaultMaxLookups + 1))
	runners = append(runners, mine)
	e := newEnv(t, ModeRemove, runners)
	e.api.ours(mine.ID)
	for i := range DefaultMaxLookups {
		id := i + 1
		e.api.refs[id] = &scaleset.RunnerReference{ID: id, RunnerScaleSetID: testScaleSetID + 1}
	}
	if res := e.sweeper.Sweep(context.Background()); res.Removed != 0 || res.Deferred != 1 {
		t.Fatalf("first sweep = %+v, want the lookups used up and 1 deferred", res)
	}
	e.sweeper.Sweep(context.Background())
	assertRemoved(t, e, mine.ID)
}

// Registrations whose removal GitHub keeps refusing use up the removal bound
// of a sweep; the next sweep starts after them and reaches the removable one.
func TestSweepReachesLaterRunnerPastRefusedRemovals(t *testing.T) {
	var runners []Runner
	e := newEnv(t, ModeRemove)
	e.api.removeErr = map[int64]error{}
	for i := range DefaultMaxRemovals {
		id := int64(i + 1)
		runners = append(runners, leaked(id))
		e.api.ours(id)
		e.api.removeErr[id] = fmt.Errorf("request DELETE failed: %w", scaleset.JobStillRunningError)
	}
	removable := leaked(int64(DefaultMaxRemovals + 1))
	runners = append(runners, removable)
	e.api.ours(removable.ID)
	e.gh.pages = [][]Runner{runners}
	for sweep := 1; sweep <= 3; sweep++ {
		e.sweeper.Sweep(context.Background())
	}
	assertRemoved(t, e, removable.ID)
}

// The list order from GitHub does not decide which runners a bounded sweep
// reaches first: runners are taken by ID.
func TestSweepTakesRunnersInIDOrder(t *testing.T) {
	var runners []Runner
	for i := 25; i >= 1; i-- {
		runners = append(runners, leaked(int64(i)))
	}
	e := newEnv(t, ModeRemove, runners)
	for i := range 25 {
		e.api.ours(int64(i + 1))
	}
	e.sweeper.Sweep(context.Background())
	var want []int64
	for i := range DefaultMaxRemovals {
		want = append(want, int64(i+1))
	}
	assertRemoved(t, e, want...)
}
