package service

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	coreapp "github.com/Jaaayden/telegram-uploader/internal/app"
	"github.com/Jaaayden/telegram-uploader/internal/diagnostics"
	"github.com/Jaaayden/telegram-uploader/internal/model"
)

const (
	maxPanelLogs       = 500
	maxInitialDiskLogs = 200
)

func (s *Service) appendLog(entry model.LogEntry) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if entry.TimeMs == 0 {
		entry.TimeMs = time.Now().UnixMilli()
	}
	s.logSeq++
	if entry.ID == "" {
		entry.ID = fmt.Sprintf("log-%d-%d", entry.TimeMs, s.logSeq)
	}
	s.logs = append(s.logs, entry)
	if len(s.logs) > maxPanelLogs {
		s.logs = append([]model.LogEntry(nil), s.logs[len(s.logs)-maxPanelLogs:]...)
	}
	ready := s.frontReady
	s.mu.Unlock()
	if ready {
		s.emit("log", entry)
	}
}

// LogsSnapshot returns a copy of the upload activity logs currently held by the service.
func (s *Service) LogsSnapshot() []model.LogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.logs) == 0 {
		return []model.LogEntry{}
	}
	out := make([]model.LogEntry, len(s.logs))
	copy(out, s.logs)
	return out
}

// ClearLogs clears all in-memory log entries and deletes persisted app.log files
// (and rotated backups) from disk.
func (s *Service) ClearLogs() error {
	if err := diagnostics.ClearAppLogs(); err != nil && !errors.Is(err, diagnostics.ErrNotStarted) {
		return fmt.Errorf("清除磁盘日志失败：%w", err)
	} else if errors.Is(err, diagnostics.ErrNotStarted) && s.paths.Root != "" {
		if dirErr := diagnostics.ClearAppLogsInDir(filepath.Join(s.paths.Root, "logs")); dirErr != nil {
			return fmt.Errorf("清除磁盘日志失败：%w", dirErr)
		}
	}
	s.mu.Lock()
	s.logs = nil
	ready := s.frontReady
	s.mu.Unlock()
	if ready {
		s.emit("logs-reset", []model.LogEntry{})
	}
	return nil
}

// DeleteLog removes a single log entry from the in-memory log panel.
func (s *Service) DeleteLog(id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	filtered := s.logs[:0]
	changed := false
	for _, entry := range s.logs {
		if entry.ID == id {
			changed = true
			continue
		}
		filtered = append(filtered, entry)
	}
	if !changed {
		s.mu.Unlock()
		return
	}
	s.logs = append([]model.LogEntry(nil), filtered...)
	out := make([]model.LogEntry, len(s.logs))
	copy(out, s.logs)
	ready := s.frontReady
	s.mu.Unlock()
	if ready {
		s.emit("logs-reset", out)
	}
}

// SetPersistLogs turns disk persistence of app.log on or off.
func (s *Service) SetPersistLogs(enabled bool) error {
	if err := s.updateSettings(func(settings *coreapp.Settings) {
		settings.DisableLogPersistence = !enabled
	}); err != nil {
		return err
	}
	if err := diagnostics.SetAppLogEnabled(enabled); err != nil && !errors.Is(err, diagnostics.ErrNotStarted) {
		return err
	}
	s.invalidate()
	return nil
}

// pruneLogsForRemovedJobs removes in-memory log entries for queue items that
// have been deleted when non-persistent mode is enabled.
func (s *Service) pruneLogsForRemovedJobs() {
	snap := s.controller.Snapshot()
	alive := make(map[string]struct{}, len(snap.Jobs))
	for i := range snap.Jobs {
		alive[snap.Jobs[i].ID] = struct{}{}
	}
	s.mu.Lock()
	if len(s.logs) == 0 {
		s.mu.Unlock()
		return
	}
	filtered := make([]model.LogEntry, 0, len(s.logs))
	changed := false
	for _, entry := range s.logs {
		if entry.JobID != "" {
			if _, ok := alive[entry.JobID]; !ok {
				changed = true
				continue
			}
		}
		filtered = append(filtered, entry)
	}
	if !changed {
		s.mu.Unlock()
		return
	}
	s.logs = filtered
	ready := s.frontReady
	s.mu.Unlock()
	if ready {
		s.emit("logs-reset", filtered)
	}
}

func (s *Service) loadPersistedLogs() {
	if s.paths.Root == "" {
		return
	}
	logPath := filepath.Join(s.paths.Root, "logs", "app.log")
	f, err := os.Open(logPath)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 16*1024), 512*1024)
	var parsed []model.LogEntry
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec struct {
			Time    string `json:"time"`
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil || rec.Message == "" {
			continue
		}
		entry, ok := parseDiskLogMessage(rec.Time, rec.Message)
		if !ok {
			continue
		}
		parsed = append(parsed, entry)
		if len(parsed) > maxInitialDiskLogs {
			parsed = parsed[len(parsed)-maxInitialDiskLogs:]
		}
	}
	if len(parsed) == 0 {
		return
	}
	s.mu.Lock()
	for _, entry := range parsed {
		s.logSeq++
		entry.ID = fmt.Sprintf("disk-%d", s.logSeq)
		s.logs = append(s.logs, entry)
	}
	if len(s.logs) > maxPanelLogs {
		s.logs = append([]model.LogEntry(nil), s.logs[len(s.logs)-maxPanelLogs:]...)
	}
	s.mu.Unlock()
}

func parseDiskLogMessage(ts string, message string) (model.LogEntry, bool) {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t = time.Now()
	}
	space := strings.IndexByte(message, ' ')
	if space <= 0 {
		return model.LogEntry{}, false
	}
	tag := message[:space]
	fields := parseLogFields(message[space+1:])
	switch tag {
	case "job_sent":
		elapsed := fields["elapsed"]
		speed := fields["avg_speed"]
		if elapsed == "" && speed == "" {
			return model.LogEntry{}, false
		}
		summary := "上传完成"
		if elapsed != "" {
			summary += " · 耗时 " + elapsed
		}
		if speed != "" {
			summary += " · 均速 " + speed
		}
		sentBytes, _ := strconv.ParseInt(fields["sent_bytes"], 10, 64)
		size, _ := strconv.ParseInt(fields["size"], 10, 64)
		return model.LogEntry{
			TimeMs:    t.UnixMilli(),
			Kind:      "sent",
			JobID:     fields["id"],
			Name:      fields["name"],
			Size:      size,
			SentBytes: sentBytes,
			Summary:   summary,
		}, true
	case "album_sent":
		return model.LogEntry{
			TimeMs:  t.UnixMilli(),
			Kind:    "album",
			Summary: fmt.Sprintf("合并发送成功：共 %s 个视频（提交耗时 %s）", fields["count"], fields["elapsed"]),
		}, true
	case "job_moved":
		dest := filepath.Base(fields["dest"])
		return model.LogEntry{
			TimeMs:  t.UnixMilli(),
			Kind:    "moved",
			JobID:   fields["id"],
			Name:    fields["name"],
			Summary: "已移动到 " + dest,
		}, true
	case "job_attempt_end":
		if fields["outcome_unknown"] == "true" {
			return model.LogEntry{
				TimeMs:  t.UnixMilli(),
				Kind:    "warn",
				JobID:   fields["id"],
				Summary: "提交结果不确定，请先检查频道（耗时 " + fields["elapsed"] + "）",
			}, true
		}
		if fields["cancelled"] == "true" {
			return model.LogEntry{
				TimeMs:  t.UnixMilli(),
				Kind:    "warn",
				JobID:   fields["id"],
				Summary: "已取消/暂停（耗时 " + fields["elapsed"] + "）",
			}, true
		}
		return model.LogEntry{
			TimeMs:  t.UnixMilli(),
			Kind:    "error",
			JobID:   fields["id"],
			Summary: "上传尝试失败（耗时 " + fields["elapsed"] + "）",
		}, true
	case "queue_stop":
		sent, _ := strconv.Atoi(fields["sent"])
		if sent <= 0 {
			return model.LogEntry{}, false
		}
		sentBytes, _ := strconv.ParseInt(fields["sent_bytes"], 10, 64)
		return model.LogEntry{
			TimeMs:    t.UnixMilli(),
			Kind:      "summary",
			SentBytes: sentBytes,
			Summary:   fmt.Sprintf("本轮上传结束：共 %d 个视频，上传耗时 %s，平均速度 %s", sent, fields["upload_time"], fields["avg_speed"]),
		}, true
	default:
		return model.LogEntry{}, false
	}
}

func parseLogFields(rest string) map[string]string {
	out := make(map[string]string)
	i := 0
	for i < len(rest) {
		for i < len(rest) && rest[i] == ' ' {
			i++
		}
		eq := strings.IndexByte(rest[i:], '=')
		if eq <= 0 {
			break
		}
		key := rest[i : i+eq]
		i += eq + 1
		if i < len(rest) && rest[i] == '"' {
			end := i + 1
			for end < len(rest) {
				if rest[end] == '\\' && end+1 < len(rest) {
					end += 2
					continue
				}
				if rest[end] == '"' {
					end++
					break
				}
				end++
			}
			val := rest[i:end]
			if unquoted, err := strconv.Unquote(val); err == nil {
				val = unquoted
			}
			out[key] = val
			i = end
		} else {
			// Value runs until the next " key=" boundary (so "12.34 MiB/s" stays intact).
			end := len(rest)
			for j := i; j < len(rest); j++ {
				if rest[j] == ' ' {
					nextEq := strings.IndexByte(rest[j+1:], '=')
					if nextEq > 0 && !strings.ContainsRune(rest[j+1:j+1+nextEq], ' ') {
						end = j
						break
					}
				}
			}
			out[key] = rest[i:end]
			i = end
		}
	}
	return out
}
