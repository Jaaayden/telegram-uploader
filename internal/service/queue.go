package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	coreapp "github.com/Jaaayden/telegram-uploader/internal/app"
	"github.com/Jaaayden/telegram-uploader/internal/model"
	"github.com/Jaaayden/telegram-uploader/internal/scanner"
)

// scanResult holds the candidates of the last scan until the user chooses
// which ones to add. Only the newest scan is kept.
type scanResult struct {
	token      string
	candidates []model.Job
	// existing is the ID of the queue entry with the same path, or "".
	existing []string
}

// Candidate is one scanned file offered for the queue.
type Candidate struct {
	Name string `json:"name"`
	// Folder is the containing directory, shown when sources are mixed.
	Folder   string `json:"folder"`
	Size     int64  `json:"size"`
	Oversize bool   `json:"oversize,omitempty"`
	// Duplicate means this exact file is already in the queue.
	// DuplicateState is the state of that entry; Replaceable is false while
	// it is uploading, being moved or awaiting confirmation.
	Duplicate      bool   `json:"duplicate,omitempty"`
	DuplicateState string `json:"duplicateState,omitempty"`
	Replaceable    bool   `json:"replaceable,omitempty"`
	// Similar means a different file with the same name and size is already
	// queued — probably a copy of the same video.
	Similar bool `json:"similar,omitempty"`
}

// ScanView is the result of scanning chosen or dropped files and folders.
type ScanView struct {
	Cancelled  bool        `json:"cancelled,omitempty"`
	Token      string      `json:"token,omitempty"`
	Source     string      `json:"source,omitempty"`
	Ignored    int         `json:"ignored,omitempty"`
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

// replaceableState reports whether a queued entry may be swapped for a fresh
// one. Entries in flight are excluded: removing them could lose track of a
// message that is being, or may have been, submitted.
func replaceableState(state model.JobState) bool {
	switch state {
	case model.JobUploading, model.JobSending, model.JobConfirming, model.JobMoving:
		return false
	}
	return true
}

// AddFolder lets the user choose a folder and scans it.
func (s *Service) AddFolder() (ScanView, error) {
	return s.pickAndScan(func() ([]string, error) {
		folder, err := s.host.PickFolder("选择包含 MP4 视频的文件夹")
		if err != nil || folder == "" {
			return nil, err
		}
		return []string{folder}, nil
	})
}

// AddFiles lets the user choose individual videos and scans them.
func (s *Service) AddFiles() (ScanView, error) {
	return s.pickAndScan(func() ([]string, error) { return s.host.PickFiles("选择要上传的 MP4 视频") })
}

func (s *Service) pickAndScan(pick func() ([]string, error)) (ScanView, error) {
	s.mu.Lock()
	busy := s.moveActive
	s.mu.Unlock()
	if busy {
		return ScanView{}, errors.New("请等待当前文件移动完成，再添加视频")
	}
	paths, err := pick()
	if err != nil {
		return ScanView{}, err
	}
	if len(paths) == 0 {
		return ScanView{Cancelled: true, Candidates: []Candidate{}}, nil
	}
	return s.ScanPaths(paths)
}

// DropPaths handles files and folders dragged onto the window. The result is
// delivered as a "scan" event because nothing in the page asked for it.
func (s *Service) DropPaths(paths []string) {
	if len(paths) == 0 || s.isClosed() {
		return
	}
	view, err := s.ScanPaths(paths)
	if err != nil {
		s.toastError(err)
		return
	}
	s.emit("scan", view)
}

// ScanPaths scans files and folders (the top level of each folder) for MP4
// videos and compares them with the queue. Nothing is queued until
// AddCandidates is called.
func (s *Service) ScanPaths(paths []string) (ScanView, error) {
	s.mu.Lock()
	if s.moveActive {
		s.mu.Unlock()
		return ScanView{}, errors.New("请等待当前文件移动完成，再添加视频")
	}
	if s.scanning {
		s.mu.Unlock()
		return ScanView{}, errors.New("正在扫描，请稍候")
	}
	s.scanning = true
	maxBytes := int64(0)
	if s.connected {
		maxBytes = s.identity.MaxUploadBytes
	}
	s.mu.Unlock()
	s.invalidate()
	defer func() {
		s.mu.Lock()
		s.scanning = false
		s.mu.Unlock()
		s.invalidate()
		s.scheduler.RetryDue()
	}()

	scanned, err := scanner.ScanPaths(paths, maxBytes)
	if err != nil {
		return ScanView{}, fmt.Errorf("扫描失败：%w", err)
	}
	source := filepath.Clean(paths[0])
	if len(paths) > 1 {
		source = fmt.Sprintf("%s 等 %d 项", filepath.Base(source), len(paths))
	} else if info, statErr := os.Stat(source); statErr == nil && info.IsDir() {
		// Remember the folder for next time; individual files are not a "source".
		if err := s.updateSettings(func(settings *coreapp.Settings) { settings.LastFolder = source }); err != nil {
			return ScanView{}, err
		}
	}

	s.mu.Lock()
	// One pass over the queue builds both lookups; each candidate is then O(1).
	type nameSize struct {
		name string
		size int64
	}
	byPath := make(map[string]*model.Job, len(s.snapshot.Jobs))
	byNameSize := make(map[nameSize]struct{}, len(s.snapshot.Jobs))
	for index := range s.snapshot.Jobs {
		job := &s.snapshot.Jobs[index]
		byPath[canonicalPathKey(job.Path)] = job
		byNameSize[nameSize{strings.ToLower(job.Name), job.Size}] = struct{}{}
	}
	s.scanCounter++
	result := &scanResult{
		token:      fmt.Sprintf("scan-%d", s.scanCounter),
		candidates: scanned.Jobs,
		existing:   make([]string, len(scanned.Jobs)),
	}
	view := ScanView{Token: result.token, Source: source, Ignored: scanned.Ignored, Candidates: make([]Candidate, len(scanned.Jobs))}
	for index, candidate := range scanned.Jobs {
		item := Candidate{
			Name:     candidate.Name,
			Folder:   filepath.Dir(candidate.Path),
			Size:     candidate.Size,
			Oversize: candidate.State == model.JobOversize,
		}
		if existing, ok := byPath[canonicalPathKey(candidate.Path)]; ok {
			result.existing[index] = existing.ID
			item.Duplicate = true
			item.DuplicateState = string(existing.State)
			item.Replaceable = replaceableState(existing.State)
		} else if _, ok := byNameSize[nameSize{strings.ToLower(candidate.Name), candidate.Size}]; ok {
			item.Similar = true
		}
		view.Candidates[index] = item
	}
	s.scan = result
	s.mu.Unlock()
	return view, nil
}

// AddCandidates queues the chosen files of a previous scan. A chosen file
// that is already queued is skipped unless replaceDuplicates is set, in which
// case its old entry is removed and the file is queued again from scratch.
func (s *Service) AddCandidates(token string, indexes []int, replaceDuplicates bool) (string, error) {
	s.mu.Lock()
	result := s.scan
	if result == nil || result.token != token {
		s.mu.Unlock()
		return "", errors.New("这次扫描结果已失效，请重新添加")
	}
	if s.moveActive {
		s.mu.Unlock()
		return "", errors.New("请等待当前文件移动完成，再添加视频")
	}
	selected := make([]model.Job, 0, len(indexes))
	var replace []string
	seen := make(map[int]struct{}, len(indexes))
	for _, index := range indexes {
		if index < 0 || index >= len(result.candidates) {
			continue
		}
		if _, ok := seen[index]; ok {
			continue
		}
		seen[index] = struct{}{}
		if existing := result.existing[index]; existing != "" {
			if !replaceDuplicates {
				continue
			}
			replace = append(replace, existing)
		}
		selected = append(selected, result.candidates[index])
	}
	s.scan = nil
	s.mu.Unlock()
	if len(selected) == 0 {
		return "", errors.New("没有选择可添加的视频")
	}

	replaced := 0
	if len(replace) > 0 {
		// Re-check against the live queue: an entry may have started uploading
		// since the scan. Those stay, and AddJobs then skips the new copy.
		states := make(map[string]model.JobState, len(replace))
		for _, job := range s.controller.Snapshot().Jobs {
			states[job.ID] = job.State
		}
		safe := replace[:0]
		for _, id := range replace {
			if state, ok := states[id]; ok && replaceableState(state) {
				safe = append(safe, id)
			}
		}
		if len(safe) > 0 {
			removal, err := s.controller.RemoveJobs(safe)
			if err != nil {
				return "", err
			}
			replaced = removal.Removed
		}
	}
	added, err := s.controller.AddJobs(selected)
	if err != nil {
		return "", err
	}
	s.scheduler.RetryDue()

	parts := []string{fmt.Sprintf("已添加 %d 个视频", added)}
	if replaced > 0 {
		parts[0] = fmt.Sprintf("已添加 %d 个视频（其中 %d 个替换了队列里的旧条目）", added, replaced)
	}
	if skipped := len(selected) - added; skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d 个仍在队列中处理，已跳过", skipped))
	}
	return strings.Join(parts, "；"), nil
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

// PauseUploads pauses the queue. By default the file that is uploading stops
// right away and continues later from where it was; with finishCurrent it is
// allowed to finish and the queue stops before the next file.
func (s *Service) PauseUploads(finishCurrent bool) error {
	var err error
	if finishCurrent {
		err = s.controller.Pause()
	} else {
		err = s.controller.PauseNow()
	}
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

// SetSchedule plans an automatic start at the given Unix time (seconds). It
// can be set at any time: on an idle or paused queue it plans when to start
// or continue, and on a running queue it pauses now and continues then.
func (s *Service) SetSchedule(unix int64) error {
	startAt := time.Unix(unix, 0)
	if !startAt.After(time.Now()) {
		return errors.New("定时开始的时间必须晚于现在")
	}
	s.mu.Lock()
	moving := s.moveActive
	s.mu.Unlock()
	if moving {
		return errors.New("文件移动进行中，不能设置定时")
	}
	if s.controller.Snapshot().Running {
		// A schedule on a running queue means "stop now, go on at that time".
		if err := s.controller.PauseNow(); err != nil {
			return err
		}
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
	// A paused queue is not an obstacle: the schedule is what resumes it. A
	// queue that is still winding down is retried when it has stopped.
	blocked := s.closed || s.snapshot.Running || s.moveActive || s.scanning
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
