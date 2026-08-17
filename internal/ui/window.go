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
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
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
	"github.com/jayden/telegram-video-uploader/internal/scanner"
	tgtransport "github.com/jayden/telegram-video-uploader/internal/telegram"
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

type connectionRequest struct {
	appID             int
	apiHash           string
	botToken          string
	proxyEnabled      bool
	proxyAddress      string
	proxyUsername     string
	proxyPassword     string
	uploadConcurrency int
}

func uploadConcurrencyOptions() []string {
	return []string{
		uploadConcurrencyCompatibilityLabel,
		uploadConcurrencyBalancedLabel,
		uploadConcurrencyFastLabel,
	}
}

func uploadConcurrencyForOption(option string) (int, bool) {
	switch option {
	case uploadConcurrencyCompatibilityLabel:
		return tgtransport.UploadConcurrencyCompatibility, true
	case uploadConcurrencyBalancedLabel:
		return tgtransport.UploadConcurrencyBalanced, true
	case uploadConcurrencyFastLabel:
		return tgtransport.UploadConcurrencyFast, true
	default:
		return 0, false
	}
}

func uploadConcurrencyOptionFor(value int) string {
	switch value {
	case tgtransport.UploadConcurrencyCompatibility:
		return uploadConcurrencyCompatibilityLabel
	case tgtransport.UploadConcurrencyFast:
		return uploadConcurrencyFastLabel
	case tgtransport.UploadConcurrencyBalanced:
		fallthrough
	default:
		return uploadConcurrencyBalancedLabel
	}
}

func normalizeUploadConcurrency(value int) int {
	if normalized, ok := uploadConcurrencyForOption(uploadConcurrencyOptionFor(value)); ok {
		return normalized
	}
	return tgtransport.DefaultUploadConcurrency
}

// Run creates and runs the single-window desktop application. It returns when
// the window is closed. A complete saved configuration is connected
// automatically; first-run and configuration failures remain available from
// the settings page without blocking queue management.
func Run(controller *coreapp.Controller, paths coreapp.Paths, secrets *credentials.Store) {
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
	u.autoConnectIfConfigured()
	u.application.Run()
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

func (u *window) registerQueueRow(row *jobRow) *jobRow {
	for _, existing := range u.queueRowPool {
		if existing.Container == row.Container {
			return existing
		}
	}
	u.queueRowPool = append(u.queueRowPool, row)
	return row
}

func jobRowFromObject(object fyne.CanvasObject) (*jobRow, bool) {
	rowContainer, ok := object.(*fyne.Container)
	if !ok || len(rowContainer.Objects) != 4 {
		return nil, false
	}
	body, ok := rowContainer.Objects[0].(*fyne.Container)
	if !ok || len(body.Objects) != 3 {
		return nil, false
	}
	name, nameOK := body.Objects[0].(*widget.Label)
	progress, progressOK := body.Objects[1].(*widget.ProgressBar)
	status, statusOK := body.Objects[2].(*widget.Label)
	selected, selectedOK := rowContainer.Objects[2].(*widget.Check)
	action, actionOK := rowContainer.Objects[3].(*widget.Button)
	if !nameOK || !progressOK || !statusOK || !selectedOK || !actionOK {
		return nil, false
	}
	return &jobRow{
		Container: rowContainer,
		selected:  selected,
		name:      name,
		status:    status,
		progress:  progress,
		action:    action,
	}, true
}

func newJobRow() *jobRow {
	selected := widget.NewCheck("", nil)
	name := widget.NewLabelWithStyle("文件名", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	name.Truncation = fyne.TextTruncateEllipsis
	status := widget.NewLabel("")
	status.Truncation = fyne.TextTruncateEllipsis
	progress := widget.NewProgressBar()
	action := widget.NewButton("详情", nil)

	body := container.NewGridWithColumns(3, name, progress, status)
	rowContainer := container.NewBorder(nil, widget.NewSeparator(), selected, action, body)
	return &jobRow{
		Container: rowContainer,
		selected:  selected,
		name:      name,
		status:    status,
		progress:  progress,
		action:    action,
	}
}

// jobRow is bookkeeping around a native Fyne container. The wrapper itself is
// never returned as a CanvasObject because platform renderers dispatch native
// containers by their concrete type.
type jobRow struct {
	*fyne.Container
	selected *widget.Check
	name     *widget.Label
	status   *widget.Label
	progress *widget.ProgressBar
	action   *widget.Button
	boundID  string
}

type jobRenderState struct {
	Position   int
	Name       string
	Status     string
	Action     string
	Progress   float64
	Checked    bool
	Selectable bool
}

type queueSnapshotStats struct {
	Runnable  bool
	Oversize  bool
	Completed int
	Reset     resetJobCounts
}

func (stats *queueSnapshotStats) add(job model.Job) {
	switch job.State {
	case model.JobQueued:
		stats.Runnable = true
	case model.JobInterrupted:
		stats.Runnable = true
		stats.Reset.Failed++
		stats.Reset.All++
	case model.JobOversize:
		stats.Oversize = true
	case model.JobSent, model.JobMoved:
		stats.Completed++
	case model.JobCancelled:
		stats.Reset.Cancelled++
		stats.Reset.All++
	case model.JobFailed:
		stats.Reset.Failed++
		stats.Reset.All++
	case model.JobSkipped:
		stats.Reset.Skipped++
		stats.Reset.All++
	}
}

func (u *window) updateJobRow(row *jobRow, job model.Job) {
	jobID := job.ID
	nameText := fmt.Sprintf("%d. %s", job.Position+1, job.Name)
	statusText := u.compactJobStatusForSnapshot(job)
	progressValue := jobFraction(job)
	selectable := u.queueSelectable
	checked := u.selectedJobs[jobID]

	if row.name.Text != nameText {
		row.name.SetText(nameText)
	}
	if row.status.Text != statusText {
		row.status.SetText(statusText)
	}
	if row.progress.Value != progressValue {
		row.progress.SetValue(progressValue)
	}

	// A List recycles native containers for different IDs. Always detach the
	// previous callback before updating the checkbox, then bind this job ID.
	rebind := row.boundID != jobID
	if row.selected.Checked != checked || rebind {
		previousCallback := row.selected.OnChanged
		row.selected.OnChanged = nil
		if row.selected.Checked != checked {
			row.selected.SetChecked(checked)
		}
		if rebind {
			row.selected.OnChanged = func(checked bool) {
				if row.boundID != jobID {
					return
				}
				if checked {
					u.selectedJobs[jobID] = true
				} else {
					delete(u.selectedJobs, jobID)
				}
				u.updateSelectionControls()
			}
		} else {
			row.selected.OnChanged = previousCallback
		}
	}
	if !selectable && !row.selected.Disabled() {
		row.selected.Disable()
	} else if selectable && row.selected.Disabled() {
		row.selected.Enable()
	}

	actionText := jobActionLabel(job)
	if rebind || row.action.Text != actionText {
		if row.action.Text != actionText {
			row.action.SetText(actionText)
		}
		row.action.OnTapped = func() {
			if row.boundID != jobID {
				return
			}
			switch actionText {
			case "取消":
				u.cancelJob(jobID)
			case "处理…":
				u.showJobActions(jobID)
			case "重试":
				u.retryJob(jobID)
			default:
				u.showJobDetails(jobID)
			}
		}
	}
	row.boundID = jobID
}

func (u *window) refreshQueueRows(jobs []model.Job) {
	orderChanged := len(u.queueJobs) != len(jobs)
	if !orderChanged {
		for i, job := range jobs {
			if u.queueJobs[i].ID != job.ID {
				orderChanged = true
				break
			}
		}
	}

	// Reuse the index and render-state maps. At 10 Hz with a 1000-item queue,
	// allocating three full maps per snapshot creates avoidable UI-thread GC.
	clear(u.queueIndex)
	for index, job := range jobs {
		u.queueIndex[job.ID] = index
	}
	for id := range u.selectedJobs {
		if _, exists := u.queueIndex[id]; !exists {
			delete(u.selectedJobs, id)
		}
	}
	for id := range u.queueRenderState {
		if _, exists := u.queueIndex[id]; !exists {
			delete(u.queueRenderState, id)
		}
	}

	selectable := !u.isMoveInFlight()
	u.queueSelectable = selectable
	var stats queueSnapshotStats
	changed := make([]int, 0, 1)
	for index, job := range jobs {
		stats.add(job)
		state := jobRenderState{
			Position:   job.Position,
			Name:       job.Name,
			Status:     u.compactJobStatusForSnapshot(job),
			Action:     jobActionLabel(job),
			Progress:   jobFraction(job),
			Checked:    u.selectedJobs[job.ID],
			Selectable: selectable,
		}
		if previous, ok := u.queueRenderState[job.ID]; !ok || previous != state {
			changed = append(changed, index)
		}
		u.queueRenderState[job.ID] = state
	}
	u.queueStats = stats
	u.queueJobs = append(u.queueJobs[:0], jobs...)
	if orderChanged || len(changed) > 8 {
		u.queueList.Refresh()
	} else {
		for _, index := range changed {
			job := u.queueJobs[index]
			for _, row := range u.queueRowPool {
				if row.boundID == job.ID {
					u.updateJobRow(row, job)
				}
			}
		}
	}
	u.updateSelectionControls()
}

func jobActionLabel(job model.Job) string {
	switch job.State {
	case model.JobQueued, model.JobUploading, model.JobSending:
		return "取消"
	case model.JobFailed, model.JobConfirming:
		return "处理…"
	case model.JobCancelled, model.JobSkipped, model.JobInterrupted:
		return "重试"
	default:
		return "详情"
	}
}

func (u *window) compactJobStatusForSnapshot(job model.Job) string {
	for _, id := range u.snapshot.PendingRemovalIDs {
		if id == job.ID {
			return "正在取消并删除…"
		}
	}
	return compactJobStatus(job)
}

func (u *window) applyLoadedSettings() {
	if u.uploadConcurrency != nil {
		uploadConcurrency := normalizeUploadConcurrency(u.settingsSnapshot().UploadConcurrency)
		u.uploadConcurrency.OnChanged = nil
		u.uploadConcurrency.SetSelected(uploadConcurrencyOptionFor(uploadConcurrency))
		u.uploadConcurrency.OnChanged = u.handleUploadConcurrencyChanged
	}
	if u.settings.APIID > 0 {
		u.apiID.SetText(strconv.Itoa(u.settings.APIID))
	}
	u.proxyEnabled.SetChecked(u.settings.ProxyEnabled)
	u.proxyAddress.SetText(u.settings.ProxyAddress)
	u.proxyUsername.SetText(u.settings.ProxyUsername)
	if u.settings.LastFolder != "" {
		u.folderLabel.SetText("最近来源：" + u.settings.LastFolder)
	}

	// Missing keyring entries are normal on first launch.  In particular, do
	// not show their implementation errors as startup failures.
	if token, err := u.secrets.GetBotToken(); err == nil {
		u.botToken.SetText(token)
	} else if !errors.Is(err, credentials.ErrNotFound) {
		u.credentialLoadErr = err
	}
	if hash, err := u.secrets.GetAPIHash(); err == nil {
		u.apiHash.SetText(hash)
	} else if !errors.Is(err, credentials.ErrNotFound) && u.credentialLoadErr == nil {
		u.credentialLoadErr = err
	}
	if password, err := u.secrets.GetProxyPassword(); err == nil {
		u.proxyPassword.SetText(password)
	} else if !errors.Is(err, credentials.ErrNotFound) && u.credentialLoadErr == nil {
		u.credentialLoadErr = err
	}
	u.updateReadinessHint()
}

func (u *window) autoConnectIfConfigured() {
	if u.settingsLoadErr != nil {
		u.connection.SetText("设置文件读取失败")
		u.updateReadinessHint()
		return
	}
	if u.credentialLoadErr != nil {
		u.connection.SetText("无法读取系统凭证")
		u.updateReadinessHint()
		return
	}
	if !u.hasCompleteConnectionConfig() {
		u.updateReadinessHint()
		return
	}
	u.connectWithMode(true)
}

func (u *window) hasCompleteConnectionConfig() bool {
	appID, err := strconv.Atoi(strings.TrimSpace(u.apiID.Text))
	return err == nil && appID > 0 && strings.TrimSpace(u.apiHash.Text) != "" && strings.TrimSpace(u.botToken.Text) != ""
}

func (u *window) canAttemptAutomaticConnection() bool {
	u.clientMu.RLock()
	hasApplied := u.hasAppliedConnection
	u.clientMu.RUnlock()
	return hasApplied || u.hasCompleteConnectionConfig()
}

func (u *window) updateReadinessHint() {
	if u.readinessHint == nil || u.mainConnection == nil || u.mainChannel == nil {
		return
	}
	channelText := "频道：未绑定"
	if u.snapshot.Channel.ID != 0 {
		title := u.snapshot.Channel.Title
		if title == "" {
			title = fmt.Sprintf("%d", u.snapshot.Channel.ID)
		}
		channelText = "频道：" + title
	}
	u.mainChannel.SetText(channelText)
	connecting := u.isConnecting()
	switch {
	case u.settingsLoadErr != nil:
		u.mainConnection.SetText("Telegram：配置读取失败")
		u.readinessHint.SetText("设置文件无法读取，请打开设置检查并重新保存")
	case u.credentialLoadErr != nil:
		u.mainConnection.SetText("Telegram：凭证不可用")
		u.readinessHint.SetText("无法读取系统钥匙串，请检查系统权限后重试")
	case u.connectionConfigErr != nil:
		u.mainConnection.SetText("Telegram：连接配置错误")
		u.readinessHint.SetText(u.connectionConfigErr.Error() + "；请打开设置修正后重新连接")
	case u.connected:
		identity := "已连接"
		if u.identity.Username != "" {
			identity += " @" + u.identity.Username
		}
		u.mainConnection.SetText("Telegram：" + identity)
		if u.snapshot.Channel.ID == 0 {
			u.readinessHint.SetText("Telegram 已连接，请在设置中绑定目标频道")
		} else {
			u.readinessHint.SetText("已就绪，可以开始或定时上传")
		}
	case connecting:
		u.mainConnection.SetText("Telegram：正在连接")
		u.readinessHint.SetText("正在连接 Telegram，请稍候…")
	case !u.canAttemptAutomaticConnection():
		u.mainConnection.SetText("Telegram：未配置")
		u.readinessHint.SetText("请打开设置，填写并保存 Telegram 凭证")
	default:
		u.mainConnection.SetText("Telegram：未连接")
		u.readinessHint.SetText("Telegram 当前未连接；程序会自动重试，也可以在设置中重新连接")
	}
}

func (u *window) handleUploadConcurrencyChanged(option string) {
	concurrency, ok := uploadConcurrencyForOption(option)
	if !ok {
		return
	}
	previous := normalizeUploadConcurrency(u.settingsSnapshot().UploadConcurrency)
	if err := u.updateSettings(func(settings *coreapp.Settings) {
		settings.UploadConcurrency = concurrency
	}); err != nil {
		if u.uploadConcurrency != nil {
			u.uploadConcurrency.OnChanged = nil
			u.uploadConcurrency.SetSelected(uploadConcurrencyOptionFor(previous))
			u.uploadConcurrency.OnChanged = u.handleUploadConcurrencyChanged
		}
		u.showError(err)
		return
	}
	u.clientMu.Lock()
	if u.hasAppliedConnection {
		u.appliedConnection.uploadConcurrency = concurrency
	}
	if u.pendingReconnect != nil {
		u.pendingReconnect.uploadConcurrency = concurrency
	}
	client := u.client
	u.clientMu.Unlock()
	if client != nil {
		concurrency = client.SetUploadConcurrency(concurrency)
	}
	u.operationLabel.SetText(fmt.Sprintf("上传并发已设置为 %d 路；当前视频不受影响，将从下一个视频开始生效", concurrency))
}

func (u *window) startObservers() {
	go u.observeController()
	u.restoreScheduledStart()
}

func (u *window) observeController() {
	for {
		select {
		case snapshot, ok := <-u.controller.Updates():
			if !ok {
				return
			}
			u.dispatchControllerSnapshot(snapshot)
		case <-u.rootCtx.Done():
			return
		}
	}
}

func (u *window) dispatchControllerSnapshot(snapshot coreapp.Snapshot) {
	if u.enqueueControllerSnapshot(snapshot) {
		u.doUI(func() {
			if current, ok := u.takeControllerSnapshot(); ok {
				u.applySnapshot(current)
			}
		})
	}
}

func (u *window) enqueueControllerSnapshot(snapshot coreapp.Snapshot) bool {
	u.snapshotDispatchMu.Lock()
	defer u.snapshotDispatchMu.Unlock()
	u.pendingSnapshot = snapshot
	if u.snapshotDispatchQueued {
		return false
	}
	u.snapshotDispatchQueued = true
	return true
}

func (u *window) takeControllerSnapshot() (coreapp.Snapshot, bool) {
	u.snapshotDispatchMu.Lock()
	defer u.snapshotDispatchMu.Unlock()
	if !u.snapshotDispatchQueued {
		return coreapp.Snapshot{}, false
	}
	snapshot := u.pendingSnapshot
	u.pendingSnapshot = coreapp.Snapshot{}
	u.snapshotDispatchQueued = false
	return snapshot, true
}

func (u *window) applySnapshot(snapshot coreapp.Snapshot) {
	if u.isClosed() {
		return
	}
	u.snapshot = snapshot
	if snapshot.Channel.ID != 0 {
		title := snapshot.Channel.Title
		if title == "" {
			title = fmt.Sprintf("频道 %d", snapshot.Channel.ID)
		}
		u.channel.SetText("已绑定：" + title)
	} else {
		u.channel.SetText("尚未绑定频道")
	}
	if snapshot.TotalBytes > 0 {
		u.progress.SetValue(float64(snapshot.DoneBytes) / float64(snapshot.TotalBytes))
	} else {
		u.progress.SetValue(0)
	}
	if snapshot.LastError != "" && !snapshot.Running {
		// The controller keeps the last error for restart diagnostics.  The UI
		// displays it in the row/state summary rather than opening a dialog on
		// every coalesced snapshot.
		u.operationLabel.SetText(snapshot.LastError)
	} else if snapshot.PauseRequested {
		u.operationLabel.SetText("暂停请求已收到，正在完成当前文件")
	} else if snapshot.Paused && !snapshot.Running {
		u.operationLabel.SetText("队列已暂停；点击“继续上传”后将从下一条待上传视频开始")
	}
	u.refreshQueueRows(snapshot.Jobs)
	u.progressSummary.SetText(formatSummaryWithCompleted(snapshot, u.queueStats.Completed))
	u.updateActionAvailability()
	u.updateScheduleStatus()
	u.updateReadinessHint()
	if !snapshot.Running {
		u.stopSleepGuard()
		if u.scheduler != nil {
			u.scheduler.RetryDue()
		}
	}
}

func (u *window) updateActionAvailability() {
	moving := u.isMoveInFlight()
	connecting := u.isConnecting()
	u.setConnectionConfigurationEnabled(!u.snapshot.Running && !moving && !connecting)
	if moving {
		u.startButton.SetText("开始上传")
		u.startButton.Disable()
		u.pauseButton.Disable()
		u.chooseFolderButton.Disable()
		u.moveButton.Disable()
		u.cancelAllButton.Disable()
		u.connectButton.Disable()
		u.bindButton.Disable()
		u.disableQueueEditing()
		u.cancelMoveButton.Show()
		u.updateScheduleControls()
		u.refreshQueueSelectionAvailability()
		return
	}
	if u.snapshot.Running {
		u.clearQueueButton.SetText("取消并清空")
		u.startButton.SetText("开始上传")
		u.startButton.Disable()
		if u.snapshot.PauseRequested {
			u.pauseButton.Disable()
		} else {
			u.pauseButton.Enable()
		}
		if u.scanInProgress() {
			u.chooseFolderButton.Disable()
		} else {
			u.chooseFolderButton.Enable()
		}
		u.moveButton.Disable()
		u.cancelAllButton.Enable()
		u.updateSelectionControls()
	} else {
		u.clearQueueButton.SetText("清空队列")
		if u.snapshot.Paused {
			u.startButton.SetText("继续上传")
		} else {
			u.startButton.SetText("开始上传")
		}
		u.pauseButton.Disable()
		if u.scanInProgress() {
			u.chooseFolderButton.Disable()
		} else {
			u.chooseFolderButton.Enable()
		}
		u.cancelAllButton.Disable()
		if u.queueStats.Oversize {
			u.moveButton.Enable()
		} else {
			u.moveButton.Disable()
		}
		if u.queueStats.Runnable {
			u.startButton.Enable()
		} else {
			u.startButton.Disable()
		}
		u.updateSelectionControls()
	}
	switch {
	case u.snapshot.Running:
		u.connectButton.Disable()
		u.bindButton.Disable()
	case connecting:
		u.connectButton.Disable()
		u.bindButton.Disable()
	case u.connected:
		u.connectButton.Enable()
		u.bindButton.Enable()
	default:
		u.connectButton.Enable()
		u.bindButton.Disable()
	}
	if u.scanInProgress() {
		u.chooseFolderButton.Disable()
	}
	u.updateScheduleControls()
	u.refreshQueueSelectionAvailability()
	u.updateReadinessHint()
}

func (u *window) setConnectionConfigurationEnabled(enabled bool) {
	if u.connectionConfigStateSet && u.connectionConfigEditable == enabled {
		return
	}
	u.connectionConfigStateSet = true
	u.connectionConfigEditable = enabled
	controls := []interface {
		Enable()
		Disable()
	}{
		u.apiID,
		u.apiHash,
		u.botToken,
		u.proxyEnabled,
		u.proxyAddress,
		u.proxyUsername,
		u.proxyPassword,
	}
	for _, control := range controls {
		if control == nil {
			continue
		}
		if enabled {
			control.Enable()
		} else {
			control.Disable()
		}
	}
}

func (u *window) updateScheduleControls() {
	if u.scheduler == nil || u.scheduleButton == nil {
		return
	}
	_, set, _ := u.scheduler.State()
	if !u.snapshot.Running && !u.snapshot.Paused && !u.snapshot.PauseRequested && !u.isMoveInFlight() && u.queueStats.Runnable && !u.scheduleStarting {
		u.scheduleButton.Enable()
	} else {
		u.scheduleButton.Disable()
	}
	if set && !u.scheduleStarting {
		u.cancelSchedule.Enable()
	} else {
		u.cancelSchedule.Disable()
	}
}

func (u *window) disableQueueEditing() {
	u.selectAllButton.Disable()
	u.selectNoneButton.Disable()
	u.resetJobsButton.Disable()
	u.removeSelected.Disable()
	u.removeCompleted.Disable()
	u.clearQueueButton.Disable()
}

func (u *window) updateSelectionControls() {
	if u.selectionLabel == nil {
		return
	}
	selected := len(u.selectedJobs)
	u.selectionLabel.SetText(fmt.Sprintf("已选择 %d 项", selected))
	if u.isMoveInFlight() {
		u.disableQueueEditing()
		return
	}
	if len(u.snapshot.Jobs) == 0 {
		u.disableQueueEditing()
		return
	}
	u.selectAllButton.Enable()
	u.clearQueueButton.Enable()
	if u.queueStats.Reset.All > 0 {
		u.resetJobsButton.Enable()
	} else {
		u.resetJobsButton.Disable()
	}
	if selected > 0 {
		u.selectNoneButton.Enable()
		u.removeSelected.Enable()
	} else {
		u.selectNoneButton.Disable()
		u.removeSelected.Disable()
	}
	if u.queueStats.Completed > 0 {
		u.removeCompleted.Enable()
	} else {
		u.removeCompleted.Disable()
	}
}

func (u *window) refreshQueueSelectionAvailability() {
	selectable := !u.isMoveInFlight()
	if u.queueList != nil && selectable != u.queueSelectable {
		u.queueSelectable = selectable
		u.queueList.Refresh()
	}
}

func (u *window) isMoveInFlight() bool {
	u.moveMu.Lock()
	defer u.moveMu.Unlock()
	return u.moveInFlight
}

func (u *window) connect() {
	u.connectWithMode(false)
}

func (u *window) queueRunning() bool {
	if u.controller != nil {
		return u.controller.Snapshot().Running
	}
	return u.snapshot.Running
}

func (u *window) connectWithMode(automatic bool) {
	if !automatic && u.queueRunning() {
		u.showError(errors.New("上传进行中不能更换 Telegram 凭证；上传并发仍可随时调整"))
		return
	}
	if !automatic {
		u.resetConnectionRetry()
	}
	var request connectionRequest
	hasApplied := false
	if automatic {
		u.clientMu.RLock()
		request = u.appliedConnection
		hasApplied = u.hasAppliedConnection
		u.clientMu.RUnlock()
	}
	if !hasApplied {
		var err error
		request, err = u.connectionRequestFromFields()
		if err != nil {
			if automatic && u.hasCompleteConnectionConfig() {
				u.connectionConfigErr = err
				u.connection.SetText("连接配置错误，请检查设置")
				u.updateReadinessHint()
			} else if !automatic {
				u.showError(err)
			}
			return
		}
	}
	u.connectWithRequest(request, automatic)
}

func (u *window) connectionRequestFromFields() (connectionRequest, error) {
	appID, err := strconv.Atoi(strings.TrimSpace(u.apiID.Text))
	if err != nil || appID <= 0 {
		return connectionRequest{}, errors.New("API ID 必须是正整数")
	}
	apiHash := strings.TrimSpace(u.apiHash.Text)
	botToken := strings.TrimSpace(u.botToken.Text)
	if apiHash == "" || botToken == "" {
		return connectionRequest{}, errors.New("API Hash 和 Bot Token 不能为空")
	}
	request := connectionRequest{
		appID:             appID,
		apiHash:           apiHash,
		botToken:          botToken,
		proxyEnabled:      u.proxyEnabled.Checked,
		proxyAddress:      strings.TrimSpace(u.proxyAddress.Text),
		proxyUsername:     strings.TrimSpace(u.proxyUsername.Text),
		proxyPassword:     u.proxyPassword.Text,
		uploadConcurrency: u.selectedUploadConcurrency(),
	}
	if request.proxyEnabled && request.proxyAddress == "" {
		return connectionRequest{}, errors.New("已启用 SOCKS5 代理，但代理地址为空")
	}
	return request, nil
}

func (u *window) connectWithRequest(request connectionRequest, automatic bool) {
	if !automatic && u.queueRunning() {
		u.showError(errors.New("上传进行中不能更换 Telegram 凭证；上传并发仍可随时调整"))
		return
	}
	u.connectionConfigErr = nil

	u.clientMu.Lock()
	if u.client != nil {
		if automatic || u.connecting {
			u.clientMu.Unlock()
			return
		}
		pending := request
		u.pendingReconnect = &pending
		u.reconnectRequested = true
		u.connecting = true
		cancel := u.clientCancel
		u.clientMu.Unlock()
		u.connected = false
		u.identity = tgtransport.Identity{}
		u.controller.SetGateway(nil)
		u.connectButton.Disable()
		u.bindButton.Disable()
		u.limit.SetText("当前上传上限：未获取")
		u.connection.SetText("正在断开旧连接并应用新设置……")
		u.updateActionAvailability()
		if cancel != nil {
			cancel()
		}
		return
	}
	if u.connecting || u.isClosed() {
		u.clientMu.Unlock()
		return
	}
	u.connecting = true
	u.clientGeneration++
	generation := u.clientGeneration
	if automatic && !u.hasAppliedConnection {
		u.appliedConnection = request
		u.hasAppliedConnection = true
	}
	u.clientMu.Unlock()
	u.stopConnectionRetryTimer()
	u.connectButton.Disable()
	if automatic {
		u.connection.SetText("正在自动连接……")
	} else {
		u.connection.SetText("正在保存凭据并连接……")
	}
	u.updateActionAvailability()
	go u.startConnectionAttempt(request, automatic, generation)
}

func (u *window) startConnectionAttempt(request connectionRequest, automatic bool, generation uint64) {
	if u.connectionStarter != nil {
		u.connectionStarter(request, automatic, generation)
		return
	}
	u.connectAsync(request, automatic, generation)
}

func (u *window) connectAsync(request connectionRequest, automatic bool, generation uint64) {
	if !automatic {
		if err := u.updateSettings(func(settings *coreapp.Settings) {
			settings.APIID = request.appID
			settings.ProxyEnabled = request.proxyEnabled
			settings.ProxyAddress = request.proxyAddress
			settings.ProxyUsername = request.proxyUsername
			settings.UploadConcurrency = normalizeUploadConcurrency(request.uploadConcurrency)
		}); err != nil {
			u.handleConnectionFailure(err, automatic, false, generation)
			return
		}
		if err := u.secrets.SetAPIHash(request.apiHash); err != nil {
			u.handleConnectionFailure(err, automatic, false, generation)
			return
		}
		if err := u.secrets.SetBotToken(request.botToken); err != nil {
			u.handleConnectionFailure(err, automatic, false, generation)
			return
		}
		if request.proxyPassword == "" {
			if err := u.secrets.DeleteProxyPassword(); err != nil && !errors.Is(err, credentials.ErrNotFound) {
				u.handleConnectionFailure(err, automatic, false, generation)
				return
			}
		} else if err := u.secrets.SetProxyPassword(request.proxyPassword); err != nil {
			u.handleConnectionFailure(err, automatic, false, generation)
			return
		}
		request.uploadConcurrency = normalizeUploadConcurrency(u.settingsSnapshot().UploadConcurrency)
		u.clientMu.Lock()
		if generation == u.clientGeneration && !u.isClosed() {
			u.appliedConnection = request
			u.hasAppliedConnection = true
		}
		u.clientMu.Unlock()
		u.doUI(func() {
			u.settingsLoadErr = nil
			u.credentialLoadErr = nil
			u.updateReadinessHint()
		})
	}
	request.uploadConcurrency = normalizeUploadConcurrency(u.settingsSnapshot().UploadConcurrency)

	var proxyConfig *tgtransport.ProxyConfig
	if request.proxyEnabled && request.proxyAddress != "" {
		proxyConfig = &tgtransport.ProxyConfig{
			Address:  request.proxyAddress,
			Username: request.proxyUsername,
			Password: request.proxyPassword,
		}
	}
	var stateClient *tgtransport.Client
	client, err := tgtransport.NewClient(tgtransport.Config{
		AppID:             request.appID,
		APIHash:           request.apiHash,
		BotToken:          request.botToken,
		Proxy:             proxyConfig,
		UploadConcurrency: normalizeUploadConcurrency(request.uploadConcurrency),
		SessionStorage:    credentials.NewSessionStorage(u.secrets, u.paths.Session),
	}, tgtransport.Events{
		OnConnectionState: func(state tgtransport.ConnectionState) {
			u.handleConnectionState(stateClient, generation, state)
		},
		OnFloodWait: func(wait time.Duration) {
			if stateClient == nil || !u.isUsableCurrentClient(stateClient, generation) {
				return
			}
			u.setConnectionStatus("Telegram 要求等待 " + formatDuration(wait))
		},
	})
	if err != nil {
		u.handleConnectionFailure(err, automatic, false, generation)
		return
	}
	stateClient = client
	request.uploadConcurrency = normalizeUploadConcurrency(u.settingsSnapshot().UploadConcurrency)
	client.SetUploadConcurrency(request.uploadConcurrency)

	ctx, cancel := context.WithCancel(u.rootCtx)
	u.clientMu.Lock()
	if generation != u.clientGeneration || u.isClosed() {
		if generation == u.clientGeneration {
			u.connecting = false
		}
		u.clientMu.Unlock()
		cancel()
		return
	}
	u.client = client
	u.clientCancel = cancel
	if u.hasAppliedConnection {
		u.appliedConnection.uploadConcurrency = request.uploadConcurrency
	}
	// Publish the transport while holding the same lifecycle lock used by
	// forceClose. This prevents a closing window from clearing the gateway and
	// then being overwritten by this now-stale connection attempt.
	u.controller.SetGateway(client)
	u.clientMu.Unlock()

	u.doUI(func() { u.connection.SetText("正在启动 Telegram 客户端……") })
	go u.runClient(client, ctx, automatic, generation)
	go u.waitClientReady(client, ctx, automatic, generation)
	go u.observeBinding(client, ctx, generation)
}

func (u *window) selectedUploadConcurrency() int {
	if u.uploadConcurrency != nil {
		if value, ok := uploadConcurrencyForOption(u.uploadConcurrency.Selected); ok {
			return value
		}
	}
	return normalizeUploadConcurrency(u.settingsSnapshot().UploadConcurrency)
}

func (u *window) handleConnectionFailure(err error, automatic, retryable bool, generation uint64) {
	u.clientMu.Lock()
	if generation != u.clientGeneration {
		u.clientMu.Unlock()
		return
	}
	u.connecting = false
	u.clientMu.Unlock()
	if !automatic {
		u.showError(err)
	}
	u.doUI(func() {
		u.clientMu.RLock()
		currentGeneration := generation == u.clientGeneration
		u.clientMu.RUnlock()
		if !currentGeneration {
			return
		}
		u.connected = false
		if !retryable {
			u.resetConnectionRetry()
		}
		if retryable {
			u.connectionConfigErr = nil
		} else {
			u.connectionConfigErr = err
		}
		u.connectButton.SetText("保存并连接")
		u.connectButton.Enable()
		if retryable {
			u.connection.SetText("自动连接失败，将稍后重试")
			u.operationLabel.SetText("Telegram 连接暂时不可用，正在等待自动重试")
		} else {
			u.connection.SetText("连接失败，请检查设置")
			u.operationLabel.SetText(err.Error())
		}
		u.updateActionAvailability()
		if retryable {
			u.scheduleConnectionRetry()
		}
		u.updateScheduleStatus()
		u.updateReadinessHint()
	})
}

func (u *window) handleConnectionState(client *tgtransport.Client, generation uint64, state tgtransport.ConnectionState) {
	u.doUI(func() {
		if client == nil || !u.isUsableCurrentClient(client, generation) || u.isClosed() {
			return
		}
		switch state {
		case tgtransport.StateConnecting:
			u.connected = false
			u.connection.SetText("正在连接 Telegram……")
		case tgtransport.StateReady:
			u.connection.SetText("Telegram 连接已建立")
			if u.identity.Username != "" {
				u.connected = true
				u.resetConnectionRetry()
				if u.scheduler != nil {
					u.scheduler.RetryDue()
				}
			}
		case tgtransport.StateDisconnected:
			u.connected = false
			u.connection.SetText("Telegram 连接已断开")
		}
		u.updateActionAvailability()
		u.updateScheduleStatus()
		u.updateReadinessHint()
	})
}

func (u *window) runClient(client *tgtransport.Client, ctx context.Context, automatic bool, generation uint64) {
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
	wasCurrent := u.client == client && u.clientGeneration == generation
	reconnect := false
	var reconnectRequest *connectionRequest
	if wasCurrent {
		if u.clientCancel != nil {
			u.clientCancel()
		}
		u.clientCancel = nil
		u.client = nil
		// Keep other automatic connection paths out until the UI-side cleanup
		// below either starts the requested replacement or schedules retry.
		u.connecting = true
		reconnect = u.reconnectRequested
		if reconnect && u.pendingReconnect != nil {
			pending := *u.pendingReconnect
			reconnectRequest = &pending
		}
		u.reconnectRequested = false
		u.pendingReconnect = nil
	}
	u.clientMu.Unlock()
	if wasCurrent {
		// Clear the transport immediately. UI delivery may be coalesced or
		// delayed, while the controller's retry loop must never reuse a finished
		// one-shot Client.
		u.controller.SetGateway(nil)
	}
	retryable := shouldRetryConnection(err, ctx.Err(), u.isClosed())
	if err != nil && !errors.Is(err, context.Canceled) && !u.isClosed() && !automatic && !retryable {
		u.showError(err)
	}
	u.doUI(func() {
		if !wasCurrent {
			return
		}
		u.clientMu.Lock()
		if u.clientGeneration != generation || u.client != nil {
			u.clientMu.Unlock()
			return
		}
		u.connecting = false
		u.clientMu.Unlock()
		u.connected = false
		if !retryable {
			u.resetConnectionRetry()
		}
		u.identity = tgtransport.Identity{}
		if retryable {
			u.connectionConfigErr = nil
		} else if err != nil && !errors.Is(err, context.Canceled) {
			u.connectionConfigErr = err
		}
		u.connectButton.SetText("保存并连接")
		u.connectButton.Enable()
		u.bindButton.Disable()
		u.limit.SetText("当前上传上限：未获取")
		if retryable {
			u.connection.SetText("自动连接失败，将稍后重试")
			u.operationLabel.SetText("Telegram 连接暂时不可用，正在等待自动重试")
		} else if err != nil && !errors.Is(err, context.Canceled) {
			u.connection.SetText("连接失败，请检查设置")
			u.operationLabel.SetText(err.Error())
		} else {
			u.connection.SetText("未连接")
		}
		u.updateActionAvailability()
		if retryable {
			u.scheduleConnectionRetry()
		}
		u.updateScheduleStatus()
		u.updateReadinessHint()
		if reconnect && reconnectRequest != nil && !u.isClosed() {
			u.connectWithRequest(*reconnectRequest, false)
		}
	})
}

func (u *window) waitClientReady(client *tgtransport.Client, ctx context.Context, automatic bool, generation uint64) {
	identity, err := client.WaitReady(ctx)
	if err != nil {
		// runClient owns cleanup and retry scheduling. Waiting here prevents a
		// failed readiness observer from racing a still-running one-shot Client.
		return
	}
	if !u.isUsableCurrentClient(client, generation) || u.isClosed() {
		return
	}
	u.doUI(func() {
		u.clientMu.Lock()
		if client == nil || u.client != client || u.clientGeneration != generation || u.reconnectRequested || u.isClosed() {
			u.clientMu.Unlock()
			return
		}
		u.connecting = false
		u.clientMu.Unlock()
		u.connected = true
		u.connectionConfigErr = nil
		u.identity = identity
		u.resetConnectionRetry()
		u.connectButton.SetText("保存并重新连接")
		if u.snapshot.Running {
			u.connectButton.Disable()
		} else {
			u.connectButton.Enable()
		}
		u.bindButton.Enable()
		u.connection.SetText("已连接 Bot：@" + identity.Username)
		limitPrefix := "当前上传上限："
		if !identity.MaxUploadExact {
			limitPrefix = "协议上限（服务端动态值未确认）："
		}
		u.limit.SetText(limitPrefix + formatBytes(identity.MaxUploadBytes))
		u.updateActionAvailability()
		u.dispatchControllerSnapshot(u.controller.Snapshot())
		if u.scheduler != nil {
			u.scheduler.RetryDue()
		}
	})

	// Queue entries may have been added while Telegram was disconnected. Apply
	// the server limit in place; never rescan LastFolder because the durable
	// queue can now contain selections from several different folders.
	if !u.isUsableCurrentClient(client, generation) || u.isClosed() {
		return
	}
	queueSnapshot := u.controller.Snapshot()
	if len(queueSnapshot.Jobs) > 0 {
		if err := u.controller.ApplyUploadLimit(identity.MaxUploadBytes); err != nil {
			if u.isUsableCurrentClient(client, generation) {
				u.showError(err)
			}
		}
	}
}

func (u *window) observeBinding(client *tgtransport.Client, ctx context.Context, generation uint64) {
	for {
		select {
		case event, ok := <-client.BindingEvents():
			if !ok {
				return
			}
			if event.Err != nil {
				if u.isUsableCurrentClient(client, generation) {
					u.showError(event.Err)
				}
				continue
			}
			channel := event.Channel
			u.clientMu.RLock()
			if u.client != client || u.clientGeneration != generation || u.reconnectRequested || u.isClosed() {
				u.clientMu.RUnlock()
				continue
			}
			err := u.controller.SetChannel(channel)
			u.clientMu.RUnlock()
			if err != nil {
				u.showError(err)
				continue
			}
			u.doUI(func() {
				if !u.isUsableCurrentClient(client, generation) {
					return
				}
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
				u.updateReadinessHint()
				u.updateActionAvailability()
				u.updateScheduleStatus()
				if u.scheduler != nil {
					u.scheduler.RetryDue()
				}
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
	if u.isMoveInFlight() {
		u.showError(errors.New("请等待当前文件移动完成，再添加文件夹"))
		return
	}
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
		if err := u.updateSettings(func(settings *coreapp.Settings) {
			settings.LastFolder = folder
		}); err != nil {
			u.showError(err)
			return
		}
		u.folderLabel.SetText("最近来源：" + folder)
		maxBytes := int64(0)
		if u.connected {
			maxBytes = u.identity.MaxUploadBytes
		}
		u.scanFolder(folder, maxBytes)
	}, u.window)
}

func (u *window) scanFolder(folder string, maxBytes int64) {
	if !u.beginScan() {
		return
	}
	u.doUI(func() {
		u.updateActionAvailability()
		u.operationLabel.SetText("正在扫描当前文件夹……")
	})
	go func() {
		candidates, err := scanner.Scan(folder, maxBytes)
		u.endScan()
		if err != nil {
			u.showError(err)
		}
		u.doUI(func() {
			u.updateActionAvailability()
			if err == nil {
				if u.isMoveInFlight() {
					u.operationLabel.SetText("扫描已完成；当前正在移动文件，请稍后重新添加")
					return
				}
				u.showCandidateDialog(folder, candidates)
			} else if u.scheduler != nil {
				u.scheduler.RetryDue()
			}
		})
	}()
}

type candidateChoice struct {
	job       model.Job
	duplicate bool
	selected  bool
	check     *widget.Check
}

func newCandidateChoices(existing, candidates []model.Job) []candidateChoice {
	existingPaths := make(map[string]struct{}, len(existing))
	for _, job := range existing {
		existingPaths[canonicalPathKey(job.Path)] = struct{}{}
	}
	choices := make([]candidateChoice, len(candidates))
	for index, candidate := range candidates {
		_, duplicate := existingPaths[canonicalPathKey(candidate.Path)]
		choices[index] = candidateChoice{
			job:       candidate,
			duplicate: duplicate,
			selected:  !duplicate,
		}
	}
	return choices
}

func (u *window) showCandidateDialog(folder string, candidates []model.Job) {
	if len(candidates) == 0 {
		u.operationLabel.SetText("当前文件夹没有可添加的 MP4 文件")
		dialog.ShowInformation("没有候选视频", "当前文件夹顶层没有找到 MP4 文件。", u.window)
		if u.scheduler != nil {
			u.scheduler.RetryDue()
		}
		return
	}

	choices := newCandidateChoices(u.snapshot.Jobs, candidates)
	rows := make([]fyne.CanvasObject, 0, len(candidates))
	selectedLabel := widget.NewLabel("")
	var addButton *widget.Button
	refreshCount := func() {
		selected := 0
		duplicates := 0
		for i := range choices {
			if choices[i].selected {
				selected++
			}
			if choices[i].duplicate {
				duplicates++
			}
		}
		selectedLabel.SetText(fmt.Sprintf("已选择 %d / %d 个；%d 个已在队列", selected, len(choices), duplicates))
		if addButton != nil {
			if selected > 0 {
				addButton.Enable()
			} else {
				addButton.Disable()
			}
		}
	}

	for index := range candidates {
		choice := &choices[index]
		choice.check = widget.NewCheck("", nil)
		choice.check.SetChecked(choice.selected)
		choice.check.OnChanged = func(checked bool) {
			choice.selected = checked
			refreshCount()
		}
		if choice.duplicate {
			choice.check.Disable()
		}
		name := widget.NewLabel(choice.job.Name)
		name.Truncation = fyne.TextTruncateEllipsis
		state := formatBytes(choice.job.Size)
		switch {
		case choice.duplicate:
			state += " · 已在队列"
		case choice.job.State == model.JobOversize:
			state += " · 超出当前上传上限"
		default:
			state += " · 可添加"
		}
		status := widget.NewLabel(state)
		rows = append(rows, container.NewBorder(nil, widget.NewSeparator(), choice.check, status, name))
	}

	list := container.NewVBox(rows...)
	scroll := container.NewVScroll(list)
	scroll.SetMinSize(fyne.NewSize(760, 360))
	content := container.NewBorder(
		container.NewVBox(
			widget.NewLabel("选择要加入队列的视频。可以稍后继续从其他文件夹追加。"),
			selectedLabel,
		),
		nil,
		nil,
		nil,
		scroll,
	)

	var candidateDialog *dialog.CustomDialog
	selectAll := widget.NewButton("全选可添加项", func() {
		for i := range choices {
			if choices[i].duplicate {
				continue
			}
			choices[i].check.SetChecked(true)
		}
		refreshCount()
	})
	selectNone := widget.NewButton("取消全选", func() {
		for i := range choices {
			if choices[i].duplicate {
				continue
			}
			choices[i].check.SetChecked(false)
		}
		refreshCount()
	})
	addButton = widget.NewButton("添加所选", func() {
		selected := make([]model.Job, 0, len(choices))
		for i := range choices {
			if choices[i].selected && !choices[i].duplicate {
				selected = append(selected, choices[i].job)
			}
		}
		if len(selected) == 0 {
			return
		}
		added, err := u.controller.AddJobs(selected)
		if err != nil {
			u.showError(err)
			return
		}
		candidateDialog.Dismiss()
		skipped := len(selected) - added
		if skipped > 0 {
			u.operationLabel.SetText(fmt.Sprintf("已添加 %d 个视频；%d 个因已在队列中而跳过", added, skipped))
		} else {
			u.operationLabel.SetText(fmt.Sprintf("已从当前文件夹添加 %d 个视频", added))
		}
	})
	candidateDialog = dialog.NewCustom("添加到上传队列", "取消", content, u.window)
	u.candidateDialog = candidateDialog
	candidateDialog.SetOnClosed(func() {
		if u.candidateDialog == candidateDialog {
			u.candidateDialog = nil
		}
		u.updateScheduleStatus()
		if u.scheduler != nil {
			u.scheduler.RetryDue()
		}
	})
	candidateDialog.SetButtons([]fyne.CanvasObject{
		selectAll,
		selectNone,
		widget.NewButton("取消", func() { candidateDialog.Dismiss() }),
		addButton,
	})
	candidateDialog.Resize(fyne.NewSize(820, 520))
	refreshCount()
	u.operationLabel.SetText(fmt.Sprintf("扫描完成：找到 %d 个 MP4 文件", len(candidates)))
	candidateDialog.Show()
}

func (u *window) beginScan() bool {
	u.scanMu.Lock()
	defer u.scanMu.Unlock()
	if u.scanning {
		return false
	}
	u.scanning = true
	return true
}

func (u *window) endScan() {
	u.scanMu.Lock()
	u.scanning = false
	u.scanMu.Unlock()
}

func (u *window) scanInProgress() bool {
	u.scanMu.Lock()
	defer u.scanMu.Unlock()
	return u.scanning
}

func (u *window) selectAllJobs() {
	if u.isMoveInFlight() {
		return
	}
	for _, job := range u.snapshot.Jobs {
		u.selectedJobs[job.ID] = true
	}
	u.refreshQueueRows(u.snapshot.Jobs)
}

func (u *window) selectNoJobs() {
	if u.isMoveInFlight() {
		return
	}
	clear(u.selectedJobs)
	u.refreshQueueRows(u.snapshot.Jobs)
}

func (u *window) selectedJobIDs() []string {
	ids := make([]string, 0, len(u.selectedJobs))
	for _, job := range u.snapshot.Jobs {
		if u.selectedJobs[job.ID] {
			ids = append(ids, job.ID)
		}
	}
	return ids
}

func (u *window) confirmRemoveSelected() {
	ids := u.selectedJobIDs()
	if len(ids) == 0 {
		return
	}
	message := fmt.Sprintf("从本地队列删除所选的 %d 个条目？\n\n不会删除磁盘上的视频，也不会删除 Telegram 中已经发送的消息。", len(ids))
	for _, id := range ids {
		if id == u.snapshot.ActiveID {
			message += "\n\n当前正在上传的任务会先被取消，安全结束后自动删除；如果消息提交结果未知，则会保留为“待确认”。"
			break
		}
	}
	dialog.ShowConfirm("删除所选条目", message, func(ok bool) {
		if !ok {
			return
		}
		result, err := u.controller.RemoveJobs(ids)
		if err != nil {
			u.showError(err)
			return
		}
		current := u.controller.Snapshot()
		existing := make(map[string]struct{}, len(current.Jobs))
		for _, job := range current.Jobs {
			existing[job.ID] = struct{}{}
		}
		pending := make(map[string]struct{}, len(result.PendingRemovalIDs))
		for _, id := range result.PendingRemovalIDs {
			pending[id] = struct{}{}
		}
		retained := 0
		for _, id := range ids {
			_, stillExists := existing[id]
			if !stillExists {
				delete(u.selectedJobs, id)
				continue
			}
			if _, waiting := pending[id]; !waiting {
				retained++
			}
		}
		if len(result.PendingRemovalIDs) > 0 {
			u.operationLabel.SetText(fmt.Sprintf("已删除 %d 个条目；%d 个活动任务正在取消；%d 个受保护条目已保留", result.Removed, len(result.PendingRemovalIDs), retained))
		} else if retained > 0 {
			u.operationLabel.SetText(fmt.Sprintf("已删除 %d 个条目；%d 个待确认或正在移动的条目因安全原因保留", result.Removed, retained))
		} else {
			u.operationLabel.SetText(fmt.Sprintf("已从本地队列删除 %d 个条目", result.Removed))
		}
	}, u.window)
}

func (u *window) showResetJobsDialog() {
	if u.isMoveInFlight() {
		return
	}
	counts := resetJobCountsFor(u.snapshot.Jobs, u.selectedJobs)
	if counts.All == 0 {
		return
	}
	selectedIDs := u.selectedJobIDs()

	var resetDialog *dialog.CustomDialog
	makeAction := func(label string, count int, mode coreapp.ResetMode, ids []string) fyne.CanvasObject {
		button := widget.NewButton(fmt.Sprintf("%s（%d）", label, count), func() {
			resetDialog.Dismiss()
			u.resetJobs(mode, ids, label)
		})
		if count == 0 {
			button.Disable()
		}
		return button
	}

	note := widget.NewLabel("重置会把任务恢复为“待上传”，不需要重新扫描文件夹。队列运行中时，会在当前文件结束后按原队列位置继续处理。为避免重复消息，待确认、已发送、已移动和超限任务不会被批量重置。")
	note.Wrapping = fyne.TextWrapWord
	content := container.NewVBox(
		note,
		widget.NewSeparator(),
		makeAction("重置所选任务", counts.Selected, coreapp.ResetSelected, selectedIDs),
		makeAction("重置已取消任务", counts.Cancelled, coreapp.ResetCancelled, nil),
		makeAction("重置失败或中断任务", counts.Failed, coreapp.ResetFailed, nil),
		makeAction("重置已跳过任务", counts.Skipped, coreapp.ResetSkipped, nil),
		makeAction("重置全部可恢复任务", counts.All, coreapp.ResetAllRecoverable, nil),
	)
	resetDialog = dialog.NewCustom("重置队列任务", "关闭", content, u.window)
	resetDialog.Resize(fyne.NewSize(560, 420))
	resetDialog.Show()
}

func (u *window) resetJobs(mode coreapp.ResetMode, ids []string, label string) {
	count, err := u.controller.ResetJobs(mode, ids)
	if err != nil {
		u.showError(err)
		return
	}
	if count == 0 {
		u.operationLabel.SetText("没有符合条件的可恢复任务")
		return
	}
	u.operationLabel.SetText(fmt.Sprintf("%s：已恢复 %d 个任务，可直接开始或继续上传", label, count))
}

func (u *window) confirmRemoveCompleted() {
	count := u.queueStats.Completed
	if count == 0 {
		return
	}
	message := fmt.Sprintf("从本地队列删除 %d 个已发送或已移动的条目？\n\nTelegram 消息和磁盘文件都不会被删除。", count)
	dialog.ShowConfirm("删除已完成条目", message, func(ok bool) {
		if !ok {
			return
		}
		result, err := u.controller.RemoveCompleted()
		if err != nil {
			u.showError(err)
			return
		}
		u.operationLabel.SetText(fmt.Sprintf("已删除 %d 个已完成条目", result.Removed))
	}, u.window)
}

func (u *window) confirmClearQueue() {
	if len(u.snapshot.Jobs) == 0 {
		return
	}
	message := fmt.Sprintf("清空本地队列中的全部 %d 个条目？\n\n此操作不会删除磁盘文件或 Telegram 消息。", len(u.snapshot.Jobs))
	if u.snapshot.Running {
		message += "\n\n当前上传会先取消；安全结束后自动删除。消息提交结果未知的任务会保留为“待确认”。"
	}
	title := "清空队列"
	if u.snapshot.Running {
		title = "取消并清空队列"
	}
	dialog.ShowConfirm(title, message, func(ok bool) {
		if !ok {
			return
		}
		result, err := u.controller.ClearQueue()
		if err != nil {
			u.showError(err)
			return
		}
		current := u.controller.Snapshot()
		pending := make(map[string]struct{}, len(result.PendingRemovalIDs))
		for _, id := range result.PendingRemovalIDs {
			pending[id] = struct{}{}
		}
		retained := 0
		for _, job := range current.Jobs {
			if _, waiting := pending[job.ID]; !waiting {
				retained++
			}
		}
		if len(result.PendingRemovalIDs) == 0 && retained == 0 {
			clear(u.selectedJobs)
			u.operationLabel.SetText("本地上传队列已清空")
		} else {
			u.operationLabel.SetText(fmt.Sprintf("已清理 %d 个条目；%d 个活动任务正在取消；%d 个待确认或正在移动的条目因安全原因保留", result.Removed, len(result.PendingRemovalIDs), retained))
		}
	}, u.window)
}

func (u *window) restoreScheduledStart() {
	if u.scheduler == nil {
		return
	}
	startUnix := u.settingsSnapshot().ScheduledStartUnix
	if startUnix <= 0 {
		u.updateScheduleStatus()
		return
	}
	u.scheduler.Set(time.Unix(startUnix, 0).Local())
	u.updateScheduleStatus()
	u.ensureScheduledConnection()
}

func (u *window) showScheduleDialog() {
	if u.snapshot.Running || u.isMoveInFlight() {
		return
	}
	now := time.Now()
	defaultTime := now.Add(10 * time.Minute).Truncate(time.Minute)
	if !defaultTime.After(now) {
		defaultTime = defaultTime.Add(time.Minute)
	}
	if at, set, _ := u.scheduler.State(); set && at.After(now) {
		defaultTime = at.Local()
	}

	dateEntry := widget.NewEntry()
	dateEntry.SetText(defaultTime.Format("2006-01-02"))
	timeEntry := widget.NewEntry()
	timeEntry.SetText(defaultTime.Format("15:04"))
	validation := widget.NewLabel("")
	validation.Wrapping = fyne.TextWrapWord
	note := widget.NewLabel("应用保持运行时会到点自动开始；完全退出应用后，系统不会自行唤醒它。重启应用后会恢复尚未执行的计划。")
	note.Wrapping = fyne.TextWrapWord
	content := container.NewVBox(
		widget.NewLabel("日期（YYYY-MM-DD）"),
		dateEntry,
		widget.NewLabel("时间（HH:mm，本机时区）"),
		timeEntry,
		validation,
		widget.NewSeparator(),
		note,
	)

	var scheduleDialog *dialog.CustomDialog
	setButton := widget.NewButton("设置定时", func() {
		startAt, err := parseScheduledTime(dateEntry.Text, timeEntry.Text, time.Local)
		if err != nil {
			validation.SetText(err.Error())
			return
		}
		if !startAt.After(time.Now()) {
			validation.SetText("定时开始时间必须晚于当前时间")
			return
		}
		if err := u.updateSettings(func(settings *coreapp.Settings) {
			settings.ScheduledStartUnix = startAt.Unix()
		}); err != nil {
			validation.SetText(err.Error())
			return
		}
		u.resetConnectionRetry()
		u.scheduler.Set(startAt)
		u.updateScheduleStatus()
		u.updateActionAvailability()
		u.ensureScheduledConnection()
		scheduleDialog.Dismiss()
		u.operationLabel.SetText("定时开始已设置")
	})
	scheduleDialog = dialog.NewCustom("定时开始上传队列", "取消", content, u.window)
	scheduleDialog.SetButtons([]fyne.CanvasObject{
		widget.NewButton("取消", func() { scheduleDialog.Dismiss() }),
		setButton,
	})
	scheduleDialog.Resize(fyne.NewSize(560, 390))
	scheduleDialog.Show()
}

func parseScheduledTime(dateText, timeText string, location *time.Location) (time.Time, error) {
	if location == nil {
		location = time.Local
	}
	value := strings.TrimSpace(dateText) + " " + strings.TrimSpace(timeText)
	parsed, err := time.ParseInLocation("2006-01-02 15:04", value, location)
	if err != nil {
		return time.Time{}, errors.New("时间格式无效，请按 YYYY-MM-DD 和 HH:mm 填写")
	}
	return parsed, nil
}

func (u *window) cancelScheduledStart() {
	if u.scheduler == nil || u.scheduleStarting {
		return
	}
	if _, set, _ := u.scheduler.State(); !set {
		return
	}
	if err := u.updateSettings(func(settings *coreapp.Settings) {
		settings.ScheduledStartUnix = 0
	}); err != nil {
		u.showError(err)
		return
	}
	u.scheduler.Cancel()
	u.updateScheduleStatus()
	u.updateActionAvailability()
	u.operationLabel.SetText("已取消定时开始")
}

func (u *window) updateScheduleStatus() {
	if u.scheduleLabel == nil || u.scheduler == nil {
		return
	}
	startAt, set, due := u.scheduler.State()
	if !set {
		u.scheduleLabel.SetText("定时：未设置")
		return
	}
	display := startAt.Local().Format("2006-01-02 15:04")
	if !due {
		u.scheduleLabel.SetText("定时：" + display + "（应用需保持打开）")
		return
	}
	suffix := "正在准备开始"
	switch {
	case u.snapshot.Paused || u.snapshot.PauseRequested:
		suffix = "时间已到，等待手动继续"
	case !u.connected:
		suffix = "时间已到，等待 Telegram 连接"
		if u.connectionRetryDelay > 0 {
			suffix += "（将自动重试）"
		}
	case u.snapshot.Channel.ID == 0:
		suffix = "时间已到，等待绑定频道"
	case !u.queueStats.Runnable:
		suffix = "时间已到，等待队列加入待上传视频"
	case u.snapshot.Running:
		suffix = "时间已到，队列正在运行"
	case u.isMoveInFlight():
		suffix = "时间已到，等待当前文件操作完成"
	}
	u.scheduleLabel.SetText("定时：" + display + " · " + suffix)
}

func (u *window) ensureScheduledConnection() {
	u.ensureConnection()
}

func (u *window) ensureConnection() {
	if u.currentClient() != nil || u.isClosed() || u.connectionConfigErr != nil {
		return
	}
	u.clientMu.RLock()
	connecting := u.connecting
	u.clientMu.RUnlock()
	if connecting {
		return
	}
	u.connected = false
	if !u.canAttemptAutomaticConnection() {
		u.updateScheduleStatus()
		u.updateReadinessHint()
		return
	}
	u.connectWithMode(true)
}

func (u *window) scheduleConnectionRetry() {
	if u.isClosed() || u.connected || !u.canAttemptAutomaticConnection() {
		return
	}
	u.connectionRetryMu.Lock()
	if u.connectionRetryTimer != nil {
		u.connectionRetryMu.Unlock()
		return
	}
	u.connectionRetryDelay = nextConnectionRetryDelay(u.connectionRetryDelay)
	delay := u.connectionRetryDelay
	u.connectionRetryTimer = time.AfterFunc(delay, func() {
		u.connectionRetryMu.Lock()
		u.connectionRetryTimer = nil
		u.connectionRetryMu.Unlock()
		u.doUI(u.ensureConnection)
	})
	u.connectionRetryMu.Unlock()
	u.operationLabel.SetText(fmt.Sprintf("Telegram 连接将在 %s 后自动重试", formatDuration(delay)))
	u.updateScheduleStatus()
}

func (u *window) resetConnectionRetry() {
	u.connectionRetryDelay = 0
	u.stopConnectionRetryTimer()
}

func (u *window) stopConnectionRetryTimer() {
	u.connectionRetryMu.Lock()
	if u.connectionRetryTimer != nil {
		u.connectionRetryTimer.Stop()
		u.connectionRetryTimer = nil
	}
	u.connectionRetryMu.Unlock()
}

func nextConnectionRetryDelay(previous time.Duration) time.Duration {
	if previous < connectionRetryInitial {
		return connectionRetryInitial
	}
	next := previous * 2
	if next > connectionRetryMaximum {
		return connectionRetryMaximum
	}
	return next
}

func (u *window) tryScheduledStart() {
	if u.scheduler == nil || u.scheduleStarting {
		return
	}
	_, set, due := u.scheduler.State()
	if !set || !due {
		return
	}
	u.updateScheduleStatus()
	if u.snapshot.Running || u.isMoveInFlight() {
		return
	}
	// A scheduled start must never clear a manual pause. The user must press
	// “继续上传”, which goes through the normal beginUploads path and lets
	// Controller.Start clear and persist the pause state.
	if u.snapshot.Paused || u.snapshot.PauseRequested {
		return
	}
	if !u.connected || u.currentClient() == nil {
		u.ensureScheduledConnection()
		return
	}
	if u.snapshot.Channel.ID == 0 || !u.queueStats.Runnable {
		return
	}

	u.scheduleStarting = true
	u.updateActionAvailability()
	err := u.beginUploads(true)
	u.scheduleStarting = false
	u.updateScheduleStatus()
	u.updateActionAvailability()
	if err != nil {
		u.operationLabel.SetText("定时启动失败，计划仍保留：" + err.Error())
	}
}

func (u *window) startUploads() {
	if !u.queueStats.Runnable {
		return
	}
	if !u.connected || u.currentClient() == nil {
		u.ensureConnection()
		if u.isConnecting() {
			u.operationLabel.SetText("Telegram 正在自动连接；连接完成后请再次点击开始上传")
		} else {
			u.operationLabel.SetText("尚未连接 Telegram，请先在设置中完成连接")
		}
		u.showSettingsPage()
		return
	}
	if u.snapshot.Channel.ID == 0 {
		u.operationLabel.SetText("尚未绑定目标频道，请先在设置中完成绑定")
		u.showSettingsPage()
		return
	}
	if err := u.beginUploads(false); err != nil {
		u.showError(err)
	}
}

func (u *window) pauseUploads() {
	if u.controller == nil || !u.snapshot.Running || u.snapshot.PauseRequested {
		return
	}
	// Disable immediately so a second click cannot queue duplicate persistence
	// requests while the controller is finishing the pause transition.
	if u.pauseButton != nil {
		u.pauseButton.Disable()
	}
	u.operationLabel.SetText("暂停请求已收到，正在完成当前文件")
	if err := u.controller.Pause(); err != nil {
		u.showError(err)
		u.dispatchControllerSnapshot(u.controller.Snapshot())
	}
}

func (u *window) beginUploads(scheduled bool) error {
	var scheduledAt time.Time
	hadSchedule := false
	wasDue := false
	if u.scheduler != nil {
		scheduledAt, hadSchedule, wasDue = u.scheduler.State()
	}
	if hadSchedule {
		if err := u.updateSettings(func(settings *coreapp.Settings) {
			settings.ScheduledStartUnix = 0
		}); err != nil {
			return err
		}
		u.scheduler.Cancel()
	}

	if err := u.controller.Start(u.rootCtx); err != nil {
		if hadSchedule {
			restoreErr := u.updateSettings(func(settings *coreapp.Settings) {
				settings.ScheduledStartUnix = scheduledAt.Unix()
			})
			if scheduled || wasDue {
				u.scheduler.HoldDue(scheduledAt)
			} else {
				u.scheduler.Set(scheduledAt)
			}
			if restoreErr != nil {
				return errors.Join(err, fmt.Errorf("恢复定时设置失败：%w", restoreErr))
			}
		}
		return err
	}

	status := "上传已开始；文件会按队列顺序逐条发送"
	if scheduled {
		status = "定时时间已到；上传队列已自动开始"
	}
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
	u.resetConnectionRetry()
	u.updateScheduleStatus()
	return nil
}

func (u *window) confirmCancelAll() {
	dialog.ShowConfirm("立即取消全部上传", "此操作会立即中断当前文件、取消所有尚未发送的文件，并覆盖暂停状态；已经发送的消息不会删除。确定继续吗？", func(ok bool) {
		if ok {
			if err := u.controller.CancelAll(); err != nil {
				u.showError(err)
				return
			}
			u.operationLabel.SetText("已立即取消全部上传；如需恢复，可使用“重置任务…”")
		}
	}, u.window)
}

func (u *window) cancelJob(id string) {
	if err := u.controller.CancelJob(id); err != nil {
		u.showError(err)
	}
}

func (u *window) jobByID(id string) (model.Job, bool) {
	for _, job := range u.snapshot.Jobs {
		if job.ID == id {
			return job, true
		}
	}
	return model.Job{}, false
}

func (u *window) jobDetailsContent(job model.Job) fyne.CanvasObject {
	path := widget.NewLabel(job.Path)
	path.Wrapping = fyne.TextWrapWord
	status := widget.NewLabel(jobStatus(job))
	status.Wrapping = fyne.TextWrapWord
	items := []fyne.CanvasObject{
		widget.NewLabelWithStyle(job.Name, fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		widget.NewLabel(fmt.Sprintf("队列位置：%d · 文件大小：%s", job.Position+1, formatBytes(job.Size))),
		status,
		widget.NewSeparator(),
		widget.NewLabelWithStyle("文件路径", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		path,
	}
	if job.MessageID != 0 {
		items = append(items, widget.NewLabel(fmt.Sprintf("Telegram Message ID：%d", job.MessageID)))
	}
	return container.NewVBox(items...)
}

func (u *window) showJobDetails(id string) {
	job, ok := u.jobByID(id)
	if !ok {
		return
	}
	var detailsDialog *dialog.CustomDialog
	detailsDialog = dialog.NewCustom("队列条目详情", "关闭", u.jobDetailsContent(job), u.window)
	detailsDialog.SetButtons([]fyne.CanvasObject{
		widget.NewButton("复制路径", func() { u.application.Clipboard().SetContent(job.Path) }),
		widget.NewButton("关闭", func() { detailsDialog.Dismiss() }),
	})
	detailsDialog.Resize(fyne.NewSize(680, 320))
	detailsDialog.Show()
}

func (u *window) showJobActions(id string) {
	job, ok := u.jobByID(id)
	if !ok {
		return
	}
	var actionsDialog *dialog.CustomDialog
	actions := []fyne.CanvasObject{
		widget.NewButton("复制路径", func() { u.application.Clipboard().SetContent(job.Path) }),
	}
	if job.State == model.JobFailed || job.State == model.JobConfirming {
		actions = append(actions, widget.NewButton("重试", func() {
			actionsDialog.Dismiss()
			u.retryJob(id)
		}))
		actions = append(actions, widget.NewButton("跳过", func() {
			actionsDialog.Dismiss()
			u.skipJob(id)
		}))
	}
	if job.State == model.JobConfirming {
		actions = append(actions, widget.NewButton("已在频道看到", func() {
			actionsDialog.Dismiss()
			u.confirmMarkSent(id)
		}))
	}
	actions = append(actions, widget.NewButton("关闭", func() { actionsDialog.Dismiss() }))
	actionsDialog = dialog.NewCustom("处理队列条目", "关闭", u.jobDetailsContent(job), u.window)
	actionsDialog.SetButtons(actions)
	actionsDialog.Resize(fyne.NewSize(720, 360))
	actionsDialog.Show()
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
	dialog.ShowConfirm("标记为已发送", "仅当你已在频道中看到这条视频时使用。标记后可重新开始后续任务，确定吗？", func(ok bool) {
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
		u.updateActionAvailability()
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
				u.updateScheduleStatus()
				if u.scheduler != nil {
					u.scheduler.RetryDue()
				}
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

func (u *window) currentClient() *tgtransport.Client {
	u.clientMu.RLock()
	defer u.clientMu.RUnlock()
	return u.client
}

func (u *window) isConnecting() bool {
	u.clientMu.RLock()
	defer u.clientMu.RUnlock()
	return u.connecting
}

func (u *window) isUsableCurrentClient(client *tgtransport.Client, generation uint64) bool {
	u.clientMu.RLock()
	defer u.clientMu.RUnlock()
	return client != nil && u.client == client && u.clientGeneration == generation && !u.reconnectRequested
}

func isTransientConnectionError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, tgtransport.ErrWrongBotSession) {
		return false
	}
	if errors.Is(err, tgtransport.ErrNotConnected) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var networkErr net.Error
	return errors.As(err, &networkErr)
}

func shouldRetryConnection(runErr, contextErr error, closed bool) bool {
	if closed {
		return false
	}
	if runErr == nil {
		return contextErr == nil
	}
	return isTransientConnectionError(runErr)
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

func completedJobCount(jobs []model.Job) int {
	count := 0
	for _, job := range jobs {
		if job.State == model.JobSent || job.State == model.JobMoved {
			count++
		}
	}
	return count
}

type resetJobCounts struct {
	Selected  int
	Cancelled int
	Failed    int
	Skipped   int
	All       int
}

func resetJobCountsFor(jobs []model.Job, selected map[string]bool) resetJobCounts {
	var counts resetJobCounts
	for _, job := range jobs {
		resettable := true
		switch job.State {
		case model.JobCancelled:
			counts.Cancelled++
		case model.JobFailed, model.JobInterrupted:
			counts.Failed++
		case model.JobSkipped:
			counts.Skipped++
		default:
			resettable = false
		}
		if !resettable {
			continue
		}
		counts.All++
		if selected[job.ID] {
			counts.Selected++
		}
	}
	return counts
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

func jobFraction(job model.Job) float64 {
	if job.State == model.JobSent || job.State == model.JobMoved {
		return 1
	}
	if job.Size <= 0 {
		return 0
	}
	fraction := float64(job.Uploaded) / float64(job.Size)
	if fraction < 0 {
		return 0
	}
	if fraction > 1 {
		return 1
	}
	return fraction
}

func compactJobStatus(job model.Job) string {
	prefix := ""
	if job.Metadata.TruncatedMediaData {
		prefix = "⚠ "
	}
	switch job.State {
	case model.JobQueued:
		return prefix + "等待 · " + formatBytes(job.Size)
	case model.JobOversize:
		return prefix + "超过上限 · " + formatBytes(job.Size)
	case model.JobUploading:
		if job.Error != "" {
			return prefix + job.Error
		}
		return prefix + fmt.Sprintf(
			"上传中 · %s / %s · %s/s",
			formatBytes(job.Uploaded),
			formatBytes(job.Size),
			formatBytes(int64(job.BytesPerSecond)),
		)
	case model.JobSending:
		return prefix + "正在提交频道消息…"
	case model.JobConfirming:
		return prefix + "待确认 · 点击处理"
	case model.JobSent:
		return prefix + "已发送 · " + formatBytes(job.Size)
	case model.JobCancelled:
		return prefix + "已取消"
	case model.JobFailed:
		return prefix + "失败 · 点击处理"
	case model.JobSkipped:
		return prefix + "已跳过"
	case model.JobMoved:
		return prefix + "已移动 · " + formatBytes(job.Size)
	case model.JobMoving:
		return prefix + "正在移动…"
	case model.JobInterrupted:
		return prefix + "上次中断 · 可重试"
	default:
		return prefix + string(job.State)
	}
}

func jobStatus(job model.Job) string {
	status := baseJobStatus(job)
	if job.Metadata.TruncatedMediaData {
		status = "⚠ 源 MP4 尾部结构不完整；不修复，原样传输 · " + status
	}
	return status
}

func baseJobStatus(job model.Job) string {
	switch job.State {
	case model.JobQueued:
		return "等待上传"
	case model.JobOversize:
		return "超过 Bot 当前上传上限，未上传"
	case model.JobUploading:
		if job.Error != "" {
			return job.Error
		}
		return fmt.Sprintf("上传中 · %s / %s · %s/s", formatBytes(job.Uploaded), formatBytes(job.Size), formatBytes(int64(job.BytesPerSecond)))
	case model.JobSending:
		return fmt.Sprintf("上传完成 · %s / %s · 正在提交频道消息……", formatBytes(job.Uploaded), formatBytes(job.Size))
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
	return formatSummaryWithCompleted(snapshot, completedJobCount(snapshot.Jobs))
}

func formatSummaryWithCompleted(snapshot coreapp.Snapshot, completed int) string {
	message := fmt.Sprintf("总进度：%s / %s · 已完成 %d / %d", formatBytes(snapshot.DoneBytes), formatBytes(snapshot.TotalBytes), completed, len(snapshot.Jobs))
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
