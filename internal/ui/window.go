// Package ui contains the small desktop user interface for the uploader.
//
// The UI deliberately owns no upload logic.  It observes app.Controller
// snapshots and sends user actions back to the controller.  This keeps the
// Telegram transport and queue usable from tests and from a future CLI.
package ui

import (
	"context"
	"fmt"
	"net/url"
	"os"
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
	"github.com/Jaaayden/telegram-uploader/internal/buildinfo"
	"github.com/Jaaayden/telegram-uploader/internal/credentials"
	"github.com/Jaaayden/telegram-uploader/internal/diagnostics"
	"github.com/Jaaayden/telegram-uploader/internal/model"
	"github.com/Jaaayden/telegram-uploader/internal/platform"
	tgtransport "github.com/Jaaayden/telegram-uploader/internal/telegram"
)

const (
	connectionRetryInitial = 5 * time.Second
	connectionRetryMaximum = time.Minute
	connectionStopTimeout  = 5 * time.Second
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
// user once the window is visible. It reports whether the window was closed
// through the normal close path.
func Run(controller *coreapp.Controller, paths coreapp.Paths, secrets *credentials.Store, notices ...error) bool {
	if controller == nil {
		return false
	}
	if secrets == nil {
		secrets = credentials.NewStore()
	}

	settings, settingsErr := coreapp.LoadSettings(paths.Settings)
	application := fyneapp.NewWithID("com.jayden.telegramvideouploader")
	application.Settings().SetTheme(newAppTheme())
	window := application.NewWindow("Telegram 视频顺序上传器")
	u := newWindow(application, window, controller, paths, secrets, settings)
	u.settingsLoadErr = settingsErr
	u.build()
	if diagnostics.PreviousRunUnclean() {
		u.operationLabel.SetText("检测到上一次运行未正常结束；诊断日志已保留，可在“设置 → 关于”中打开")
	}
	u.startObservers()
	u.window.Show()
	for _, notice := range notices {
		u.showError(notice)
	}
	u.autoConnectIfConfigured()
	u.application.Run()
	clean := u.isClosed()
	if !clean {
		// A native window/driver can end Fyne's event loop without invoking the
		// close intercept. Always release the connection, scheduler and sleep
		// guard, but preserve the durable queue as interrupted rather than
		// rewriting every job as explicitly cancelled.
		u.shutdown(false, false)
	}
	stopDeadline := time.Now().Add(connectionStopTimeout)
	if !u.waitForController(time.Until(stopDeadline)) {
		_ = diagnostics.Logf("queue_stop_timeout timeout_ms=%d", connectionStopTimeout.Milliseconds())
		clean = false
	}
	if !u.waitForConnectionWorkers(time.Until(stopDeadline)) {
		_ = diagnostics.Logf("telegram_workers_stop_timeout timeout_ms=%d", connectionStopTimeout.Milliseconds())
		clean = false
	}
	return clean
}

// ShowFatal displays an unrecoverable startup error in a small window and
// returns when the user closes it. It exists because a GUI build has no
// visible stderr.
func ShowFatal(message string) {
	application := fyneapp.NewWithID("com.jayden.telegramvideouploader")
	application.Settings().SetTheme(newAppTheme())
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

	connectionWorkerMu       sync.Mutex
	connectionWorkers        sync.WaitGroup
	connectionWorkersStopped bool

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
	progressDispatchMu     sync.Mutex
	pendingProgress        coreapp.ProgressUpdate
	progressDispatchQueued bool

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

	repositoryURL, _ := url.Parse(buildinfo.RepositoryURL)
	logDirectory := filepath.Join(u.paths.Root, "logs")
	logDirectoryLabel := widget.NewLabel("诊断日志：" + logDirectory)
	logDirectoryLabel.Wrapping = fyne.TextWrapWord
	logPrivacyHint := widget.NewLabel("日志只保存在本机，不会自动上传；向他人发送 crash 日志前请先检查其中是否含有敏感内容。")
	logPrivacyHint.Wrapping = fyne.TextWrapWord
	lastRunStatus := "上一次运行：已正常结束或没有历史记录"
	if diagnostics.PreviousRunUnclean() {
		lastRunStatus = "上一次运行：未检测到正常关闭，相关日志已保留"
	}
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
		widget.NewSeparator(),
		widget.NewLabelWithStyle("关于", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel(fmt.Sprintf("%s · v%s（build %d）", buildinfo.Name, buildinfo.Version, buildinfo.Build)),
		widget.NewLabel("源码仓库"),
		widget.NewHyperlink(buildinfo.RepositoryURL, repositoryURL),
		widget.NewLabel(lastRunStatus),
		logDirectoryLabel,
		widget.NewButton("打开诊断日志文件夹", u.openLogDirectory),
		logPrivacyHint,
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

func (u *window) dispatchControllerProgress(progress coreapp.ProgressUpdate) {
	if !u.enqueueControllerProgress(progress) {
		return
	}
	u.doUI(func() {
		if current, ok := u.takeControllerProgress(); ok {
			u.applyProgressUpdate(current)
		}
	})
}

func (u *window) enqueueControllerProgress(progress coreapp.ProgressUpdate) bool {
	u.progressDispatchMu.Lock()
	defer u.progressDispatchMu.Unlock()
	u.pendingProgress = progress
	if u.progressDispatchQueued {
		return false
	}
	u.progressDispatchQueued = true
	return true
}

func (u *window) takeControllerProgress() (coreapp.ProgressUpdate, bool) {
	u.progressDispatchMu.Lock()
	defer u.progressDispatchMu.Unlock()
	if !u.progressDispatchQueued {
		return coreapp.ProgressUpdate{}, false
	}
	progress := u.pendingProgress
	u.pendingProgress = coreapp.ProgressUpdate{}
	u.progressDispatchQueued = false
	return progress, true
}

// applyProgressUpdate updates the active row and aggregate counters without
// cloning or walking the complete queue. Full snapshots remain authoritative
// for structural/state changes and reset the local index when a retry or job
// transition occurs.
func (u *window) applyProgressUpdate(update coreapp.ProgressUpdate) {
	if u.isClosed() || update.JobID == "" {
		return
	}
	if u.snapshot.ActiveID != update.JobID {
		return
	}
	// A zero ActiveAttempt is the retry-wait/no-active-upload boundary. Do not
	// treat it as a wildcard: an old attempt arriving after that snapshot must
	// be discarded just like an old attempt after a newer retry.
	if update.AttemptID != u.snapshot.ActiveAttempt {
		return
	}
	index, ok := u.queueIndex[update.JobID]
	if !ok || index < 0 || index >= len(u.queueJobs) || index >= len(u.snapshot.Jobs) {
		return
	}
	job := &u.queueJobs[index]
	snapshotJob := &u.snapshot.Jobs[index]
	if job.ID != update.JobID || snapshotJob.ID != update.JobID {
		return
	}
	if (job.State != model.JobUploading && job.State != model.JobSending) ||
		(snapshotJob.State != model.JobUploading && snapshotJob.State != model.JobSending) {
		return
	}
	if update.Uploaded < job.Uploaded || update.Uploaded < snapshotJob.Uploaded {
		return
	}

	oldUploaded := job.Uploaded
	oldBytesPerSecond := job.BytesPerSecond
	if oldUploaded < 0 {
		oldUploaded = 0
	}
	if oldUploaded > job.Size && job.Size > 0 {
		oldUploaded = job.Size
	}
	newUploaded := update.Uploaded
	if newUploaded < 0 {
		newUploaded = 0
	}
	if job.Size > 0 && newUploaded > job.Size {
		newUploaded = job.Size
	}
	job.Uploaded = newUploaded
	job.BytesPerSecond = update.BytesPerSecond
	snapshotJob.Uploaded = newUploaded
	snapshotJob.BytesPerSecond = update.BytesPerSecond

	delta := newUploaded - oldUploaded
	u.snapshot.DoneBytes += delta
	if u.snapshot.DoneBytes < 0 {
		u.snapshot.DoneBytes = 0
	}
	if u.snapshot.TotalBytes >= 0 && u.snapshot.DoneBytes > u.snapshot.TotalBytes {
		u.snapshot.DoneBytes = u.snapshot.TotalBytes
	}
	u.snapshot.BytesPerSecond += update.BytesPerSecond - oldBytesPerSecond
	if u.snapshot.BytesPerSecond < 0 {
		u.snapshot.BytesPerSecond = 0
	}
	if u.snapshot.BytesPerSecond > 0 && u.snapshot.TotalBytes > u.snapshot.DoneBytes {
		u.snapshot.ETA = time.Duration(float64(u.snapshot.TotalBytes-u.snapshot.DoneBytes)/u.snapshot.BytesPerSecond) * time.Second
	} else {
		u.snapshot.ETA = 0
	}

	state := u.queueRenderState[update.JobID]
	state.Progress = jobFraction(*job)
	state.Status = u.compactJobStatusForSnapshot(*job)
	u.queueRenderState[update.JobID] = state
	for _, row := range u.queueRowPool {
		if row.boundID == update.JobID {
			u.updateJobRow(row, *job)
		}
	}
	if u.snapshot.TotalBytes > 0 {
		u.progress.SetValue(float64(u.snapshot.DoneBytes) / float64(u.snapshot.TotalBytes))
	} else {
		u.progress.SetValue(0)
	}
	u.progressSummary.SetText(formatSummaryWithCompleted(u.snapshot, u.queueStats.Completed))
}

func (u *window) startConnectionWorker(worker func()) bool {
	if worker == nil {
		return false
	}
	u.connectionWorkerMu.Lock()
	if u.connectionWorkersStopped {
		u.connectionWorkerMu.Unlock()
		return false
	}
	u.connectionWorkers.Add(1)
	u.connectionWorkerMu.Unlock()
	go func() {
		defer u.connectionWorkers.Done()
		worker()
	}()
	return true
}

func (u *window) stopConnectionWorkers() {
	u.connectionWorkerMu.Lock()
	u.connectionWorkersStopped = true
	u.connectionWorkerMu.Unlock()
}

func (u *window) waitForConnectionWorkers(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		u.connectionWorkers.Wait()
		close(done)
	}()
	if timeout <= 0 {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func (u *window) waitForController(timeout time.Duration) bool {
	if u.controller == nil {
		return true
	}
	if timeout <= 0 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return u.controller.WaitStopped(ctx) == nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return u.controller.WaitStopped(ctx) == nil
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
	u.shutdown(true, true)
}

// shutdown is the single idempotent owner of process-lifetime resources.
// cancelQueue is reserved for an explicit user close; an unexpected event-loop
// return should interrupt the active operation through rootCancel without
// changing every queued record to an intentional cancellation.
func (u *window) shutdown(cancelQueue, closeWindow bool) {
	u.closedMu.Lock()
	if u.closed {
		u.closedMu.Unlock()
		return
	}
	u.closed = true
	u.closedMu.Unlock()
	_ = diagnostics.Logf("ui_shutdown explicit=%t close_window=%t queue_running=%t", cancelQueue, closeWindow, u.queueRunning())
	u.stopConnectionWorkers()
	u.stopConnectionRetryTimer()

	if cancelQueue && u.controller != nil && u.queueRunning() {
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
	if u.controller != nil {
		u.controller.SetGateway(nil)
	}
	if u.rootCancel != nil {
		u.rootCancel()
	}
	if u.scheduler != nil {
		u.scheduler.Stop()
	}
	u.stopSleepGuard()
	if closeWindow && u.window != nil {
		u.window.Close()
	}
}

func (u *window) stopSleepGuard() {
	u.sleepMu.Lock()
	guard := u.sleepGuard
	u.sleepGuard = nil
	u.sleepMu.Unlock()
	if guard != nil {
		if err := guard.Stop(); err != nil && !u.isClosed() {
			u.showError(fmt.Errorf("恢复系统休眠设置失败：%w", err))
		}
	}
}

func (u *window) openLogDirectory() {
	logDirectory := filepath.Join(u.paths.Root, "logs")
	if err := os.MkdirAll(logDirectory, 0o700); err != nil {
		u.showError(fmt.Errorf("创建诊断日志目录失败：%w", err))
		return
	}
	if u.application == nil {
		return
	}
	if err := u.application.OpenURL(localFileURL(logDirectory)); err != nil {
		u.showError(fmt.Errorf("打开诊断日志目录失败：%w", err))
	}
}

func localFileURL(path string) *url.URL {
	slashPath := strings.ReplaceAll(filepath.ToSlash(filepath.Clean(path)), "\\", "/")
	// url.URL requires a leading slash for a Windows drive path to serialize
	// as file:///C:/..., while Unix absolute paths already have one.
	if len(slashPath) >= 2 && slashPath[1] == ':' && !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	return &url.URL{Scheme: "file", Path: slashPath}
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
