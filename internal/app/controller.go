package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Jaaayden/telegram-uploader/internal/model"
	"github.com/Jaaayden/telegram-uploader/internal/mover"
	tgtransport "github.com/Jaaayden/telegram-uploader/internal/telegram"
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
	// ActiveAttempt identifies the current upload attempt for ActiveID. It is
	// part of structural snapshots so consumers can discard progress callbacks
	// that belong to a previous retry of the same job.
	ActiveAttempt uint64
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

// ProgressUpdate is the lightweight, non-structural update emitted while a
// file is being uploaded. Unlike Snapshot it never contains the complete
// queue. The controller keeps only the newest update because intermediate
// byte counters have no value once a newer counter is available.
type ProgressUpdate struct {
	JobID          string
	AttemptID      uint64
	Uploaded       int64
	BytesPerSecond float64
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
	runDone            chan struct{}
	cancelJobID        string
	cancelAllRequested bool
	retryWaitActive    bool
	pendingRemoval     map[string]struct{}
	updates            chan Snapshot
	progressUpdates    chan ProgressUpdate
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
		progressUpdates:   make(chan ProgressUpdate, 1),
		pendingRemoval:    make(map[string]struct{}),
		uploadRetryDelays: append([]time.Duration(nil), defaultUploadRetryDelays...),
		uploadRetryWait:   waitForUploadRetry,
	}
}

func (c *Controller) SetGateway(gateway tgtransport.Gateway) {
	c.mu.Lock()
	c.gateway = gateway
	c.mu.Unlock()
}

func (c *Controller) Updates() <-chan Snapshot { return c.updates }

// ProgressUpdates returns a single-slot stream containing the newest byte
// counter for the active upload. Structural queue changes continue to use
// Updates and therefore retain their complete-snapshot semantics.
func (c *Controller) ProgressUpdates() <-chan ProgressUpdate { return c.progressUpdates }

// WaitStopped waits until the active queue goroutine has completed its final
// state transition and persistence pass. It lets the desktop shutdown path
// distinguish a fully drained queue from a process that is about to exit while
// cleanup is still in flight.
func (c *Controller) WaitStopped(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.RLock()
	done := c.runDone
	c.mu.RUnlock()
	if done == nil {
		return nil
	}
	// Prefer an already completed queue over a simultaneously cancelled
	// caller context. This makes a zero-budget shutdown check deterministic.
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// Completion may race the deadline between the optimistic check above
		// and this select. Prefer a fully drained queue in that boundary case.
		select {
		case <-done:
			return nil
		default:
		}
		return ctx.Err()
	}
}

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

func (c *Controller) setPersistenceError(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	c.lastError = "保存本地状态失败：" + err.Error()
	c.notifyLocked()
	c.mu.Unlock()
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

// notifyProgressLocked publishes only the newest upload progress. This runs
// while c.mu is held and intentionally never blocks an upload callback. A
// full progress history is unnecessary: the UI and other observers can safely
// render the latest monotonic byte counter.
func (c *Controller) notifyProgressLocked(update ProgressUpdate) {
	if c.progressUpdates == nil {
		// Controllers constructed as zero values are used by a few focused tests;
		// retain their old in-memory behavior without panicking.
		return
	}
	select {
	case c.progressUpdates <- update:
		return
	default:
	}
	select {
	case <-c.progressUpdates:
	default:
	}
	select {
	case c.progressUpdates <- update:
	default:
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
		ActiveAttempt:  c.activeAttempt,
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
