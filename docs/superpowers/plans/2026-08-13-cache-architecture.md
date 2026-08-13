# 統一快取架構與磁碟守門員 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把四套各說各話的快取機制收斂到 `CacheStore` 抽象之下,並加上以可用磁碟空間為主控制的守門員與用量可見性。

**Architecture:** 新增 `internal/cachestore`(每塊可回收儲存的統一介面,帶 `Kind` 分類)與 `internal/diskguard`(per-filesystem 門檻判斷 + 逐階回收)。現有四個 sweeper 保持不變,改為與守門員共用同一組 store 實作。回收階層由 `Kind` 推導,使「交接區未過期資料」在型別層面就不可能被誤刪。

**Tech Stack:** Go 1.26、`syscall.Statfs`、既有的 `DockerAPI` / `CommandRunner` 抽象與其 mock。

**Spec:** `docs/superpowers/specs/2026-08-13-cache-architecture-design.md`

## Global Constraints

- **既有設定零改動**:leg host、Mac Studio、tower-git-worker 經 self-update 取得新版,設定檔不會同步更新。舊 key(`cache-space-budget`、`shared-volume-ttl`)必須繼續生效且不得產生 unknown-key 警告。
- **`KindScratch` 未過期的部分在任何階層都不可回收**。這是防資料遺失的關鍵不變量,必須有測試釘住。
- **便宜路徑不得呼叫 `Measure()`**。啟動 runner 前的檢查只做 `statfs`。
- **守門員尊重各 store 的啟用開關**:`prune = false` 時不得代為 prune,只在警告中說明無法回收。
- **shared volume 的 `max-age` 下限必須大於最長 workflow 執行時間**;現行 `find -mtime` 的整天粒度與 1 天下限不得改細。
- 每個 task 結束前必須全綠:`go build ./... && go test ./... -count=1 && go vet ./... && gofmt -l .`(需無輸出)`&& golangci-lint run`。
- 註解一律英文,commit 用 conventional commits。

---

### Task 1: `cachestore` 套件骨架

**Files:**
- Create: `internal/cachestore/store.go`
- Test: `internal/cachestore/store_test.go`

**Interfaces:**
- Consumes: 無
- Produces: `StoreKind`(`KindGarbage`/`KindCache`/`KindScratch`)、`Tier`(`Tier1`…`Tier4`)、`CacheStore` 介面、`func TiersFor(k StoreKind) []Tier`

- [ ] **Step 1: 寫失敗的測試**

```go
package cachestore

import (
	"slices"
	"testing"
)

func TestTiersFor(t *testing.T) {
	tests := []struct {
		kind StoreKind
		want []Tier
	}{
		{KindGarbage, []Tier{Tier1}},
		{KindCache, []Tier{Tier2, Tier4}},
		{KindScratch, []Tier{Tier3}},
	}
	for _, tt := range tests {
		got := TiersFor(tt.kind)
		if !slices.Equal(got, tt.want) {
			t.Errorf("TiersFor(%v) = %v, want %v", tt.kind, got, tt.want)
		}
	}
}

// TestScratchNeverReclaimedWholesale pins the invariant that protects
// in-flight workflow handoff data: a scratch store is only ever reclaimable
// at Tier3 (its TTL-expired portion), never at the wholesale Tier4.
func TestScratchNeverReclaimedWholesale(t *testing.T) {
	for _, tier := range TiersFor(KindScratch) {
		if tier == Tier4 {
			t.Fatal("KindScratch must never be reclaimable at Tier4 — " +
				"wholesale removal would destroy an in-flight run's handoff data")
		}
	}
}
```

- [ ] **Step 2: 執行測試確認失敗**

Run: `go test ./internal/cachestore/ -run 'TestTiersFor|TestScratchNever' -v`
Expected: FAIL,編譯錯誤 `undefined: StoreKind`

- [ ] **Step 3: 寫最小實作**

```go
// Package cachestore models the reclaimable storage runner manages on a
// host — Docker images and build cache, named cache volumes, the shared
// volume, and Tart's image cache — behind one interface so the disk guard
// can reclaim from all of them without knowing their details.
package cachestore

import "context"

// StoreKind classifies a store by what happens when its contents vanish.
// The reclaim ladder is derived from this rather than hand-maintained, so a
// new store type cannot be filed into a tier that would destroy live data.
type StoreKind int

const (
	// KindGarbage is already-dead data: nothing observes its removal.
	KindGarbage StoreKind = iota
	// KindCache is regenerable: removal only costs time on the next job.
	KindCache
	// KindScratch is live handoff data between jobs of one workflow run.
	// Removing the un-expired portion breaks the run outright, so only its
	// TTL-expired portion is ever reclaimable.
	KindScratch
)

// Tier orders reclamation from free to expensive. The guard walks tiers in
// ascending order and stops as soon as the free-space target is met.
type Tier int

const (
	Tier1 Tier = iota + 1 // dangling images, stopped containers, buildx orphans
	Tier2                 // build cache and image layers past their max-age
	Tier3                 // shared-volume files past their max-age
	Tier4                 // wipe cache volumes — next build starts cold
)

// TiersFor returns the tiers at which a store of the given kind may be
// reclaimed. KindScratch deliberately excludes Tier4.
func TiersFor(k StoreKind) []Tier {
	switch k {
	case KindGarbage:
		return []Tier{Tier1}
	case KindCache:
		return []Tier{Tier2, Tier4}
	case KindScratch:
		return []Tier{Tier3}
	default:
		return nil
	}
}

// CacheStore is one reclaimable store on the host.
type CacheStore interface {
	Name() string
	Kind() StoreKind
	// Path is where this store's data lives, used to resolve which
	// filesystem its usage counts against.
	Path() string
	// Enabled reports whether the operator left this store's cleanup on.
	// The guard skips disabled stores rather than overriding the choice.
	Enabled() bool
	// Measure reports current usage. May be expensive (it can walk a
	// volume), so it is never called on the pre-job check path.
	Measure(ctx context.Context) (uint64, error)
	// Reclaim frees what this store can release at the given tier and
	// reports the bytes freed. Tiers the store does not participate in are
	// a no-op returning 0.
	Reclaim(ctx context.Context, tier Tier) (uint64, error)
}
```

- [ ] **Step 4: 執行測試確認通過**

Run: `go test ./internal/cachestore/ -count=1 -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/cachestore/
git commit -m "feat(cachestore): add store abstraction with kind-derived reclaim tiers"
```

---

### Task 2: Docker daemon store

**Files:**
- Create: `internal/cachestore/docker.go`
- Test: `internal/cachestore/docker_test.go`

**Interfaces:**
- Consumes: Task 1 的 `CacheStore`/`StoreKind`/`Tier`;既有 `backend.DockerAPI`
- Produces:
  - `func NewDockerGarbageStore(client backend.DockerAPI, cfg DockerGarbageConfig) CacheStore` —— `KindGarbage`,只做 Tier1(stopped containers + dangling images)
  - `func NewDockerBuildCacheStore(client backend.DockerAPI, cfg DockerBuildCacheConfig) CacheStore` —— `KindCache`,Tier2 依 max-age/budget 修剪,Tier4 全清(`All: true` 無 filter,緊急回收)
  - `type DockerGarbageConfig struct { Enabled bool; PruneTTL time.Duration }`
  - `type DockerBuildCacheConfig struct { Enabled bool; MaxAge time.Duration; BudgetGB int }`

**為何拆兩個**:一個 store 只能有一個 `Kind`,而守門員以 `TiersFor(store.Kind())` 挑選 store。若把兩者合成一個 `KindGarbage` 的 store,它的 Tier2 永遠不會被守門員呼叫,build cache 就無法用於紓解磁碟壓力。

備註:此 store 的 `Path()` 回傳 Docker 的資料根目錄。以 `client.Info(ctx).DockerRootDir` 取得;`DockerAPI` 介面需新增 `Info`。

- [ ] **Step 1: 寫失敗的測試**

```go
package cachestore

import (
	"context"
	"testing"
	"time"
)

func TestDockerDaemonStore_Tier1PrunesGarbageOnly(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewDockerDaemonStore(fake, DockerDaemonConfig{
		Enabled: true, PruneTTL: 24 * time.Hour,
	})

	if _, err := s.Reclaim(context.Background(), Tier1); err != nil {
		t.Fatalf("Reclaim(Tier1) error: %v", err)
	}
	if fake.containersPruneCalls != 1 || fake.imagesPruneCalls != 1 {
		t.Errorf("Tier1 should prune stopped containers and dangling images, got %d/%d",
			fake.containersPruneCalls, fake.imagesPruneCalls)
	}
	if fake.buildCachePruneCalls != 0 {
		t.Errorf("Tier1 must not touch build cache, got %d calls", fake.buildCachePruneCalls)
	}
}

func TestDockerDaemonStore_DisabledReclaimsNothing(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewDockerDaemonStore(fake, DockerDaemonConfig{Enabled: false, PruneTTL: 24 * time.Hour})

	freed, err := s.Reclaim(context.Background(), Tier1)
	if err != nil {
		t.Fatalf("Reclaim error: %v", err)
	}
	if freed != 0 || fake.containersPruneCalls != 0 {
		t.Error("a disabled store must not prune — the operator turned it off deliberately")
	}
}
```

`fakeDockerAPI` 放在 `internal/cachestore/fake_test.go`,實作 `backend.DockerAPI` 全部方法。欄位如下(**後續 task 的測試依賴這些名稱**):

```go
type fakeDockerAPI struct {
	rootDir string // Info() 回傳的 DockerRootDir

	containersPruneCalls int
	imagesPruneCalls     int
	buildCachePruneCalls int
	buildCachePruneOpts  []build.CachePruneOptions

	volumesRemoved    []string // VolumeRemove 收到的 volume 名稱
	createdContainers int      // ContainerCreate 次數(helper container 用)
	helperScripts     []string // 每個 helper container 的 sh -c 腳本
	diskUsage         types.DiskUsage
}
```

`ContainerWait` 回傳 status 0 立即完成,`Info` 回傳 `system.Info{DockerRootDir: f.rootDir}`。

- [ ] **Step 2: 執行測試確認失敗**

Run: `go test ./internal/cachestore/ -run TestDockerDaemonStore -v`
Expected: FAIL,`undefined: NewDockerDaemonStore`

- [ ] **Step 3: 寫最小實作**

```go
package cachestore

import (
	"context"
	"fmt"
	"time"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/filters"

	"github.com/ysya/runscaler/internal/backend"
)

// DockerDaemonConfig mirrors the [docker] prune settings for one daemon.
type DockerDaemonConfig struct {
	Enabled            bool
	PruneTTL           time.Duration
	BuildCacheMaxAge   time.Duration
	BuildCacheBudgetGB int
	RootDir            string
}

type dockerDaemonStore struct {
	client backend.DockerAPI
	cfg    DockerDaemonConfig
}

// NewDockerDaemonStore reclaims daemon-owned garbage and build cache.
// It is KindGarbage: everything it removes is already unreferenced.
func NewDockerDaemonStore(client backend.DockerAPI, cfg DockerDaemonConfig) CacheStore {
	return &dockerDaemonStore{client: client, cfg: cfg}
}

func (s *dockerDaemonStore) Name() string    { return "docker-daemon" }
func (s *dockerDaemonStore) Kind() StoreKind { return KindGarbage }
func (s *dockerDaemonStore) Path() string    { return s.cfg.RootDir }
func (s *dockerDaemonStore) Enabled() bool   { return s.cfg.Enabled }

func (s *dockerDaemonStore) Measure(ctx context.Context) (uint64, error) {
	usage, err := s.client.DiskUsage(ctx)
	if err != nil {
		return 0, fmt.Errorf("docker disk usage: %w", err)
	}
	var total uint64
	for _, img := range usage.Images {
		if img != nil && img.Size > 0 {
			total += uint64(img.Size)
		}
	}
	for _, bc := range usage.BuildCache {
		if bc != nil && bc.Size > 0 {
			total += uint64(bc.Size)
		}
	}
	return total, nil
}

func (s *dockerDaemonStore) Reclaim(ctx context.Context, tier Tier) (uint64, error) {
	if !s.cfg.Enabled {
		return 0, nil
	}
	switch tier {
	case Tier1:
		return s.reclaimGarbage(ctx)
	case Tier2:
		return s.reclaimBuildCache(ctx)
	default:
		return 0, nil
	}
}

func (s *dockerDaemonStore) reclaimGarbage(ctx context.Context) (uint64, error) {
	if s.cfg.PruneTTL <= 0 {
		return 0, nil
	}
	until := filters.NewArgs(filters.Arg("until", s.cfg.PruneTTL.String()))
	var freed uint64
	if r, err := s.client.ContainersPrune(ctx, until); err == nil {
		freed += r.SpaceReclaimed
	} else {
		return freed, fmt.Errorf("prune stopped containers: %w", err)
	}
	imgFilters := filters.NewArgs(
		filters.Arg("dangling", "true"),
		filters.Arg("until", s.cfg.PruneTTL.String()),
	)
	r, err := s.client.ImagesPrune(ctx, imgFilters)
	if err != nil {
		return freed, fmt.Errorf("prune dangling images: %w", err)
	}
	return freed + r.SpaceReclaimed, nil
}

func (s *dockerDaemonStore) reclaimBuildCache(ctx context.Context) (uint64, error) {
	if s.cfg.BuildCacheMaxAge <= 0 && s.cfg.BuildCacheBudgetGB <= 0 {
		return 0, nil
	}
	opts := build.CachePruneOptions{All: true}
	if s.cfg.BuildCacheMaxAge > 0 {
		opts.Filters = filters.NewArgs(filters.Arg("until", s.cfg.BuildCacheMaxAge.String()))
	}
	if s.cfg.BuildCacheBudgetGB > 0 {
		opts.MaxUsedSpace = int64(s.cfg.BuildCacheBudgetGB) * 1024 * 1024 * 1024
	}
	r, err := s.client.BuildCachePrune(ctx, opts)
	if err != nil {
		return 0, fmt.Errorf("prune build cache: %w", err)
	}
	return r.SpaceReclaimed, nil
}
```

同步在 `internal/backend/docker.go` 的 `DockerAPI` 介面加入:

```go
	Info(ctx context.Context) (system.Info, error)
	DiskUsage(ctx context.Context) (types.DiskUsage, error)
```

並在 `internal/backend/docker_test.go` 的 `mockDocker` 補上這兩個方法(回傳零值即可,`Info` 回傳 `system.Info{DockerRootDir: "/var/lib/docker"}`)。

- [ ] **Step 4: 執行測試確認通過**

Run: `go test ./internal/cachestore/ ./internal/backend/ -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/cachestore/ internal/backend/
git commit -m "feat(cachestore): add docker daemon store for garbage and build cache"
```

---

### Task 3: Shared volume store 與 cache volume store

**Files:**
- Create: `internal/cachestore/volume.go`
- Test: `internal/cachestore/volume_test.go`

**Interfaces:**
- Consumes: Task 1、Task 2 的 `fakeDockerAPI`
- Produces:
  - `func NewSharedVolumeStore(client backend.DockerAPI, cfg SharedVolumeConfig) CacheStore`
  - `type SharedVolumeConfig struct { VolumeName, MountPath, HelperImage, RootDir string; MaxAge time.Duration }`
  - `func NewCacheVolumeStore(client backend.DockerAPI, cfg CacheVolumeConfig) CacheStore`
  - `type CacheVolumeConfig struct { VolumeName, MountPath, HelperImage, RootDir string; BudgetBytes uint64; OnExceed string }`

- [ ] **Step 1: 寫失敗的測試**

```go
func TestSharedVolumeStore_IsScratchAndOnlyTier3(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewSharedVolumeStore(fake, SharedVolumeConfig{
		VolumeName: "runner-shared", MountPath: "/shared",
		HelperImage: "img", MaxAge: 72 * time.Hour,
	})

	if s.Kind() != KindScratch {
		t.Fatalf("Kind() = %v, want KindScratch — it holds live handoff data", s.Kind())
	}
	// Tier4 would remove the whole volume, destroying an in-flight run.
	freed, err := s.Reclaim(context.Background(), Tier4)
	if err != nil {
		t.Fatalf("Reclaim(Tier4) error: %v", err)
	}
	if freed != 0 || fake.volumesRemoved != nil {
		t.Error("shared volume must never be removed wholesale")
	}
}

func TestCacheVolumeStore_Tier4WipesOnlyWhenAllowed(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewCacheVolumeStore(fake, CacheVolumeConfig{
		VolumeName: "ccache", MountPath: "/home/runner/.ccache", HelperImage: "img",
	})

	if _, err := s.Reclaim(context.Background(), Tier2); err != nil {
		t.Fatalf("Reclaim(Tier2) error: %v", err)
	}
	if fake.createdContainers != 0 {
		t.Error("cache volume has no age-based reclaim; Tier2 must be a no-op")
	}
	if _, err := s.Reclaim(context.Background(), Tier4); err != nil {
		t.Fatalf("Reclaim(Tier4) error: %v", err)
	}
	if fake.createdContainers != 1 {
		t.Error("Tier4 should run one helper container to empty the volume")
	}
}
```

- [ ] **Step 2: 執行測試確認失敗**

Run: `go test ./internal/cachestore/ -run 'SharedVolumeStore|CacheVolumeStore' -v`
Expected: FAIL,`undefined: NewSharedVolumeStore`

- [ ] **Step 3: 寫最小實作**

兩個 store 都以 helper container 掛載 volume 來量測與回收,沿用 `internal/backend/docker.go` 的 `CleanupSharedVolumeStale` 模式(建立容器 → start → wait → 一律移除)。抽出共用函式:

```go
// runVolumeHelper runs a throwaway container with volumeName mounted at
// mountPath, executing script via sh -c, and returns its stdout. The
// container is always removed, and the call is bounded by a timeout so a
// wedged daemon cannot hang a sweep.
func runVolumeHelper(ctx context.Context, client backend.DockerAPI,
	image, volumeName, mountPath, script string) (string, error)
```

**實作方式:把 `internal/backend/docker.go` 的 `CleanupSharedVolumeStale`(約 530–600 行)整段複製過來改寫** —— 它已經是這個形狀:`ContainerCreate`(User root、Mounts 掛 named volume、Labels `managed-by=runner`)→ `defer ContainerRemove(force)` → `ContainerStart` → `ContainerWait` 三路 select(errCh / statusCh / ctx.Done),外層 `context.WithTimeout(ctx, 10*time.Minute)`。差異只有兩點:script 由參數傳入而非寫死,以及需要回傳 stdout(`Measure` 要讀 `du -sb` 的輸出),因此 `DockerAPI` 需再加 `ContainerLogs(ctx, id string, opts container.LogsOptions) (io.ReadCloser, error)`,並在 `mockDocker` / `fakeDockerAPI` 補上。

- `sharedVolumeStore.Kind()` 回傳 `KindScratch`;`Reclaim` 只在 `Tier3` 執行既有的 `find -mtime +N -delete` 腳本,**其他階層一律回傳 0**。
- `cacheVolumeStore.Kind()` 回傳 `KindCache`;`Reclaim` 只在 `Tier4` 執行 `find <path> -mindepth 1 -delete`,`Tier2` 回傳 0(cache volume 沒有以年齡淘汰的安全方式)。
- 兩者的 `Measure` 都跑 `du -sb <path>` 並解析第一個欄位。
- 兩者的 `Path()` 都回傳 `cfg.RootDir`(named volume 實際落在 Docker 資料根目錄下)。

- [ ] **Step 4: 執行測試確認通過**

Run: `go test ./internal/cachestore/ -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/cachestore/
git commit -m "feat(cachestore): add shared-volume (scratch) and cache-volume stores"
```

---

### Task 4: Buildx store 與 Tart store

**Files:**
- Create: `internal/cachestore/buildx.go`, `internal/cachestore/tart.go`
- Test: `internal/cachestore/buildx_test.go`, `internal/cachestore/tart_test.go`

**Interfaces:**
- Consumes: Task 1;既有 `backend.CleanupOrphanedBuildxBuilders`、`backend.PruneTartCache`、`backend.CommandRunner`
- Produces:
  - `func NewBuildxStore(client backend.DockerAPI, cfg BuildxConfig) CacheStore`
  - `type BuildxConfig struct { Enabled bool; MaxAge time.Duration; RootDir string }`
  - `func NewTartStore(runner backend.CommandRunner, cfg TartConfig) CacheStore`
  - `type TartConfig struct { Enabled bool; Home string; MaxAge time.Duration; BudgetGB int }`

- [ ] **Step 1: 寫失敗的測試**

```go
func TestBuildxStore_OnlyTier1(t *testing.T) {
	fake := &fakeDockerAPI{rootDir: "/var/lib/docker"}
	s := NewBuildxStore(fake, BuildxConfig{Enabled: true, MaxAge: 24 * time.Hour})
	if s.Kind() != KindGarbage {
		t.Errorf("Kind() = %v, want KindGarbage", s.Kind())
	}
	for _, tier := range []Tier{Tier2, Tier3, Tier4} {
		freed, err := s.Reclaim(context.Background(), tier)
		if err != nil || freed != 0 {
			t.Errorf("Reclaim(%v) should be a no-op, got freed=%d err=%v", tier, freed, err)
		}
	}
}

func TestTartStore_PathFollowsTartHome(t *testing.T) {
	s := NewTartStore(nil, TartConfig{Enabled: true, Home: "/Volumes/FrankData/tart"})
	if s.Path() != "/Volumes/FrankData/tart" {
		t.Errorf("Path() = %q, want the configured TART_HOME — usage counts "+
			"against that filesystem, not the system disk", s.Path())
	}
}
```

- [ ] **Step 2: 執行測試確認失敗**

Run: `go test ./internal/cachestore/ -run 'BuildxStore|TartStore' -v`
Expected: FAIL,`undefined: NewBuildxStore`

- [ ] **Step 3: 寫最小實作**

- `buildxStore`:`Kind() = KindGarbage`,`Reclaim(Tier1)` 呼叫既有的 `backend.CleanupOrphanedBuildxBuilders(ctx, client, cfg.MaxAge, logger)`。該函式目前不回傳釋出量,先回傳 0 並在註解說明(守門員以回收後重新 `statfs` 判斷是否達標,不依賴精確的 freed 值)。
- `tartStore`:`Kind() = KindCache`,`Reclaim(Tier2)` 呼叫既有的 `backend.PruneTartCache(ctx, cfg.Home, cfg.MaxAge, cfg.BudgetGB, logger)`;`Tier4` 回傳 0(Tart 映像不整體清空——重拉 140GB 代價過高,且 `tart prune` 已有 LRU)。
- `tartStore.Path()` 回傳 `cfg.Home`;`Home` 為空時回傳 `$HOME/.tart`。
- `Measure` 對 Tart 而言以 `du -sb <home>/cache` 取得。

- [ ] **Step 4: 執行測試確認通過**

Run: `go test ./internal/cachestore/ -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/cachestore/
git commit -m "feat(cachestore): add buildx and tart image cache stores"
```

---

### Task 5: 門檻解析與 per-filesystem 分組

**Files:**
- Create: `internal/diskguard/threshold.go`, `internal/diskguard/fs_unix.go`
- Test: `internal/diskguard/threshold_test.go`, `internal/diskguard/fs_unix_test.go`

**Interfaces:**
- Consumes: 無
- Produces:
  - `type Threshold struct { Percent float64; Bytes uint64 }`
  - `func ParseThreshold(s string) (Threshold, error)`
  - `func (t Threshold) BytesOf(totalBytes uint64) uint64`
  - `type FSStat struct { ID string; TotalBytes, FreeBytes uint64 }`
  - `func StatFor(path string) (FSStat, error)`
  - `func GroupByFilesystem(paths []string) (map[string][]string, error)`

- [ ] **Step 1: 寫失敗的測試**

```go
package diskguard

import "testing"

func TestParseThreshold(t *testing.T) {
	tests := []struct {
		in      string
		percent float64
		bytes   uint64
		wantErr bool
	}{
		{in: "10%", percent: 10},
		{in: "20GB", bytes: 20 * 1024 * 1024 * 1024},
		{in: "512MB", bytes: 512 * 1024 * 1024},
		{in: "0%", percent: 0},
		{in: "101%", wantErr: true},
		{in: "-5%", wantErr: true},
		{in: "abc", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseThreshold(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseThreshold(%q) expected error, got %+v", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseThreshold(%q) error: %v", tt.in, err)
			continue
		}
		if got.Percent != tt.percent || got.Bytes != tt.bytes {
			t.Errorf("ParseThreshold(%q) = %+v, want percent=%v bytes=%v",
				tt.in, got, tt.percent, tt.bytes)
		}
	}
}

func TestThresholdBytesOf(t *testing.T) {
	pct, _ := ParseThreshold("10%")
	if got := pct.BytesOf(1000); got != 100 {
		t.Errorf("10%% of 1000 = %d, want 100", got)
	}
	abs, _ := ParseThreshold("20GB")
	if got := abs.BytesOf(1000); got != 20*1024*1024*1024 {
		t.Errorf("absolute threshold must ignore total, got %d", got)
	}
}

func TestStatForGroupsSamePathsTogether(t *testing.T) {
	dir := t.TempDir()
	a, err := StatFor(dir)
	if err != nil {
		t.Fatalf("StatFor: %v", err)
	}
	b, err := StatFor(dir + "/.")
	if err != nil {
		t.Fatalf("StatFor: %v", err)
	}
	if a.ID != b.ID {
		t.Errorf("same filesystem must share an ID, got %q and %q", a.ID, b.ID)
	}
	if a.TotalBytes == 0 {
		t.Error("TotalBytes should be non-zero for a real filesystem")
	}
}
```

- [ ] **Step 2: 執行測試確認失敗**

Run: `go test ./internal/diskguard/ -v`
Expected: FAIL,`undefined: ParseThreshold`

- [ ] **Step 3: 寫最小實作**

`ParseThreshold` 接受結尾 `%` 的百分比(0–100,超出或負值為錯誤),否則以 `strconv` 解析數字加單位後綴(`B`/`KB`/`MB`/`GB`/`TB`,1024 進位,不分大小寫)。錯誤訊息須含合法格式範例,例如 `invalid threshold "abc": want a percentage like "10%" or a size like "20GB"`。

`StatFor` 以 `syscall.Statfs` 取得 `Bsize`、`Blocks`、`Bavail`,`TotalBytes = Blocks * Bsize`、`FreeBytes = Bavail * Bsize`。`ID` 以 `Fsid` 的兩個 int32 格式化為字串(darwin 與 linux 的欄位型別不同,以 `fmt.Sprintf("%v", st.Fsid)` 迴避型別差異)。`GroupByFilesystem` 對每個 path 呼叫 `StatFor`,以 `ID` 為 key 分組;單一 path 失敗時回傳錯誤讓呼叫端決定跳過。

- [ ] **Step 4: 執行測試確認通過**

Run: `go test ./internal/diskguard/ -count=1 -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/diskguard/
git commit -m "feat(diskguard): parse percentage/absolute thresholds and group paths by filesystem"
```

---

### Task 6: 階層回收迴圈

**Files:**
- Create: `internal/diskguard/guard.go`
- Test: `internal/diskguard/guard_test.go`

**Interfaces:**
- Consumes: Task 1 的 `cachestore`、Task 5 的 `Threshold`/`FSStat`
- Produces:
  - `type Config struct { Enabled bool; MinFree, TargetFree Threshold; MaxTier cachestore.Tier }`
  - `type Guard struct { ... }`
  - `func New(cfg Config, stores []cachestore.CacheStore, statFn func(string) (FSStat, error), logger *slog.Logger) *Guard`
  - `func (g *Guard) Sweep(ctx context.Context) error`
  - `func (g *Guard) NeedsReclaim() (bool, error)` — 便宜路徑,只做 statfs

- [ ] **Step 1: 寫失敗的測試**

```go
func TestGuard_StopsAsSoonAsTargetMet(t *testing.T) {
	// 第一次 statfs 低於 min-free,Tier1 回收後即達標 → 不應走到 Tier2。
	calls := 0
	statFn := func(string) (FSStat, error) {
		calls++
		if calls == 1 {
			return FSStat{ID: "fs1", TotalBytes: 100, FreeBytes: 5}, nil
		}
		return FSStat{ID: "fs1", TotalBytes: 100, FreeBytes: 30}, nil
	}
	s := &fakeStore{name: "garbage", kind: cachestore.KindGarbage, path: "/"}
	g := New(Config{
		Enabled: true,
		MinFree: mustThreshold("10%"), TargetFree: mustThreshold("20%"),
		MaxTier: cachestore.Tier3,
	}, []cachestore.CacheStore{s}, statFn, slog.New(slog.DiscardHandler))

	if err := g.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep error: %v", err)
	}
	if got := s.reclaimedTiers; len(got) != 1 || got[0] != cachestore.Tier1 {
		t.Errorf("reclaimed tiers = %v, want only Tier1 — the guard must stop once the target is met", got)
	}
}

func TestGuard_SkipsDisabledStore(t *testing.T) {
	statFn := func(string) (FSStat, error) {
		return FSStat{ID: "fs1", TotalBytes: 100, FreeBytes: 1}, nil
	}
	s := &fakeStore{name: "docker", kind: cachestore.KindGarbage, path: "/", disabled: true}
	g := New(Config{Enabled: true, MinFree: mustThreshold("10%"),
		TargetFree: mustThreshold("20%"), MaxTier: cachestore.Tier4},
		[]cachestore.CacheStore{s}, statFn, slog.New(slog.DiscardHandler))

	if err := g.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep error: %v", err)
	}
	if len(s.reclaimedTiers) != 0 {
		t.Error("a disabled store must not be reclaimed — the operator turned it off deliberately")
	}
}

func TestGuard_NeverExceedsMaxTier(t *testing.T) {
	statFn := func(string) (FSStat, error) {
		return FSStat{ID: "fs1", TotalBytes: 100, FreeBytes: 1} // 永遠不達標
	}
	s := &fakeStore{name: "cache", kind: cachestore.KindCache, path: "/"}
	g := New(Config{Enabled: true, MinFree: mustThreshold("10%"),
		TargetFree: mustThreshold("20%"), MaxTier: cachestore.Tier3},
		[]cachestore.CacheStore{s}, statFn, slog.New(slog.DiscardHandler))

	_ = g.Sweep(context.Background())
	for _, tier := range s.reclaimedTiers {
		if tier > cachestore.Tier3 {
			t.Fatalf("guard reclaimed at %v, above MaxTier=Tier3", tier)
		}
	}
}

func TestGuard_NeedsReclaimDoesNotMeasure(t *testing.T) {
	statFn := func(string) (FSStat, error) {
		return FSStat{ID: "fs1", TotalBytes: 100, FreeBytes: 5}, nil
	}
	s := &fakeStore{name: "cache", kind: cachestore.KindCache, path: "/"}
	g := New(Config{Enabled: true, MinFree: mustThreshold("10%"),
		TargetFree: mustThreshold("20%"), MaxTier: cachestore.Tier3},
		[]cachestore.CacheStore{s}, statFn, slog.New(slog.DiscardHandler))

	need, err := g.NeedsReclaim()
	if err != nil || !need {
		t.Fatalf("NeedsReclaim() = %v, %v; want true, nil", need, err)
	}
	if s.measureCalls != 0 {
		t.Error("NeedsReclaim must stay on the cheap path — Measure() may walk a whole volume")
	}
}
```

`fakeStore` 放在 `internal/diskguard/fake_test.go`,記錄 `reclaimedTiers []cachestore.Tier` 與 `measureCalls int`。`mustThreshold` 是測試輔助函式,解析失敗即 `panic`。

- [ ] **Step 2: 執行測試確認失敗**

Run: `go test ./internal/diskguard/ -run TestGuard -v`
Expected: FAIL,`undefined: New`

- [ ] **Step 3: 寫最小實作**

`Sweep` 流程:

1. `Enabled` 為 false 直接回傳 nil。
2. 以 `GroupByFilesystem` 將 stores 依 `Path()` 分組(單一 path 的 statfs 失敗只警告並跳過該 store)。
3. 對每個檔案系統:`statFn` 取得現況,`FreeBytes >= MinFree.BytesOf(Total)` 則跳過。
4. 由 `Tier1` 逐階到 `MaxTier`:對該階層有份的 store(`slices.Contains(cachestore.TiersFor(s.Kind()), tier)`)且 `s.Enabled()` 為 true 者呼叫 `Reclaim`。單一 store 失敗只警告並繼續。
5. **每階結束後重新 `statFn`**,達到 `TargetFree` 即停止(不依賴各 store 回報的 freed 精度)。
6. 走完 `MaxTier` 仍未達標:`logger.Warn` 說明差額;若有 store 因 `Enabled() == false` 被跳過,一併列出其名稱與「因為已停用而無法回收」。

`NeedsReclaim` 只對所有 store 的 path 做一次分組與 statfs,任一檔案系統低於 `MinFree` 即回傳 true。**不得呼叫 `Measure`**。

- [ ] **Step 4: 執行測試確認通過**

Run: `go test ./internal/diskguard/ -count=1 -v`
Expected: PASS

- [ ] **Step 5: 寫 budget 執行的失敗測試**

budget 屬於第 2 層,與磁碟壓力無關:即使磁碟充裕,超過 `budget` 且 `on-exceed = "wipe"` 的 store 仍要被清空。

```go
func TestGuard_EnforcesBudgetEvenWhenDiskHealthy(t *testing.T) {
	// 磁碟充裕 → tier ladder 完全不該啟動
	statFn := func(string) (FSStat, error) {
		return FSStat{ID: "fs1", TotalBytes: 100, FreeBytes: 90}, nil
	}
	over := &fakeStore{
		name: "ccache", kind: cachestore.KindCache, path: "/",
		size: 25 << 30, budget: 20 << 30, onExceed: "wipe",
	}
	under := &fakeStore{
		name: "gradle", kind: cachestore.KindCache, path: "/",
		size: 1 << 30, budget: 20 << 30, onExceed: "wipe",
	}
	g := New(Config{Enabled: true, MinFree: mustThreshold("10%"),
		TargetFree: mustThreshold("20%"), MaxTier: cachestore.Tier3},
		[]cachestore.CacheStore{over, under}, statFn, slog.New(slog.DiscardHandler))

	if err := g.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep error: %v", err)
	}
	if len(over.reclaimedTiers) != 1 || over.reclaimedTiers[0] != cachestore.Tier4 {
		t.Errorf("over-budget store should be wiped regardless of disk pressure, got %v",
			over.reclaimedTiers)
	}
	if len(under.reclaimedTiers) != 0 {
		t.Errorf("under-budget store must be left alone, got %v", under.reclaimedTiers)
	}
}

func TestGuard_BudgetWarnDoesNotTouchData(t *testing.T) {
	statFn := func(string) (FSStat, error) {
		return FSStat{ID: "fs1", TotalBytes: 100, FreeBytes: 90}, nil
	}
	over := &fakeStore{
		name: "ccache", kind: cachestore.KindCache, path: "/",
		size: 25 << 30, budget: 20 << 30, onExceed: "warn",
	}
	g := New(Config{Enabled: true, MinFree: mustThreshold("10%"),
		TargetFree: mustThreshold("20%"), MaxTier: cachestore.Tier3},
		[]cachestore.CacheStore{over}, statFn, slog.New(slog.DiscardHandler))

	_ = g.Sweep(context.Background())
	if len(over.reclaimedTiers) != 0 {
		t.Error(`on-exceed="warn" must never delete data — the tool's own LRU handles it`)
	}
}
```

`fakeStore` 需補 `size`、`budget`、`onExceed` 欄位,並讓 `Measure` 回傳 `size` 且遞增 `measureCalls`。`CacheStore` 介面加入 `Budget() (bytes uint64, onExceed string)`,無 budget 的 store 回傳 `(0, "")`。

- [ ] **Step 6: 執行測試確認失敗**

Run: `go test ./internal/diskguard/ -run TestGuard_.*Budget -v`
Expected: FAIL,`Budget undefined`

- [ ] **Step 7: 實作 budget 執行**

在 `Sweep` 的階層迴圈**之前**加入 budget 階段:對每個 `Enabled()` 且 `Budget()` 回傳非零 bytes 的 store 呼叫 `Measure`;超過時,`onExceed == "wipe"` 則 `Reclaim(ctx, Tier4)`,`"warn"`(含空字串)則只 `logger.Warn` 說明用量、budget 與「細粒度淘汰由工具自身負責」。此階段**不受 `MaxTier` 限制**——它是 store 自己的保留策略,不是磁碟壓力下的緊急手段。

`NeedsReclaim` 不執行此階段(它會呼叫 `Measure`,必須留在昂貴路徑)。

- [ ] **Step 8: 執行測試確認通過**

Run: `go test ./internal/diskguard/ -count=1 -v`
Expected: PASS

- [ ] **Step 9: Commit**

```bash
git add internal/diskguard/ internal/cachestore/
git commit -m "feat(diskguard): reclaim by tier under pressure and enforce per-store budgets"
```

---

### Task 7: `[disk]` 設定與週期守門員接線

**Files:**
- Modify: `internal/config/config.go`, `internal/config/defaults.go`, `cmd/runner/main.go`
- Test: `internal/config/config_test.go`, `internal/config/load_test.go`

**Interfaces:**
- Consumes: Task 5 的 `ParseThreshold`、Task 6 的 `Guard`
- Produces: `type DiskConfig struct { Guard *bool; MinFree, TargetFree string; Interval time.Duration; MaxTier int }`、`Config.Disk DiskConfig`、`func (c *Config) IsDiskGuardEnabled() bool`

- [ ] **Step 1: 寫失敗的測試**

```go
func TestDiskConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		disk    DiskConfig
		wantErr string
	}{
		{name: "defaults are valid", disk: DiskConfig{}},
		{name: "min >= target rejected",
			disk:    DiskConfig{MinFree: "30%", TargetFree: "20%"},
			wantErr: "min-free must be less than target-free"},
		{name: "equal rejected",
			disk:    DiskConfig{MinFree: "20%", TargetFree: "20%"},
			wantErr: "min-free must be less than target-free"},
		{name: "bad threshold rejected",
			disk:    DiskConfig{MinFree: "lots"},
			wantErr: "invalid threshold"},
		{name: "max-tier out of range",
			disk:    DiskConfig{MaxTier: 9},
			wantErr: "max-tier must be between 1 and 4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.disk.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() unexpected error: %v", err)
				}
				return
			}
			if err == nil || !contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}
```

- [ ] **Step 2: 執行測試確認失敗**

Run: `go test ./internal/config/ -run TestDiskConfig -v`
Expected: FAIL,`disk.Validate undefined`

- [ ] **Step 3: 寫最小實作**

`internal/config/defaults.go` 新增:

```go
	// DefaultDiskGuard enables the disk guard by default. Unlike prune, the
	// guard only acts under disk pressure and never overrides a store the
	// operator disabled, so leaving it on is safe.
	DefaultDiskGuard = true
	// DefaultDiskMinFree is the free-space level that triggers reclamation.
	DefaultDiskMinFree = "10%"
	// DefaultDiskTargetFree is the level reclamation aims to restore.
	DefaultDiskTargetFree = "20%"
	// DefaultDiskGuardInterval is the period between guard sweeps.
	DefaultDiskGuardInterval = time.Hour
	// DefaultDiskMaxTier stops short of wiping cache volumes — that makes
	// the next build cold, so it requires an explicit opt-in.
	DefaultDiskMaxTier = 3
```

`Config` 加 `Disk DiskConfig \`mapstructure:"disk"\``;`DiskConfig.Validate()` 檢查門檻可解析、`MinFree < TargetFree`(以百分比或位元組各自比較,混用單位時以「無法比較」為錯誤並要求兩者同型)、`MaxTier` 介於 1–4。`Config.Validate()` 需呼叫它,使 `runner validate` 能攔下。

`cmd/runner/main.go` 在既有四個 sweeper 旁新增 `startDiskGuard(ctx, stores, cfg, logger)`,結構完全比照 `startDockerPrune`:nil guard、Info 記錄生效設定、先掃一次再進 ticker、錯誤 Warn。store 集合由一個新的 `buildCacheStores(cfg, dockerClients, logger) []cachestore.CacheStore` 組出。

**同時消除回收邏輯的重複(必做)。** 現有四個 sweeper 各自直接呼叫 `internal/backend` 的回收函式,而 `internal/cachestore` 的 store 現在也實作了同一套邏輯——兩份都會刪使用者資料,長期並行會漂移。spec 要求兩者「透過同一組 `CacheStore` 實作共用回收邏輯」,因此本 task 必須:

- `startSharedVolumeCleanup` 改為呼叫對應 store 的 `Reclaim(ctx, cachestore.Tier3)`,不再呼叫 `backend.CleanupSharedVolumeStale`
- `startDockerPrune` 改為呼叫 garbage store 的 `Reclaim(Tier1)` 與 build cache store 的 `Reclaim(Tier2)`,不再呼叫 `backend.PruneDockerRuntime`
- `startBuildxCleanup` 改為呼叫 buildx store 的 `Reclaim(Tier1)`
- `startTartCacheCleanup` 改為呼叫 tart store 的 `Reclaim(Tier2)`
- 上述 `internal/backend` 中已無呼叫者的回收函式一併刪除,連同其測試(測試改由 `internal/cachestore` 覆蓋)。若某函式仍有其他呼叫者則保留並在報告中說明。

sweeper 保留(它們是各 store 的例行保留策略,與守門員的觸發條件不同),改變的只是它們的實作路徑。

- [ ] **Step 4: 執行測試確認通過**

Run: `go build ./... && go test ./internal/config/ ./cmd/runner/ -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/config/ cmd/runner/
git commit -m "feat(config): add [disk] guard settings and wire the periodic sweep"
```

---

### Task 8: `runner cache` 指令與 health 的 disk 區塊

**Files:**
- Create: `cmd/runner/cmd_cache.go`
- Modify: `internal/health/health.go`, `cmd/runner/main.go`
- Test: `cmd/runner/cmd_cache_test.go`, `internal/health/health_test.go`

**Interfaces:**
- Consumes: Task 1–7
- Produces: `runner cache [--json]` 子指令;`health.DiskStatus{ Filesystem string; FreePercent float64; FreeBytes, TotalBytes uint64 }` 與 `Status.Disk []DiskStatus`

- [ ] **Step 1: 寫失敗的測試**

```go
func TestFormatCacheTable(t *testing.T) {
	rows := []cacheRow{
		{Store: "ccache", Filesystem: "/", SizeBytes: 19 * 1024 * 1024 * 1024,
			FSFreePercent: 38, Policy: "budget=20GB on-exceed=warn"},
		{Store: "runner-shared", Filesystem: "/", SizeBytes: 3 * 1024 * 1024 * 1024,
			FSFreePercent: 38, Policy: "max-age=72h"},
	}
	out := formatCacheTable(rows)
	for _, want := range []string{"ccache", "19.0 GiB", "38%", "budget=20GB", "runner-shared"} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q:\n%s", want, out)
		}
	}
}

func TestCacheRowsJSONIncludesUnmeasurableStores(t *testing.T) {
	rows := []cacheRow{
		{Store: "ok", SizeBytes: 100},
		{Store: "broken", MeasureError: "permission denied"},
	}
	data, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), "broken") || !strings.Contains(string(data), "permission denied") {
		t.Error("a store that failed to measure must still appear, with its error")
	}
}
```

- [ ] **Step 2: 執行測試確認失敗**

Run: `go test ./cmd/runner/ -run TestFormatCacheTable -v`
Expected: FAIL,`undefined: cacheRow`

- [ ] **Step 3: 寫最小實作**

`cmd_cache.go` 建立 store 集合(重用 Task 7 的 `buildCacheStores`),對每個 store 呼叫 `Measure` 與 `StatFor(Path())`,量測失敗者填 `MeasureError` 但仍列出。`formatCacheTable` 以 `text/tabwriter` 對齊,大小以既有的 `backend.FormatBytes` 呈現。`--json` 直接輸出 `[]cacheRow`。

`internal/health/health.go` 的 `Status` 加 `Disk []DiskStatus \`json:"disk,omitempty"\``,由 `HealthServer` 持有一個 `func() []DiskStatus` 提供者(由 main 注入,內容只做 statfs,不做 Measure)。

- [ ] **Step 4: 執行測試確認通過**

Run: `go test ./cmd/runner/ ./internal/health/ -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/runner/ internal/health/
git commit -m "feat(cache): add runner cache command and disk section in healthz"
```

---

### Task 9: 啟動 runner 前的便宜檢查

**Files:**
- Modify: `internal/scaler/scaler.go:126`, `cmd/runner/main.go`
- Test: `internal/scaler/scaler_test.go`

**Interfaces:**
- Consumes: Task 6 的 `Guard`
- Produces: `type DiskChecker interface { NeedsReclaim() (bool, error); Sweep(ctx context.Context) error }`;`scaler.NewScaler` 新增選用的 `WithDiskChecker(c DiskChecker)` option

- [ ] **Step 1: 寫失敗的測試**

```go
func TestStartRunner_ReclaimsWhenDiskLow(t *testing.T) {
	chk := &fakeChecker{needs: true}
	s := NewScaler(1, 0, 1, &fakeBackend{}, &fakeScalesetClient{},
		slog.New(slog.DiscardHandler), WithDiskChecker(chk))

	if _, err := s.startRunner(context.Background()); err != nil {
		t.Fatalf("startRunner error: %v", err)
	}
	if chk.sweeps != 1 {
		t.Errorf("expected one reclaim before starting the runner, got %d", chk.sweeps)
	}
}

func TestStartRunner_SkipsReclaimWhenDiskFine(t *testing.T) {
	chk := &fakeChecker{needs: false}
	s := NewScaler(1, 0, 1, &fakeBackend{}, &fakeScalesetClient{},
		slog.New(slog.DiscardHandler), WithDiskChecker(chk))

	if _, err := s.startRunner(context.Background()); err != nil {
		t.Fatalf("startRunner error: %v", err)
	}
	if chk.sweeps != 0 {
		t.Errorf("healthy disk must not pay for a sweep, got %d", chk.sweeps)
	}
}

func TestStartRunner_ProceedsWhenReclaimFails(t *testing.T) {
	chk := &fakeChecker{needs: true, sweepErr: errors.New("daemon down")}
	s := NewScaler(1, 0, 1, &fakeBackend{}, &fakeScalesetClient{},
		slog.New(slog.DiscardHandler), WithDiskChecker(chk))

	if _, err := s.startRunner(context.Background()); err != nil {
		t.Fatalf("a failed reclaim must not block the job: %v", err)
	}
}
```

- [ ] **Step 2: 執行測試確認失敗**

Run: `go test ./internal/scaler/ -run TestStartRunner_ -v`
Expected: FAIL,`undefined: WithDiskChecker`

- [ ] **Step 3: 寫最小實作**

`startRunner` 在 `GenerateJitRunnerConfig` 之前插入:

```go
	// Cheap statfs before committing to a job: a runner that fills the disk
	// mid-build can take the whole host down. Reclaim failures are logged,
	// never fatal — refusing to serve jobs is worse than a full disk warning.
	if s.diskChecker != nil {
		if need, err := s.diskChecker.NeedsReclaim(); err != nil {
			s.logger.Warn("Disk check failed, starting runner anyway", slog.Any("error", err))
		} else if need {
			s.logger.Info("Free space below threshold, reclaiming before starting runner")
			if err := s.diskChecker.Sweep(ctx); err != nil {
				s.logger.Warn("Reclaim before runner start failed", slog.Any("error", err))
			}
		}
	}
```

`NewScaler` 改為可變參數 option 形式,既有呼叫端不需修改。

- [ ] **Step 4: 執行測試確認通過**

Run: `go test ./internal/scaler/ ./cmd/runner/ -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/scaler/ cmd/runner/
git commit -m "feat(scaler): reclaim disk before starting a runner when free space is low"
```

---

### Task 10: shared volume 退出時不再刪除

**Files:**
- Modify: `cmd/runner/main.go`(約 434–455 行)、`internal/backend/docker.go`(`CleanupSharedDocker`)
- Test: `internal/backend/docker_test.go`

**Interfaces:**
- Consumes: 無
- Produces: `CleanupSharedDocker(ctx, client, pruneDaemon bool, logger)` — 移除 `volumeNames []string` 參數

- [ ] **Step 1: 寫失敗的測試**

```go
func TestCleanupSharedDocker_NeverRemovesVolumes(t *testing.T) {
	md := &mockDocker{}

	CleanupSharedDocker(context.Background(), md, false, slog.New(slog.DiscardHandler))

	if len(md.volumesRemoved) != 0 {
		t.Errorf("exit cleanup removed volumes %v — the shared volume holds "+
			"handoff data for in-flight runs and must survive a restart",
			md.volumesRemoved)
	}
}
```

- [ ] **Step 2: 執行測試確認失敗**

Run: `go test ./internal/backend/ -run TestCleanupSharedDocker_NeverRemoves -v`
Expected: FAIL,編譯錯誤(參數個數不符)

- [ ] **Step 3: 寫最小實作**

從 `CleanupSharedDocker` 移除 volume 移除迴圈與 `volumeNames` 參數,更新 doc comment 說明原因:

```go
// CleanupSharedDocker prunes dangling images at exit. It deliberately does
// NOT remove the shared volume: that volume carries handoff data between
// jobs of one workflow run (a build job writes, a later job reads), so
// deleting it on restart breaks runs that are still in flight — and the
// failure surfaces in the workflow, not here. Reclamation is left to the
// max-age sweep and the disk guard, matching every other store's lifecycle.
```

同步刪除 `cmd/runner/main.go` 中計算 `volumeNames` 的區塊,並更新該處註解。

- [ ] **Step 4: 執行測試確認通過**

Run: `go build ./... && go test ./... -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/backend/ cmd/runner/
git commit -m "fix(docker): stop deleting the shared volume at exit

It carries handoff data between jobs of one workflow run, so a restart
mid-run broke the run — and the error surfaced in the workflow, not in
runner. Reclamation is now the max-age sweep's and the disk guard's job,
matching every other store's lifecycle."
```

---

### Task 11: 詞彙 alias 與 cache volume 完整式

**Files:**
- Modify: `internal/config/load.go`, `internal/config/config.go`, `config.example.toml`, `README.md`
- Test: `internal/config/load_test.go`

**Interfaces:**
- Consumes: Task 1–10
- Produces: alias 映射 `cache-space-budget`→`cache-budget`、`shared-volume-ttl`→`shared-volume-max-age`;`[[docker.cache]]` 完整式解析

- [ ] **Step 1: 寫失敗的測試**

```go
func TestLoad_LegacyCacheKeysStillWork(t *testing.T) {
	v := viper.New()
	v.SetConfigType("toml")
	if err := v.ReadConfig(strings.NewReader(`
url = "https://github.com/org"
name = "runners"
token = "ghp_x"
[docker]
shared-volume = "/shared"
shared-volume-ttl = "72h"
[tart]
cache-space-budget = 150
`)); err != nil {
		t.Fatalf("read config: %v", err)
	}
	cfg, err := Load(v)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("legacy keys must not warn, got %q", cfg.Warnings)
	}
	ss := cfg.ResolveScaleSets()[0]
	if ss.Docker.SharedVolumeMaxAge != 72*time.Hour {
		t.Errorf("shared-volume-ttl should map to shared-volume-max-age, got %v",
			ss.Docker.SharedVolumeMaxAge)
	}
	if ss.Tart.CacheBudgetGB != 150 {
		t.Errorf("cache-space-budget should map to cache-budget, got %d", ss.Tart.CacheBudgetGB)
	}
}

func TestLoad_NewKeyWinsOverLegacyWithWarning(t *testing.T) {
	v := viper.New()
	v.SetConfigType("toml")
	_ = v.ReadConfig(strings.NewReader(`
url = "https://github.com/org"
name = "runners"
token = "ghp_x"
[docker]
shared-volume = "/shared"
shared-volume-ttl = "24h"
shared-volume-max-age = "72h"
`))
	cfg, err := Load(v)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	ss := cfg.ResolveScaleSets()[0]
	if ss.Docker.SharedVolumeMaxAge != 72*time.Hour {
		t.Errorf("new key must win, got %v", ss.Docker.SharedVolumeMaxAge)
	}
	if len(cfg.Warnings) == 0 {
		t.Error("setting both the legacy and new key should warn")
	}
}
```

- [ ] **Step 2: 執行測試確認失敗**

Run: `go test ./internal/config/ -run TestLoad_Legacy -v`
Expected: FAIL,`ss.Docker.SharedVolumeMaxAge undefined`

- [ ] **Step 3: 寫最小實作**

`DockerConfig` 新增 `SharedVolumeMaxAge time.Duration \`mapstructure:"shared-volume-max-age"\``,保留舊欄位為 deprecated;`TartConfig` 新增 `CacheBudgetGB int \`mapstructure:"cache-budget"\``。在 `load.go` 的 map 合併之後、decode 之前插入 `applyAliases(settings map[string]any) []string`:對每組 (舊 key, 新 key),舊 key 存在且新 key 不存在時搬移;兩者皆存在時保留新 key 並產生警告。alias 的舊 key 在搬移後從 map 刪除,故不會觸發 unknown-key 警告。

`[[docker.cache]]` 完整式:新增 `type CacheVolumeSpec struct { Name, Path, Budget, OnExceed string }`、`DockerConfig.Cache []CacheVolumeSpec \`mapstructure:"cache"\``。`ParseCacheVolumes` 合併簡式與完整式,同名重複時完整式勝出;`OnExceed` 僅接受 `""`(視為 `warn`)、`warn`、`wipe`,其餘為驗證錯誤。

文件同步更新:`config.example.toml` 加入 `[disk]` 與 `[[docker.cache]]` 範例;`README.md` 的 Caching 章節改寫為三層架構(守門員 / 各 store 保留策略 / 可見性)並加入分類表。

- [ ] **Step 4: 執行測試確認通過**

Run: `go build ./... && go test ./... -count=1 && go vet ./... && gofmt -l . && golangci-lint run`
Expected: 全部通過,`gofmt -l .` 無輸出,lint 0 issues

- [ ] **Step 5: Commit**

```bash
git add internal/config/ config.example.toml README.md
git commit -m "feat(config): unify cache vocabulary with aliases and add cache volume long form"
```

---

## 驗收

全部 task 完成後,以實際設定驗證相容性:

```bash
# 三台主機的現行設定必須零警告載入
go run ./cmd/runner validate --config <各主機 config 副本>
```

並在 leg host 上確認 `runner cache` 能列出各 store 用量、`/healthz` 含 `disk` 區塊。
