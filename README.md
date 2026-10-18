<p align="center">
  <img src="packaging/icon/icon-256.png" alt="Telegram 视频顺序上传器图标" width="128" height="128">
</p>

<h1 align="center">Telegram 视频顺序上传器</h1>

<p align="center">
  一个只做一件事的 Windows / macOS 桌面应用：把文件夹里的 MP4 放进持久化队列，<br>
  按顺序逐个上传到你的 Telegram 频道。每个视频一条消息，Caption 就是文件名。
</p>

<p align="center">
  <a href="https://github.com/Jaaayden/telegram-uploader/releases/latest">下载最新版</a> ·
  <a href="#快速开始">快速开始</a> ·
  <a href="#可靠性与恢复">可靠性与恢复</a> ·
  <a href="#从源码构建">从源码构建</a>
</p>

---

## 目录

- [特点](#特点)
- [快速开始](#快速开始)
- [日常使用](#日常使用)
- [可靠性与恢复](#可靠性与恢复)
- [数据位置与诊断日志](#数据位置与诊断日志)
- [设计说明](#设计说明)
- [从源码构建](#从源码构建)
- [验证与持续集成](#验证与持续集成)
- [发布流程](#发布流程)

## 特点

- **严格顺序**：队列逐个上传，频道里的消息顺序与队列一致；每个视频独占一条消息，Caption 为去掉 `.mp4` 后缀的文件名，不发送媒体组。
- **只用 Bot 身份**：不登录个人手机号账号，避免个人账号自动化带来的额外风控。
- **绕过 50 MB 限制**：直接通过 Telegram MTProto 上传，不经过云端 HTTP Bot API；连接后会查询并显示当前 Bot 实际可用的文件大小上限（通常约 2 GiB）。
- **可管理的持久化队列**：多文件夹、多选删除、批量重置、暂停 / 继续、定时开始；崩溃或退出后可恢复。
- **不重复发送**：消息提交阶段的中断不会被盲目重试，而是标为“待确认”，由你检查频道后决定。
- **三档上传并发**：兼容（4 路）、均衡（8 路，默认）、高速（12 路），随时切换，只影响下一个视频。
- **凭据不落盘**：`Bot Token`、`API Hash`、代理密码存入系统凭据库；Telegram session 用 AES-256-GCM 加密。
- **自包含**：最终用户不需要安装 Go、Python、FFmpeg、TDLib 或本地 Bot API Server。

> 任何程序都不能承诺“绝不会被限制”。请只向自己管理的频道上传合法内容，不要同时运行多个使用同一 Bot 的实例，也不要用于 spam。

## 快速开始

### 1. 下载

从 [GitHub Releases](https://github.com/Jaaayden/telegram-uploader/releases) 下载对应平台的文件：

| 平台 | 文件 |
| --- | --- |
| macOS（Apple 芯片） | `TelegramVideoUploader-macos-arm64.zip` |
| macOS（Intel） | `TelegramVideoUploader-macos-x86_64.zip` |
| Windows（64 位） | `TelegramVideoUploader-windows-amd64.exe` |

`v*` 是正式版本；`nightly` 是始终指向主分支最新提交的滚动预览版（每次合并后被替换，不会累积旧版本）。每个 Release 都附带 `SHA256SUMS`，可用来核对文件完整性。

> **首次打开**：应用目前没有 Apple 公证和 Windows 代码签名。
> - **macOS**：Gatekeeper 会提示“无法验证开发者”。在 Finder 中右键应用选择“打开”，或执行 `xattr -dr com.apple.quarantine /path/to/TelegramVideoUploader.app`。
> - **Windows**：SmartScreen 可能提示“未知发布者”，选择“更多信息 → 仍要运行”。

### 2. 准备 Telegram 凭据

1. 在 [my.telegram.org](https://my.telegram.org/) 创建应用，获得 `API ID` 和 `API Hash`。
2. 通过 [BotFather](https://t.me/BotFather) 创建 Bot，获得 `Bot Token`。
3. 把 Bot 加入目标 Channel，设为管理员，只需授予“发布消息”权限。

### 3. 连接并绑定频道

1. 打开应用，填写三项凭据并点击“保存并连接”；需要时可选填 `SOCKS5` 代理。
2. 点击“绑定频道”，复制一次性验证码，把它作为一条临时消息发到目标频道。应用收到更新后会自动验证发帖权限并保存频道。

### 4. 先小规模试一次

建议第一次只用两个很小的 MP4，验证顺序、Caption 和频道权限，再上传大文件。

## 日常使用

**添加视频**

- 点击“添加文件夹…”。应用只扫描顶层普通 `.mp4` 文件，不递归子文件夹，也不跟随 symlink。
- 候选视频按文件名自然升序（`2.mp4` 在 `10.mp4` 之前）显示，可全选、取消全选或逐项勾选。
- 可重复添加不同文件夹；相同规范化路径不会重复加入。

**管理队列**

- 每个条目在一行内显示文件名、独立进度、状态和当前可用操作；顶部显示整个队列的总进度。
- 支持多选、删除所选、删除已完成和清空队列。这些操作只改本地队列，**不会**删除磁盘文件或 Telegram 消息。
- 取消、失败、中断或跳过的任务，可通过“重置任务…”按所选、按状态或全部批量恢复为“待上传”，无需重新扫描。待确认、已发送、已移动和超限任务不会被批量重置。

**开始、暂停与定时**

- “开始上传”立即运行；也可以设置本机时间点定时开始。应用保持打开时到点启动；完全退出后系统不会自动唤醒它，重新打开后会恢复尚未消费的计划。
- “暂停队列”是软暂停：正在上传的视频会完整上传并提交消息，然后在下一条之前停止；如果当前正在等待网络重试，则立即停止等待并保留为可恢复的中断任务。暂停期间可以继续编辑队列。
- 需要立即中止正在上传的视频，请用该条目的“取消”或“取消全部”。

**并发档位**

默认的“均衡（8 路）”最多保持约 4 MiB 数据在途，适合大多数网络。受限代理或不稳定网络可改用“兼容”；延迟高而上行带宽充足时可试“高速”。

应用不会通过额外上传测试文件来“自动测速”——结果容易受 Telegram 路由和瞬时波动影响，还会浪费流量。想比较档位，请用同一个较大的视频分别测试；高速档没有稳定提升就继续用均衡档。个人账号 Premium 的客户端速度不能直接等同于 Bot 会话能达到的速度。

## 可靠性与恢复

| 场景 | 行为 |
| --- | --- |
| 分片或连接在消息提交前中断 | 从文件开头自动重试 5 次（2、5、10、20、40 秒退避）；耗尽后标记失败并继续下一条，不卡住整个队列 |
| 取消 / 断线恰好发生在消息提交阶段 | 不盲目重试，标为“待确认”；请先检查频道，再选择“已发送”或明确确认重新上传 |
| 进程崩溃或被终止 | 上传阶段的任务标为“中断”，可重新开始；队列、暂停状态、每条消息的 `random_id` 等关键状态用原子文件持久化 |
| Telegram 要求等待（`FLOOD_WAIT`） | 按服务端要求等待，不启用付费 flood skip，也不会用多文件并发绕过限制 |
| MP4 的 `moov` 完整但末尾 `mdat` 声明长度超过实际文件 | 显示“源 MP4 尾部结构不完整”警告并原样上传，不修复源文件；重要视频建议重新复制完整文件 |
| 文件超过 Bot 当前上限 | 不上传；可一键移动到指定文件夹。移动从不覆盖同名目标，跨卷时复制、校验 SHA-256、同步落盘后才删除源文件 |
| 加密 session 损坏，或凭据库里的加密密钥丢失 | 旧文件改名为 `telegram-session.enc.invalid`，用 Bot Token 自动重新登录；凭据库只是暂时不可用时不会丢弃 session |
| 电脑自动休眠 | 上传期间 macOS 使用 `caffeinate`、Windows 使用 `SetThreadExecutionState` 阻止休眠 |

中间的字节进度只保存在内存里：Telegram 的分片无法跨进程续传，这样也避免上传线程每秒等待磁盘同步。

## 数据位置与诊断日志

| 平台 | 应用数据目录 |
| --- | --- |
| macOS | `~/Library/Application Support/TelegramVideoUploader/` |
| Windows | `%AppData%\TelegramVideoUploader\` |

目录里保存队列、设置和加密 session。**敏感凭据不在任何 JSON 文件中**，只在系统凭据库（macOS Keychain / Windows Credential Manager）里。

“设置”页底部的“关于”显示版本、构建号和源码仓库，并可直接打开日志目录。每次启动都会在 `logs/` 子目录写入低频运行日志：

- `app.log`：启动、正常退出及少量关键生命周期信息；单文件上限 5 MiB，最多保留 3 份轮转备份。
- `crash-*.log`：接收 Go runtime 能捕获的未处理 panic 或致命错误；正常运行且内容为空时自动删除。
- `run-state.json`：运行 ID、系统 / 架构、进程号和起止时间，用于判断上一次是否正常关闭。

应用日志不会主动记录 Bot Token、API Hash、代理密码或 session，常见凭据格式在写入前还会脱敏；应用也不会自动上传任何日志。`crash-*.log` 是 Go runtime 原样写入的故障现场，无法经过脱敏，发给别人前请先自行检查。

如果 Windows 上再次出现窗口无提示消失，请保留该时间段的 `app.log` 和对应的 `crash-*.log`，并同时查看“可靠性监视器”或“事件查看器 → Windows 日志 → 应用程序”。原生图形驱动崩溃、系统重启、注销或外部程序终止进程不一定能写入 Go 的崩溃文件，Windows 事件记录可作补充。

## 设计说明

- **为什么用 Bot**：只用 Bot 身份，不登录个人账号。个人账号的 Premium 不会转移给 Bot，Telegram 目前也不支持 Premium Bot。
- **分片与并发**：单个文件内部使用 Telegram 允许的最大 `512 KiB` 分片，并按所选档位并发；文件之间严格串行，既不改变频道消息顺序，也更适合高带宽、高延迟的链路。
- **字体**：Fyne 默认字体不含汉字，会对每个汉字各自回退到不同的系统字体，同一行文字因此粗细和基线不一。应用内置了裁剪过的 Noto Sans SC（SIL OFL 1.1，见 `internal/ui/fonts/`）。
- **图标**：源文件是 `packaging/icon/icon.svg`，其他尺寸和格式由它生成，见 [`packaging/icon/README.md`](packaging/icon/README.md)。

## 从源码构建

要求 Go 1.26 或更新版本（以 `go.mod` 为准）。Fyne 桌面驱动在构建时需要平台 C 编译器；构建出的应用是自包含的。

macOS：

```bash
./scripts/build.sh
open build/TelegramVideoUploader.app
```

Windows PowerShell：

```powershell
./scripts/build.ps1
./build/TelegramVideoUploader.exe
```

版本号只在 `FyneApp.toml` 维护。构建脚本通过 `-ldflags -X` 注入 `buildinfo.Version`，`scripts/check-version.sh [tag]` 会校验 `Info.plist` 与（可选的）发布 tag 是否一致，CI 每次构建都会运行它。

## 验证与持续集成

```bash
go test ./...
go test -race ./internal/app ./internal/credentials ./internal/diagnostics ./internal/fsutil ./internal/media ./internal/mover ./internal/queue ./internal/scanner ./internal/telegram
go vet ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
./scripts/check-version.sh
```

离线测试覆盖自然排序、MP4 元数据、队列恢复、逐项 / 全部取消、待确认消息、跨卷安全移动、加密 session 和关键并发路径。真实 Telegram 端到端测试必须由使用者在本机输入自己的凭据完成；**不要把凭据提交到仓库或发送到聊天中**。

GitHub Actions 的分工：

| 检查 | 运行位置 | 内容 |
| --- | --- | --- |
| Lint and test | Linux | gofmt、`go mod tidy`、版本一致性、非 UI 包的 `go vet`、`go test -race`、staticcheck |
| macOS / Windows 构建 | 原生 runner | 完整 `go vet`、`go test`，并构建、打包应用 |
| Vulnerability scan | macOS | `govulncheck`；每次推送和每周一次运行，新公告不会阻塞发版 |

## 发布流程

1. 在 `FyneApp.toml` 更新 `Version` 和 `Build`，并同步 `packaging/macos/Info.plist`（`./scripts/check-version.sh` 会检查）。
2. 合并到 `main` 后，CI 自动更新滚动的 `nightly` 预发布。
3. 打 `vX.Y.Z` tag 并推送，CI 会校验 tag 与版本一致，构建三个平台的产物，生成 `SHA256SUMS` 并创建正式 Release。
