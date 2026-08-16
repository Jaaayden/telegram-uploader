package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jayden/telegram-video-uploader/internal/media"
	"github.com/jayden/telegram-video-uploader/internal/model"
	"github.com/jayden/telegram-video-uploader/internal/mover"
	"github.com/jayden/telegram-video-uploader/internal/scanner"
	tgtransport "github.com/jayden/telegram-video-uploader/internal/telegram"
)

type QueueStore interface {
	Save([]model.Job, model.Channel, bool) error
	Load() ([]model.Job, model.Channel, bool, error)
}

type Snapshot struct {
	Jobs    []model.Job
	Channel model.Channel
	Folder  string
	Running bool
	// Paused means that the queue has an explicit pause intent. When Running
	// is also true, the current file is allowed to finish and the queue will
	// stop before starting the next file.
	Paused bool
	// PauseRequested is true only while a running queue is finishing its
	// current file in response to Pause(). It is exposed separately so the UI
	// can explain why the active file is still moving.
	PauseRequested bool
	ActiveID       string
	LastError      string
	DoneBytes      int64
	TotalBytes     int64
	BytesPerSecond float64
	ETA            time.Duration
}

// ResetMode selects which recoverable queue jobs should return to JobQueued.
// Sent, moved, oversize and confirming jobs are deliberately excluded from
// every bulk mode: resetting them without an explicit per-job decision could
// duplicate a Telegram post, lose move history, or retry an ineligible file.
type ResetMode uint8

const (
	ResetSelected ResetMode = iota + 1
	ResetCancelled
	ResetFailed
	ResetSkipped
	ResetAllRecoverable
)

type Controller struct {
	mu        sync.RWMutex
	persistMu sync.Mutex
	opMu      sync.Mutex

	store   QueueStore
	mover   *mover.Mover
	gateway tgtransport.Gateway

	jobs    []model.Job
	channel model.Channel
	folder  string
	running bool
	paused  bool
	// pauseRequested is a transient view of a pause intent while the queue is
	// still running. paused remains true until Resume or Start clears it.
	pauseRequested bool
	// pauseRevision prevents a failed persistence attempt from rolling back a
	// newer Pause, Resume, Start, or CancelAll action.
	pauseRevision uint64
	activeID      string
	lastError     string

	activeCancel       context.CancelFunc
	allCancel          context.CancelFunc
	cancelJobID        string
	cancelAllRequested bool
	updates            chan Snapshot
	lastPersist        time.Time
}

func NewController(store QueueStore, fileMover *mover.Mover) *Controller {
	if fileMover == nil {
		fileMover = mover.New()
	}
	return &Controller{
		store:   store,
		mover:   fileMover,
		updates: make(chan Snapshot, 1),
	}
}

func (c *Controller) SetGateway(gateway tgtransport.Gateway) {
	c.mu.Lock()
	c.gateway = gateway
	c.mu.Unlock()
}

func (c *Controller) Updates() <-chan Snapshot { return c.updates }

func (c *Controller) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snapshotLocked()
}

func (c *Controller) Load() error {
	if !c.opMu.TryLock() {
		return errors.New("另一个文件操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()
	if c.store == nil {
		return nil
	}
	jobs, channel, paused, err := c.store.Load()
	if err != nil {
		return err
	}
	changed := reconcileMovingJobs(jobs)
	if reindexJobs(jobs) {
		changed = true
	}
	c.mu.Lock()
	c.jobs = append([]model.Job(nil), jobs...)
	c.channel = channel
	c.paused = paused
	c.pauseRequested = false
	if len(jobs) > 0 {
		c.folder = filepath.Dir(jobs[0].Path)
	}
	c.lastError = ""
	c.notifyLocked()
	c.mu.Unlock()
	if changed {
		return c.persist()
	}
	return nil
}

func reconcileMovingJobs(jobs []model.Job) bool {
	changed := false
	for i := range jobs {
		job := &jobs[i]
		if job.State != model.JobMoving {
			continue
		}
		changed = true
		sourceExists, sourceErr := regularFileExists(job.Path)
		destinationExists, destinationErr := regularFileExists(job.MoveDestination)
		switch {
		case sourceErr != nil || destinationErr != nil:
			job.State = model.JobFailed
			job.Error = "恢复上次文件移动时无法检查源文件或目标文件"
		case sourceExists && !destinationExists:
			job.State = model.JobOversize
			job.MoveDestination = ""
			job.Error = "上次移动在完成前中断，源文件仍然安全保留"
		case !sourceExists && destinationExists:
			job.State = model.JobMoved
			job.Path = job.MoveDestination
			job.MoveDestination = ""
			job.Error = ""
		case sourceExists && destinationExists:
			job.State = model.JobOversize
			job.Error = "上次移动中断后源文件和目标文件都存在；未自动删除任何一份"
		default:
			job.State = model.JobFailed
			job.Error = "上次移动中断，源文件和目标文件都不存在"
		}
	}
	return changed
}

func regularFileExists(path string) (bool, error) {
	if path == "" {
		return false, nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular(), nil
}

func (c *Controller) Scan(folder string, maxBytes int64) error {
	if !c.opMu.TryLock() {
		return errors.New("上传、扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()
	c.mu.RLock()
	running := c.running
	c.mu.RUnlock()
	if running {
		return errors.New("上传进行中，不能重新扫描文件夹")
	}
	jobs, err := scanner.Scan(folder, maxBytes)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.jobs = jobs
	c.folder = folder
	c.lastError = ""
	c.notifyLocked()
	c.mu.Unlock()
	return c.persist()
}

// Pause requests a soft pause. It never cancels the active Telegram request:
// the current file is allowed to finish, then runQueue stops before selecting
// another runnable job. The active-run path intentionally bypasses opMu
// because the upload goroutine owns that lock for the duration of a run;
// idle pauses still use opMu to serialize with queue editing.
func (c *Controller) Pause() error {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		if !c.opMu.TryLock() {
			return errors.New("上传、扫描或移动操作正在进行，请稍后重试")
		}
		defer c.opMu.Unlock()
		c.mu.Lock()
	}
	wasPaused := c.paused
	wasRequested := c.pauseRequested
	c.paused = true
	if c.running {
		c.pauseRequested = true
	}
	stateChanged := !wasPaused || wasRequested != c.pauseRequested
	if stateChanged {
		c.pauseRevision++
		c.notifyLocked()
	}
	revision := c.pauseRevision
	c.mu.Unlock()
	if !stateChanged {
		return nil
	}
	if err := c.persist(); err != nil {
		c.mu.Lock()
		if c.pauseRevision == revision {
			c.paused = wasPaused
			c.pauseRequested = wasRequested && c.running
			c.pauseRevision++
			c.notifyLocked()
		}
		c.mu.Unlock()
		c.setPersistenceError(err)
		return err
	}
	return nil
}

// Resume clears a soft pause without starting a queue by itself. Callers can
// then use Start, which also accepts a paused queue and resumes it. If the
// queue is still finishing its active file, clearing the request lets it
// continue with the next file as soon as that operation returns.
func (c *Controller) Resume() error {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		if !c.opMu.TryLock() {
			return errors.New("上传、扫描或移动操作正在进行，请稍后重试")
		}
		defer c.opMu.Unlock()
		c.mu.Lock()
	}
	if !c.paused && !c.pauseRequested {
		c.mu.Unlock()
		return nil
	}
	wasPaused := c.paused
	wasRequested := c.pauseRequested
	c.paused = false
	c.pauseRequested = false
	c.pauseRevision++
	revision := c.pauseRevision
	c.notifyLocked()
	c.mu.Unlock()
	if err := c.persist(); err != nil {
		c.mu.Lock()
		if c.pauseRevision == revision {
			c.paused = wasPaused
			c.pauseRequested = wasRequested && c.running
			c.pauseRevision++
			c.notifyLocked()
		}
		c.mu.Unlock()
		c.setPersistenceError(err)
		return err
	}
	return nil
}

// IsPaused reports an explicit pause intent. It is safe to call while an
// upload is active and is equivalent to Snapshot().Paused.
func (c *Controller) IsPaused() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.paused
}

// AddJobs appends scanned candidates to the durable queue while the queue is
// idle. Candidates are de-duplicated by their normalized absolute path. An
// existing path always wins, so a previously sent/failed job keeps its ID,
// RandomID and state instead of being replaced by a newly scanned copy.
//
// The scanner already returns candidates in natural filename order. When
// adding candidates from another folder, their order is preserved and the
// new group is appended after the existing queue. Positions are rebuilt for
// the complete queue after every successful append.
func (c *Controller) AddJobs(candidates []model.Job) error {
	if len(candidates) == 0 {
		return nil
	}
	if !c.opMu.TryLock() {
		return errors.New("上传、扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()

	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return errors.New("上传进行中，不能追加队列")
	}

	nextJobs := append([]model.Job(nil), c.jobs...)
	paths := make(map[string]struct{}, len(c.jobs)+len(candidates))
	ids := make(map[string]struct{}, len(c.jobs)+len(candidates))
	randomIDs := make(map[int64]struct{}, len(c.jobs)+len(candidates))
	for _, job := range c.jobs {
		if key := jobPathKey(job.Path); key != "" {
			paths[key] = struct{}{}
		}
		if job.ID != "" {
			ids[job.ID] = struct{}{}
		}
		if job.RandomID != 0 {
			randomIDs[job.RandomID] = struct{}{}
		}
	}
	added := 0
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate.Path) == "" {
			c.mu.Unlock()
			return errors.New("追加任务失败：文件路径不能为空")
		}
		key := jobPathKey(candidate.Path)
		if _, exists := paths[key]; exists {
			continue
		}
		if strings.TrimSpace(candidate.ID) == "" {
			c.mu.Unlock()
			return fmt.Errorf("追加任务 %q 失败：Job ID 不能为空", candidate.Name)
		}
		if _, exists := ids[candidate.ID]; exists {
			c.mu.Unlock()
			return fmt.Errorf("追加任务 %q 失败：Job ID 重复", candidate.Name)
		}
		if candidate.RandomID == 0 {
			c.mu.Unlock()
			return fmt.Errorf("追加任务 %q 失败：Telegram RandomID 不能为空", candidate.Name)
		}
		if _, exists := randomIDs[candidate.RandomID]; exists {
			c.mu.Unlock()
			return fmt.Errorf("追加任务 %q 失败：Telegram RandomID 重复", candidate.Name)
		}
		if strings.TrimSpace(candidate.Name) == "" || candidate.Size < 0 {
			c.mu.Unlock()
			return errors.New("追加任务失败：文件名或大小无效")
		}
		if candidate.State == "" {
			candidate.State = model.JobQueued
		}
		if candidate.State != model.JobQueued && candidate.State != model.JobOversize {
			c.mu.Unlock()
			return fmt.Errorf("追加任务 %q 失败：不允许的初始状态 %q", candidate.Name, candidate.State)
		}
		nextJobs = append(nextJobs, candidate)
		paths[key] = struct{}{}
		ids[candidate.ID] = struct{}{}
		randomIDs[candidate.RandomID] = struct{}{}
		added++
	}
	if added == 0 {
		c.mu.Unlock()
		return nil
	}
	reindexJobs(nextJobs)
	channel := c.channel
	paused := c.paused
	c.mu.Unlock()

	if err := c.saveSnapshot(nextJobs, channel, paused); err != nil {
		c.setPersistenceError(err)
		return err
	}
	c.mu.Lock()
	c.jobs = nextJobs
	c.lastError = ""
	c.notifyLocked()
	c.mu.Unlock()
	return nil
}

// RemoveJobs removes the specified local queue entries while the queue is
// idle. This only changes the local queue; it never deletes a message from
// Telegram. Unknown IDs are ignored so repeated UI actions are safe.
func (c *Controller) RemoveJobs(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if !c.opMu.TryLock() {
		return errors.New("上传、扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()

	selected := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id != "" {
			selected[id] = struct{}{}
		}
	}
	if len(selected) == 0 {
		return nil
	}

	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return errors.New("上传进行中，不能删除队列任务")
	}
	nextJobs, removed := filteredJobs(c.jobs, func(job model.Job) bool {
		_, ok := selected[job.ID]
		return ok
	})
	if removed == 0 {
		c.mu.Unlock()
		return nil
	}
	channel := c.channel
	paused := c.paused
	c.mu.Unlock()

	if err := c.saveSnapshot(nextJobs, channel, paused); err != nil {
		c.setPersistenceError(err)
		return err
	}
	c.mu.Lock()
	c.jobs = nextJobs
	if len(nextJobs) == 0 {
		c.folder = ""
	}
	c.lastError = ""
	c.notifyLocked()
	c.mu.Unlock()
	return nil
}

// ClearQueue removes every local queue entry while idle. It does not affect
// already uploaded Telegram messages.
func (c *Controller) ClearQueue() error {
	if !c.opMu.TryLock() {
		return errors.New("上传、扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()

	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return errors.New("上传进行中，不能清空队列")
	}
	if len(c.jobs) == 0 {
		c.mu.Unlock()
		return nil
	}
	channel := c.channel
	paused := c.paused
	c.mu.Unlock()

	if err := c.saveSnapshot(nil, channel, paused); err != nil {
		c.setPersistenceError(err)
		return err
	}
	c.mu.Lock()
	c.jobs = nil
	c.folder = ""
	c.lastError = ""
	c.notifyLocked()
	c.mu.Unlock()
	return nil
}

// RemoveCompleted removes jobs whose local terminal state means the file was
// already handled: sent to Telegram or moved as an oversize file. Failed,
// skipped and cancelled entries remain available for recovery or inspection.
func (c *Controller) RemoveCompleted() error {
	if !c.opMu.TryLock() {
		return errors.New("上传、扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()

	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return errors.New("上传进行中，不能删除已完成任务")
	}
	nextJobs, removed := filteredJobs(c.jobs, func(job model.Job) bool {
		return job.State == model.JobSent || job.State == model.JobMoved
	})
	if removed == 0 {
		c.mu.Unlock()
		return nil
	}
	channel := c.channel
	paused := c.paused
	c.mu.Unlock()

	if err := c.saveSnapshot(nextJobs, channel, paused); err != nil {
		c.setPersistenceError(err)
		return err
	}
	c.mu.Lock()
	c.jobs = nextJobs
	if len(nextJobs) == 0 {
		c.folder = ""
	}
	c.lastError = ""
	c.notifyLocked()
	c.mu.Unlock()
	return nil
}

// ResetJobs returns recoverable terminal jobs to the queued state while the
// queue is idle. ResetSelected applies the same safety rules to the supplied
// job IDs; the other modes ignore ids and select jobs by durable state.
//
// The complete next queue is persisted before it is published to observers,
// so a storage failure cannot leave the UI and queue.json disagreeing.
func (c *Controller) ResetJobs(mode ResetMode, ids []string) (int, error) {
	if !validResetMode(mode) {
		return 0, errors.New("未知的任务重置方式")
	}

	selected := make(map[string]struct{}, len(ids))
	if mode == ResetSelected {
		for _, id := range ids {
			if id != "" {
				selected[id] = struct{}{}
			}
		}
		if len(selected) == 0 {
			return 0, nil
		}
	}

	if !c.opMu.TryLock() {
		return 0, errors.New("上传、扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()

	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return 0, errors.New("上传进行中，不能重置队列任务")
	}
	nextJobs := append([]model.Job(nil), c.jobs...)
	channel := c.channel
	paused := c.paused
	c.mu.Unlock()

	resetCount := 0
	for i := range nextJobs {
		job := &nextJobs[i]
		if !matchesResetMode(*job, mode, selected) {
			continue
		}
		job.State = model.JobQueued
		resetPendingJobProgress(job)
		resetCount++
	}
	if resetCount == 0 {
		return 0, nil
	}

	if err := c.saveSnapshot(nextJobs, channel, paused); err != nil {
		c.setPersistenceError(err)
		return 0, err
	}
	c.mu.Lock()
	c.jobs = nextJobs
	c.lastError = ""
	c.notifyLocked()
	c.mu.Unlock()
	return resetCount, nil
}

func validResetMode(mode ResetMode) bool {
	switch mode {
	case ResetSelected, ResetCancelled, ResetFailed, ResetSkipped, ResetAllRecoverable:
		return true
	default:
		return false
	}
}

func matchesResetMode(job model.Job, mode ResetMode, selected map[string]struct{}) bool {
	if !isRecoverableJobState(job.State) {
		return false
	}
	switch mode {
	case ResetSelected:
		_, ok := selected[job.ID]
		return ok
	case ResetCancelled:
		return job.State == model.JobCancelled
	case ResetFailed:
		return job.State == model.JobFailed || job.State == model.JobInterrupted
	case ResetSkipped:
		return job.State == model.JobSkipped
	case ResetAllRecoverable:
		return true
	default:
		return false
	}
}

func isRecoverableJobState(state model.JobState) bool {
	switch state {
	case model.JobCancelled, model.JobFailed, model.JobInterrupted, model.JobSkipped:
		return true
	default:
		return false
	}
}

func filteredJobs(jobs []model.Job, remove func(model.Job) bool) ([]model.Job, int) {
	kept := make([]model.Job, 0, len(jobs))
	removed := 0
	for _, job := range jobs {
		if remove(job) {
			removed++
			continue
		}
		kept = append(kept, job)
	}
	if removed > 0 {
		reindexJobs(kept)
	}
	return kept, removed
}

func reindexJobs(jobs []model.Job) bool {
	changed := false
	for index := range jobs {
		if jobs[index].Position != index {
			changed = true
		}
		jobs[index].Position = index
	}
	return changed
}

func jobPathKey(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	absPath, err := filepath.Abs(path)
	if err == nil {
		path = absPath
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return path
}

// ApplyUploadLimit updates only jobs whose eligibility can still change. It
// is used when a folder was scanned before Telegram reported the current Bot
// upload limit, and deliberately preserves sent, failed and active history.
func (c *Controller) ApplyUploadLimit(maxBytes int64) error {
	if maxBytes <= 0 {
		return errors.New("上传上限必须大于 0")
	}
	if !c.opMu.TryLock() {
		return errors.New("上传、扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()

	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return errors.New("上传进行中，不能更新文件上限")
	}
	changed := false
	for i := range c.jobs {
		job := &c.jobs[i]
		switch job.State {
		case model.JobQueued, model.JobInterrupted, model.JobOversize:
		default:
			continue
		}
		if job.Size > maxBytes {
			if job.State != model.JobOversize || job.Uploaded != 0 {
				job.State = model.JobOversize
				resetPendingJobProgress(job)
				changed = true
			}
		} else if job.State == model.JobOversize {
			job.State = model.JobQueued
			resetPendingJobProgress(job)
			changed = true
		}
	}
	if changed {
		c.notifyLocked()
	}
	c.mu.Unlock()
	if !changed {
		return nil
	}
	if err := c.persist(); err != nil {
		c.setPersistenceError(err)
		return err
	}
	return nil
}

func resetPendingJobProgress(job *model.Job) {
	job.Uploaded = 0
	job.BytesPerSecond = 0
	job.MessageID = 0
	job.ChannelID = 0
	job.Metadata = model.VideoMetadata{}
	job.Error = ""
	job.StartedAt = nil
	job.CompletedAt = nil
	job.MoveDestination = ""
}

func (c *Controller) SetChannel(channel model.Channel) error {
	if !c.opMu.TryLock() {
		return errors.New("上传、扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return errors.New("上传进行中，不能切换频道")
	}
	c.channel = channel
	c.notifyLocked()
	c.mu.Unlock()
	return c.persist()
}

func (c *Controller) Start(parent context.Context) error {
	if !c.opMu.TryLock() {
		return errors.New("上传、扫描或移动操作正在进行，请稍后重试")
	}
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		c.opMu.Unlock()
		return errors.New("上传队列已经在运行")
	}
	if c.gateway == nil {
		c.mu.Unlock()
		c.opMu.Unlock()
		return errors.New("请先连接 Telegram Bot")
	}
	if c.channel.ID == 0 {
		c.mu.Unlock()
		c.opMu.Unlock()
		return errors.New("请先绑定目标频道")
	}
	if !hasRunnableJob(c.jobs) {
		c.mu.Unlock()
		c.opMu.Unlock()
		return errors.New("队列中没有待上传视频")
	}
	wasPaused := c.paused || c.pauseRequested
	if wasPaused {
		c.paused = false
		c.pauseRequested = false
		c.pauseRevision++
	}
	startPauseRevision := c.pauseRevision
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	c.running = true
	c.allCancel = cancel
	c.cancelJobID = ""
	c.cancelAllRequested = false
	c.lastError = ""
	c.notifyLocked()
	c.mu.Unlock()

	// Persist every random_id before the first send can happen.
	if err := c.persist(); err != nil {
		cancel()
		c.mu.Lock()
		c.running = false
		c.allCancel = nil
		if wasPaused && c.pauseRevision == startPauseRevision {
			c.paused = true
			c.pauseRevision++
		}
		// A pause request is transient and only meaningful while Running is
		// true. Preserve any newer paused intent, but normalize its view.
		c.pauseRequested = false
		c.lastError = err.Error()
		c.notifyLocked()
		c.mu.Unlock()
		c.opMu.Unlock()
		return fmt.Errorf("保存上传队列失败：%w", err)
	}
	go c.runQueue(ctx)
	return nil
}

func hasRunnableJob(jobs []model.Job) bool {
	for _, job := range jobs {
		if job.State == model.JobQueued || job.State == model.JobInterrupted {
			return true
		}
	}
	return false
}

func openVerifiedJob(job model.Job) (*os.File, model.VideoMetadata, error) {
	pathInfo, err := os.Lstat(job.Path)
	if err != nil {
		return nil, model.VideoMetadata{}, fmt.Errorf("上传前检查失败：%w", err)
	}
	if !pathInfo.Mode().IsRegular() || pathInfo.Size() != job.Size || !pathInfo.ModTime().Equal(job.ModTime) {
		return nil, model.VideoMetadata{}, errors.New("上传前检查失败：文件已被替换或修改")
	}
	file, err := os.Open(job.Path)
	if err != nil {
		return nil, model.VideoMetadata{}, fmt.Errorf("打开视频文件失败：%w", err)
	}
	fileInfo, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, model.VideoMetadata{}, fmt.Errorf("读取视频文件状态失败：%w", err)
	}
	if !fileInfo.Mode().IsRegular() || !os.SameFile(pathInfo, fileInfo) || fileInfo.Size() != job.Size || !fileInfo.ModTime().Equal(job.ModTime) {
		_ = file.Close()
		return nil, model.VideoMetadata{}, errors.New("上传前检查失败：文件在扫描后发生了变化")
	}
	metadata, err := media.ParseMP4MetadataFile(file)
	if err != nil {
		_ = file.Close()
		return nil, model.VideoMetadata{}, err
	}
	return file, metadata, nil
}

func (c *Controller) runQueue(ctx context.Context) {
	defer func() {
		c.mu.Lock()
		c.running = false
		if c.pauseRequested {
			c.pauseRevision++
		}
		c.pauseRequested = false
		c.activeID = ""
		c.activeCancel = nil
		c.allCancel = nil
		c.cancelJobID = ""
		c.cancelAllRequested = false
		c.notifyLocked()
		c.mu.Unlock()
		if err := c.persist(); err != nil {
			c.setPersistenceError(err)
		}
		c.opMu.Unlock()
	}()

	for {
		index, job, gateway, channel, ok := c.nextJob()
		if !ok {
			return
		}
		if err := ctx.Err(); err != nil {
			return
		}
		if gateway == nil {
			c.failJob(index, job.ID, tgtransport.ErrNotConnected)
			return
		}

		file, metadata, err := openVerifiedJob(job)
		if err != nil {
			c.failJob(index, job.ID, err)
			return
		}

		fileCtx, fileCancel := context.WithCancel(ctx)
		now := time.Now()
		c.mu.Lock()
		// Pause may have been requested after nextJob selected this entry but
		// before the file was opened. Re-check under the same mutex used by
		// Pause so a between-files pause cannot start a new Telegram request.
		if c.paused || c.pauseRequested {
			c.mu.Unlock()
			fileCancel()
			_ = file.Close()
			return
		}
		c.jobs[index].Metadata = metadata
		c.jobs[index].State = model.JobUploading
		c.jobs[index].Uploaded = 0
		c.jobs[index].BytesPerSecond = 0
		c.jobs[index].StartedAt = &now
		c.jobs[index].Error = ""
		c.activeID = job.ID
		c.activeCancel = fileCancel
		c.notifyLocked()
		c.mu.Unlock()
		if err := c.persist(); err != nil {
			fileCancel()
			_ = file.Close()
			c.failJob(index, job.ID, fmt.Errorf("保存上传状态失败：%w", err))
			return
		}

		messageID, err := gateway.UploadVideo(fileCtx, tgtransport.UploadRequest{
			Channel:  channel,
			Path:     job.Path,
			File:     file,
			Name:     job.Name,
			Caption:  captionFromFilename(job.Name),
			RandomID: job.RandomID,
			Metadata: metadata,
		}, func(progress model.Progress) {
			c.applyProgress(index, job.ID, progress)
		})
		fileCancel()
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = fmt.Errorf("关闭视频文件失败：%w", closeErr)
		}

		c.mu.Lock()
		c.activeCancel = nil
		c.activeID = ""
		stateAtReturn := c.jobs[index].State
		cancelCurrent := c.cancelJobID == job.ID
		if cancelCurrent {
			c.cancelJobID = ""
		}
		cancelAll := c.cancelAllRequested
		c.mu.Unlock()

		if err != nil {
			if errors.Is(err, context.Canceled) {
				c.mu.Lock()
				if stateAtReturn == model.JobSending || stateAtReturn == model.JobConfirming {
					c.jobs[index].State = model.JobConfirming
					c.jobs[index].Error = "取消发生在消息提交阶段，结果可能已送达；请先检查频道"
					c.lastError = c.jobs[index].Error
					c.notifyLocked()
					c.mu.Unlock()
					if persistErr := c.persist(); persistErr != nil {
						c.setPersistenceError(persistErr)
					}
					return
				}
				if cancelCurrent || cancelAll {
					c.jobs[index].State = model.JobCancelled
					c.jobs[index].Error = ""
				} else {
					c.jobs[index].State = model.JobInterrupted
					c.jobs[index].Error = "程序关闭或连接中断，可重新开始上传"
				}
				c.notifyLocked()
				c.mu.Unlock()
				if persistErr := c.persist(); persistErr != nil {
					c.setPersistenceError(persistErr)
				}
				if ctx.Err() != nil {
					return
				}
				continue
			}
			if errors.Is(err, tgtransport.ErrSendOutcomeUnknown) {
				c.mu.Lock()
				c.jobs[index].State = model.JobConfirming
				c.jobs[index].Error = "消息可能已经送达；请先检查频道，再选择“已发送”或“重试”"
				c.lastError = c.jobs[index].Error
				c.notifyLocked()
				c.mu.Unlock()
				if persistErr := c.persist(); persistErr != nil {
					c.setPersistenceError(persistErr)
				}
				return
			}
			c.failJob(index, job.ID, err)
			return
		}

		completed := time.Now()
		c.mu.Lock()
		c.jobs[index].State = model.JobSent
		c.jobs[index].Uploaded = c.jobs[index].Size
		c.jobs[index].MessageID = messageID
		c.jobs[index].ChannelID = channel.ID
		c.jobs[index].CompletedAt = &completed
		c.jobs[index].Error = ""
		c.notifyLocked()
		c.mu.Unlock()
		if err := c.persist(); err != nil {
			c.mu.Lock()
			c.lastError = "消息已发送，但保存本地状态失败：" + err.Error()
			c.notifyLocked()
			c.mu.Unlock()
			return
		}
	}
}

func (c *Controller) nextJob() (int, model.Job, tgtransport.Gateway, model.Channel, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.paused || c.pauseRequested {
		return 0, model.Job{}, nil, model.Channel{}, false
	}
	for i, job := range c.jobs {
		if job.State == model.JobQueued || job.State == model.JobInterrupted {
			return i, job, c.gateway, c.channel, true
		}
	}
	return 0, model.Job{}, nil, model.Channel{}, false
}

func captionFromFilename(name string) string {
	extension := filepath.Ext(name)
	if strings.EqualFold(extension, ".mp4") {
		return strings.TrimSuffix(name, extension)
	}
	return name
}

func (c *Controller) applyProgress(index int, jobID string, progress model.Progress) {
	c.mu.Lock()
	if index < 0 || index >= len(c.jobs) || c.jobs[index].ID != jobID {
		c.mu.Unlock()
		return
	}
	job := &c.jobs[index]
	if job.State != model.JobUploading && job.State != model.JobSending {
		c.mu.Unlock()
		return
	}
	if progress.BytesDone > job.Uploaded {
		job.Uploaded = progress.BytesDone
	}
	job.BytesPerSecond = progress.BytesPerSecond
	if progress.BytesTotal > 0 && progress.BytesDone >= progress.BytesTotal {
		job.State = model.JobSending
	}
	c.notifyLocked()
	shouldPersist := time.Since(c.lastPersist) >= time.Second
	if shouldPersist {
		c.lastPersist = time.Now()
	}
	c.mu.Unlock()
	if shouldPersist {
		if err := c.persist(); err != nil {
			c.setPersistenceError(err)
		}
	}
}

func (c *Controller) failJob(index int, jobID string, err error) {
	c.mu.Lock()
	if index >= 0 && index < len(c.jobs) && c.jobs[index].ID == jobID {
		c.jobs[index].State = model.JobFailed
		c.jobs[index].Error = err.Error()
	}
	c.lastError = err.Error()
	c.notifyLocked()
	c.mu.Unlock()
	if persistErr := c.persist(); persistErr != nil {
		c.setPersistenceError(persistErr)
	}
}

func (c *Controller) setPersistenceError(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	c.lastError = "保存本地状态失败：" + err.Error()
	c.notifyLocked()
	c.mu.Unlock()
}

func (c *Controller) CancelJob(id string) error {
	// Active uploads must remain cancellable while Start owns opMu. Handle that
	// path first, then take opMu for mutations of the durable idle queue.
	c.mu.Lock()
	for i := range c.jobs {
		if c.jobs[i].ID == id && (c.jobs[i].State == model.JobUploading || c.jobs[i].State == model.JobSending) {
			if c.activeID == id && c.activeCancel != nil {
				c.cancelJobID = id
				c.activeCancel()
				c.mu.Unlock()
				return nil
			}
			c.mu.Unlock()
			return errors.New("该任务当前无法取消")
		}
	}
	c.mu.Unlock()

	if !c.opMu.TryLock() {
		return errors.New("上传、扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()
	c.mu.Lock()
	for i := range c.jobs {
		if c.jobs[i].ID != id {
			continue
		}
		switch c.jobs[i].State {
		case model.JobQueued, model.JobInterrupted, model.JobFailed, model.JobSkipped:
			c.jobs[i].State = model.JobCancelled
			c.jobs[i].Error = ""
			c.notifyLocked()
			c.mu.Unlock()
			return c.persist()
		default:
			c.mu.Unlock()
			return errors.New("该任务已经结束")
		}
	}
	c.mu.Unlock()
	return errors.New("没有找到上传任务")
}

func (c *Controller) CancelAll() error {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		if !c.opMu.TryLock() {
			return errors.New("上传、扫描或移动操作正在进行，请稍后重试")
		}
		defer c.opMu.Unlock()
		c.mu.Lock()
	}
	c.pauseRevision++
	c.paused = false
	c.pauseRequested = false
	c.cancelAllRequested = true
	if c.allCancel != nil {
		c.allCancel()
	}
	for i := range c.jobs {
		switch c.jobs[i].State {
		case model.JobQueued, model.JobInterrupted, model.JobFailed, model.JobSkipped:
			c.jobs[i].State = model.JobCancelled
			c.jobs[i].Error = ""
		}
	}
	c.notifyLocked()
	c.mu.Unlock()
	return c.persist()
}

func (c *Controller) Retry(id string) error {
	if !c.opMu.TryLock() {
		return errors.New("上传、扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return errors.New("请先等待当前队列停止")
	}
	nextJobs := append([]model.Job(nil), c.jobs...)
	channel := c.channel
	paused := c.paused
	c.mu.Unlock()
	for i := range nextJobs {
		if nextJobs[i].ID != id {
			continue
		}
		switch nextJobs[i].State {
		case model.JobFailed, model.JobInterrupted, model.JobCancelled, model.JobSkipped, model.JobConfirming:
			nextJobs[i].State = model.JobQueued
			resetPendingJobProgress(&nextJobs[i])
			if err := c.saveSnapshot(nextJobs, channel, paused); err != nil {
				c.setPersistenceError(err)
				return err
			}
			c.mu.Lock()
			c.jobs = nextJobs
			c.lastError = ""
			c.notifyLocked()
			c.mu.Unlock()
			return nil
		default:
			return errors.New("该任务当前不能重试")
		}
	}
	return errors.New("没有找到上传任务")
}

func (c *Controller) Skip(id string) error {
	if !c.opMu.TryLock() {
		return errors.New("上传、扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return errors.New("请先等待当前队列停止")
	}
	for i := range c.jobs {
		if c.jobs[i].ID == id && (c.jobs[i].State == model.JobFailed || c.jobs[i].State == model.JobConfirming) {
			c.jobs[i].State = model.JobSkipped
			c.jobs[i].Error = ""
			c.notifyLocked()
			c.mu.Unlock()
			return c.persist()
		}
	}
	c.mu.Unlock()
	return errors.New("该任务当前不能跳过")
}

func (c *Controller) MarkSent(id string) error {
	if !c.opMu.TryLock() {
		return errors.New("上传、扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()
	c.mu.Lock()
	for i := range c.jobs {
		if c.jobs[i].ID == id && c.jobs[i].State == model.JobConfirming {
			now := time.Now()
			c.jobs[i].State = model.JobSent
			c.jobs[i].Uploaded = c.jobs[i].Size
			c.jobs[i].CompletedAt = &now
			c.jobs[i].Error = ""
			c.notifyLocked()
			c.mu.Unlock()
			return c.persist()
		}
	}
	c.mu.Unlock()
	return errors.New("该任务不处于待确认状态")
}

func (c *Controller) MoveOversize(ctx context.Context, destinationDir string, onProgress func(model.Progress)) error {
	if !c.opMu.TryLock() {
		return errors.New("上传、扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	directoryInfo, err := os.Stat(destinationDir)
	if err != nil {
		return fmt.Errorf("读取目标文件夹失败：%w", err)
	}
	if !directoryInfo.IsDir() {
		return errors.New("超限文件的目标路径不是文件夹")
	}

	c.mu.RLock()
	if c.running {
		c.mu.RUnlock()
		return errors.New("上传进行中，不能移动超限文件")
	}
	type item struct {
		index       int
		job         model.Job
		destination string
	}
	var items []item
	var total int64
	for i, job := range c.jobs {
		if job.State == model.JobOversize {
			destination, pathErr := safeMoveDestination(destinationDir, job.Name)
			if pathErr != nil {
				c.mu.RUnlock()
				return pathErr
			}
			if _, statErr := os.Lstat(destination); statErr == nil {
				c.mu.RUnlock()
				return fmt.Errorf("目标文件已存在，不会覆盖：%s", destination)
			} else if !errors.Is(statErr, os.ErrNotExist) {
				c.mu.RUnlock()
				return fmt.Errorf("检查目标文件失败：%w", statErr)
			}
			items = append(items, item{index: i, job: job, destination: destination})
			total += job.Size
		}
	}
	c.mu.RUnlock()
	if len(items) == 0 {
		return errors.New("没有超限文件需要移动")
	}

	var offset int64
	for _, current := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Lstat(current.job.Path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != current.job.Size || !info.ModTime().Equal(current.job.ModTime) {
			if err == nil {
				err = errors.New("文件已被替换或修改")
			}
			return fmt.Errorf("移动前检查失败：%w", err)
		}

		// Persist the move intent before touching the source. If the process
		// stops after the filesystem move but before the final save, Load can
		// deterministically recover by checking source and destination.
		c.mu.Lock()
		if current.index >= len(c.jobs) || c.jobs[current.index].ID != current.job.ID || c.jobs[current.index].State != model.JobOversize {
			c.mu.Unlock()
			return errors.New("超限文件队列已发生变化，请重新扫描")
		}
		c.jobs[current.index].State = model.JobMoving
		c.jobs[current.index].MoveDestination = current.destination
		c.jobs[current.index].Error = ""
		c.notifyLocked()
		c.mu.Unlock()
		if err := c.persist(); err != nil {
			c.mu.Lock()
			c.jobs[current.index].State = model.JobOversize
			c.jobs[current.index].MoveDestination = ""
			c.notifyLocked()
			c.mu.Unlock()
			return fmt.Errorf("保存移动意图失败：%w", err)
		}

		itemOffset := offset
		err = c.mover.Move(ctx, current.job.Path, current.destination, func(progress model.Progress) {
			if onProgress != nil {
				progress.BytesDone += itemOffset
				progress.BytesTotal = total
				onProgress(progress)
			}
		})
		if err != nil {
			c.mu.Lock()
			c.jobs[current.index].State = model.JobOversize
			c.jobs[current.index].MoveDestination = ""
			c.jobs[current.index].Error = err.Error()
			c.notifyLocked()
			c.mu.Unlock()
			if persistErr := c.persist(); persistErr != nil {
				return errors.Join(err, fmt.Errorf("保存移动失败状态失败：%w", persistErr))
			}
			return err
		}
		offset += current.job.Size
		now := time.Now()
		c.mu.Lock()
		c.jobs[current.index].State = model.JobMoved
		c.jobs[current.index].Path = current.destination
		c.jobs[current.index].MoveDestination = ""
		c.jobs[current.index].CompletedAt = &now
		c.jobs[current.index].Error = ""
		c.notifyLocked()
		c.mu.Unlock()
		if err := c.persist(); err != nil {
			return err
		}
	}
	return nil
}

func safeMoveDestination(destinationDir, name string) (string, error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return "", fmt.Errorf("不安全的文件名，拒绝移动：%q", name)
	}
	directory, err := filepath.Abs(destinationDir)
	if err != nil {
		return "", fmt.Errorf("解析目标文件夹失败：%w", err)
	}
	destination := filepath.Join(directory, name)
	relative, err := filepath.Rel(directory, destination)
	if err != nil || relative == ".." || filepath.IsAbs(relative) || len(relative) >= 3 && relative[:3] == ".."+string(os.PathSeparator) {
		return "", fmt.Errorf("文件名会逃逸目标文件夹，拒绝移动：%q", name)
	}
	return destination, nil
}

func (c *Controller) persist() error {
	if c.store == nil {
		return nil
	}
	c.persistMu.Lock()
	defer c.persistMu.Unlock()
	c.mu.RLock()
	jobs := append([]model.Job(nil), c.jobs...)
	channel := c.channel
	paused := c.paused
	c.mu.RUnlock()
	return c.store.Save(jobs, channel, paused)
}

func (c *Controller) saveSnapshot(jobs []model.Job, channel model.Channel, paused bool) error {
	if c.store == nil {
		return nil
	}
	c.persistMu.Lock()
	defer c.persistMu.Unlock()
	return c.store.Save(append([]model.Job(nil), jobs...), channel, paused)
}

func (c *Controller) notifyLocked() {
	snapshot := c.snapshotLocked()
	select {
	case c.updates <- snapshot:
	default:
		select {
		case <-c.updates:
		default:
		}
		select {
		case c.updates <- snapshot:
		default:
		}
	}
}

func (c *Controller) snapshotLocked() Snapshot {
	jobs := append([]model.Job(nil), c.jobs...)
	snapshot := Snapshot{
		Jobs:           jobs,
		Channel:        c.channel,
		Folder:         c.folder,
		Running:        c.running,
		Paused:         c.paused,
		PauseRequested: c.pauseRequested,
		ActiveID:       c.activeID,
		LastError:      c.lastError,
	}
	for _, job := range jobs {
		total, done := jobProgressBytes(job)
		snapshot.TotalBytes += total
		snapshot.DoneBytes += done
		if job.State == model.JobUploading || job.State == model.JobSending {
			snapshot.BytesPerSecond += job.BytesPerSecond
		}
	}
	if snapshot.BytesPerSecond > 0 && snapshot.TotalBytes > snapshot.DoneBytes {
		snapshot.ETA = time.Duration(float64(snapshot.TotalBytes-snapshot.DoneBytes)/snapshot.BytesPerSecond) * time.Second
	}
	return snapshot
}

func jobProgressBytes(job model.Job) (total, done int64) {
	if job.Size <= 0 {
		return 0, 0
	}
	total = job.Size
	if job.State == model.JobSent || job.State == model.JobMoved {
		return total, total
	}
	done = job.Uploaded
	if done < 0 {
		done = 0
	}
	if done > total {
		done = total
	}
	return total, done
}
