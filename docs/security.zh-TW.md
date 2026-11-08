<a id="security--isolation"></a>

# 安全性與隔離

[English](security.md) | [繁體中文](security.zh-TW.md)

本文件為 [英文版](security.md) 的繁體中文版本；若內容有差異，以英文版為準。

**`dind = true` 是共用 Docker socket，而不是具沙箱隔離的 Docker-in-Docker。** runner 容器會掛載主機的 Docker socket，因此任何工作都能控制主機常駐程式，包括啟動特權容器、掛載主機檔案系統、檢查其他工作的容器。這相當於擁有主機的 root 權限。請只執行你信任的程式碼：**絕對不要讓這些 runner 處理來自公開 fork 的 pull request**。不需要建置映像檔的 runner 規模集，請設定 `dind = false`。涵蓋整個常駐程式的 buildx／prune 清理程序預設關閉，而且只應在 runner 專用的 Docker 常駐程式上啟用。

隔離設定皆位於各 runner 規模集的 `[docker]` 下：

| 設定鍵 | 效果 |
| --- | ------ |
| `memory`, `cpu` | 每個 runner 容器的 cgroup 限制 |
| `pids-limit` | 限制每個容器的程序與執行緒總數（例如 `4096`），避免 fork 炸彈拖垮主機 |
| `network` | 將 runner 連接至預先建立的 Docker 網路，而非預設橋接網路，例如 `docker network create --opt com.docker.network.bridge.enable_icc=false runners` |
| `shared-volume-name` | 讓每個 runner 規模集的共用磁碟區使用不同的具名磁碟區，避免不同信任等級的 runner 規模集共用檔案 |
| `dind = false` | 完全不掛載 socket，提供容器層級最強的隔離，但無法建置映像檔 |

Tart 供應器在 Hypervisor 邊界進行隔離：每個工作都會取得從基礎映像檔複製的全新 macOS VM，並在結束後刪除。
