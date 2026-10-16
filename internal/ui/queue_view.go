package ui

import (
	"fmt"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"

	coreapp "github.com/Jaaayden/telegram-uploader/internal/app"
	"github.com/Jaaayden/telegram-uploader/internal/model"
)

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

func completedJobCount(jobs []model.Job) int {
	count := 0
	for _, job := range jobs {
		if job.State == model.JobSent || job.State == model.JobMoved {
			count++
		}
	}
	return count
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
