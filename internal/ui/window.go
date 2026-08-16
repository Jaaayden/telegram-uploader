// Package ui contains the small desktop user interface for the uploader.
//
// The UI deliberately owns no upload logic.  It observes app.Controller
// snapshots and sends user actions back to the controller.  This keeps the
// Telegram transport and queue usable from tests and from a future CLI.
package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	fyneapp "fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
	coreapp "github.com/jayden/telegram-video-uploader/internal/app"
	"github.com/jayden/telegram-video-uploader/internal/credentials"
	"github.com/jayden/telegram-video-uploader/internal/model"
	"github.com/jayden/telegram-video-uploader/internal/platform"
	tgtransport "github.com/jayden/telegram-video-uploader/internal/telegram"
)

// Run creates and runs the single-window desktop application.  It returns
// when the window is closed.  No credentials are required to construct the
// window; connection is intentionally deferred until the user presses the
// connect button.
func Run(controller *coreapp.Controller, paths coreapp.Paths, secrets *credentials.Store) {
	if controller == nil {
		return
	}
	if secrets == nil {
		secrets = credentials.NewStore()
	}

	settings, _ := coreapp.LoadSettings(paths.Settings)
	application := fyneapp.NewWithID("com.jayden.telegramvideouploader")
	window := application.NewWindow("Telegram 视频顺序上传器")
	u := newWindow(application, window, controller, paths, secrets, settings)
	u.build()
	u.startObservers()
	u.window.ShowAndRun()
}

type window struct {
	application fyne.App
	window      fyne.Window
	controller  *coreapp.Controller
	paths       coreapp.Paths
	secrets     *credentials.Store

	settingsMu sync.Mutex
	settings   coreapp.Settings

	rootCtx    context.Context
	rootCancel context.CancelFunc

	clientMu     sync.RWMutex
	client       *tgtransport.Client
	clientCancel context.CancelFunc

	closedMu sync.RWMutex
	closed   bool

	connected bool
	identity  tgtransport.Identity
	snapshot  coreapp.Snapshot

	// Settings controls.
	apiID         *widget.Entry
	apiHash       *widget.Entry
	botToken      *widget.Entry
	proxyEnabled  *widget.Check
	proxyAddress  *widget.Entry
	proxyUsername *widget.Entry
	proxyPassword *widget.Entry
	connectButton *widget.Button
	bindButton    *widget.Button
	connection    *widget.Label
	channel       *widget.Label
	limit         *widget.Label

	// Queue controls.
	folderLabel        *widget.Label
	chooseFolderButton *widget.Button
	startButton        *widget.Button
	cancelAllButton    *widget.Button
	moveButton         *widget.Button
	cancelMoveButton   *widget.Button
	list               *widget.List
	progress           *widget.ProgressBar
	progressSummary    *widget.Label
	operationProgress  *widget.ProgressBar
	operationLabel     *widget.Label

	bindMu     sync.Mutex
	bindDialog *dialog.CustomDialog

	moveMu       sync.Mutex
	moveLastUI   time.Time
	moveCancel   context.CancelFunc
	moveInFlight bool

	sleepMu    sync.Mutex
	sleepGuard platform.SleepGuard
}

func newWindow(application fyne.App, fyneWindow fyne.Window, controller *coreapp.Controller, paths coreapp.Paths, secrets *credentials.Store, settings coreapp.Settings) *window {
	ctx, cancel := context.WithCancel(context.Background())
	return &window{
		application: application,
		window:      fyneWindow,
		controller:  controller,
		paths:       paths,
		secrets:     secrets,
		settings:    settings,
		rootCtx:     ctx,
		rootCancel:  cancel,
	}
}

func (u *window) build() {
	u.buildFields()
	u.buildQueue()

	settingsPanel := container.NewVBox(
		widget.NewLabelWithStyle("Telegram 连接", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewSeparator(),
		widget.NewLabel("API ID"),
		u.apiID,
		widget.NewLabel("API Hash"),
		u.apiHash,
		widget.NewLabel("Bot Token"),
		u.botToken,
		widget.NewLabel("可选：SOCKS5 代理"),
		u.proxyEnabled,
		u.proxyAddress,
		u.proxyUsername,
		u.proxyPassword,
		u.connectButton,
		u.connection,
		widget.NewSeparator(),
		widget.NewLabelWithStyle("频道", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		u.channel,
		u.bindButton,
		u.limit,
		widget.NewLabel("提示：Bot 只需要目标频道的发帖权限，不会登录个人账号。"),
	)
	settingsScroll := container.NewVScroll(settingsPanel)

	queueTop := container.NewVBox(
		widget.NewLabelWithStyle("上传队列", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		container.NewBorder(nil, nil, nil, u.chooseFolderButton, u.folderLabel),
		container.NewHBox(u.startButton, u.cancelAllButton, u.moveButton, u.cancelMoveButton),
		u.progress,
		u.progressSummary,
		u.operationProgress,
		u.operationLabel,
	)
	queueContent := container.NewBorder(queueTop, nil, nil, nil, u.list)

	split := container.NewHSplit(settingsScroll, queueContent)
	split.SetOffset(0.30)
	u.window.SetContent(container.NewPadded(split))
	u.window.Resize(fyne.NewSize(1180, 760))
	u.window.SetCloseIntercept(u.closeIntercept)

	u.applyLoadedSettings()
	u.applySnapshot(u.controller.Snapshot())
}

func (u *window) buildFields() {
	u.apiID = widget.NewEntry()
	u.apiID.SetPlaceHolder("例如 1234567")
	u.apiHash = widget.NewPasswordEntry()
	u.apiHash.SetPlaceHolder("从 my.telegram.org 获取")
	u.botToken = widget.NewPasswordEntry()
	u.botToken.SetPlaceHolder("从 BotFather 获取")

	u.proxyEnabled = widget.NewCheck("使用 SOCKS5", nil)
	u.proxyAddress = widget.NewEntry()
	u.proxyAddress.SetPlaceHolder("host:port，例如 127.0.0.1:1080")
	u.proxyUsername = widget.NewEntry()
	u.proxyUsername.SetPlaceHolder("代理用户名（可选）")
	u.proxyPassword = widget.NewPasswordEntry()
	u.proxyPassword.SetPlaceHolder("代理密码（可选）")

	u.connection = widget.NewLabel("未连接")
	u.channel = widget.NewLabel("尚未绑定频道")
	u.limit = widget.NewLabel("当前上传上限：未获取")
	u.connectButton = widget.NewButton("连接 Telegram", u.connect)
	u.bindButton = widget.NewButton("绑定频道", u.beginBinding)

	u.chooseFolderButton = widget.NewButton("选择文件夹", u.chooseFolder)
	u.folderLabel = widget.NewLabel("尚未选择文件夹")
	u.startButton = widget.NewButton("开始上传", u.startUploads)
	u.cancelAllButton = widget.NewButton("取消全部", u.confirmCancelAll)
	u.moveButton = widget.NewButton("移动全部超限文件", u.chooseMoveDestination)
	u.cancelMoveButton = widget.NewButton("取消移动", u.cancelMove)
	u.progress = widget.NewProgressBar()
	u.progressSummary = widget.NewLabel("总进度：0 B / 0 B")
	u.operationProgress = widget.NewProgressBar()
	u.operationLabel = widget.NewLabel("")
	u.operationProgress.Hide()

	// These controls remain unavailable until a usable Telegram identity and a
	// channel have been selected.
	u.bindButton.Disable()
	u.startButton.Disable()
	u.cancelAllButton.Disable()
	u.moveButton.Disable()
	u.cancelMoveButton.Hide()
}

func (u *window) buildQueue() {
	u.list = widget.NewList(
		func() int { return len(u.snapshot.Jobs) },
		func() fyne.CanvasObject { return newJobRow() },
		u.updateJobRow,
	)
	u.list.HideSeparators = false
}

func newJobRow() *jobRow {
	name := widget.NewLabel("")
	details := widget.NewLabel("")
	status := widget.NewLabel("")
	progress := widget.NewProgressBar()
	cancel := widget.NewButton("取消", nil)
	retry := widget.NewButton("重试", nil)
	skip := widget.NewButton("跳过", nil)
	markSent := widget.NewButton("已发送", nil)

	actions := container.NewHBox(cancel, retry, skip, markSent)
	body := container.NewVBox(name, details, progress, status)
	return &jobRow{
		Container: container.NewBorder(nil, nil, nil, actions, body),
		name:      name,
		details:   details,
		status:    status,
		progress:  progress,
		cancel:    cancel,
		retry:     retry,
		skip:      skip,
		markSent:  markSent,
	}
}

// jobRow is used as a pooled widget.List row.  UpdateJobRow must rebind all
// callbacks because Fyne reuses the same row for different item IDs while
// scrolling.
type jobRow struct {
	*fyne.Container
	name     *widget.Label
	details  *widget.Label
	status   *widget.Label
	progress *widget.ProgressBar
	cancel   *widget.Button
	retry    *widget.Button
	skip     *widget.Button
	markSent *widget.Button
}

func (u *window) updateJobRow(id widget.ListItemID, object fyne.CanvasObject) {
	row, ok := object.(*jobRow)
	if !ok || int(id) < 0 || int(id) >= len(u.snapshot.Jobs) {
		return
	}
	job := u.snapshot.Jobs[int(id)]
	row.name.SetText(job.Name)
	row.details.SetText(fmt.Sprintf("%s · %s", formatBytes(job.Size), job.Path))
	row.status.SetText(jobStatus(job))
	row.progress.SetValue(jobFraction(job))

	row.cancel.Hide()
	row.retry.Hide()
	row.skip.Hide()
	row.markSent.Hide()
	jobID := job.ID
	switch job.State {
	case model.JobQueued, model.JobInterrupted, model.JobUploading, model.JobSending:
		row.cancel.Show()
		row.cancel.OnTapped = func() { u.cancelJob(jobID) }
	case model.JobFailed, model.JobConfirming:
		row.retry.Show()
		row.retry.OnTapped = func() { u.retryJob(jobID) }
		row.skip.Show()
		row.skip.OnTapped = func() { u.skipJob(jobID) }
		if job.State == model.JobConfirming {
			row.markSent.Show()
			row.markSent.OnTapped = func() { u.confirmMarkSent(jobID) }
		}
	case model.JobCancelled, model.JobSkipped:
		row.retry.Show()
		row.retry.OnTapped = func() { u.retryJob(jobID) }
	}
	row.Container.Refresh()
}

func (u *window) applyLoadedSettings() {
	if u.settings.APIID > 0 {
		u.apiID.SetText(strconv.Itoa(u.settings.APIID))
	}
	u.proxyEnabled.SetChecked(u.settings.ProxyEnabled)
	u.proxyAddress.SetText(u.settings.ProxyAddress)
	u.proxyUsername.SetText(u.settings.ProxyUsername)
	if u.settings.LastFolder != "" {
		u.folderLabel.SetText(u.settings.LastFolder)
	}

	// Missing keyring entries are normal on first launch.  In particular, do
	// not show their implementation errors as startup failures.
	if token, err := u.secrets.GetBotToken(); err == nil {
		u.botToken.SetText(token)
	}
	if hash, err := u.secrets.GetAPIHash(); err == nil {
		u.apiHash.SetText(hash)
	}
	if password, err := u.secrets.GetProxyPassword(); err == nil {
		u.proxyPassword.SetText(password)
	}
}

func (u *window) startObservers() {
	go u.observeController()
}

func (u *window) observeController() {
	for {
		select {
		case snapshot := <-u.controller.Updates():
			current := snapshot
			u.doUI(func() { u.applySnapshot(current) })
		case <-u.rootCtx.Done():
			return
		}
	}
}

func (u *window) applySnapshot(snapshot coreapp.Snapshot) {
	if u.isClosed() {
		return
	}
	u.snapshot = snapshot
	if snapshot.Folder != "" {
		u.folderLabel.SetText(snapshot.Folder)
	}
	if snapshot.Channel.ID != 0 {
		title := snapshot.Channel.Title
		if title == "" {
			title = fmt.Sprintf("频道 %d", snapshot.Channel.ID)
		}
		u.channel.SetText("已绑定：" + title)
	}
	if snapshot.TotalBytes > 0 {
		u.progress.SetValue(float64(snapshot.DoneBytes) / float64(snapshot.TotalBytes))
	} else {
		u.progress.SetValue(0)
	}
	u.progressSummary.SetText(formatSummary(snapshot))
	if snapshot.LastError != "" && !snapshot.Running {
		// The controller keeps the last error for restart diagnostics.  The UI
		// displays it in the row/state summary rather than opening a dialog on
		// every coalesced snapshot.
		u.operationLabel.SetText(snapshot.LastError)
	}
	u.list.Refresh()
	u.updateActionAvailability()
	if !snapshot.Running {
		u.stopSleepGuard()
	}
}

func (u *window) updateActionAvailability() {
	if u.isMoveInFlight() {
		u.startButton.Disable()
		u.chooseFolderButton.Disable()
		u.moveButton.Disable()
		u.cancelAllButton.Disable()
		u.cancelMoveButton.Show()
		return
	}
	if u.snapshot.Running {
		u.startButton.Disable()
		u.chooseFolderButton.Disable()
		u.moveButton.Disable()
		u.cancelAllButton.Enable()
	} else {
		u.chooseFolderButton.Enable()
		u.cancelAllButton.Disable()
		if u.hasOversizeJobs() {
			u.moveButton.Enable()
		} else {
			u.moveButton.Disable()
		}
		if u.connected && u.snapshot.Channel.ID != 0 && hasRunnableJobs(u.snapshot.Jobs) {
			u.startButton.Enable()
		} else {
			u.startButton.Disable()
		}
	}
	if !u.connected {
		u.bindButton.Disable()
	}
}

func (u *window) isMoveInFlight() bool {
	u.moveMu.Lock()
	defer u.moveMu.Unlock()
	return u.moveInFlight
}

func (u *window) connect() {
	appID, err := strconv.Atoi(strings.TrimSpace(u.apiID.Text))
	if err != nil || appID <= 0 {
		u.showError(errors.New("API ID 必须是正整数"))
		return
	}
	apiHash := strings.TrimSpace(u.apiHash.Text)
	botToken := strings.TrimSpace(u.botToken.Text)
	if apiHash == "" || botToken == "" {
		u.showError(errors.New("API Hash 和 Bot Token 不能为空"))
		return
	}

	proxyEnabled := u.proxyEnabled.Checked
	proxyAddress := strings.TrimSpace(u.proxyAddress.Text)
	proxyUsername := strings.TrimSpace(u.proxyUsername.Text)
	proxyPassword := u.proxyPassword.Text

	u.connectButton.Disable()
	u.connection.SetText("正在保存凭据并连接……")
	go u.connectAsync(appID, apiHash, botToken, proxyEnabled, proxyAddress, proxyUsername, proxyPassword)
}

func (u *window) connectAsync(appID int, apiHash, botToken string, proxyEnabled bool, proxyAddress, proxyUsername, proxyPassword string) {
	settings := u.settingsSnapshot()
	settings.APIID = appID
	settings.ProxyEnabled = proxyEnabled
	settings.ProxyAddress = proxyAddress
	settings.ProxyUsername = proxyUsername
	u.settingsMu.Lock()
	u.settings = settings
	u.settingsMu.Unlock()
	if err := coreapp.SaveSettings(u.paths.Settings, settings); err != nil {
		u.showError(err)
		u.doUI(func() { u.connectButton.Enable(); u.connection.SetText("未连接") })
		return
	}
	if err := u.secrets.SetAPIHash(apiHash); err != nil {
		u.showError(err)
		u.doUI(func() { u.connectButton.Enable(); u.connection.SetText("未连接") })
		return
	}
	if err := u.secrets.SetBotToken(botToken); err != nil {
		u.showError(err)
		u.doUI(func() { u.connectButton.Enable(); u.connection.SetText("未连接") })
		return
	}
	if proxyPassword == "" {
		if err := u.secrets.DeleteProxyPassword(); err != nil && !errors.Is(err, credentials.ErrNotFound) {
			u.showError(err)
			u.doUI(func() { u.connectButton.Enable(); u.connection.SetText("未连接") })
			return
		}
	} else if err := u.secrets.SetProxyPassword(proxyPassword); err != nil {
		u.showError(err)
		u.doUI(func() { u.connectButton.Enable(); u.connection.SetText("未连接") })
		return
	}

	var proxyConfig *tgtransport.ProxyConfig
	if proxyEnabled && proxyAddress != "" {
		proxyConfig = &tgtransport.ProxyConfig{
			Address:  proxyAddress,
			Username: proxyUsername,
			Password: proxyPassword,
		}
	}
	client, err := tgtransport.NewClient(tgtransport.Config{
		AppID:          appID,
		APIHash:        apiHash,
		BotToken:       botToken,
		Proxy:          proxyConfig,
		SessionStorage: credentials.NewSessionStorage(u.secrets, u.paths.Session),
	}, tgtransport.Events{
		OnConnectionState: func(state tgtransport.ConnectionState) {
			switch state {
			case tgtransport.StateConnecting:
				u.setConnectionStatus("正在连接 Telegram……")
			case tgtransport.StateReady:
				u.setConnectionStatus("Telegram 连接已建立")
			case tgtransport.StateDisconnected:
				u.setConnectionStatus("Telegram 连接已断开")
			}
		},
		OnFloodWait: func(wait time.Duration) {
			u.setConnectionStatus("Telegram 要求等待 " + formatDuration(wait))
		},
	})
	if err != nil {
		u.showError(err)
		u.doUI(func() { u.connectButton.Enable(); u.connection.SetText("未连接") })
		return
	}

	ctx, cancel := context.WithCancel(u.rootCtx)
	u.clientMu.Lock()
	u.client = client
	u.clientCancel = cancel
	u.clientMu.Unlock()
	u.controller.SetGateway(client)

	u.doUI(func() { u.connection.SetText("正在启动 Telegram 客户端……") })
	go u.runClient(client, ctx)
	go u.waitClientReady(client, ctx)
	go u.observeBinding(client, ctx)
}

func (u *window) runClient(client *tgtransport.Client, ctx context.Context) {
	err := client.Run(ctx)
	if errors.Is(err, tgtransport.ErrWrongBotSession) {
		if removeErr := os.Remove(u.paths.Session); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("清理旧 Bot 会话失败：%w", removeErr))
		} else {
			err = fmt.Errorf("%w；旧的加密 Bot 会话已清理，请重新连接", err)
		}
	}
	// A failed Run must also stop the readiness and binding observers.  A
	// normal close already cancelled this context, so this is harmless there.
	u.clientMu.Lock()
	if u.client == client && u.clientCancel != nil {
		u.clientCancel()
		u.clientCancel = nil
	}
	u.clientMu.Unlock()
	if err != nil && !errors.Is(err, context.Canceled) && !u.isClosed() {
		u.showError(err)
	}
	u.doUI(func() {
		if u.currentClient() != client {
			return
		}
		u.connected = false
		u.identity = tgtransport.Identity{}
		u.controller.SetGateway(nil)
		u.connectButton.Enable()
		u.bindButton.Disable()
		u.limit.SetText("当前上传上限：未获取")
		u.updateActionAvailability()
	})
}

func (u *window) waitClientReady(client *tgtransport.Client, ctx context.Context) {
	identity, err := client.WaitReady(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !u.isClosed() {
			u.showError(err)
		}
		u.doUI(func() {
			if u.currentClient() == client {
				u.connectButton.Enable()
				u.connection.SetText("未连接")
			}
		})
		return
	}
	u.doUI(func() {
		if u.currentClient() != client || u.isClosed() {
			return
		}
		u.connected = true
		u.identity = identity
		u.connectButton.Disable()
		u.bindButton.Enable()
		u.connection.SetText("已连接 Bot：@" + identity.Username)
		limitPrefix := "当前上传上限："
		if !identity.MaxUploadExact {
			limitPrefix = "协议上限（服务端动态值未确认）："
		}
		u.limit.SetText(limitPrefix + formatBytes(identity.MaxUploadBytes))
		u.applySnapshot(u.controller.Snapshot())
	})

	// A folder selected before connection is scanned only after the server's
	// current upload limit is known.
	folder := u.settingsSnapshot().LastFolder
	if folder != "" && u.controller.Snapshot().Folder == "" {
		u.scanFolder(folder, identity.MaxUploadBytes)
	}
}

func (u *window) observeBinding(client *tgtransport.Client, ctx context.Context) {
	for {
		select {
		case event, ok := <-client.BindingEvents():
			if !ok {
				return
			}
			if event.Err != nil {
				u.showError(event.Err)
				continue
			}
			channel := event.Channel
			if err := u.controller.SetChannel(channel); err != nil {
				u.showError(err)
				continue
			}
			u.doUI(func() {
				u.bindMu.Lock()
				d := u.bindDialog
				u.bindDialog = nil
				u.bindMu.Unlock()
				if d != nil {
					d.Dismiss()
				}
				title := channel.Title
				if title == "" {
					title = fmt.Sprintf("频道 %d", channel.ID)
				}
				u.channel.SetText("已绑定：" + title)
				u.operationLabel.SetText("频道绑定成功")
				u.updateActionAvailability()
			})
		case <-ctx.Done():
			return
		}
	}
}

func (u *window) beginBinding() {
	client := u.currentClient()
	if client == nil || !u.connected {
		u.showError(errors.New("请先连接 Telegram Bot"))
		return
	}
	code, err := client.BeginChannelBinding()
	if err != nil {
		u.showError(err)
		return
	}

	instruction := widget.NewLabel("请把 Bot 设为目标频道管理员，并在目标频道发送下面这条临时消息。验证成功后应用会自动保存频道信息。")
	codeEntry := widget.NewEntry()
	codeEntry.SetText(code)
	copyButton := widget.NewButton("复制验证码", func() {
		u.application.Clipboard().SetContent(code)
		u.operationLabel.SetText("验证码已复制")
	})
	content := container.NewVBox(instruction, codeEntry)
	d := dialog.NewCustom("绑定频道", "关闭", content, u.window)
	d.SetOnClosed(func() {
		u.bindMu.Lock()
		if u.bindDialog == d {
			u.bindDialog = nil
		}
		u.bindMu.Unlock()
	})
	d.SetButtons([]fyne.CanvasObject{copyButton, widget.NewButton("关闭", func() { d.Dismiss() })})
	u.bindMu.Lock()
	u.bindDialog = d
	u.bindMu.Unlock()
	d.Show()
}

func (u *window) chooseFolder() {
	dialog.ShowFolderOpen(func(uri fyne.ListableURI, err error) {
		if err != nil {
			u.showError(err)
			return
		}
		if uri == nil {
			return
		}
		if uri.Scheme() != "file" {
			u.showError(errors.New("请选择本地文件夹"))
			return
		}
		folder := filepath.Clean(uri.Path())
		if folder == "." || folder == "" {
			u.showError(errors.New("没有读取到有效的文件夹路径"))
			return
		}
		u.settingsMu.Lock()
		u.settings.LastFolder = folder
		settings := u.settings
		u.settingsMu.Unlock()
		if err := coreapp.SaveSettings(u.paths.Settings, settings); err != nil {
			u.showError(err)
			return
		}
		u.folderLabel.SetText(folder)
		if !u.connected {
			u.operationLabel.SetText("文件夹已选择，连接 Bot 后会按当前上传上限扫描")
			return
		}
		u.scanFolder(folder, u.identity.MaxUploadBytes)
	}, u.window)
}

func (u *window) scanFolder(folder string, maxBytes int64) {
	u.doUI(func() {
		u.chooseFolderButton.Disable()
		u.operationLabel.SetText("正在扫描当前文件夹……")
	})
	go func() {
		err := u.controller.Scan(folder, maxBytes)
		if err != nil {
			u.showError(err)
		}
		u.doUI(func() {
			u.chooseFolderButton.Enable()
			if err == nil {
				u.operationLabel.SetText("扫描完成")
			}
		})
	}()
}

func (u *window) startUploads() {
	if err := u.controller.Start(u.rootCtx); err != nil {
		u.showError(err)
		return
	}
	status := "上传已开始；文件会按自然文件名顺序逐条发送"
	if guard, err := platform.PreventSleep(); err != nil {
		status += "；无法阻止系统休眠：" + err.Error()
	} else {
		u.sleepMu.Lock()
		old := u.sleepGuard
		u.sleepGuard = guard
		u.sleepMu.Unlock()
		if old != nil {
			_ = old.Stop()
		}
	}
	u.operationLabel.SetText(status)
}

func (u *window) confirmCancelAll() {
	dialog.ShowConfirm("取消全部上传", "取消会终止当前文件并取消所有尚未发送的文件，已经发送的消息不会删除。确定继续吗？", func(ok bool) {
		if ok {
			if err := u.controller.CancelAll(); err != nil {
				u.showError(err)
				return
			}
			u.operationLabel.SetText("已请求取消全部上传")
		}
	}, u.window)
}

func (u *window) cancelJob(id string) {
	if err := u.controller.CancelJob(id); err != nil {
		u.showError(err)
	}
}

func (u *window) retryJob(id string) {
	for _, job := range u.snapshot.Jobs {
		if job.ID == id && job.State == model.JobConfirming {
			dialog.ShowConfirm("确认重新上传", "请先检查频道，确认这条视频没有出现。重新上传仍可能产生重复消息，确定继续吗？", func(ok bool) {
				if ok {
					u.retryJobNow(id)
				}
			}, u.window)
			return
		}
	}
	u.retryJobNow(id)
}

func (u *window) retryJobNow(id string) {
	if err := u.controller.Retry(id); err != nil {
		u.showError(err)
	}
}

func (u *window) confirmMarkSent(id string) {
	dialog.ShowConfirm("标记为已发送", "仅当你已在频道中看到这条视频时使用。标记后程序会继续后续任务，确定吗？", func(ok bool) {
		if !ok {
			return
		}
		if err := u.controller.MarkSent(id); err != nil {
			u.showError(err)
		}
	}, u.window)
}

func (u *window) skipJob(id string) {
	if err := u.controller.Skip(id); err != nil {
		u.showError(err)
	}
}

func (u *window) chooseMoveDestination() {
	dialog.ShowFolderOpen(func(uri fyne.ListableURI, err error) {
		if err != nil {
			u.showError(err)
			return
		}
		if uri == nil {
			return
		}
		if uri.Scheme() != "file" {
			u.showError(errors.New("请选择本地目标文件夹"))
			return
		}
		destination := filepath.Clean(uri.Path())
		ctx, cancel := context.WithCancel(u.rootCtx)
		u.moveMu.Lock()
		if u.moveInFlight {
			u.moveMu.Unlock()
			cancel()
			return
		}
		u.moveInFlight = true
		u.moveCancel = cancel
		u.moveLastUI = time.Time{}
		u.moveMu.Unlock()
		u.moveButton.Disable()
		u.cancelMoveButton.Show()
		u.operationProgress.Show()
		u.operationLabel.Show()
		u.operationLabel.SetText("正在移动超限文件……")
		go func() {
			err := u.controller.MoveOversize(ctx, destination, u.moveProgress)
			cancel()
			u.moveMu.Lock()
			u.moveInFlight = false
			u.moveCancel = nil
			u.moveMu.Unlock()
			if err != nil && !errors.Is(err, context.Canceled) {
				u.showError(err)
			}
			u.doUI(func() {
				u.operationProgress.Hide()
				u.cancelMoveButton.Hide()
				u.operationLabel.SetText(mapOperationResult(err, "超限文件移动完成"))
				u.updateActionAvailability()
			})
		}()
	}, u.window)
}

func (u *window) cancelMove() {
	u.moveMu.Lock()
	cancel := u.moveCancel
	u.moveMu.Unlock()
	if cancel != nil {
		cancel()
		u.operationLabel.SetText("正在取消文件移动……")
	}
}

func (u *window) moveProgress(progress model.Progress) {
	now := progress.At
	if now.IsZero() {
		now = time.Now()
	}
	u.moveMu.Lock()
	if !u.moveLastUI.IsZero() && now.Sub(u.moveLastUI) < 100*time.Millisecond && progress.BytesDone < progress.BytesTotal {
		u.moveMu.Unlock()
		return
	}
	u.moveLastUI = now
	u.moveMu.Unlock()
	u.doUI(func() {
		if progress.BytesTotal > 0 {
			u.operationProgress.SetValue(float64(progress.BytesDone) / float64(progress.BytesTotal))
		}
		u.operationLabel.SetText(fmt.Sprintf("移动：%s / %s", formatBytes(progress.BytesDone), formatBytes(progress.BytesTotal)))
	})
}

func (u *window) closeIntercept() {
	if u.snapshot.Running {
		dialog.ShowConfirm("退出并取消上传", "当前仍有上传任务。退出会取消当前和排队中的上传，但不会删除已经发送的消息。确定退出吗？", func(ok bool) {
			if ok {
				u.forceClose()
			}
		}, u.window)
		return
	}
	u.forceClose()
}

func (u *window) forceClose() {
	u.closedMu.Lock()
	if u.closed {
		u.closedMu.Unlock()
		return
	}
	u.closed = true
	u.closedMu.Unlock()

	if u.snapshot.Running {
		u.controller.CancelAll()
	}
	u.moveMu.Lock()
	if u.moveCancel != nil {
		u.moveCancel()
	}
	u.moveMu.Unlock()
	u.clientMu.Lock()
	if u.clientCancel != nil {
		u.clientCancel()
	}
	u.clientCancel = nil
	u.clientMu.Unlock()
	u.rootCancel()
	u.stopSleepGuard()
	u.window.Close()
}

func (u *window) stopSleepGuard() {
	u.sleepMu.Lock()
	guard := u.sleepGuard
	u.sleepGuard = nil
	u.sleepMu.Unlock()
	if guard != nil {
		go func() {
			if err := guard.Stop(); err != nil && !u.isClosed() {
				u.showError(fmt.Errorf("恢复系统休眠设置失败：%w", err))
			}
		}()
	}
}

func (u *window) settingsSnapshot() coreapp.Settings {
	u.settingsMu.Lock()
	defer u.settingsMu.Unlock()
	return u.settings
}

func (u *window) currentClient() *tgtransport.Client {
	u.clientMu.RLock()
	defer u.clientMu.RUnlock()
	return u.client
}

func (u *window) isClosed() bool {
	u.closedMu.RLock()
	defer u.closedMu.RUnlock()
	return u.closed
}

func (u *window) doUI(fn func()) {
	if u.isClosed() {
		return
	}
	fyne.Do(func() {
		if !u.isClosed() {
			fn()
		}
	})
}

func (u *window) setConnectionStatus(status string) {
	u.doUI(func() { u.connection.SetText(status) })
}

func (u *window) showError(err error) {
	if err == nil || u.isClosed() {
		return
	}
	u.doUI(func() { dialog.ShowError(err, u.window) })
}

func hasRunnableJobs(jobs []model.Job) bool {
	for _, job := range jobs {
		if job.State == model.JobQueued || job.State == model.JobInterrupted {
			return true
		}
	}
	return false
}

func (u *window) hasOversizeJobs() bool {
	for _, job := range u.snapshot.Jobs {
		if job.State == model.JobOversize {
			return true
		}
	}
	return false
}

func jobFraction(job model.Job) float64 {
	switch job.State {
	case model.JobSent, model.JobMoved:
		return 1
	case model.JobUploading, model.JobSending:
		if job.Size > 0 {
			fraction := float64(job.Uploaded) / float64(job.Size)
			if fraction < 0 {
				return 0
			}
			if fraction > 1 {
				return 1
			}
			return fraction
		}
	}
	return 0
}

func jobStatus(job model.Job) string {
	switch job.State {
	case model.JobQueued:
		return "等待上传"
	case model.JobOversize:
		return "超过 Bot 当前上传上限，未上传"
	case model.JobUploading:
		return fmt.Sprintf("上传中 · %s/s", formatBytes(int64(job.BytesPerSecond)))
	case model.JobSending:
		return "正在提交频道消息……"
	case model.JobConfirming:
		if job.Error != "" {
			return "待确认：" + job.Error
		}
		return "待确认消息是否已送达"
	case model.JobSent:
		return "已发送"
	case model.JobCancelled:
		return "已取消"
	case model.JobFailed:
		if job.Error != "" {
			return "失败：" + job.Error
		}
		return "失败"
	case model.JobSkipped:
		return "已跳过"
	case model.JobMoved:
		return "已移动到超限文件夹"
	case model.JobMoving:
		return "正在安全移动……"
	case model.JobInterrupted:
		return "上次运行中断，可重试"
	default:
		return string(job.State)
	}
}

func formatSummary(snapshot coreapp.Snapshot) string {
	countSent := 0
	for _, job := range snapshot.Jobs {
		if job.State == model.JobSent || job.State == model.JobMoved {
			countSent++
		}
	}
	message := fmt.Sprintf("总进度：%s / %s · 已完成 %d / %d", formatBytes(snapshot.DoneBytes), formatBytes(snapshot.TotalBytes), countSent, len(snapshot.Jobs))
	if snapshot.BytesPerSecond > 0 {
		message += fmt.Sprintf(" · %s/s · ETA %s", formatBytes(int64(snapshot.BytesPerSecond)), formatDuration(snapshot.ETA))
	}
	return message
}

func formatBytes(bytes int64) string {
	if bytes < 0 {
		bytes = 0
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	value := float64(bytes)
	unit := 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%d %s", bytes, units[unit])
	}
	return fmt.Sprintf("%.1f %s", value, units[unit])
}

func formatDuration(duration time.Duration) string {
	if duration <= 0 {
		return "—"
	}
	seconds := int64(duration.Round(time.Second) / time.Second)
	hours := seconds / 3600
	seconds %= 3600
	minutes := seconds / 60
	seconds %= 60
	if hours > 0 {
		return fmt.Sprintf("%dh %02dm", hours, minutes)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm %02ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}

func mapOperationResult(err error, success string) string {
	if err == nil {
		return success
	}
	if errors.Is(err, context.Canceled) {
		return "操作已取消"
	}
	return "操作失败：" + err.Error()
}
