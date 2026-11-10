package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	coreapp "github.com/Jaaayden/telegram-uploader/internal/app"
	"github.com/Jaaayden/telegram-uploader/internal/model"
	"github.com/Jaaayden/telegram-uploader/internal/scanner"
)

// scanResult holds the candidates of the last folder scan until the user
// chooses which ones to add. Only the newest scan is kept.
type scanResult struct {
	token      string
	candidates []model.Job
	duplicate  []bool
}

// Candidate is one scanned file offered for the queue.
type Candidate struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Oversize  bool   `json:"oversize,omitempty"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

// ScanView is the result of choosing and scanning a folder.
type ScanView struct {
	Cancelled  bool        `json:"cancelled,omitempty"`
	Token      string      `json:"token,omitempty"`
	Folder     string      `json:"folder,omitempty"`
	Candidates []Candidate `json:"candidates"`
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

// AddFolder lets the user choose a folder and scans its top level for MP4
// files. Nothing is queued until AddCandidates is called.
func (s *Service) AddFolder() (ScanView, error) {
	s.mu.Lock()
	if s.moveActive {
		s.mu.Unlock()
		return ScanView{}, errors.New("请等待当前文件移动完成，再添加文件夹")
	}
	if s.scanning {
		s.mu.Unlock()
		return ScanView{}, errors.New("正在扫描另一个文件夹，请稍候")
	}
	s.scanning = true
	s.mu.Unlock()
	s.invalidate()
	defer func() {
		s.mu.Lock()
		s.scanning = false
		s.mu.Unlock()
		s.invalidate()
		s.scheduler.RetryDue()
	}()

	folder, err := s.host.PickFolder("选择包含 MP4 视频的文件夹")
	if err != nil {
		return ScanView{}, err
	}
	if folder == "" {
		return ScanView{Cancelled: true, Candidates: []Candidate{}}, nil
	}
	folder = filepath.Clean(folder)
	if err := s.updateSettings(func(settings *coreapp.Settings) { settings.LastFolder = folder }); err != nil {
		return ScanView{}, err
	}
	s.mu.Lock()
	maxBytes := int64(0)
	if s.connected {
		maxBytes = s.identity.MaxUploadBytes
	}
	s.mu.Unlock()

	candidates, err := scanner.Scan(folder, maxBytes)
	if err != nil {
		return ScanView{}, fmt.Errorf("扫描文件夹失败：%w", err)
	}

	s.mu.Lock()
	existing := make(map[string]struct{}, len(s.snapshot.Jobs))
	for index := range s.snapshot.Jobs {
		existing[canonicalPathKey(s.snapshot.Jobs[index].Path)] = struct{}{}
	}
	s.scanCounter++
	result := &scanResult{
		token:      fmt.Sprintf("scan-%d", s.scanCounter),
		candidates: candidates,
		duplicate:  make([]bool, len(candidates)),
	}
	view := ScanView{Token: result.token, Folder: folder, Candidates: make([]Candidate, len(candidates))}
	for index, candidate := range candidates {
		_, duplicate := existing[canonicalPathKey(candidate.Path)]
		result.duplicate[index] = duplicate
		view.Candidates[index] = Candidate{
			Name:      candidate.Name,
			Size:      candidate.Size,
			Oversize:  candidate.State == model.JobOversize,
			Duplicate: duplicate,
		}
	}
	s.scan = result
	s.mu.Unlock()
	return view, nil
}

// AddCandidates queues the chosen files of a previous scan.
func (s *Service) AddCandidates(token string, indexes []int) (string, error) {
	s.mu.Lock()
	result := s.scan
	if result == nil || result.token != token {
		s.mu.Unlock()
		return "", errors.New("这次扫描结果已失效，请重新添加文件夹")
	}
	if s.moveActive {
		s.mu.Unlock()
		return "", errors.New("请等待当前文件移动完成，再添加视频")
	}
	selected := make([]model.Job, 0, len(indexes))
	seen := make(map[int]struct{}, len(indexes))
	for _, index := range indexes {
		if index < 0 || index >= len(result.candidates) || result.duplicate[index] {
			continue
		}
		if _, ok := seen[index]; ok {
			continue
		}
		seen[index] = struct{}{}
		selected = append(selected, result.candidates[index])
	}
	s.scan = nil
	s.mu.Unlock()
	if len(selected) == 0 {
		return "", errors.New("没有选择可添加的视频")
	}
	added, err := s.controller.AddJobs(selected)
	if err != nil {
		return "", err
	}
	s.scheduler.RetryDue()
	if skipped := len(selected) - added; skipped > 0 {
		return fmt.Sprintf("已添加 %d 个视频；%d 个已在队列中，已跳过", added, skipped), nil
	}
	return fmt.Sprintf("已添加 %d 个视频", added), nil
}

// DiscardScan forgets a scan the user dismissed.
func (s *Service) DiscardScan(token string) {
	s.mu.Lock()
	if s.scan != nil && s.scan.token == token {
		s.scan = nil
	}
	s.mu.Unlock()
}

// StartUploads starts or resumes the queue.
func (s *Service) StartUploads() error {
	s.mu.Lock()
	runnable := s.stats.Runnable
	connected := s.connected && s.client != nil
	connecting := s.connecting
	channelID := s.snapshot.Channel.ID
	s.mu.Unlock()
	if !runnable {
		return errors.New("队列里没有待上传的视频")
	}
	if !connected {
		s.ensureConnection()
		if connecting || s.isConnecting() {
			return errors.New("Telegram 正在连接，连接完成后再点开始")
		}
		return errors.New("还没有连接 Telegram，请先在设置里完成连接")
	}
	if channelID == 0 {
		return errors.New("还没有绑定目标频道，请先在设置里绑定")
	}
	return s.beginUploads(false)
}

func (s *Service) isConnecting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connecting
}

// beginUploads consumes a pending schedule, starts the controller and keeps
// the machine awake. If the start fails the schedule is restored.
func (s *Service) beginUploads(scheduled bool) error {
	scheduledAt, hadSchedule, wasDue := s.scheduler.State()
	if hadSchedule {
		if err := s.updateSettings(func(settings *coreapp.Settings) { settings.ScheduledStartUnix = 0 }); err != nil {
			return err
		}
		s.scheduler.Cancel()
	}
	if err := s.controller.Start(s.rootCtx); err != nil {
		if hadSchedule {
			restoreErr := s.updateSettings(func(settings *coreapp.Settings) { settings.ScheduledStartUnix = scheduledAt.Unix() })
			if scheduled || wasDue {
				s.scheduler.HoldDue(scheduledAt)
			} else {
				s.scheduler.Set(scheduledAt)
			}
			if restoreErr != nil {
				return errors.Join(err, fmt.Errorf("恢复定时设置失败：%w", restoreErr))
			}
		}
		s.invalidate()
		return err
	}

	message := "上传已开始，视频会按队列顺序逐个发送"
	if scheduled {
		message = "定时时间到，上传已自动开始"
	}
	if guard, err := s.keepAwake(); err != nil {
		message += "（无法阻止系统休眠：" + err.Error() + "）"
	} else {
		s.mu.Lock()
		previous := s.sleepGuard
		s.sleepGuard = guard
		s.resetRetryLocked()
		s.mu.Unlock()
		if previous != nil {
			_ = previous.Stop()
		}
	}
	s.toast("info", message)
	s.invalidate()
	return nil
}

// PauseUploads finishes the current file and stops before the next one.
func (s *Service) PauseUploads() error {
	err := s.controller.Pause()
	s.invalidate()
	return err
}

// CancelAll aborts the current file and cancels everything not yet sent.
func (s *Service) CancelAll() (string, error) {
	if err := s.controller.CancelAll(); err != nil {
		return "", err
	}
	return "已取消全部上传；需要恢复时可以用“重置任务”", nil
}

// RemoveJobs deletes the given rows from the local queue only.
func (s *Service) RemoveJobs(ids []string) (string, error) {
	if len(ids) == 0 {
		return "", nil
	}
	result, err := s.controller.RemoveJobs(ids)
	if err != nil {
		return "", err
	}
	current := s.controller.Snapshot()
	existing := make(map[string]struct{}, len(current.Jobs))
	for index := range current.Jobs {
		existing[current.Jobs[index].ID] = struct{}{}
	}
	pending := make(map[string]struct{}, len(result.PendingRemovalIDs))
	for _, id := range result.PendingRemovalIDs {
		pending[id] = struct{}{}
	}
	retained := 0
	for _, id := range ids {
		if _, still := existing[id]; !still {
			continue
		}
		if _, waiting := pending[id]; !waiting {
			retained++
		}
	}
	return removalSummary(result, retained), nil
}

func removalSummary(result coreapp.RemovalResult, retained int) string {
	parts := []string{fmt.Sprintf("已删除 %d 个条目", result.Removed)}
	if count := len(result.PendingRemovalIDs); count > 0 {
		parts = append(parts, fmt.Sprintf("%d 个正在上传的任务会在取消后删除", count))
	}
	if retained > 0 {
		parts = append(parts, fmt.Sprintf("%d 个待确认或正在移动的条目为安全起见保留", retained))
	}
	return strings.Join(parts, "；")
}

// RemoveCompleted deletes sent and moved rows.
func (s *Service) RemoveCompleted() (string, error) {
	result, err := s.controller.RemoveCompleted()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("已删除 %d 个已完成的条目", result.Removed), nil
}

// ClearQueue deletes every row that can safely be deleted.
func (s *Service) ClearQueue() (string, error) {
	result, err := s.controller.ClearQueue()
	if err != nil {
		return "", err
	}
	current := s.controller.Snapshot()
	pending := make(map[string]struct{}, len(result.PendingRemovalIDs))
	for _, id := range result.PendingRemovalIDs {
		pending[id] = struct{}{}
	}
	retained := 0
	for index := range current.Jobs {
		if _, waiting := pending[current.Jobs[index].ID]; !waiting {
			retained++
		}
	}
	if len(result.PendingRemovalIDs) == 0 && retained == 0 {
		return "队列已清空", nil
	}
	return removalSummary(result, retained), nil
}

// ResetJobs returns recoverable rows to "queued". mode is one of selected,
// cancelled, failed, skipped, all.
func (s *Service) ResetJobs(mode string, ids []string) (string, error) {
	var resetMode coreapp.ResetMode
	switch mode {
	case "selected":
		resetMode = coreapp.ResetSelected
	case "cancelled":
		resetMode = coreapp.ResetCancelled
	case "failed":
		resetMode = coreapp.ResetFailed
	case "skipped":
		resetMode = coreapp.ResetSkipped
	case "all":
		resetMode = coreapp.ResetAllRecoverable
	default:
		return "", errors.New("不支持的重置方式")
	}
	count, err := s.controller.ResetJobs(resetMode, ids)
	if err != nil {
		return "", err
	}
	s.scheduler.RetryDue()
	if count == 0 {
		return "没有符合条件的可恢复任务", nil
	}
	return fmt.Sprintf("已恢复 %d 个任务，可以直接开始或继续上传", count), nil
}

// MoveOversize lets the user choose a destination and moves every oversize
// file there in the background. Progress is reported through the state.
func (s *Service) MoveOversize() error {
	s.mu.Lock()
	if s.moveActive {
		s.mu.Unlock()
		return errors.New("文件移动已经在进行")
	}
	if s.snapshot.Running {
		s.mu.Unlock()
		return errors.New("上传进行中，不能移动超限文件")
	}
	s.mu.Unlock()

	destination, err := s.host.PickFolder("选择存放超限视频的文件夹")
	if err != nil {
		return err
	}
	if destination == "" {
		return nil
	}
	destination = filepath.Clean(destination)

	ctx, cancel := context.WithCancel(s.rootCtx)
	s.mu.Lock()
	if s.moveActive || s.closed {
		s.mu.Unlock()
		cancel()
		return nil
	}
	s.moveActive = true
	s.moveCancel = cancel
	s.moveDone, s.moveTotal = 0, 0
	s.workers.Add(1)
	s.mu.Unlock()
	s.invalidate()

	go func() {
		defer s.workers.Done()
		err := s.controller.MoveOversize(ctx, destination, func(progress model.Progress) {
			s.mu.Lock()
			s.moveDone, s.moveTotal = progress.BytesDone, progress.BytesTotal
			s.mu.Unlock()
			// State events are coalesced, so this cannot flood the interface.
			s.invalidate()
		})
		cancel()
		s.mu.Lock()
		s.moveActive = false
		s.moveCancel = nil
		s.mu.Unlock()
		switch {
		case err == nil:
			s.toast("success", "超限文件已全部移动")
		case errors.Is(err, context.Canceled):
			s.toast("info", "文件移动已取消")
		default:
			s.toast("error", "移动失败："+err.Error())
		}
		s.invalidate()
		s.scheduler.RetryDue()
	}()
	return nil
}

// CancelMove stops a running move after the current file.
func (s *Service) CancelMove() {
	s.mu.Lock()
	cancel := s.moveCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Service) restoreSchedule() {
	startUnix := s.settingsCopy().ScheduledStartUnix
	if startUnix <= 0 {
		return
	}
	s.scheduler.Set(time.Unix(startUnix, 0))
	s.invalidate()
}

// SetSchedule plans an automatic start at the given Unix time (seconds).
func (s *Service) SetSchedule(unix int64) error {
	startAt := time.Unix(unix, 0)
	if !startAt.After(time.Now()) {
		return errors.New("定时开始的时间必须晚于现在")
	}
	s.mu.Lock()
	busy := s.snapshot.Running || s.moveActive
	s.mu.Unlock()
	if busy {
		return errors.New("上传或文件移动进行中，不能设置定时")
	}
	if err := s.updateSettings(func(settings *coreapp.Settings) { settings.ScheduledStartUnix = startAt.Unix() }); err != nil {
		return err
	}
	s.mu.Lock()
	s.resetRetryLocked()
	s.mu.Unlock()
	s.scheduler.Set(startAt)
	s.ensureConnection()
	s.invalidate()
	return nil
}

// CancelSchedule removes a planned start.
func (s *Service) CancelSchedule() error {
	if _, set, _ := s.scheduler.State(); !set {
		return nil
	}
	if err := s.updateSettings(func(settings *coreapp.Settings) { settings.ScheduledStartUnix = 0 }); err != nil {
		return err
	}
	s.scheduler.Cancel()
	s.invalidate()
	return nil
}

// tryScheduledStart runs whenever a due schedule might be able to start: when
// the time arrives and again after every change that could unblock it.
func (s *Service) tryScheduledStart() {
	if !s.scheduleStartMu.TryLock() {
		return
	}
	defer s.scheduleStartMu.Unlock()
	if _, set, due := s.scheduler.State(); !set || !due {
		return
	}
	s.mu.Lock()
	blocked := s.closed || s.snapshot.Running || s.moveActive || s.scanning || s.snapshot.Paused || s.snapshot.PauseRequested
	connected := s.connected && s.client != nil
	ready := s.snapshot.Channel.ID != 0 && s.stats.Runnable
	s.mu.Unlock()
	s.invalidate()
	if blocked {
		return
	}
	if !connected {
		s.ensureConnection()
		return
	}
	if !ready {
		return
	}
	if err := s.beginUploads(true); err != nil {
		s.toast("error", "定时启动失败，计划仍然保留："+err.Error())
	}
}
