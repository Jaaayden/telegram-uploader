package ui

import (
	"errors"
	"fmt"
	"strconv"

	coreapp "github.com/Jaaayden/telegram-uploader/internal/app"
	"github.com/Jaaayden/telegram-uploader/internal/credentials"
)

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
		case progress, ok := <-u.controller.ProgressUpdates():
			if !ok {
				return
			}
			u.dispatchControllerProgress(progress)
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

func (u *window) queueRunning() bool {
	if u.controller != nil {
		return u.controller.Snapshot().Running
	}
	return u.snapshot.Running
}
