package ui

import (
	"errors"
	"fmt"
	"path/filepath"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"

	coreapp "github.com/Jaaayden/telegram-uploader/internal/app"
	"github.com/Jaaayden/telegram-uploader/internal/model"
	"github.com/Jaaayden/telegram-uploader/internal/scanner"
)

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
