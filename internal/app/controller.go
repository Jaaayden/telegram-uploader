package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sort"
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
	// PendingRemovalIDs contains active jobs for which the user requested a
	// removal. The IDs are intentionally not persisted: they describe an
	// in-flight cancellation and are rebuilt only for the lifetime of the
	// process.
	PendingRemovalIDs []string
	LastError         string
	DoneBytes         int64
	TotalBytes        int64
	BytesPerSecond    float64
	ETA               time.Duration
}

// RemovalResult describes a queue removal request. Removed is the number of
// records removed immediately. PendingRemovalIDs contains active records that
// are being cancelled and will be removed after the upload goroutine exits.
// A Telegram message is never deleted by a queue removal operation.
type RemovalResult struct {
	Removed           int
	PendingRemovalIDs []string
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
	// queueRevision changes whenever a durable queue or channel/pause value is
	// changed. Persistence uses it to avoid publishing an older structural
	// snapshot after a concurrent upload update.
	queueRevision uint64
	running       bool
	paused        bool
	// pauseRequested is a transient view of a pause intent while the queue is
	// still running. paused remains true until Resume or Start clears it.
	pauseRequested bool
	// pauseRevision prevents a failed persistence attempt from rolling back a
	// newer Pause, Resume, Start, or CancelAll action.
	pauseRevision uint64
	activeID      string
	lastError     string

	activeCancel       context.CancelFunc
	activeAttempt      uint64
	attemptRevision    uint64
	allCancel          context.CancelFunc
	cancelJobID        string
	cancelAllRequested bool
	retryWaitActive    bool
	pendingRemoval     map[string]struct{}
	updates            chan Snapshot
	uploadRetryDelays  []time.Duration
	uploadRetryWait    func(context.Context, time.Duration) error
	// beforeUploadAttempt is a deterministic test seam for the narrow interval
	// after queue selection and before the job is reserved as active.
	beforeUploadAttempt func(model.Job)
}

var defaultUploadRetryDelays = []time.Duration{
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
	20 * time.Second,
	40 * time.Second,
}

var (
	errQueuePaused          = errors.New("上传队列已暂停")
	errQueuePersistenceLost = errors.New("无法可靠保存上传队列状态")
	errQueueChanged         = errors.New("上传队列已发生变化")
	errQueueJobCancelled    = errors.New("上传任务已取消")
	errQueueJobNotRunnable  = errors.New("上传任务已不再可运行")
)

func NewController(store QueueStore, fileMover *mover.Mover) *Controller {
	if fileMover == nil {
		fileMover = mover.New()
	}
	return &Controller{
		store:             store,
		mover:             fileMover,
		updates:           make(chan Snapshot, 1),
		pendingRemoval:    make(map[string]struct{}),
		uploadRetryDelays: append([]time.Duration(nil), defaultUploadRetryDelays...),
		uploadRetryWait:   waitForUploadRetry,
	}
}

func waitForUploadRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
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
	c.mu.RLock()
	running := c.running || c.activeID != "" || len(c.pendingRemoval) > 0
	c.mu.RUnlock()
	if running {
		return errors.New("上传或活动任务清理进行中，不能重新载入队列")
	}
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
	c.queueRevision++
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
	for {
		c.mu.RLock()
		channel := c.channel
		paused := c.paused
		revision := c.queueRevision
		c.mu.RUnlock()
		if err := c.persistCandidate(jobs, channel, paused, revision); err != nil {
			if errors.Is(err, errQueueChanged) {
				continue
			}
			c.setPersistenceError(err)
			return err
		}
		if c.commitCandidate(jobs, folder, revision) {
			return nil
		}
	}
}

// Pause requests a soft pause. It never cancels the active Telegram request:
// the current file is allowed to finish, then runQueue stops before selecting
// another runnable job. The upload goroutine does not hold opMu for its whole
// lifetime, so queue edits can proceed while an upload is active.
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
	var cancelRetryWait context.CancelFunc
	c.paused = true
	if c.running {
		c.pauseRequested = true
		if c.retryWaitActive {
			cancelRetryWait = c.activeCancel
		}
	}
	stateChanged := !wasPaused || wasRequested != c.pauseRequested
	if stateChanged {
		c.pauseRevision++
		c.queueRevision++
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
	// A normal upload remains a soft pause and is allowed to finish. Waiting
	// after a failed pre-send attempt is different: no Telegram request is in
	// flight, so interrupt the backoff immediately and leave the job
	// recoverable instead of making Pause wait for the longest retry delay.
	if cancelRetryWait != nil {
		cancelRetryWait()
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
	c.queueRevision++
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

// AddJobs appends scanned candidates to the durable queue, including while an
// upload is active. Candidates are de-duplicated by normalized absolute path. An
// existing path always wins, so a previously sent/failed job keeps its ID,
// RandomID and state instead of being replaced by a newly scanned copy.
//
// The scanner already returns candidates in natural filename order. When
// adding candidates from another folder, their order is preserved and the
// new group is appended after the existing queue. Positions are rebuilt for
// the complete queue after every successful append.
func (c *Controller) AddJobs(candidates []model.Job) (int, error) {
	if len(candidates) == 0 {
		return 0, nil
	}
	if !c.opMu.TryLock() {
		return 0, errors.New("扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()
	for {
		c.mu.RLock()
		current := cloneJobsForPersistence(c.jobs)
		revision := c.queueRevision
		channel := c.channel
		paused := c.paused
		folder := c.folder
		c.mu.RUnlock()

		nextJobs := append([]model.Job(nil), current...)
		paths := make(map[string]struct{}, len(current)+len(candidates))
		ids := make(map[string]struct{}, len(current)+len(candidates))
		randomIDs := make(map[int64]struct{}, len(current)+len(candidates))
		for _, job := range current {
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
				return 0, errors.New("追加任务失败：文件路径不能为空")
			}
			key := jobPathKey(candidate.Path)
			if _, exists := paths[key]; exists {
				continue
			}
			if strings.TrimSpace(candidate.ID) == "" {
				return 0, fmt.Errorf("追加任务 %q 失败：Job ID 不能为空", candidate.Name)
			}
			if _, exists := ids[candidate.ID]; exists {
				return 0, fmt.Errorf("追加任务 %q 失败：Job ID 重复", candidate.Name)
			}
			if candidate.RandomID == 0 {
				return 0, fmt.Errorf("追加任务 %q 失败：Telegram RandomID 不能为空", candidate.Name)
			}
			if _, exists := randomIDs[candidate.RandomID]; exists {
				return 0, fmt.Errorf("追加任务 %q 失败：Telegram RandomID 重复", candidate.Name)
			}
			if strings.TrimSpace(candidate.Name) == "" || candidate.Size < 0 {
				return 0, errors.New("追加任务失败：文件名或大小无效")
			}
			if candidate.State == "" {
				candidate.State = model.JobQueued
			}
			if candidate.State != model.JobQueued && candidate.State != model.JobOversize {
				return 0, fmt.Errorf("追加任务 %q 失败：不允许的初始状态 %q", candidate.Name, candidate.State)
			}
			nextJobs = append(nextJobs, candidate)
			paths[key] = struct{}{}
			ids[candidate.ID] = struct{}{}
			randomIDs[candidate.RandomID] = struct{}{}
			added++
		}
		if added == 0 {
			return 0, nil
		}
		reindexJobs(nextJobs)
		if err := c.persistCandidate(nextJobs, channel, paused, revision); err != nil {
			if errors.Is(err, errQueueChanged) {
				continue
			}
			c.setPersistenceError(err)
			return 0, err
		}
		if c.commitCandidate(nextJobs, folder, revision) {
			return added, nil
		}
	}
}

func isRemovableJobState(state model.JobState) bool {
	switch state {
	case model.JobUploading, model.JobSending, model.JobConfirming, model.JobMoving:
		return false
	default:
		return true
	}
}

func pendingRemovalEligibleState(state model.JobState) bool {
	switch state {
	case model.JobUploading, model.JobSending, model.JobSent, model.JobCancelled, model.JobInterrupted, model.JobFailed:
		return true
	default:
		return false
	}
}

func (c *Controller) requestPendingRemovalLocked(id string) context.CancelFunc {
	if c.pendingRemoval == nil {
		c.pendingRemoval = make(map[string]struct{})
	}
	c.pendingRemoval[id] = struct{}{}
	if c.activeID == id {
		c.cancelJobID = id
		return c.activeCancel
	}
	return nil
}

// requestActiveRemovalLocked records a cancellation only after any structural
// queue edit in the same user operation has been durably committed. A nil
// selected set means every active job is selected (the ClearQueue case).
// c.mu must be held by the caller.
func (c *Controller) requestActiveRemovalLocked(selected map[string]struct{}) ([]string, context.CancelFunc) {
	id := c.activeID
	if id == "" {
		return nil, nil
	}
	if selected != nil {
		if _, ok := selected[id]; !ok {
			return nil, nil
		}
	}
	index := findJobIndexLocked(c.jobs, id)
	if index < 0 || !pendingRemovalEligibleState(c.jobs[index].State) {
		return nil, nil
	}
	cancel := c.requestPendingRemovalLocked(id)
	c.notifyLocked()
	return []string{id}, cancel
}

// RemoveJobs removes local records while a queue is running as well. An active
// upload is cancelled first and removed only after its outcome is known.
func (c *Controller) RemoveJobs(ids []string) (RemovalResult, error) {
	selected := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id != "" {
			selected[id] = struct{}{}
		}
	}
	if len(selected) == 0 {
		return RemovalResult{}, nil
	}
	if !c.opMu.TryLock() {
		return RemovalResult{}, errors.New("扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()
	for {
		c.mu.Lock()
		current := cloneJobsForPersistence(c.jobs)
		revision := c.queueRevision
		channel := c.channel
		paused := c.paused
		folder := c.folder
		nextJobs, removed := filteredJobs(current, func(job model.Job) bool {
			_, selectedJob := selected[job.ID]
			return selectedJob && isRemovableJobState(job.State) && job.ID != c.activeID
		})
		if removed == 0 {
			pending, cancel := c.requestActiveRemovalLocked(selected)
			c.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			return RemovalResult{PendingRemovalIDs: pending}, nil
		}
		c.mu.Unlock()
		if err := c.persistCandidate(nextJobs, channel, paused, revision); err != nil {
			if errors.Is(err, errQueueChanged) {
				continue
			}
			c.setPersistenceError(err)
			return RemovalResult{}, err
		}
		committed, pending, cancel := c.commitRemovalCandidate(
			nextJobs,
			folderIfJobs(nextJobs, folder),
			revision,
			selected,
		)
		if committed {
			if cancel != nil {
				cancel()
			}
			return RemovalResult{Removed: removed, PendingRemovalIDs: pending}, nil
		}
	}
}

func folderIfJobs(jobs []model.Job, current string) string {
	if len(jobs) == 0 {
		return ""
	}
	return current
}

// ClearQueue removes every removable local record. The active upload follows
// the same cancel-then-remove path as RemoveJobs; confirming/moving records are
// retained because their external outcome is not safely reversible.
func (c *Controller) ClearQueue() (RemovalResult, error) {
	if !c.opMu.TryLock() {
		return RemovalResult{}, errors.New("扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()
	for {
		c.mu.Lock()
		current := cloneJobsForPersistence(c.jobs)
		revision := c.queueRevision
		channel := c.channel
		paused := c.paused
		folder := c.folder
		nextJobs, removed := filteredJobs(current, func(job model.Job) bool {
			return isRemovableJobState(job.State) && job.ID != c.activeID
		})
		if removed == 0 {
			pending, cancel := c.requestActiveRemovalLocked(nil)
			c.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			return RemovalResult{PendingRemovalIDs: pending}, nil
		}
		c.mu.Unlock()
		if err := c.persistCandidate(nextJobs, channel, paused, revision); err != nil {
			if errors.Is(err, errQueueChanged) {
				continue
			}
			c.setPersistenceError(err)
			return RemovalResult{}, err
		}
		committed, pending, cancel := c.commitRemovalCandidate(
			nextJobs,
			folderIfJobs(nextJobs, folder),
			revision,
			nil,
		)
		if committed {
			if cancel != nil {
				cancel()
			}
			return RemovalResult{Removed: removed, PendingRemovalIDs: pending}, nil
		}
	}
}

// RemoveCompleted can run concurrently with uploads and removes only jobs
// whose Telegram/file operation has already reached a terminal success state.
func (c *Controller) RemoveCompleted() (RemovalResult, error) {
	if !c.opMu.TryLock() {
		return RemovalResult{}, errors.New("扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()
	for {
		c.mu.RLock()
		current := cloneJobsForPersistence(c.jobs)
		revision := c.queueRevision
		channel := c.channel
		paused := c.paused
		folder := c.folder
		c.mu.RUnlock()
		nextJobs, removed := filteredJobs(current, func(job model.Job) bool {
			return job.State == model.JobSent || job.State == model.JobMoved
		})
		result := RemovalResult{Removed: removed}
		if removed == 0 {
			return result, nil
		}
		if err := c.persistCandidate(nextJobs, channel, paused, revision); err != nil {
			if errors.Is(err, errQueueChanged) {
				continue
			}
			c.setPersistenceError(err)
			return RemovalResult{}, err
		}
		if c.commitCandidate(nextJobs, folderIfJobs(nextJobs, folder), revision) {
			return result, nil
		}
	}
}

// ResetJobs returns recoverable terminal jobs to the queued state. It is safe
// to invoke while the queue is active; the current upload is never reset and
// newly queued jobs are picked up by the next selection pass.
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
		return 0, errors.New("扫描或移动操作正在进行，请稍后重试")
	}
	defer c.opMu.Unlock()

	for {
		c.mu.RLock()
		current := cloneJobsForPersistence(c.jobs)
		revision := c.queueRevision
		channel := c.channel
		paused := c.paused
		folder := c.folder
		activeID := c.activeID
		pendingRemoval := make(map[string]struct{}, len(c.pendingRemoval))
		for id := range c.pendingRemoval {
			pendingRemoval[id] = struct{}{}
		}
		c.mu.RUnlock()

		nextJobs := append([]model.Job(nil), current...)
		resetCount := 0
		for i := range nextJobs {
			job := &nextJobs[i]
			_, pending := pendingRemoval[job.ID]
			if job.ID == activeID || pending || !matchesResetMode(*job, mode, selected) {
				continue
			}
			job.State = model.JobQueued
			resetPendingJobProgress(job)
			resetCount++
		}
		if resetCount == 0 {
			return 0, nil
		}
		if err := c.persistCandidate(nextJobs, channel, paused, revision); err != nil {
			if errors.Is(err, errQueueChanged) {
				continue
			}
			c.setPersistenceError(err)
			return 0, err
		}
		if c.commitCandidate(nextJobs, folder, revision) {
			return resetCount, nil
		}
	}
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

func findJobIndexLocked(jobs []model.Job, id string) int {
	if id == "" {
		return -1
	}
	for index := range jobs {
		if jobs[index].ID == id {
			return index
		}
	}
	return -1
}

// persistCandidate serializes writes and validates the queue revision before
// and after Save. The disk write is deliberately outside c.mu so a slow fsync
// cannot stall upload progress callbacks; callers retry with a fresh candidate
// when a concurrent durable update is detected.
func (c *Controller) persistCandidate(jobs []model.Job, channel model.Channel, paused bool, revision uint64) error {
	if c.store == nil {
		return nil
	}
	c.persistMu.Lock()
	defer c.persistMu.Unlock()
	c.mu.RLock()
	if c.queueRevision != revision {
		c.mu.RUnlock()
		return errQueueChanged
	}
	copyJobs := cloneJobsForPersistence(jobs)
	c.mu.RUnlock()
	if err := c.store.Save(copyJobs, channel, paused); err != nil {
		return err
	}
	c.mu.RLock()
	changed := c.queueRevision != revision
	c.mu.RUnlock()
	if changed {
		return errQueueChanged
	}
	return nil
}

func cloneJobsForPersistence(jobs []model.Job) []model.Job {
	copyJobs := append([]model.Job(nil), jobs...)
	for i := range copyJobs {
		if jobs[i].StartedAt != nil {
			started := *jobs[i].StartedAt
			copyJobs[i].StartedAt = &started
		}
		if jobs[i].CompletedAt != nil {
			completed := *jobs[i].CompletedAt
			copyJobs[i].CompletedAt = &completed
		}
	}
	return copyJobs
}

// commitCandidate publishes a candidate only if the revision used to persist
// it is still current. The caller must not hold c.mu.
func (c *Controller) commitCandidate(jobs []model.Job, folder string, revision uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.queueRevision != revision {
		return false
	}
	c.preserveActiveProgressLocked(jobs)
	c.jobs = cloneJobsForPersistence(jobs)
	if folder != "" || len(jobs) == 0 {
		c.folder = folder
	}
	c.queueRevision++
	c.lastError = ""
	c.notifyLocked()
	return true
}

// commitRemovalCandidate atomically publishes a durably saved structural
// deletion and records cancellation of the selected active job. Keeping these
// actions under one lock prevents an active job from reaching a terminal state
// in the gap between the queue commit and the pending-removal marker.
func (c *Controller) commitRemovalCandidate(
	jobs []model.Job,
	folder string,
	revision uint64,
	selected map[string]struct{},
) (bool, []string, context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.queueRevision != revision {
		return false, nil, nil
	}
	c.preserveActiveProgressLocked(jobs)
	c.jobs = cloneJobsForPersistence(jobs)
	if folder != "" || len(jobs) == 0 {
		c.folder = folder
	}
	c.queueRevision++
	c.lastError = ""
	pending, cancel := c.requestActiveRemovalLocked(selected)
	if len(pending) == 0 {
		c.notifyLocked()
	}
	return true, pending, cancel
}

func (c *Controller) preserveActiveProgressLocked(jobs []model.Job) {
	// Progress callbacks intentionally do not advance queueRevision. Preserve
	// the current active record when a structural edit was prepared from an
	// earlier snapshot, otherwise an edit completing just after Save could
	// roll Uploaded/BytesPerSecond/attempt state back to stale values.
	if c.activeID != "" {
		if currentIndex := findJobIndexLocked(c.jobs, c.activeID); currentIndex >= 0 {
			for index := range jobs {
				if jobs[index].ID == c.activeID {
					// Only progress fields are intentionally omitted from
					// queueRevision. Keep the candidate's Position and all
					// durable state so deletion/reindex edits remain coherent.
					jobs[index].Uploaded = c.jobs[currentIndex].Uploaded
					jobs[index].BytesPerSecond = c.jobs[currentIndex].BytesPerSecond
					break
				}
			}
		}
	}
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

	for {
		c.mu.RLock()
		jobs := cloneJobsForPersistence(c.jobs)
		channel := c.channel
		paused := c.paused
		folder := c.folder
		revision := c.queueRevision
		c.mu.RUnlock()

		changed := false
		for i := range jobs {
			job := &jobs[i]
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
		if !changed {
			return nil
		}
		if err := c.persistCandidate(jobs, channel, paused, revision); err != nil {
			if errors.Is(err, errQueueChanged) {
				continue
			}
			c.setPersistenceError(err)
			return err
		}
		if c.commitCandidate(jobs, folder, revision) {
			return nil
		}
	}
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
	for {
		c.mu.RLock()
		if c.running {
			c.mu.RUnlock()
			return errors.New("上传进行中，不能切换频道")
		}
		jobs := cloneJobsForPersistence(c.jobs)
		paused := c.paused
		revision := c.queueRevision
		c.mu.RUnlock()
		if err := c.persistCandidate(jobs, channel, paused, revision); err != nil {
			if errors.Is(err, errQueueChanged) {
				continue
			}
			c.setPersistenceError(err)
			return err
		}
		c.mu.Lock()
		if c.queueRevision != revision {
			c.mu.Unlock()
			continue
		}
		c.channel = channel
		c.queueRevision++
		c.lastError = ""
		c.notifyLocked()
		c.mu.Unlock()
		return nil
	}
}

func (c *Controller) Start(parent context.Context) error {
	if !c.opMu.TryLock() {
		return errors.New("另一个队列操作正在准备，请稍后重试")
	}
	defer c.opMu.Unlock()
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return errors.New("上传队列已经在运行")
	}
	if c.gateway == nil {
		c.mu.Unlock()
		return errors.New("请先连接 Telegram Bot")
	}
	if c.channel.ID == 0 {
		c.mu.Unlock()
		return errors.New("请先绑定目标频道")
	}
	if !hasRunnableJob(c.jobs) {
		c.mu.Unlock()
		return errors.New("队列中没有待上传视频")
	}
	wasPaused := c.paused || c.pauseRequested
	if wasPaused {
		c.paused = false
		c.pauseRequested = false
		c.pauseRevision++
		c.queueRevision++
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
			c.queueRevision++
		}
		// A pause request is transient and only meaningful while Running is
		// true. Preserve any newer paused intent, but normalize its view.
		c.pauseRequested = false
		c.lastError = err.Error()
		c.notifyLocked()
		c.mu.Unlock()
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
		c.activeAttempt = 0
		c.retryWaitActive = false
		c.allCancel = nil
		c.cancelJobID = ""
		c.cancelAllRequested = false
		c.notifyLocked()
		c.mu.Unlock()
		if err := c.persist(); err != nil {
			c.setPersistenceError(err)
		}
	}()

	for {
		job, channel, ok := c.nextJob()
		if !ok {
			return
		}
		if err := ctx.Err(); err != nil {
			return
		}

		jobCtx, jobCancel := context.WithCancel(ctx)
		messageID, err := c.uploadJobWithRetry(jobCtx, job, channel, jobCancel)
		jobContextCancelled := jobCtx.Err() != nil

		c.mu.Lock()
		c.activeCancel = nil
		c.activeAttempt = 0
		c.retryWaitActive = false
		cancelCurrent := c.cancelJobID == job.ID
		if cancelCurrent {
			c.cancelJobID = ""
		}
		cancelAll := c.cancelAllRequested
		c.mu.Unlock()
		jobCancel()

		if err != nil {
			if errors.Is(err, errQueueJobCancelled) || errors.Is(err, errQueueJobNotRunnable) {
				c.finishActiveJob(job.ID)
				continue
			}
			if errors.Is(err, tgtransport.ErrSendOutcomeUnknown) {
				c.mu.Lock()
				if current := findJobIndexLocked(c.jobs, job.ID); current >= 0 {
					c.jobs[current].State = model.JobConfirming
					c.jobs[current].BytesPerSecond = 0
					c.jobs[current].Error = "消息可能已经送达；请先检查频道，再选择“已发送”或“重试”"
					c.lastError = c.jobs[current].Error
				}
				// An unknown outcome must stay visible for manual confirmation;
				// never delete it merely because the user clicked Remove.
				delete(c.pendingRemoval, job.ID)
				c.queueRevision++
				c.notifyLocked()
				c.mu.Unlock()
				if persistErr := c.persist(); persistErr != nil {
					c.setPersistenceError(persistErr)
				}
				return
			}
			if errors.Is(err, errQueuePaused) {
				jobContextCancelled = true
			}
			if errors.Is(err, errQueuePersistenceLost) {
				c.clearPendingRemoval(job.ID)
				_ = c.failJob(job.ID, err)
				return
			}
			if jobContextCancelled || cancelCurrent || cancelAll || ctx.Err() != nil {
				c.mu.Lock()
				current := findJobIndexLocked(c.jobs, job.ID)
				if current >= 0 {
					if cancelCurrent || cancelAll {
						c.jobs[current].State = model.JobCancelled
						c.jobs[current].Error = ""
					} else {
						c.jobs[current].State = model.JobInterrupted
						if c.paused || c.pauseRequested {
							c.jobs[current].Error = "队列已暂停，可重新开始当前任务"
						} else {
							c.jobs[current].Error = "程序关闭或连接中断，可重新开始上传"
						}
					}
					c.jobs[current].BytesPerSecond = 0
					c.queueRevision++
					c.notifyLocked()
				}
				c.mu.Unlock()
				if persistErr := c.persist(); persistErr != nil {
					c.clearPendingRemoval(job.ID)
					c.setPersistenceError(persistErr)
					c.finishActiveJob(job.ID)
					return
				} else if c.finishActiveJobUnlessPending(job.ID) {
					if removeErr := c.removeAfterTerminalPersist(job.ID); removeErr != nil {
						c.setPersistenceError(removeErr)
					}
					c.finishActiveJob(job.ID)
				}
				if ctx.Err() != nil {
					return
				}
				continue
			}
			if persistErr := c.failJob(job.ID, err); persistErr != nil {
				c.clearPendingRemoval(job.ID)
				c.finishActiveJob(job.ID)
				return
			}
			if c.finishActiveJobUnlessPending(job.ID) {
				if removeErr := c.removeAfterTerminalPersist(job.ID); removeErr != nil {
					c.setPersistenceError(removeErr)
					c.finishActiveJob(job.ID)
					return
				}
				c.finishActiveJob(job.ID)
			}
			continue
		}

		completed := time.Now()
		c.mu.Lock()
		current := findJobIndexLocked(c.jobs, job.ID)
		if current < 0 {
			c.mu.Unlock()
			continue
		}
		c.jobs[current].State = model.JobSent
		c.jobs[current].Uploaded = c.jobs[current].Size
		c.jobs[current].MessageID = messageID
		c.jobs[current].ChannelID = channel.ID
		c.jobs[current].CompletedAt = &completed
		c.jobs[current].Error = ""
		c.queueRevision++
		c.notifyLocked()
		c.mu.Unlock()
		if err := c.persist(); err != nil {
			c.clearPendingRemoval(job.ID)
			c.mu.Lock()
			c.lastError = "消息已发送，但保存本地状态失败：" + err.Error()
			c.notifyLocked()
			c.mu.Unlock()
			return
		}
		if c.finishActiveJobUnlessPending(job.ID) {
			if removeErr := c.removeAfterTerminalPersist(job.ID); removeErr != nil {
				c.setPersistenceError(removeErr)
				c.finishActiveJob(job.ID)
				return
			}
			c.finishActiveJob(job.ID)
		}
	}
}

// finishActiveJobUnlessPending atomically closes an active job only when a
// concurrent removal request has not arrived. If it returns true, the caller
// keeps the active identity until removeAfterTerminalPersist finishes.
func (c *Controller) finishActiveJobUnlessPending(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.activeID != id {
		return false
	}
	if _, pending := c.pendingRemoval[id]; pending {
		return true
	}
	c.clearActiveJobLocked(id)
	c.notifyLocked()
	return false
}

func (c *Controller) finishActiveJob(id string) {
	c.mu.Lock()
	if c.activeID == id {
		c.clearActiveJobLocked(id)
		c.notifyLocked()
	}
	c.mu.Unlock()
}

func (c *Controller) clearActiveJobLocked(id string) {
	c.activeID = ""
	c.activeCancel = nil
	c.activeAttempt = 0
	c.retryWaitActive = false
	if c.cancelJobID == id {
		c.cancelJobID = ""
	}
}

func (c *Controller) clearPendingRemoval(id string) {
	c.mu.Lock()
	if _, ok := c.pendingRemoval[id]; ok {
		delete(c.pendingRemoval, id)
		c.notifyLocked()
	}
	c.mu.Unlock()
}

// removeAfterTerminalPersist performs the destructive local queue edit only
// after the terminal state (Sent, Cancelled, Interrupted, or Failed) has been durably saved. The
// filtered candidate is persisted before it is published, so a failed write
// leaves the terminal record visible and recoverable.
func (c *Controller) removeAfterTerminalPersist(id string) error {
	for {
		c.mu.RLock()
		current := cloneJobsForPersistence(c.jobs)
		revision := c.queueRevision
		channel := c.channel
		paused := c.paused
		folder := c.folder
		index := findJobIndexLocked(current, id)
		c.mu.RUnlock()
		if index < 0 {
			c.clearPendingRemoval(id)
			return nil
		}
		remaining, _ := filteredJobs(current, func(job model.Job) bool { return job.ID == id })
		if err := c.persistCandidate(remaining, channel, paused, revision); err != nil {
			if errors.Is(err, errQueueChanged) {
				continue
			}
			c.clearPendingRemoval(id)
			return err
		}
		c.mu.Lock()
		if c.queueRevision != revision {
			c.mu.Unlock()
			continue
		}
		c.jobs = cloneJobsForPersistence(remaining)
		delete(c.pendingRemoval, id)
		if len(c.jobs) == 0 {
			c.folder = ""
		} else {
			c.folder = folder
		}
		c.queueRevision++
		c.lastError = ""
		c.notifyLocked()
		c.mu.Unlock()
		return nil
	}
}

func (c *Controller) uploadJobWithRetry(
	ctx context.Context,
	job model.Job,
	channel model.Channel,
	cancel context.CancelFunc,
) (int, error) {
	for retryIndex := 0; ; retryIndex++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if c.queuePaused() {
			return 0, errQueuePaused
		}
		if retryIndex == 0 && c.beforeUploadAttempt != nil {
			c.beforeUploadAttempt(job)
		}

		// Reopen and revalidate the source for every attempt. Telegram uploads
		// cannot resume a partially transferred file, and reusing an os.File at
		// EOF would make the next attempt incomplete.
		file, metadata, err := openVerifiedJob(job)
		if err != nil {
			return 0, err
		}
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return 0, err
		}

		now := time.Now()
		c.mu.Lock()
		if err := ctx.Err(); err != nil {
			c.mu.Unlock()
			_ = file.Close()
			return 0, err
		}
		// Pause may have been requested after the file check. Re-check while
		// holding the same mutex used by Pause so no new request starts between
		// files or retry attempts.
		if c.paused || c.pauseRequested {
			c.mu.Unlock()
			_ = file.Close()
			return 0, errQueuePaused
		}
		currentIndex := findJobIndexLocked(c.jobs, job.ID)
		if currentIndex < 0 {
			c.mu.Unlock()
			_ = file.Close()
			return 0, errQueueJobNotRunnable
		}
		current := &c.jobs[currentIndex]
		validState := retryIndex == 0 && (current.State == model.JobQueued || current.State == model.JobInterrupted) ||
			retryIndex > 0 && (current.State == model.JobUploading || current.State == model.JobSending)
		if !validState {
			if current.State == model.JobCancelled {
				c.mu.Unlock()
				_ = file.Close()
				return 0, errQueueJobCancelled
			}
			if retryIndex == 0 {
				c.mu.Unlock()
				_ = file.Close()
				return 0, errQueueJobNotRunnable
			}
			c.mu.Unlock()
			_ = file.Close()
			return 0, fmt.Errorf("任务状态已变为 %s，停止上传", current.State)
		}
		current.Metadata = metadata
		current.State = model.JobUploading
		current.Uploaded = 0
		current.BytesPerSecond = 0
		if retryIndex == 0 {
			current.StartedAt = &now
		}
		current.Error = ""
		c.activeID = job.ID
		c.activeCancel = cancel
		c.attemptRevision++
		attemptID := c.attemptRevision
		c.activeAttempt = attemptID
		c.retryWaitActive = false
		c.queueRevision++
		c.notifyLocked()
		c.mu.Unlock()
		if err := c.persist(); err != nil {
			_ = file.Close()
			return 0, fmt.Errorf("%w：保存上传状态失败：%v", errQueuePersistenceLost, err)
		}
		// Persist can overlap a Pause or CancelAll call. Treat this locked check
		// as the attempt's start boundary: an intent recorded before it prevents
		// a new Telegram request; an intent recorded afterwards applies the
		// existing soft-pause/cancel semantics to an already-started attempt.
		c.mu.Lock()
		ctxErr := ctx.Err()
		paused := c.paused || c.pauseRequested
		if ctxErr != nil || paused {
			if c.activeID == job.ID && c.activeAttempt == attemptID {
				c.activeAttempt = 0
			}
		}
		c.mu.Unlock()
		if ctxErr != nil {
			_ = file.Close()
			return 0, ctxErr
		}
		if paused {
			_ = file.Close()
			return 0, errQueuePaused
		}

		gateway := c.currentGateway()
		if gateway == nil {
			err = tgtransport.ErrNotConnected
		} else {
			messageID, uploadErr := gateway.UploadVideo(ctx, tgtransport.UploadRequest{
				Channel:  channel,
				Path:     job.Path,
				File:     file,
				Name:     job.Name,
				Caption:  captionFromFilename(job.Name),
				RandomID: job.RandomID,
				Metadata: metadata,
				BeforeSend: func() error {
					return c.prepareSendForAttempt(job.ID, attemptID)
				},
			}, func(progress model.Progress) {
				c.applyProgressForAttempt(job.ID, attemptID, progress)
			})
			err = uploadErr
			if err == nil {
				// A read-only file close failure after Telegram confirmed the send
				// must not turn a successful message into a retryable failure.
				_ = file.Close()
				return messageID, nil
			}
		}
		_ = file.Close()
		c.mu.Lock()
		if c.activeID == job.ID && c.activeAttempt == attemptID {
			c.activeAttempt = 0
		}
		c.mu.Unlock()

		if !isRetryableUploadError(ctx, err) {
			return 0, err
		}
		if retryIndex >= len(c.uploadRetryDelays) {
			if len(c.uploadRetryDelays) == 0 {
				return 0, err
			}
			return 0, fmt.Errorf("自动重试 %d 次后仍失败：%w", len(c.uploadRetryDelays), err)
		}

		delay := c.uploadRetryDelays[retryIndex]
		if err := c.waitBeforeUploadRetry(ctx, job.ID, err, delay, retryIndex+1); err != nil {
			return 0, err
		}
	}
}

func (c *Controller) waitBeforeUploadRetry(
	ctx context.Context,
	jobID string,
	uploadErr error,
	delay time.Duration,
	retryNumber int,
) error {
	retryMessage := fmt.Sprintf(
		"连接中断，%s 后自动重试（%d/%d）：%v",
		formatRetryDelay(delay),
		retryNumber,
		len(c.uploadRetryDelays),
		uploadErr,
	)

	c.mu.Lock()
	if c.paused || c.pauseRequested {
		c.mu.Unlock()
		return errQueuePaused
	}
	if findJobIndexLocked(c.jobs, jobID) < 0 {
		c.mu.Unlock()
		return errors.New("上传队列在重试期间发生了变化")
	}
	current := findJobIndexLocked(c.jobs, jobID)
	c.jobs[current].State = model.JobUploading
	c.jobs[current].Uploaded = 0
	c.jobs[current].BytesPerSecond = 0
	c.jobs[current].Error = retryMessage
	c.queueRevision++
	c.retryWaitActive = true
	c.notifyLocked()
	c.mu.Unlock()

	if err := c.persist(); err != nil {
		c.mu.Lock()
		c.retryWaitActive = false
		c.mu.Unlock()
		return fmt.Errorf("%w：保存重试状态失败：%v", errQueuePersistenceLost, err)
	}

	wait := c.uploadRetryWait
	if wait == nil {
		wait = waitForUploadRetry
	}
	waitErr := wait(ctx, delay)
	c.mu.Lock()
	c.retryWaitActive = false
	paused := c.paused || c.pauseRequested
	c.mu.Unlock()
	if waitErr != nil {
		return waitErr
	}
	if paused {
		return errQueuePaused
	}
	return nil
}

func formatRetryDelay(delay time.Duration) string {
	if delay%time.Second == 0 {
		return fmt.Sprintf("%d 秒", int(delay/time.Second))
	}
	return delay.String()
}

func isRetryableUploadError(ctx context.Context, err error) bool {
	if err == nil || errors.Is(err, tgtransport.ErrSendOutcomeUnknown) {
		return false
	}
	if errors.Is(err, context.Canceled) {
		// A caller cancellation is deliberate. A transport-internal
		// cancellation while our job context is still live is safe to retry
		// because UploadVideo wraps every messages.sendMedia error as an
		// unknown outcome before it reaches this controller.
		return ctx.Err() == nil
	}
	if errors.Is(err, tgtransport.ErrNotConnected) || errors.Is(err, tgtransport.ErrUploadData) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var networkErr net.Error
	return errors.As(err, &networkErr)
}

func (c *Controller) currentGateway() tgtransport.Gateway {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.gateway
}

func (c *Controller) queuePaused() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.paused || c.pauseRequested
}

func (c *Controller) nextJob() (model.Job, model.Channel, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.paused || c.pauseRequested {
		return model.Job{}, model.Channel{}, false
	}
	for _, job := range c.jobs {
		if job.State == model.JobQueued || job.State == model.JobInterrupted {
			return job, c.channel, true
		}
	}
	return model.Job{}, model.Channel{}, false
}

func captionFromFilename(name string) string {
	extension := filepath.Ext(name)
	if strings.EqualFold(extension, ".mp4") {
		return strings.TrimSuffix(name, extension)
	}
	return name
}

func (c *Controller) applyProgress(jobID string, progress model.Progress) {
	c.applyProgressForAttempt(jobID, 0, progress)
}

func (c *Controller) applyProgressForAttempt(jobID string, attemptID uint64, progress model.Progress) {
	c.mu.Lock()
	currentIndex := findJobIndexLocked(c.jobs, jobID)
	if currentIndex < 0 {
		c.mu.Unlock()
		return
	}
	if attemptID != 0 && (c.activeID != jobID || c.activeAttempt != attemptID) {
		c.mu.Unlock()
		return
	}
	job := &c.jobs[currentIndex]
	if job.State != model.JobUploading && job.State != model.JobSending {
		c.mu.Unlock()
		return
	}
	// Several gotd part workers can finish almost simultaneously. A callback
	// that was prepared first may acquire the controller after a newer one; do
	// not let it overwrite newer byte or speed information.
	if progress.BytesDone < job.Uploaded {
		c.mu.Unlock()
		return
	}
	job.Uploaded = progress.BytesDone
	job.BytesPerSecond = progress.BytesPerSecond
	c.notifyLocked()
	c.mu.Unlock()
}

func (c *Controller) prepareSendForAttempt(jobID string, attemptID uint64) error {
	c.mu.Lock()
	currentIndex := findJobIndexLocked(c.jobs, jobID)
	if currentIndex < 0 {
		c.mu.Unlock()
		return errors.New("消息提交前上传队列发生了变化")
	}
	if c.activeID != jobID || c.activeAttempt != attemptID {
		c.mu.Unlock()
		return errors.New("消息提交前上传任务已失效")
	}
	job := &c.jobs[currentIndex]
	if job.State != model.JobUploading && job.State != model.JobSending {
		state := job.State
		c.mu.Unlock()
		return fmt.Errorf("消息提交前任务状态已变为 %s", state)
	}
	if job.State == model.JobSending {
		c.mu.Unlock()
		return nil
	}
	job.State = model.JobSending
	job.Uploaded = job.Size
	job.BytesPerSecond = 0
	c.queueRevision++
	c.notifyLocked()
	c.mu.Unlock()

	// Partial file parts cannot resume, so intermediate byte counters stay in
	// memory. This one transition is different: it is the durable boundary that
	// makes a crash during messages.sendMedia recover as Confirming rather than
	// blindly retrying a possibly delivered message.
	if err := c.persist(); err != nil {
		c.setPersistenceError(err)
		return fmt.Errorf("%w：保存消息提交状态失败：%v", errQueuePersistenceLost, err)
	}
	return nil
}

func (c *Controller) failJob(jobID string, err error) error {
	c.mu.Lock()
	if currentIndex := findJobIndexLocked(c.jobs, jobID); currentIndex >= 0 {
		c.jobs[currentIndex].State = model.JobFailed
		c.jobs[currentIndex].BytesPerSecond = 0
		c.jobs[currentIndex].Error = err.Error()
		c.queueRevision++
	}
	c.lastError = err.Error()
	c.notifyLocked()
	c.mu.Unlock()
	if persistErr := c.persist(); persistErr != nil {
		c.setPersistenceError(persistErr)
		return persistErr
	}
	return nil
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

func (c *Controller) persistJobMutation(id string, before model.Job, revision uint64) error {
	if err := c.persist(); err != nil {
		c.mu.Lock()
		if c.queueRevision == revision {
			if index := findJobIndexLocked(c.jobs, id); index >= 0 {
				c.jobs[index] = before
				c.queueRevision++
				c.notifyLocked()
			}
		}
		c.mu.Unlock()
		c.setPersistenceError(err)
		return err
	}
	return nil
}

func (c *Controller) CancelJob(id string) error {
	if id == "" {
		return errors.New("任务 ID 不能为空")
	}
	var cancel context.CancelFunc
	c.mu.Lock()
	index := findJobIndexLocked(c.jobs, id)
	if index < 0 {
		c.mu.Unlock()
		return errors.New("没有找到上传任务")
	}
	if _, pending := c.pendingRemoval[id]; pending {
		c.mu.Unlock()
		return errors.New("该任务正在取消并删除")
	}
	if (c.jobs[index].State == model.JobUploading || c.jobs[index].State == model.JobSending) && c.activeID == id {
		if c.activeCancel == nil {
			c.mu.Unlock()
			return errors.New("该任务当前无法取消")
		}
		c.cancelJobID = id
		cancel = c.activeCancel
		c.notifyLocked()
		c.mu.Unlock()
		cancel()
		return nil
	}
	switch c.jobs[index].State {
	case model.JobQueued, model.JobInterrupted, model.JobFailed, model.JobSkipped:
		before := c.jobs[index]
		c.jobs[index].State = model.JobCancelled
		c.jobs[index].Error = ""
		c.queueRevision++
		revision := c.queueRevision
		c.notifyLocked()
		c.mu.Unlock()
		return c.persistJobMutation(id, before, revision)
	default:
		c.mu.Unlock()
		return errors.New("该任务已经结束或等待人工确认")
	}
}

func (c *Controller) CancelAll() error {
	for {
		c.mu.RLock()
		jobs := cloneJobsForPersistence(c.jobs)
		channel := c.channel
		folder := c.folder
		revision := c.queueRevision
		c.mu.RUnlock()

		for i := range jobs {
			switch jobs[i].State {
			case model.JobQueued, model.JobInterrupted, model.JobFailed, model.JobSkipped:
				jobs[i].State = model.JobCancelled
				jobs[i].Error = ""
			}
		}

		// Save the non-active cancellation states before publishing them or
		// interrupting the current transfer. If durable storage is unavailable,
		// the caller gets an error and both memory and queue.json retain the
		// pre-cancellation state instead of disagreeing after a restart.
		if err := c.persistCandidate(jobs, channel, false, revision); err != nil {
			if errors.Is(err, errQueueChanged) {
				continue
			}
			c.setPersistenceError(err)
			return err
		}

		var cancel context.CancelFunc
		c.mu.Lock()
		if c.queueRevision != revision {
			c.mu.Unlock()
			continue
		}
		c.preserveActiveProgressLocked(jobs)
		c.jobs = cloneJobsForPersistence(jobs)
		if folder != "" || len(jobs) == 0 {
			c.folder = folder
		}
		c.pauseRevision++
		c.paused = false
		c.pauseRequested = false
		c.cancelAllRequested = true
		cancel = c.allCancel
		c.queueRevision++
		c.lastError = ""
		c.notifyLocked()
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return nil
	}
}

func (c *Controller) Retry(id string) error {
	c.mu.Lock()
	index := findJobIndexLocked(c.jobs, id)
	if index < 0 {
		c.mu.Unlock()
		return errors.New("没有找到上传任务")
	}
	if _, pending := c.pendingRemoval[id]; pending {
		c.mu.Unlock()
		return errors.New("该任务正在取消并删除")
	}
	if c.jobs[index].ID == c.activeID && (c.jobs[index].State == model.JobUploading || c.jobs[index].State == model.JobSending) {
		c.mu.Unlock()
		return errors.New("当前上传任务不能重试，请先取消")
	}
	switch c.jobs[index].State {
	case model.JobFailed, model.JobInterrupted, model.JobCancelled, model.JobSkipped, model.JobConfirming:
		before := c.jobs[index]
		resetPendingJobProgress(&c.jobs[index])
		c.jobs[index].State = model.JobQueued
		c.queueRevision++
		revision := c.queueRevision
		c.lastError = ""
		c.notifyLocked()
		c.mu.Unlock()
		return c.persistJobMutation(id, before, revision)
	default:
		c.mu.Unlock()
		return errors.New("该任务当前不能重试")
	}
}

func (c *Controller) Skip(id string) error {
	c.mu.Lock()
	index := findJobIndexLocked(c.jobs, id)
	if _, pending := c.pendingRemoval[id]; pending {
		c.mu.Unlock()
		return errors.New("该任务正在取消并删除")
	}
	if index >= 0 && c.jobs[index].ID != c.activeID && (c.jobs[index].State == model.JobFailed || c.jobs[index].State == model.JobConfirming) {
		before := c.jobs[index]
		c.jobs[index].State = model.JobSkipped
		c.jobs[index].Error = ""
		c.queueRevision++
		revision := c.queueRevision
		c.notifyLocked()
		c.mu.Unlock()
		return c.persistJobMutation(id, before, revision)
	}
	c.mu.Unlock()
	return errors.New("该任务当前不能跳过")
}

func (c *Controller) MarkSent(id string) error {
	c.mu.Lock()
	index := findJobIndexLocked(c.jobs, id)
	if _, pending := c.pendingRemoval[id]; pending {
		c.mu.Unlock()
		return errors.New("该任务正在取消并删除")
	}
	if index >= 0 && c.jobs[index].State == model.JobConfirming {
		before := c.jobs[index]
		now := time.Now()
		c.jobs[index].State = model.JobSent
		c.jobs[index].Uploaded = c.jobs[index].Size
		c.jobs[index].CompletedAt = &now
		c.jobs[index].Error = ""
		c.queueRevision++
		revision := c.queueRevision
		c.notifyLocked()
		c.mu.Unlock()
		return c.persistJobMutation(id, before, revision)
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
		c.queueRevision++
		c.notifyLocked()
		c.mu.Unlock()
		if err := c.persist(); err != nil {
			c.mu.Lock()
			c.jobs[current.index].State = model.JobOversize
			c.jobs[current.index].MoveDestination = ""
			c.queueRevision++
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
			c.queueRevision++
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
		c.queueRevision++
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
	for {
		c.mu.RLock()
		jobs := cloneJobsForPersistence(c.jobs)
		channel := c.channel
		paused := c.paused
		revision := c.queueRevision
		c.mu.RUnlock()
		err := c.persistCandidate(jobs, channel, paused, revision)
		if errors.Is(err, errQueueChanged) {
			continue
		}
		return err
	}
}

func (c *Controller) saveSnapshot(jobs []model.Job, channel model.Channel, paused bool) error {
	// Kept for file-move and compatibility paths that already own a complete
	// snapshot. New queue edits use persistCandidate so they can publish only
	// after a revision-checked write.
	if c.store == nil {
		return nil
	}
	c.persistMu.Lock()
	defer c.persistMu.Unlock()
	return c.store.Save(cloneJobsForPersistence(jobs), channel, paused)
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
	if len(c.pendingRemoval) > 0 {
		snapshot.PendingRemovalIDs = make([]string, 0, len(c.pendingRemoval))
		for id := range c.pendingRemoval {
			snapshot.PendingRemovalIDs = append(snapshot.PendingRemovalIDs, id)
		}
		sort.Strings(snapshot.PendingRemovalIDs)
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
