package ui

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	coreapp "github.com/Jaaayden/telegram-uploader/internal/app"
	"github.com/Jaaayden/telegram-uploader/internal/model"
	"github.com/Jaaayden/telegram-uploader/internal/platform"
)

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
	detailsDialog := dialog.NewCustom("队列条目详情", "关闭", u.jobDetailsContent(job), u.window)
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

func mapOperationResult(err error, success string) string {
	if err == nil {
		return success
	}
	if errors.Is(err, context.Canceled) {
		return "操作已取消"
	}
	return "操作失败：" + err.Error()
}
