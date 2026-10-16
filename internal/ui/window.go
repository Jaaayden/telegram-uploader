// Package ui contains the small desktop user interface for the uploader.
//
// The UI deliberately owns no upload logic.  It observes app.Controller
// snapshots and sends user actions back to the controller.  This keeps the
// Telegram transport and queue usable from tests and from a future CLI.
package ui

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	fyneapp "fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	coreapp "github.com/Jaaayden/telegram-uploader/internal/app"
	"github.com/Jaaayden/telegram-uploader/internal/credentials"
	"github.com/Jaaayden/telegram-uploader/internal/model"
	"github.com/Jaaayden/telegram-uploader/internal/platform"
	tgtransport "github.com/Jaaayden/telegram-uploader/internal/telegram"
)

const (
	connectionRetryInitial = 5 * time.Second
	connectionRetryMaximum = time.Minute
)

const (
	uploadConcurrencyCompatibilityLabel = "兼容（4 路）"
	uploadConcurrencyBalancedLabel      = "均衡（8 路，推荐）"
	uploadConcurrencyFastLabel          = "高速（12 路）"
)

// Run creates and runs the single-window desktop application. It returns when
// the window is closed. A complete saved configuration is connected
// automatically; first-run and configuration failures remain available from
// the settings page without blocking queue management. Each non-nil notice
// (for example a queue file that could not be read at startup) is shown to the
// user once the window is visible.
func Run(controller *coreapp.Controller, paths coreapp.Paths, secrets *credentials.Store, notices ...error) {
	if controller == nil {
		return
	}
	if secrets == nil {
		secrets = credentials.NewStore()
	}

	settings, settingsErr := coreapp.LoadSettings(paths.Settings)
	application := fyneapp.NewWithID("com.jayden.telegramvideouploader")
	window := application.NewWindow("Telegram 视频顺序上传器")
	u := newWindow(application, window, controller, paths, secrets, settings)
	u.settingsLoadErr = settingsErr
	u.build()
	u.startObservers()
	u.window.Show()
	for _, notice := range notices {
		u.showError(notice)
	}
	u.autoConnectIfConfigured()
	u.application.Run()
}

// ShowFatal displays an unrecoverable startup error in a small window and
// returns when the user closes it. It exists because a GUI build has no
// visible stderr.
func ShowFatal(message string) {
	application := fyneapp.NewWithID("com.jayden.telegramvideouploader")
	win := application.NewWindow("Telegram 视频顺序上传器")
	label := widget.NewLabel(message)
	label.Wrapping = fyne.TextWrapWord
	win.SetContent(container.NewVBox(label, widget.NewButton("关闭", win.Close)))
	win.Resize(fyne.NewSize(480, 160))
	win.SetFixedSize(true)
	win.ShowAndRun()
}

type window struct {
	application fyne.App
	window      fyne.Window
	controller  *coreapp.Controller
	paths       coreapp.Paths
	secrets     *credentials.Store

	settingsMu          sync.Mutex
	settings            coreapp.Settings
	settingsLoadErr     error
	credentialLoadErr   error
	connectionConfigErr error
	scanMu              sync.Mutex
	scanning            bool

	rootCtx    context.Context
	rootCancel context.CancelFunc

	clientMu             sync.RWMutex
	client               *tgtransport.Client
	clientCancel         context.CancelFunc
	clientGeneration     uint64
	reconnectRequested   bool
	appliedConnection    connectionRequest
	hasAppliedConnection bool
	pendingReconnect     *connectionRequest
	// connectionStarter is a narrow test seam. Production leaves it nil and
	// uses connectAsync; tests can observe startup policy without contacting
	// Telegram or replacing the transport implementation.
	connectionStarter func(connectionRequest, bool, uint64)

	closedMu sync.RWMutex
	closed   bool

	connected  bool
	connecting bool
	identity   tgtransport.Identity
	snapshot   coreapp.Snapshot

	connectionRetryMu    sync.Mutex
	connectionRetryTimer *time.Timer

	snapshotDispatchMu     sync.Mutex
	pendingSnapshot        coreapp.Snapshot
	snapshotDispatchQueued bool

	// Settings controls.
	apiID                    *widget.Entry
	apiHash                  *widget.Entry
	botToken                 *widget.Entry
	proxyEnabled             *widget.Check
	proxyAddress             *widget.Entry
	proxyUsername            *widget.Entry
	proxyPassword            *widget.Entry
	uploadConcurrency        *widget.Select
	connectButton            *widget.Button
	bindButton               *widget.Button
	connection               *widget.Label
	channel                  *widget.Label
	limit                    *widget.Label
	settingsButton           *widget.Button
	backButton               *widget.Button
	mainConnection           *widget.Label
	mainChannel              *widget.Label
	readinessHint            *widget.Label
	mainPage                 *fyne.Container
	settingsPage             *fyne.Container
	connectionConfigEditable bool
	connectionConfigStateSet bool

	// Queue controls.
	folderLabel         *widget.Label
	chooseFolderButton  *widget.Button
	startButton         *widget.Button
	pauseButton         *widget.Button
	scheduleButton      *widget.Button
	cancelSchedule      *widget.Button
	scheduleLabel       *widget.Label
	cancelAllButton     *widget.Button
	moveButton          *widget.Button
	cancelMoveButton    *widget.Button
	selectAllButton     *widget.Button
	selectNoneButton    *widget.Button
	resetJobsButton     *widget.Button
	removeSelected      *widget.Button
	removeCompleted     *widget.Button
	clearQueueButton    *widget.Button
	selectionLabel      *widget.Label
	queueList           *widget.List
	queueJobs           []model.Job
	queueIndex          map[string]int
	queueRenderState    map[string]jobRenderState
	queueStats          queueSnapshotStats
	queueRowPool        []*jobRow
	selectedJobs        map[string]bool
	queueRowCreateCount int
	queueSelectable     bool
	progress            *widget.ProgressBar
	progressSummary     *widget.Label
	operationProgress   *widget.ProgressBar
	operationLabel      *widget.Label

	bindMu     sync.Mutex
	bindDialog *dialog.CustomDialog

	candidateDialog *dialog.CustomDialog

	moveMu       sync.Mutex
	moveLastUI   time.Time
	moveCancel   context.CancelFunc
	moveInFlight bool

	sleepMu    sync.Mutex
	sleepGuard platform.SleepGuard

	scheduler            *scheduleCoordinator
	scheduleStarting     bool
	connectionRetryDelay time.Duration
}

func newWindow(application fyne.App, fyneWindow fyne.Window, controller *coreapp.Controller, paths coreapp.Paths, secrets *credentials.Store, settings coreapp.Settings) *window {
	ctx, cancel := context.WithCancel(context.Background())
	u := &window{
		application: application,
		window:      fyneWindow,
		controller:  controller,
		paths:       paths,
		secrets:     secrets,
		settings:    settings,
		rootCtx:     ctx,
		rootCancel:  cancel,
	}
	u.scheduler = newScheduleCoordinator(func() {
		u.doUI(u.tryScheduledStart)
	})
	return u
}

func (u *window) showSettingsPage() {
	if u.mainPage == nil || u.settingsPage == nil {
		return
	}
	u.mainPage.Hide()
	u.settingsPage.Show()
}

func (u *window) showMainPage() {
	if u.mainPage == nil || u.settingsPage == nil {
		return
	}
	u.settingsPage.Hide()
	u.mainPage.Show()
}

func (u *window) build() {
	u.buildFields()
	u.buildQueue()
	uploadPerformanceHint := widget.NewLabel("高速档可能改善高延迟网络，也会使用更多连接；若速度没有提升，请使用均衡档。切换后从下一个视频生效。")
	uploadPerformanceHint.Wrapping = fyne.TextWrapWord

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
		widget.NewSeparator(),
		widget.NewLabelWithStyle("上传性能", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel("上传并发"),
		u.uploadConcurrency,
		uploadPerformanceHint,
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
	settingsHeader := container.NewBorder(
		nil,
		widget.NewSeparator(),
		u.backButton,
		nil,
		widget.NewLabelWithStyle("设置", fyne.TextAlignCenter, fyne.TextStyle{Bold: true}),
	)
	u.settingsPage = container.NewBorder(settingsHeader, nil, nil, nil, container.NewPadded(settingsScroll))

	queueTop := container.NewVBox(
		container.NewBorder(nil, nil, nil, u.chooseFolderButton, u.folderLabel),
		container.NewHBox(u.selectAllButton, u.selectNoneButton, u.resetJobsButton, u.removeSelected, u.removeCompleted, u.clearQueueButton, u.selectionLabel),
		container.NewHBox(u.startButton, u.pauseButton, u.scheduleButton, u.cancelSchedule, u.cancelAllButton, u.moveButton, u.cancelMoveButton),
		u.scheduleLabel,
		u.progress,
		u.progressSummary,
		u.operationProgress,
		u.operationLabel,
	)
	queueContent := container.NewBorder(queueTop, nil, nil, nil, u.queueList)
	status := u.buildMainStatusLine()
	mainHeader := container.NewBorder(nil, widget.NewSeparator(), nil, u.settingsButton, status)
	u.mainPage = container.NewBorder(mainHeader, nil, nil, nil, queueContent)
	u.settingsPage.Hide()
	u.window.SetContent(container.NewPadded(container.NewStack(u.mainPage, u.settingsPage)))
	u.window.Resize(fyne.NewSize(1180, 760))
	u.window.SetCloseIntercept(u.closeIntercept)

	u.applyLoadedSettings()
	u.applySnapshot(u.controller.Snapshot())
}

func (u *window) buildMainStatusLine() *fyne.Container {
	return container.NewBorder(
		nil,
		nil,
		container.NewHBox(u.mainConnection, widget.NewLabel("·"), u.mainChannel, widget.NewLabel("·")),
		nil,
		u.readinessHint,
	)
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
	u.uploadConcurrency = widget.NewSelect(uploadConcurrencyOptions(), nil)
	u.uploadConcurrency.PlaceHolder = uploadConcurrencyBalancedLabel

	u.connection = widget.NewLabel("未连接")
	u.channel = widget.NewLabel("尚未绑定频道")
	u.limit = widget.NewLabel("当前上传上限：未获取")
	u.connectButton = widget.NewButton("连接 Telegram", u.connect)
	u.bindButton = widget.NewButton("绑定频道", u.beginBinding)
	u.settingsButton = widget.NewButton("设置", u.showSettingsPage)
	u.backButton = widget.NewButton("返回队列", u.showMainPage)
	u.mainConnection = widget.NewLabel("Telegram：未连接")
	u.mainChannel = widget.NewLabel("频道：未绑定")
	u.readinessHint = widget.NewLabel("请在设置中完成 Telegram 配置")
	u.readinessHint.Wrapping = fyne.TextWrapOff
	u.readinessHint.Truncation = fyne.TextTruncateEllipsis

	u.chooseFolderButton = widget.NewButton("添加文件夹…", u.chooseFolder)
	u.folderLabel = widget.NewLabel("尚未添加来源文件夹")
	u.startButton = widget.NewButton("开始上传", u.startUploads)
	u.pauseButton = widget.NewButton("暂停队列", u.pauseUploads)
	u.scheduleButton = widget.NewButton("定时开始…", u.showScheduleDialog)
	u.cancelSchedule = widget.NewButton("取消定时", u.cancelScheduledStart)
	u.scheduleLabel = widget.NewLabel("定时：未设置")
	u.cancelAllButton = widget.NewButton("取消全部", u.confirmCancelAll)
	u.moveButton = widget.NewButton("移动全部超限文件", u.chooseMoveDestination)
	u.cancelMoveButton = widget.NewButton("取消移动", u.cancelMove)
	u.selectAllButton = widget.NewButton("全选", u.selectAllJobs)
	u.selectNoneButton = widget.NewButton("取消选择", u.selectNoJobs)
	u.resetJobsButton = widget.NewButton("重置任务…", u.showResetJobsDialog)
	u.removeSelected = widget.NewButton("删除所选", u.confirmRemoveSelected)
	u.removeCompleted = widget.NewButton("删除已完成", u.confirmRemoveCompleted)
	u.clearQueueButton = widget.NewButton("清空队列", u.confirmClearQueue)
	u.selectionLabel = widget.NewLabel("已选择 0 项")
	u.progress = widget.NewProgressBar()
	u.progressSummary = widget.NewLabel("总进度：0 B / 0 B")
	u.operationProgress = widget.NewProgressBar()
	u.operationLabel = widget.NewLabel("")
	u.operationProgress.Hide()

	// These controls remain unavailable until a usable Telegram identity and a
	// channel have been selected.
	u.bindButton.Disable()
	u.startButton.Disable()
	u.pauseButton.Disable()
	u.scheduleButton.Disable()
	u.cancelSchedule.Disable()
	u.cancelAllButton.Disable()
	u.moveButton.Disable()
	u.selectAllButton.Disable()
	u.selectNoneButton.Disable()
	u.resetJobsButton.Disable()
	u.removeSelected.Disable()
	u.removeCompleted.Disable()
	u.clearQueueButton.Disable()
	u.cancelMoveButton.Hide()
}

func (u *window) buildQueue() {
	u.queueIndex = make(map[string]int)
	u.queueRenderState = make(map[string]jobRenderState)
	u.selectedJobs = make(map[string]bool)
	u.queueSelectable = true
	u.queueList = widget.NewList(
		func() int { return len(u.queueJobs) },
		func() fyne.CanvasObject {
			u.queueRowCreateCount++
			row := newJobRow()
			// Fyne's canvas drivers dispatch renderers by the concrete object
			// type. Return the native container, not the jobRow bookkeeping
			// wrapper, or some platforms render an empty list item.
			return row.Container
		},
		func(id widget.ListItemID, object fyne.CanvasObject) {
			row, ok := jobRowFromObject(object)
			if !ok || id < 0 || id >= len(u.queueJobs) {
				return
			}
			row = u.registerQueueRow(row)
			u.updateJobRow(row, u.queueJobs[id])
		},
	)
}

func (u *window) closeIntercept() {
	if u.queueRunning() {
		dialog.ShowConfirm("退出并取消上传", "当前仍有上传任务。退出会取消当前和排队中的上传，但不会删除已经发送的消息。确定退出吗？", func(ok bool) {
			if ok {
				u.forceClose()
			}
		}, u.window)
		return
	}
	if u.scheduler != nil {
		if startAt, set, _ := u.scheduler.State(); set {
			message := "仍有定时开始计划（" + startAt.Local().Format("2006-01-02 15:04") + "）。应用退出期间不会自动启动；计划会保留，并在下次打开应用时恢复。确定退出吗？"
			dialog.ShowConfirm("退出并保留定时计划", message, func(ok bool) {
				if ok {
					u.forceClose()
				}
			}, u.window)
			return
		}
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
	u.stopConnectionRetryTimer()

	if u.queueRunning() && u.controller != nil {
		_ = u.controller.CancelAll()
	}
	u.moveMu.Lock()
	if u.moveCancel != nil {
		u.moveCancel()
	}
	u.moveMu.Unlock()
	u.clientMu.Lock()
	u.clientGeneration++
	u.connecting = false
	u.reconnectRequested = false
	u.pendingReconnect = nil
	if u.clientCancel != nil {
		u.clientCancel()
	}
	u.clientCancel = nil
	u.client = nil
	u.clientMu.Unlock()
	u.controller.SetGateway(nil)
	u.rootCancel()
	if u.scheduler != nil {
		u.scheduler.Stop()
	}
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

func (u *window) updateSettings(update func(*coreapp.Settings)) error {
	u.settingsMu.Lock()
	defer u.settingsMu.Unlock()
	next := u.settings
	update(&next)
	if err := coreapp.SaveSettings(u.paths.Settings, next); err != nil {
		return err
	}
	u.settings = next
	return nil
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
	u.doUI(func() {
		u.connection.SetText(status)
		u.updateReadinessHint()
	})
}

func (u *window) showError(err error) {
	if err == nil || u.isClosed() {
		return
	}
	u.doUI(func() { dialog.ShowError(err, u.window) })
}

func canonicalPathKey(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}
