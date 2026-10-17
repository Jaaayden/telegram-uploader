package ui

import (
	"errors"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	coreapp "github.com/Jaaayden/telegram-uploader/internal/app"
)

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
