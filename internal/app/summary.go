package app

import (
	"fmt"
	"time"
)

// uploadStats accumulates what one queue run has uploaded, so the log can end
// with a summary line. It is only touched from the runQueue goroutine.
type uploadStats struct {
	jobs    int
	bytes   int64
	elapsed time.Duration
}

// add records one successfully sent video.
func (s *uploadStats) add(bytes int64, elapsed time.Duration) {
	s.jobs++
	s.bytes += bytes
	s.elapsed += elapsed
}

// transferRate is bytes per second, or 0 when the elapsed time is too short to
// give a meaningful number.
func transferRate(bytes int64, elapsed time.Duration) float64 {
	if bytes <= 0 || elapsed <= 0 {
		return 0
	}
	return float64(bytes) / elapsed.Seconds()
}

// formatRate renders a transfer rate in binary units, e.g. "12.34 MiB/s".
func formatRate(bytesPerSecond float64) string {
	if bytesPerSecond <= 0 {
		return "n/a"
	}
	units := []string{"B/s", "KiB/s", "MiB/s", "GiB/s"}
	value := bytesPerSecond
	unit := 0
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	if unit == 0 {
		return fmt.Sprintf("%.0f %s", value, units[unit])
	}
	return fmt.Sprintf("%.2f %s", value, units[unit])
}

// formatElapsed rounds a duration for reading: 100 ms below a minute, a whole
// second above it.
func formatElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

// formatLocalTime renders a moment in the machine's local zone with its
// offset, which is easier to match against the clock than the UTC time stamp
// every log record already carries.
func formatLocalTime(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04:05 -07:00")
}
