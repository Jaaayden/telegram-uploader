// Package diagnostics provides the small amount of on-disk diagnostics that is
// useful when an application exits while a long-running upload is in progress.
//
// The portable implementation uses only the standard library; the Windows
// atomic-replacement helper uses the project's existing x/sys/windows module.
// A caller can therefore initialise diagnostics very early during process
// startup and fall back to stderr if Start returns an error.
package diagnostics

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

const (
	// MaxLogBytes is the maximum size of the active app.log before the next
	// record causes a rotation.
	MaxLogBytes int64 = 5 * 1024 * 1024
	// MaxLogBackups is the number of rotated app.log files retained.
	MaxLogBackups = 3

	stateSchemaVersion = 1
)

var (
	// ErrNotStarted is returned when a global logging helper is used before a
	// diagnostics session has been started (or after it has been closed).
	ErrNotStarted = errors.New("diagnostics: no active session")
	// ErrClosed is returned when a Session is used after Close has started.
	ErrClosed = errors.New("diagnostics: session is closed")
	// ErrAlreadyStarted prevents one process from silently replacing the crash
	// sink and global logger owned by another active diagnostics session.
	ErrAlreadyStarted = errors.New("diagnostics: another session is already active")
)

// Session represents one process run.  A session owns the app log and the
// run-specific crash file until Close is called.
type Session struct {
	root      string
	logsDir   string
	statePath string
	appLog    string
	crashPath string
	runID     string
	started   time.Time
	metadata  runMetadata

	previousRunUnclean bool
	logger             *rotatingLogger
	crash              *os.File

	mu        sync.Mutex
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

type runMetadata struct {
	RunID   string
	GOOS    string
	GOARCH  string
	Runtime string
	PID     int
}

type runState struct {
	SchemaVersion int    `json:"schema_version"`
	RunID         string `json:"run_id"`
	StartedAt     string `json:"started_at"`
	EndedAt       string `json:"ended_at,omitempty"`
	Clean         bool   `json:"clean"`
	GOOS          string `json:"goos"`
	GOARCH        string `json:"goarch"`
	Runtime       string `json:"runtime"`
	PID           int    `json:"pid"`
}

type logRecord struct {
	Time               string `json:"time"`
	Level              string `json:"level"`
	Event              string `json:"event,omitempty"`
	Message            string `json:"message,omitempty"`
	RunID              string `json:"run_id"`
	GOOS               string `json:"goos"`
	GOARCH             string `json:"goarch"`
	Runtime            string `json:"runtime"`
	PID                int    `json:"pid"`
	PreviousRunUnclean bool   `json:"previous_run_unclean,omitempty"`
}

var (
	globalMu      sync.RWMutex
	globalSession *Session
)

// Start creates the diagnostics directory below root and starts a new run.
// The active app log is root/logs/app.log.  The crash output is written to a
// unique root/logs/crash-<run-id>.log file for this run.
func Start(root string) (*Session, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("diagnostics: root directory is empty")
	}

	root = filepath.Clean(root)
	if err := ensureDir(root); err != nil {
		return nil, fmt.Errorf("diagnostics: create root directory: %w", err)
	}
	logsDir := filepath.Join(root, "logs")
	if err := ensureDir(logsDir); err != nil {
		return nil, fmt.Errorf("diagnostics: create logs directory: %w", err)
	}

	statePath := filepath.Join(root, "run-state.json")
	previousUnclean, err := previousRunUnclean(statePath)
	if err != nil {
		return nil, fmt.Errorf("diagnostics: read run state: %w", err)
	}

	started := time.Now().UTC()
	runID, err := newRunID(started)
	if err != nil {
		return nil, fmt.Errorf("diagnostics: generate run id: %w", err)
	}
	metadata := runMetadata{
		RunID:   runID,
		GOOS:    runtime.GOOS,
		GOARCH:  runtime.GOARCH,
		Runtime: runtime.Version(),
		PID:     os.Getpid(),
	}

	logger, err := newRotatingLogger(filepath.Join(logsDir, "app.log"))
	if err != nil {
		return nil, fmt.Errorf("diagnostics: open app log: %w", err)
	}

	crashPath := filepath.Join(logsDir, "crash-"+runID+".log")
	crash, err := os.OpenFile(crashPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = logger.Close()
		return nil, fmt.Errorf("diagnostics: create crash log: %w", err)
	}
	if err := protectFile(crashPath); err != nil {
		_ = crash.Close()
		_ = logger.Close()
		_ = os.Remove(crashPath)
		return nil, fmt.Errorf("diagnostics: protect crash log: %w", err)
	}

	session := &Session{
		root:               root,
		logsDir:            logsDir,
		statePath:          statePath,
		appLog:             filepath.Join(logsDir, "app.log"),
		crashPath:          crashPath,
		runID:              runID,
		started:            started,
		metadata:           metadata,
		previousRunUnclean: previousUnclean,
		logger:             logger,
		crash:              crash,
	}

	if err := activate(session); err != nil {
		_ = crash.Close()
		_ = logger.Close()
		_ = os.Remove(crashPath)
		return nil, fmt.Errorf("diagnostics: install crash output: %w", err)
	}

	// Keep the previous run-state untouched until the crash sink and logger are
	// both usable. If either remaining initialization step fails, the next
	// launch still sees the real previous state instead of a false unclean run.
	session.mu.Lock()
	err = session.writeRecordLocked(logRecord{
		Time:               started.Format(time.RFC3339Nano),
		Level:              "INFO",
		Event:              "run_start",
		Message:            "diagnostics session started",
		RunID:              metadata.RunID,
		GOOS:               metadata.GOOS,
		GOARCH:             metadata.GOARCH,
		Runtime:            metadata.Runtime,
		PID:                metadata.PID,
		PreviousRunUnclean: previousUnclean,
	})
	session.mu.Unlock()
	if err != nil {
		_, _ = deactivate(session)
		_ = crash.Close()
		_ = logger.Close()
		_ = os.Remove(crashPath)
		return nil, fmt.Errorf("diagnostics: write run-start record: %w", err)
	}
	if err := writeRunState(statePath, runStateFor(session, false, time.Time{})); err != nil {
		_, _ = deactivate(session)
		_ = crash.Close()
		_ = logger.Close()
		_ = os.Remove(crashPath)
		return nil, fmt.Errorf("diagnostics: write run state: %w", err)
	}
	return session, nil
}

// Close records a normal run end, marks run-state.json clean, disables the
// crash sink, and closes the log files.  It is safe to call Close more than
// once; subsequent calls return the result of the first call.
func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		var errs []error
		endTime := time.Now().UTC()

		s.mu.Lock()
		if !s.closed {
			if err := s.writeRecordLocked(logRecord{
				Time:               endTime.Format(time.RFC3339Nano),
				Level:              "INFO",
				Event:              "run_end",
				Message:            "diagnostics session ended normally",
				RunID:              s.metadata.RunID,
				GOOS:               s.metadata.GOOS,
				GOARCH:             s.metadata.GOARCH,
				Runtime:            s.metadata.Runtime,
				PID:                s.metadata.PID,
				PreviousRunUnclean: s.previousRunUnclean,
			}); err != nil {
				errs = append(errs, fmt.Errorf("write run-end record: %w", err))
			}
			s.closed = true
		}
		s.mu.Unlock()

		// Keep ownership of the global diagnostics slot until the clean marker
		// is durable. Otherwise another Start in the same process could install a
		// new run and then have its state overwritten by this older Close.
		if err := writeRunState(s.statePath, runStateFor(s, true, endTime)); err != nil {
			errs = append(errs, fmt.Errorf("write clean run state: %w", err))
		}

		if owns, err := deactivate(s); owns && err != nil {
			errs = append(errs, fmt.Errorf("disable crash output: %w", err))
		}

		if s.crash != nil {
			info, statErr := s.crash.Stat()
			if statErr != nil {
				errs = append(errs, fmt.Errorf("stat crash log: %w", statErr))
			}
			if err := s.crash.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close crash log: %w", err))
			}
			if statErr == nil && info.Size() == 0 {
				if err := os.Remove(s.crashPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					errs = append(errs, fmt.Errorf("remove empty crash log: %w", err))
				}
			}
		}
		if s.logger != nil {
			if err := s.logger.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close app log: %w", err))
			}
		}
		s.closeErr = errors.Join(errs...)
	})
	return s.closeErr
}

// End is an alias for Close, useful at call sites that describe a run as an
// explicit start/end pair.
func (s *Session) End() error { return s.Close() }

// Stop is an alias for Close.
func (s *Session) Stop() error { return s.Close() }

// RunID returns the unique identifier for this process run.
func (s *Session) RunID() string {
	if s == nil {
		return ""
	}
	return s.runID
}

// PreviousRunUnclean reports whether run-state.json indicated that the
// previous process did not reach Close.
func (s *Session) PreviousRunUnclean() bool {
	return s != nil && s.previousRunUnclean
}

// LogsDir returns the directory containing app.log and crash files.
func (s *Session) LogsDir() string {
	if s == nil {
		return ""
	}
	return s.logsDir
}

// AppLogPath returns the path of the active rotating app log.
func (s *Session) AppLogPath() string {
	if s == nil {
		return ""
	}
	return s.appLog
}

// CrashPath returns the path of this run's crash output file.
func (s *Session) CrashPath() string {
	if s == nil {
		return ""
	}
	return s.crashPath
}

// Logf writes a sanitized informational record for this session.  Returning an
// error lets the caller decide whether to fall back to stderr or continue.
func (s *Session) Logf(format string, args ...any) error {
	if s == nil {
		return ErrNotStarted
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	message := redact(fmt.Sprintf(format, args...))
	return s.writeRecordLocked(s.messageRecord("INFO", message))
}

// LogError writes an error record without exposing common credential formats.
// The error itself is sanitized just like the formatted message.
func (s *Session) LogError(err error, format string, args ...any) error {
	message := fmt.Sprintf(format, args...)
	if err != nil {
		if message == "" {
			message = err.Error()
		} else {
			message += ": " + err.Error()
		}
	}
	return s.logMessage("ERROR", redact(message))
}

// Logf writes through the currently active session.  It is safe to call from
// concurrent goroutines and safe to call when diagnostics is unavailable.
func Logf(format string, args ...any) error {
	s := currentSession()
	if s == nil {
		return ErrNotStarted
	}
	return s.Logf(format, args...)
}

// LogError writes through the currently active session.
func LogError(err error, format string, args ...any) error {
	s := currentSession()
	if s == nil {
		return ErrNotStarted
	}
	return s.LogError(err, format, args...)
}

// PreviousRunUnclean reports the active session's startup finding. It returns
// false when diagnostics could not be started, keeping this optional feature
// from affecting application control flow.
func PreviousRunUnclean() bool {
	s := currentSession()
	return s != nil && s.PreviousRunUnclean()
}

func (s *Session) logMessage(level, message string) error {
	if s == nil {
		return ErrNotStarted
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	return s.writeRecordLocked(s.messageRecord(level, redact(message)))
}

func (s *Session) messageRecord(level, message string) logRecord {
	return logRecord{
		Time:    time.Now().UTC().Format(time.RFC3339Nano),
		Level:   level,
		Message: message,
		RunID:   s.metadata.RunID,
		GOOS:    s.metadata.GOOS,
		GOARCH:  s.metadata.GOARCH,
		Runtime: s.metadata.Runtime,
		PID:     s.metadata.PID,
	}
}

func (s *Session) writeRecordLocked(record logRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return s.logger.Write(data)
}

func runStateFor(s *Session, clean bool, ended time.Time) runState {
	state := runState{
		SchemaVersion: stateSchemaVersion,
		RunID:         s.metadata.RunID,
		StartedAt:     s.started.Format(time.RFC3339Nano),
		Clean:         clean,
		GOOS:          s.metadata.GOOS,
		GOARCH:        s.metadata.GOARCH,
		Runtime:       s.metadata.Runtime,
		PID:           s.metadata.PID,
	}
	if !ended.IsZero() {
		state.EndedAt = ended.UTC().Format(time.RFC3339Nano)
	}
	return state
}

func previousRunUnclean(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var state runState
	if err := json.Unmarshal(data, &state); err != nil {
		// A truncated diagnostic marker is itself evidence that the previous
		// process did not complete its atomic update. Do not let optional
		// diagnostics become a reason the main application cannot start logging.
		return true, nil
	}
	if state.RunID == "" {
		return true, nil
	}
	return !state.Clean, nil
}

func writeRunState(path string, state runState) error {
	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".run-state-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		return err
	}
	if _, err := tmp.Write(payload); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := replaceFile(tmpPath, path); err != nil {
		return err
	}
	committed = true
	return nil
}

func ensureDir(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(path, 0o700); err != nil && runtime.GOOS != "windows" {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	return nil
}

func protectFile(path string) error {
	if err := os.Chmod(path, 0o600); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return nil
}

func newRunID(now time.Time) (string, error) {
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%d-%s", now.Format("20060102T150405.000000000Z"), os.Getpid(), hex.EncodeToString(random[:])), nil
}

func currentSession() *Session {
	globalMu.RLock()
	s := globalSession
	globalMu.RUnlock()
	return s
}

func activate(s *Session) error {
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalSession != nil && globalSession != s {
		return ErrAlreadyStarted
	}
	if err := debug.SetCrashOutput(s.crash, debug.CrashOptions{}); err != nil {
		return err
	}
	globalSession = s
	return nil
}

// deactivate also serializes SetCrashOutput(nil) with activation of a newer
// session.  This matters when tests or an embedding process start more than
// one session in the same process.
func deactivate(s *Session) (bool, error) {
	globalMu.Lock()
	defer globalMu.Unlock()
	if globalSession != s {
		return false, nil
	}
	err := debug.SetCrashOutput(nil, debug.CrashOptions{})
	globalSession = nil
	return true, err
}
