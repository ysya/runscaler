# 快取架構:已知的後續事項

日期:2026-08-14
來源:`2026-08-13-cache-architecture` 實作過程中,由逐 task review 與最終整支 review 累積並經裁決保留的項目。全部**不阻擋合併**,但都是真實的、有具體失敗情境的問題。

## 行為類

**[已解決] `doctor --fix` 的 shared-volume 孤兒判定。** `1162110` 起不再以「volume 存在」判定為孤兒,介面也不再提供自動刪除能力。doctor 只報告名稱/大小並提供明確的人工移除指令;shared volume 的內容回收仍由 `shared-volume-max-age` 與 disk guard 負責。

**守門員逾時後仍走完剩餘階層。** `internal/diskguard/guard.go` 的 `sweepFilesystem` 全程沒有 `ctx.Err()` 檢查。啟動 job 前的同步回收加上 2 分鐘上限後,逾時從「只在關機時發生」變成常態;逾時後迴圈仍會遍歷剩餘的 tier×store,每個立即失敗並各噴一則警告,最後再發一則誤導的「已達 max-tier 仍未達標」。不影響資料,純噪音。

**逾時警告的文案不精確。** `internal/scaler/scaler.go` 在父 context 先到期時,警告仍印 `timeout=2m0s`,而實際生效的是父的期限。

**shared volume 的 scaleset 換手不發警告。** 同一個 volume 名稱被多個 scaleset 設定時,「先設定 max-age 者勝出」的換手是靜默的,操作者不會知道前一個 scaleset 的 path/interval 被覆蓋。方向是安全的(有 TTL 優於無 TTL),但不透明。

## 韌性類

**Docker `Path()` 解析失敗後永不重試。** `internal/cachestore/docker.go` 的 `resolveDockerRootDir` 在建構時解析一次,失敗回 `""`,且該值被複製給同一 socket 的所有 store。一旦失敗(例如 Ping 成功但 Info 逾時),該 socket 全部 store 的 `Path()` 永久為空 → `statfs` 永遠失敗 → 守門員對這台 Docker 完全失效,直到重啟。

**`/healthz` 在持有 read lock 時做 statfs。** `internal/health/health.go` 在鎖內呼叫磁碟提供者。正常是微秒級,但若 `TART_HOME` 指向失效的網路/外接掛載,statfs 可能長時間阻塞,連帶卡住需要 write lock 的 `MarkConnected`/`RegisterScaler`。建議在鎖外取得 `diskFn` 再呼叫。

**`tartCacheStoreFor` 的 fallback 分支未設 `interval`。** 留下 `interval == 0`,目前靠呼叫端 `startTartCacheCleanup` 的 `!enabled` 提前跳過才不會 `NewTicker(0)` panic。是活的陷阱,應在來源補預設值而非依賴呼叫端。

**`serialized` 包裝會遮蔽未來新增的方法。** `internal/cachestore/store.go` 的每 store 序列化包裝以 embed `CacheStore` 實作。目前全 repo 無對 store 的型別斷言,但日後若某個 store 新增介面之外的方法,包裝後將無法取用。

## 覆蓋率類

**沒有以三台主機的真實設定為案例的測試。** 計畫的驗收步驟要求「三台主機的現行設定原樣載入無警告」。舊 key 的零警告目前只由合成案例覆蓋(`TestLoad_LegacyCacheKeysStillWork` 等),真實設定僅由人工實測確認過一次,沒有測試釘住。

**`fakeDockerAPI.imagePruneErr` 從未被任何測試設定。** garbage store 同時發生兩個錯誤時的 `errors.Join` 行為未被釘住(build cache store 的有)。

**`bytesize` 的兩個解析限制。** 不接受 `"1.5GB"` 這類小數大小(用 `ParseUint`);TB 乘法未檢查 `uint64` 溢位,`"20000000TB"` 會靜默繞回。

## 已裁決維持現狀

**`NeedsReclaim` 在所有 statfs 失敗時回傳 `(true, err)`。** 曾有 reviewer 主張應為 `(false, err)`,理由是誤設定的主機會每次 job 啟動白跑一次完整 sweep。最終 review 查證後推翻此論述:唯一的呼叫者 `internal/scaler/scaler.go` 先檢查 `err`,有錯就只記警告並直接開 job,`else if need` 分支根本進不去。因此 `true` 對現行行為零影響,它的作用僅止於「保護未來某個不檢查 error 就用 bool 的呼叫者」,而那是站得住的理由。
