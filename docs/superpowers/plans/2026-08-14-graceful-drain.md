# 優雅 drain 關閉與啟動時孤兒對帳 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 讓停止 runner 不再中斷進行中的 job,並讓任何非優雅結束留下的孤兒容器/VM 在下次啟動時自動清除。

**Architecture:** 兩條互補路徑。優雅路徑:`Scaler` 進入 drain 狀態,停止擴充、立即移除 idle runner、等 busy 自然完成。非優雅路徑(SIGKILL/OOM/斷電):啟動時依 `managed-by=runner` 標籤與名稱樣式找出孤兒並移除,安全性建立在既有的單一實例鎖上。服務範本補上停止逾時,使服務管理器不會在 drain 完成前開槍。

**Tech Stack:** Go 1.26;`github.com/moby/moby/client`;既有的 `backend.DockerAPI` / `CommandRunner` 抽象與其 mock。

**Spec:** `docs/superpowers/specs/2026-08-14-graceful-drain-design.md`

**Implementation status:** Complete. The checklists below are retained as the original task-by-task execution record.

## Global Constraints

- **`SIGINT`(Ctrl-C)一律立即關閉,不進入 drain。** 互動式執行按下 Ctrl-C 後最長等兩小時是不可接受的意外;此行為與現況相同,不得改變。
- **drain 期間 listener 必須繼續運作。** job 完成是透過 listener 訊息得知(`HandleJobCompleted`),先關 listener 再等 busy 歸零會永遠等不到。
- **啟動對帳只處理容器與 VM,絕不觸碰任何 volume。** shared volume 與 cache volume 依設計長期存在,依「存在即孤兒」判定會刪掉進行中 workflow 的交接資料。此為防資料遺失的關鍵不變量,須有專屬測試。
- **啟動對帳的安全性依賴單一實例鎖**(`internal/lock`)。程式碼註解須寫明此依賴,以免日後放寬單一實例限制時漏改。
- 既有設定必須維持零警告載入;未設 `drain-timeout` 者取得預設值。
- 註解一律英文;conventional commits。
- 每個 task 結束前必須全綠:`go build ./... && go test ./... -count=1 && go vet ./... && gofmt -l .`(需無輸出)`&& golangci-lint run`(0 issues)。涉及併發的 task 另跑 `-race`。

---

### Task 1: 共用的孤兒查詢與移除

**Files:**
- Create: `cmd/runner/orphans.go`
- Create: `cmd/runner/orphans_test.go`
- Modify: `cmd/runner/cmd_doctor.go`(改用共用函式)

**Interfaces:**
- Consumes: 既有 `backend.DockerAPI`;`cmd_doctor.go:45` 的 `runnerNamePattern`
- Produces:
  - `func findOrphanContainers(ctx context.Context, client backend.DockerAPI) ([]OrphanContainer, error)`
  - `type OrphanContainer struct { ID, Name, Status string }`
  - `func removeOrphanContainers(ctx context.Context, client backend.DockerAPI, orphans []OrphanContainer, logger *slog.Logger) (removed int)`

備註:`cmd_doctor.go:241` 的 `checkDockerContainers` 目前同時做「列出」與「依 `fix` 決定要不要移除」。把查詢與移除拆成上述兩個函式,`doctor` 與啟動對帳各自組合使用。

- [ ] **Step 1: 寫失敗的測試**

```go
package main

import (
	"context"
	"log/slog"
	"testing"
)

func TestFindOrphanContainers_MatchesLabelAndNamePattern(t *testing.T) {
	md := &orphanFake{containers: []fakeContainer{
		{id: "a1", names: []string{"/runner-deadbeef"}, labels: map[string]string{"managed-by": "runner"}},
		{id: "b2", names: []string{"/runner-0011aabb"}, labels: nil},           // 名稱樣式
		{id: "c3", names: []string{"/postgres"}, labels: map[string]string{"app": "db"}},
		{id: "d4", names: []string{"/buildx_buildkit_x0"}, labels: nil},
	}}

	got, err := findOrphanContainers(context.Background(), md)
	if err != nil {
		t.Fatalf("findOrphanContainers: %v", err)
	}
	ids := map[string]bool{}
	for _, o := range got {
		ids[o.ID] = true
	}
	if !ids["a1"] || !ids["b2"] {
		t.Errorf("expected runner-owned containers a1 and b2, got %v", ids)
	}
	if ids["c3"] || ids["d4"] {
		t.Errorf("must not claim containers runner did not create, got %v", ids)
	}
}

func TestRemoveOrphanContainers_ContinuesAfterFailure(t *testing.T) {
	md := &orphanFake{removeErrOn: "a1"}
	orphans := []OrphanContainer{{ID: "a1", Name: "runner-1"}, {ID: "b2", Name: "runner-2"}}

	removed := removeOrphanContainers(context.Background(), md, orphans, slog.New(slog.DiscardHandler))

	if removed != 1 {
		t.Errorf("removed = %d, want 1 (b2 succeeds even though a1 failed)", removed)
	}
	if len(md.removed) != 2 {
		t.Errorf("both removals should be attempted, got attempts on %v", md.removed)
	}
}

// TestRemoveOrphanContainers_NeverTouchesVolumes pins the invariant that
// protects in-flight workflow handoff data: reconciliation removes
// containers only. The shared volume is designed to outlive the process.
func TestRemoveOrphanContainers_NeverTouchesVolumes(t *testing.T) {
	md := &orphanFake{}
	removeOrphanContainers(context.Background(), md,
		[]OrphanContainer{{ID: "a1", Name: "runner-1"}}, slog.New(slog.DiscardHandler))

	if len(md.volumesRemoved) != 0 {
		t.Errorf("reconciliation removed volumes %v — the shared volume carries "+
			"handoff data between jobs of one workflow run", md.volumesRemoved)
	}
}
```

`orphanFake` 放在 `cmd/runner/orphans_test.go`,實作 `backend.DockerAPI` 全部方法。`ContainerList` 由 `containers` 欄位驅動;`ContainerRemove` 記錄到 `removed []string`,若 ID 等於 `removeErrOn` 則回傳錯誤;`VolumeRemove` 記錄到 `volumesRemoved []string`。其餘方法回零值。

- [ ] **Step 2: 執行測試確認失敗**

Run: `go test ./cmd/runner/ -run TestFindOrphan -v`
Expected: FAIL,`undefined: findOrphanContainers`

- [ ] **Step 3: 寫最小實作**

```go
// OrphanContainer is a container runner created that no live process is
// tracking — left behind when a previous run was killed without a chance to
// clean up (SIGKILL, OOM, power loss, panic).
type OrphanContainer struct {
	ID     string
	Name   string
	Status string
}

// findOrphanContainers lists containers this tool created, identified by the
// managed-by label it sets on every runner container, plus a name-pattern
// fallback for containers created before the label existed.
//
// SAFETY: every container this returns is assumed to belong to a previous
// run of this process. That holds only because internal/lock guarantees a
// single runner per host — without it, this would claim containers another
// live runner is actively using. If that guarantee is ever relaxed, this
// function and its callers must be revisited.
func findOrphanContainers(ctx context.Context, client backend.DockerAPI) ([]OrphanContainer, error)

// removeOrphanContainers force-removes each orphan and returns how many were
// removed. A failure on one container is logged and does not stop the rest —
// a leftover container is a smaller problem than refusing to start.
func removeOrphanContainers(ctx context.Context, client backend.DockerAPI, orphans []OrphanContainer, logger *slog.Logger) int
```

`findOrphanContainers` 以 `client.ContainerList(ctx, dockerclient.ContainerListOptions{All: true})` 取得全部容器(含已停止),判定條件與 `cmd_doctor.go:250-258` 現行邏輯相同:`Labels["managed-by"] == "runner"`,或任一名稱符合 `runnerNamePattern`。

改寫 `cmd_doctor.go` 的 `checkDockerContainers`,改為呼叫這兩個函式,保留它現有的輸出格式與 `fix` 旗標語意。

- [ ] **Step 4: 執行測試確認通過**

Run: `go test ./cmd/runner/ -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/runner/orphans.go cmd/runner/orphans_test.go cmd/runner/cmd_doctor.go
git commit -m "refactor(doctor): extract orphan container query and removal"
```

---

### Task 2: `doctor --fix` 不再把 shared volume 當孤兒

**Files:**
- Modify: `cmd/runner/cmd_doctor.go`(volume 檢查段落)
- Test: `cmd/runner/cmd_doctor_test.go`

**Interfaces:**
- Consumes: Task 1
- Produces: 無新 API

背景:`doctor --fix` 以「shared volume 存在」判定它是孤兒。這在 runner 退出時會刪除該 volume 的年代是對的;v0.5.0 起它**本來就該長期存在**,於是 `doctor --fix` 會在升級停機的空檔刪掉進行中 workflow 的交接資料。

注意簽名:`checkDockerVolume(ctx context.Context, client volumeAPI, fix bool) (int, error)`(`cmd_doctor.go:315`)收的是該檔自訂的 `volumeAPI` 介面,不是 `backend.DockerAPI`。測試用的 `orphanFake` 需同時滿足兩者(把 `volumeAPI` 需要的方法一併實作即可,兩個介面的 volume 相關方法簽名相同)。

- [ ] **Step 1: 寫失敗的測試**

```go
func TestCheckDockerVolume_SharedVolumeIsNotOrphanedByExistence(t *testing.T) {
	md := &orphanFake{volumes: []string{"runner-shared"}}

	// fix=true must still not remove it: the volume is designed to outlive
	// the process, so its mere existence says nothing about whether it is
	// still needed.
	if _, err := checkDockerVolume(context.Background(), md, true); err != nil {
		t.Fatalf("checkDockerVolume: %v", err)
	}
	if len(md.volumesRemoved) != 0 {
		t.Errorf("doctor --fix removed %v; the shared volume carries handoff "+
			"data between jobs of one workflow run", md.volumesRemoved)
	}
}
```

- [ ] **Step 2: 執行測試確認失敗**

Run: `go test ./cmd/runner/ -run TestCheckDockerVolume_SharedVolumeIsNot -v`
Expected: FAIL,volume 被移除

- [ ] **Step 3: 寫最小實作**

移除 volume 的自動刪除。改為**報告而不刪除**:列出 volume 名稱與大小,並說明它依設計長期存在、若確定不再需要可用 `docker volume rm` 手動移除。`fix` 旗標對此項不再有作用。

doc comment 須說明為何不刪:volume 的存在本身不帶任何「是否仍被需要」的資訊,而誤刪的後果會出現在使用者的 workflow 而非這裡。

- [ ] **Step 4: 執行測試確認通過**

Run: `go test ./cmd/runner/ -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/runner/cmd_doctor.go cmd/runner/cmd_doctor_test.go
git commit -m "fix(doctor): stop treating the shared volume as orphaned by existence

Since runner stopped deleting it at exit, the volume is meant to outlive
the process. doctor --fix would delete an in-flight run's handoff data
during an upgrade window, and the failure would surface in the user's
workflow rather than here."
```

---

### Task 3: 啟動時孤兒對帳接線

**Files:**
- Modify: `cmd/runner/main.go`(`run()`,在 scale set 啟動之前)
- Create: `cmd/runner/orphans_tart.go`
- Test: `cmd/runner/orphans_test.go`(新增 Tart 案例)

**Interfaces:**
- Consumes: Task 1 的 `findOrphanContainers` / `removeOrphanContainers`
- Produces:
  - `func reconcileOrphans(ctx context.Context, dockerClients map[string]*dockerclient.Client, tartHomes []string, logger *slog.Logger)`
  - `func findOrphanTartVMs(ctx context.Context, runner backend.CommandRunner) ([]string, error)`

- [ ] **Step 1: 寫失敗的測試**

```go
func TestFindOrphanTartVMs_MatchesRunnerAndPoolNames(t *testing.T) {
	mc := &mockCommandRunner{}
	mc.setResult("tart list --format json", `[
	  {"Name":"runner-deadbeef","Source":"local"},
	  {"Name":"pool-0-1778066579017","Source":"local"},
	  {"Name":"macos-tahoe-base","Source":"local"},
	  {"Name":"ghcr.io/cirruslabs/macos-tahoe-xcode:latest","Source":"OCI"}
	]`)

	got, err := findOrphanTartVMs(context.Background(), mc)
	if err != nil {
		t.Fatalf("findOrphanTartVMs: %v", err)
	}
	want := map[string]bool{"runner-deadbeef": true, "pool-0-1778066579017": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want exactly %v — base images and user VMs must not be claimed", got, want)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("claimed %q, which runner did not create", n)
		}
	}
}
```

`mockCommandRunner` 已存在於 `internal/backend/tart_test.go`;本 task 需在 `cmd/runner` 建立等價的測試替身(欄位名沿用:`setResult(key, out string)`)。

- [ ] **Step 2: 執行測試確認失敗**

Run: `go test ./cmd/runner/ -run TestFindOrphanTartVMs -v`
Expected: FAIL,`undefined: findOrphanTartVMs`

- [ ] **Step 3: 寫最小實作**

`findOrphanTartVMs` 以 `tart list --format json` 取得清單,只認 `Source == "local"` 且名稱符合以下兩種樣式者:

- `runner-<8 位十六進位>`(`internal/scaler/scaler.go:157` 的 `fmt.Sprintf("runner-%s", uuid.NewString()[:8])`)
- `pool-<數字>-<數字>`(`internal/backend/tart.go:194` 的 `fmt.Sprintf("pool-%d-%d", slot, time.Now().UnixMilli())`)

移除方式為先 `tart stop <name>`(失敗只記 Debug,VM 可能本來就沒在跑)再 `tart delete <name>`。

`reconcileOrphans` 在 `run()` 中呼叫,位置在**取得單一實例鎖之後、啟動任何 scale set 之前**。對每個 Docker socket 與每個 TART_HOME 各做一次。結果以 Info 記錄(移除數量與名稱);無孤兒時記 Debug。任何失敗只警告,不阻擋啟動。

- [ ] **Step 4: 執行測試確認通過**

Run: `go build ./... && go test ./cmd/runner/ -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/runner/
git commit -m "feat(runner): reconcile orphaned containers and VMs at startup

A process killed without a chance to clean up (SIGKILL, OOM, power loss)
leaves its runner containers and VMs behind, and nothing reclaimed them
until an operator ran doctor --fix. Safe because internal/lock guarantees
a single runner per host."
```

---

### Task 4: `Scaler.Drain`

**Files:**
- Modify: `internal/scaler/scaler.go`
- Test: `internal/scaler/scaler_test.go`

**Interfaces:**
- Consumes: 既有 `runnerState`、`backend.RunnerBackend`
- Produces:
  - `func (s *Scaler) Drain(ctx context.Context) error`
  - `func (s *Scaler) IsDraining() bool`

- [ ] **Step 1: 寫失敗的測試**

```go
func TestDrain_RemovesIdleKeepsBusy(t *testing.T) {
	mb := &mockBackend{}
	s := NewScaler(1, 0, 5, mb, &mockScaleset{}, slog.New(slog.DiscardHandler))
	s.runners.addIdle("runner-idle", "res-idle")
	s.runners.addIdle("runner-busy", "res-busy")
	s.runners.markBusy("runner-busy")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = s.Drain(ctx) // busy never finishes; deadline ends the drain

	if !contains(mb.removed, "res-idle") {
		t.Error("idle runner should be removed immediately — it holds no work")
	}
	if contains(mb.removed, "res-busy") {
		t.Error("busy runner was removed during drain; its job is still running")
	}
}

func TestDrain_ReturnsWhenBusyReachesZero(t *testing.T) {
	mb := &mockBackend{}
	s := NewScaler(1, 0, 5, mb, &mockScaleset{}, slog.New(slog.DiscardHandler))
	s.runners.addIdle("runner-busy", "res-busy")
	s.runners.markBusy("runner-busy")

	go func() {
		time.Sleep(50 * time.Millisecond)
		s.runners.markDone("runner-busy")
	}()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Drain(ctx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("Drain should return as soon as the last busy runner finishes")
	}
}

func TestDrain_StopsScalingUp(t *testing.T) {
	mb := &mockBackend{}
	s := NewScaler(1, 0, 5, mb, &mockScaleset{}, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	go func() { _ = s.Drain(ctx) }()
	time.Sleep(20 * time.Millisecond)

	got, err := s.HandleDesiredRunnerCount(context.Background(), 3)
	if err != nil {
		t.Fatalf("HandleDesiredRunnerCount: %v", err)
	}
	if got != 0 || len(mb.started) != 0 {
		t.Errorf("draining scaler started %d runners (count=%d); it must take no new work",
			len(mb.started), got)
	}
}
```

- [ ] **Step 2: 執行測試確認失敗**

Run: `go test ./internal/scaler/ -run TestDrain -v`
Expected: FAIL,`s.Drain undefined`

- [ ] **Step 3: 寫最小實作**

`Scaler` 新增 `draining atomic.Bool`。

`Drain`:設 `draining` 為 true → 取得 idle 快照並逐一 `RemoveRunner`(失敗只警告)→ 迴圈輪詢 busy 數量,歸零即回傳 nil;`ctx.Done()` 則回傳 `ctx.Err()`。輪詢間隔 1 秒,每 30 秒以 Info 記錄剩餘 busy 數量與已等待時間,讓操作者知道在等什麼。

`HandleDesiredRunnerCount` 開頭加入:`draining` 為 true 時直接回傳目前數量與 nil,不進入擴充分支。

`IsDraining` 回傳 `draining.Load()`。

doc comment 須寫明:drain 期間 listener 必須繼續運作,因為 busy 歸零的判定來自 `HandleJobCompleted`。

- [ ] **Step 4: 執行測試確認通過**

Run: `go test ./internal/scaler/ -count=1 -race`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/scaler/
git commit -m "feat(scaler): add Drain to stop taking work and wait for in-flight jobs"
```

---

### Task 5: 訊號序列與 drain 接線

**Files:**
- Modify: `cmd/runner/main.go`(`startScaling` 的訊號處理、`run`、`runScaleSet`)
- Modify: `internal/config/config.go`、`internal/config/defaults.go`
- Test: `cmd/runner/main_test.go`、`internal/config/config_test.go`

**Interfaces:**
- Consumes: Task 4 的 `Drain` / `IsDraining`
- Produces: `Config.DrainTimeout *time.Duration`(`mapstructure:"drain-timeout"`)、`func (c *Config) EffectiveDrainTimeout() time.Duration`、`DefaultDrainTimeout = 2 * time.Hour`

**為何是指標**:必須區分「未設定」(套用預設 2 小時)與「明確設為 0」(停用 drain,回到立即關閉)。平面的 `time.Duration` 兩者都是 0,無法區分。這個 repo 對同類問題已有既定慣例——`DisableUpdate`、`BuildxCleanup`、`Prune` 都是 `*bool`,原因相同。沿用它,不要發明「以負值表示停用」這種需要額外記憶的規則。

- [ ] **Step 1: 寫失敗的測試**

```go
func TestEffectiveDrainTimeout(t *testing.T) {
	zero := time.Duration(0)
	tenMin := 10 * time.Minute
	tests := []struct {
		name string
		set  *time.Duration
		want time.Duration
	}{
		{"unset inherits default", nil, DefaultDrainTimeout},
		{"explicit zero disables drain", &zero, 0},
		{"explicit value wins", &tenMin, tenMin},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{DrainTimeout: tt.set}
			if got := c.EffectiveDrainTimeout(); got != tt.want {
				t.Errorf("EffectiveDrainTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoad_DrainTimeoutUnsetDoesNotWarn(t *testing.T) {
	v := viper.New()
	v.SetConfigType("toml")
	if err := v.ReadConfig(strings.NewReader(`
url = "https://github.com/org"
name = "runners"
token = "ghp_x"
`)); err != nil {
		t.Fatalf("read config: %v", err)
	}
	cfg, err := Load(v)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("a config without drain-timeout must not warn, got %q", cfg.Warnings)
	}
	if cfg.DrainTimeout != nil {
		t.Errorf("unset drain-timeout should decode to nil, got %v", *cfg.DrainTimeout)
	}
	if got := cfg.EffectiveDrainTimeout(); got != DefaultDrainTimeout {
		t.Errorf("EffectiveDrainTimeout() = %v, want the default %v", got, DefaultDrainTimeout)
	}
}

func TestLoad_DrainTimeoutExplicitZeroDisables(t *testing.T) {
	v := viper.New()
	v.SetConfigType("toml")
	_ = v.ReadConfig(strings.NewReader(`
url = "https://github.com/org"
name = "runners"
token = "ghp_x"
drain-timeout = "0s"
`))
	cfg, err := Load(v)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DrainTimeout == nil || *cfg.DrainTimeout != 0 {
		t.Fatalf("explicit 0s should decode to a non-nil zero, got %v", cfg.DrainTimeout)
	}
	if got := cfg.EffectiveDrainTimeout(); got != 0 {
		t.Errorf("EffectiveDrainTimeout() = %v, want 0 (drain disabled)", got)
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("explicit zero must not warn, got %q", cfg.Warnings)
	}
}
```

- [ ] **Step 2: 執行測試確認失敗**

Run: `go test ./internal/config/ -run TestDrainTimeout -v`
Expected: FAIL,`cfg.DrainTimeout undefined`

- [ ] **Step 3: 寫最小實作**

**設定**:`Config` 新增 `DrainTimeout *time.Duration \`mapstructure:"drain-timeout"\``;`defaults.go` 新增 `DefaultDrainTimeout = 2 * time.Hour`,doc comment 說明取值理由(涵蓋實際 job 長度,與 GitLab Runner 建議值一致)。`EffectiveDrainTimeout()` 在 nil 時回傳預設值、非 nil 時回傳其值(含 0 = 停用),寫法比照既有的 `IsDinD()` / `IsDockerPruneEnabled()`。所有使用端一律透過 `EffectiveDrainTimeout()` 取值,不直接讀欄位。

**訊號**:`startScaling` 現行的 `signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)` 改為自行管理:

```go
// SIGINT (Ctrl-C) means "stop now" by convention, so it never drains —
// an interactive user must not be made to wait out a two-hour drain.
// SIGTERM and SIGQUIT drain; a second signal of any kind stops immediately.
sigCh := make(chan os.Signal, 3)
signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
```

第一個訊號:若為 `os.Interrupt` → 直接取消 ctx(現行行為);否則 → 觸發 drain(取消一個獨立的 `drainCtx`,由 `runScaleSet` 監看)。第二個訊號 → 取消主 ctx。第三個 → `os.Exit(1)`(現行已有)。

**接線**:`runScaleSet` 在 listen 迴圈旁增加一個 goroutine,收到 drain 訊號時以 `cfg.DrainTimeout`(或預設值)為上限呼叫 `s.Drain(drainCtx)`,完成後才結束 listen 迴圈。既有的 defer(刪除 scale set、`s.Shutdown`)維持不變,在 drain 之後執行。

- [ ] **Step 4: 執行測試確認通過**

Run: `go build ./... && go test ./... -count=1 && go test ./internal/scaler/ ./cmd/runner/ -race -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/runner/ internal/config/
git commit -m "feat(runner): drain on SIGTERM/SIGQUIT, stop immediately on SIGINT"
```

---

### Task 6: 服務範本的停止逾時與啟動警告

**Files:**
- Modify: `cmd/runner/cmd_service.go`(systemd 與 launchd 範本)
- Test: `cmd/runner/cmd_service_test.go`

**Interfaces:**
- Consumes: Task 5 的 `DefaultDrainTimeout`
- Produces: 無新 API

- [ ] **Step 1: 寫失敗的測試**

```go
func TestSystemdTemplateBoundsStopByDrainTimeout(t *testing.T) {
	unit := renderSystemdUnit(installOpts{user: true, configPath: "/etc/runner/config.toml",
		binaryPath: "/usr/local/bin/runner"})

	if !strings.Contains(unit, "TimeoutStopSec=") {
		t.Fatal("unit has no TimeoutStopSec; systemd's 90s default would SIGKILL " +
			"runner mid-drain, killing the very jobs drain exists to protect")
	}
	// The stop timeout must exceed the drain budget, or draining is pointless.
	want := int((config.DefaultDrainTimeout + time.Minute).Seconds())
	if !strings.Contains(unit, fmt.Sprintf("TimeoutStopSec=%d", want)) {
		t.Errorf("TimeoutStopSec must exceed the drain budget (%v), unit was:\n%s",
			config.DefaultDrainTimeout, unit)
	}
}

func TestLaunchdTemplateSetsExitTimeOut(t *testing.T) {
	plist := renderLaunchdPlist(installOpts{configPath: "/Users/admin/runner/config.toml",
		binaryPath: "/usr/local/bin/runner"})

	if !strings.Contains(plist, "ExitTimeOut") {
		t.Fatal("plist has no ExitTimeOut; launchd would SIGKILL runner mid-drain")
	}
}
```

**這兩個算繪函式目前不存在,必須先抽出。** 現行程式碼把範本直接寫進檔案(`cmd_service.go:344` 的 `systemdTmpl.Execute(f, data)` 與 `:520` 的 `launchdTmpl.Execute(f, data)`),測試無從取得算繪結果。抽出 `renderSystemdUnit(opts installOpts) (string, error)` 與 `renderLaunchdPlist(opts installOpts) (string, error)`,讓那兩處改為呼叫它們再寫檔。這是純重構,既有的 service 測試必須維持通過。

- [ ] **Step 2: 執行測試確認失敗**

Run: `go test ./cmd/runner/ -run 'TemplateBounds|ExitTimeOut' -v`
Expected: FAIL,無 `TimeoutStopSec`

- [ ] **Step 3: 寫最小實作**

systemd 範本加入 `TimeoutStopSec=<drain 上限 + 60 秒>`;launchd plist 加入 `<key>ExitTimeOut</key><integer><同值></integer>`。兩者皆以 `config.DefaultDrainTimeout` 推導,並在範本註解說明為何要大於 drain 上限。

**啟動警告**:`run()` 在偵測到自己由服務管理器啟動(systemd 設定 `INVOCATION_ID` 環境變數;launchd 可用 `XPC_SERVICE_NAME`)且 drain 已啟用時,發出一則 Info 提示:若服務 unit 是舊版本(缺少停止逾時設定),重啟時 drain 可能被截斷,重跑 `runner service install` 可修正。**不讀取也不修改服務管理器的狀態**——即使警告被忽略,Task 3 的啟動對帳也會在下次啟動時收拾殘局。

- [ ] **Step 4: 執行測試確認通過**

Run: `go test ./cmd/runner/ -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/runner/
git commit -m "feat(service): bound service stop timeout by the drain budget"
```

---

### Task 7: `runner update` 的版本提示與文件

**Files:**
- Modify: `cmd/runner/cmd_update.go`
- Modify: `config.example.toml`、`README.md`
- Test: `cmd/runner/cmd_update_test.go`

**Interfaces:**
- Consumes: Task 5 的設定;既有的 health endpoint
- Produces: 無新 API

- [ ] **Step 1: 寫失敗的測試**

```go
func TestUpdateNotice_WarnsWhenRunningVersionDiffers(t *testing.T) {
	notice := updateRestartNotice("0.5.0", "0.4.1")
	for _, want := range []string{"0.4.1", "0.5.0", "runner service restart"} {
		if !strings.Contains(notice, want) {
			t.Errorf("notice must name both versions and the command to apply the "+
				"new one; missing %q in:\n%s", want, notice)
		}
	}
}

func TestUpdateNotice_QuietWhenNothingRunning(t *testing.T) {
	if got := updateRestartNotice("0.5.0", ""); got != "" {
		t.Errorf("with no running instance there is nothing to restart, got %q", got)
	}
}
```

- [ ] **Step 2: 執行測試確認失敗**

Run: `go test ./cmd/runner/ -run TestUpdateNotice -v`
Expected: FAIL,`undefined: updateRestartNotice`

- [ ] **Step 3: 寫最小實作**

```go
// updateRestartNotice returns the message shown after a successful update.
// Empty when nothing is running (running == "") or when the running instance
// already matches the installed binary — there is nothing to act on.
//
// The previous message ("Restart runner to use the new version") did not say
// whether a service was running or which version it was on, so an operator
// who updated and forgot to restart had no signal at all: `runner --version`
// reported the new binary while the old one kept serving jobs.
func updateRestartNotice(installed, running string) string
```

`update` 成功後查詢本機 health endpoint 取得執行中版本(失敗則視為沒有執行中實例,回傳空字串),再印出 `updateRestartNotice` 的結果。

**文件**:`config.example.toml` 加入註解掉的 `drain-timeout`;`README.md` 增補一節說明訊號語意(SIGTERM/SIGQUIT drain、SIGINT 立即)、drain 的預設值與停用寫法、以及建議的升級流程(更新 → `runner service restart` → drain 自動保護進行中的 job)。互動式執行的 drain 方式(`kill -TERM <pid>`)須寫明,因為 Ctrl-C 刻意不 drain。

- [ ] **Step 4: 執行測試確認通過**

Run: `go build ./... && go test ./... -count=1 && go vet ./... && gofmt -l . && golangci-lint run`
Expected: 全部通過,`gofmt -l .` 無輸出,lint 0 issues

- [ ] **Step 5: Commit**

```bash
git add cmd/runner/ config.example.toml README.md
git commit -m "feat(update): tell the operator when the running version differs"
```

---

## 驗收

全部 task 完成後:

```bash
# 既有設定零警告
go run ./cmd/runner validate --config <各主機 config 副本>

# drain 實際行為(在有 job 在跑的主機上)
kill -TERM <pid>   # 應記錄剩餘 busy 數量並等待,不中斷 job
kill -TERM <pid>   # 第二次應立即停止
```

並確認 `runner service install` 產生的 unit 含 `TimeoutStopSec`,以及重啟後 `runner cache` 與 `/healthz` 一切正常。
