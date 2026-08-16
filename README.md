# Telegram 视频顺序上传器

一个只做一件事的 Windows/macOS 桌面应用：从一个或多个文件夹选择顶层 MP4，加入可管理的持久化队列，然后按队列顺序逐个上传到 Telegram Channel。每个视频独占一条消息，Caption 等于去掉 `.mp4` 后缀的文件名，不发送媒体组。

## 为什么采用这个方案

- 仅使用 Bot 身份，不登录个人手机号账号，避免个人账号自动化带来的额外风控。
- 直接通过 Telegram MTProto 上传，绕过云端 HTTP Bot API 的 50 MB 新上传限制。
- 普通 Bot 当前按运行时配置支持约 2 GiB 级别文件；应用连接后会查询并显示实际可用上限。Bot 没有 Premium 上传开关。
- 文件队列严格串行，单个文件内部使用 4 个分片 worker；这样既保持频道消息顺序，又能利用上行带宽。
- 遇到 Telegram `FLOOD_WAIT` 会按服务端要求等待，不启用付费 flood skip，也不会用多文件并发绕过限制。

任何程序都不能承诺“绝不会被限制”。请只向自己管理的频道上传合法内容，不要同时运行多个使用同一 Bot 的上传实例，也不要用于 spam。

## 使用方法

需要三项 Telegram 凭据：

1. 在 [my.telegram.org](https://my.telegram.org/) 创建应用，获得 `API ID` 和 `API Hash`。
2. 通过 [BotFather](https://t.me/BotFather) 创建 Bot，获得 `Bot Token`。
3. 把 Bot 加入目标 Channel，设为管理员，只需授予“发布消息”权限。

然后：

1. 打开应用，填写三项凭据并连接；可选填 `SOCKS5` 代理。
2. 点击“绑定频道”，复制一次性验证码，并把验证码作为一条临时消息发布到目标频道。应用收到更新后会自动验证发帖权限并保存频道。
3. 点击“添加文件夹…”。应用只扫描顶层普通 `.mp4` 文件，不递归子文件夹，也不跟随 symlink；候选视频按文件名自然升序显示，可全选、取消全选或逐项勾选后加入队列。
4. 可重复添加不同文件夹。相同规范化路径不会重复加入；主队列支持多选、删除所选、删除已完成和清空队列，这些操作只改本地队列，不删除磁盘文件或 Telegram 消息。
5. 每个条目在一行内显示文件名、独立进度、状态和当前可用操作；上方同时显示整个队列的总进度。
6. 取消、失败、中断或跳过的任务可通过“重置任务…”按所选任务、状态或全部可恢复任务批量恢复为“待上传”，无需清空队列或重新扫描文件夹。待确认、已发送、已移动和超限任务不会被批量重置。
7. 点击“开始上传”立即运行，也可设置本机时间点定时开始。应用保持打开时会到点启动；应用完全退出后系统不会自动唤醒它，重新打开后会恢复尚未消费的计划。
8. “暂停队列”采用安全的软暂停：当前视频会完整上传并提交消息，然后在下一条之前停止。此时可继续上传或编辑队列；如需立即中止当前视频，仍使用该条目的“取消”或“取消全部”。

建议第一次先用两个很小的 MP4 验证顺序、Caption 和频道权限，再上传大文件。

## 下载版本

请从 [GitHub Releases](https://github.com/Jaaayden/telegram-uploader/releases) 下载构建好的 Windows 或 macOS 应用。`v*` 版本是正式版本；`main-*` 是每次合并到主分支后自动生成的开发预览版。每个 Release 都附带 `SHA256SUMS`，可用于核对下载文件完整性。

## 安全与恢复语义

- `Bot Token`、`API Hash` 和代理密码存入 macOS Keychain 或 Windows Credential Manager。
- Telegram session 使用 AES-256-GCM 加密；加密密钥只存系统凭据库，磁盘上不写明文 session。
- 队列、暂停状态、每条消息的 `random_id`、进度和频道信息使用原子文件持久化。崩溃后，上传阶段的任务会标为“中断”，可重新开始。
- 如果取消/断线恰好发生在消息提交阶段，应用不会盲目重试，而是标为“待确认”。请先检查频道，再选择“已发送”或明确确认重新上传。
- 如果 MP4 的 `moov` 元数据完整、但末尾 `mdat` 声明长度超过实际文件，应用会显示“源 MP4 尾部结构不完整”警告并原样上传，而不是阻止任务。应用不会修复或补全源文件；重要视频仍建议从原始来源重新复制一份完整文件。
- 超过 Bot 当前上限的文件不会上传。可一键移动到指定文件夹；移动操作从不覆盖同名目标，跨卷时会复制、校验 SHA-256、同步落盘后才删除源文件。
- macOS 上传期间调用系统自带的 `caffeinate`；Windows 使用 `SetThreadExecutionState`，避免电脑自动休眠中断上传。

应用数据位置：

- macOS：`~/Library/Application Support/TelegramVideoUploader/`
- Windows：`%AppData%\TelegramVideoUploader\`

敏感凭据不在上述目录的 JSON 文件中。

## 本地构建

要求 Go 1.25 或更新版本。最终用户使用已构建产物时不需要安装 Go、Python、FFmpeg、TDLib 或本地 Bot API Server。

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

Fyne 桌面驱动在构建时需要平台 C 编译器；发布后的应用是自包含产物。仓库中的 GitHub Actions 工作流会分别在原生 macOS 和 Windows runner 上构建。

## 验证

```bash
go test ./...
go test -race ./internal/app ./internal/credentials ./internal/media ./internal/mover ./internal/queue ./internal/scanner ./internal/telegram
go vet ./...
```

离线测试覆盖自然排序、MP4 元数据、队列恢复、逐项/全部取消、待确认消息、跨卷安全移动、加密 session 和关键并发路径。真实 Telegram 端到端测试必须由使用者在本机输入自己的凭据完成；不要把凭据提交到仓库或发送到聊天中。
