// Package logx is zen-gate's logger: daily-sharded files with retention,
// plus an in-memory ring surfaced to the dashboard's log viewer.
package logx

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Entry is one log line kept in the ring.
type Entry struct {
	At    time.Time `json:"at"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
}

const (
	ringSize  = 1000
	keepDays  = 7
	timeLayout = "2006-01-02 15:04:05.000"
)

type Logger struct {
	dir  string
	mu   sync.Mutex
	file *os.File
	day  string
	ring []Entry
	mu2  sync.Mutex // guards ring
	min  string     // minimum level written to file: "" = all
}

// New creates the logger; dir = <data>/logs. Old files beyond keepDays are
// swept on start.
func New(dataDir string) *Logger {
	dir := filepath.Join(dataDir, "logs")
	_ = os.MkdirAll(dir, 0o700)
	l := &Logger{dir: dir}
	l.sweep()
	return l
}

// SetFileLevel filters what reaches the file: "info" (default), "warn", "error".
func (l *Logger) SetFileLevel(level string) {
	l.mu.Lock()
	l.min = strings.ToLower(level)
	l.mu.Unlock()
}

func levelRank(level string) int {
	switch strings.ToLower(level) {
	case "debug":
		return 0
	case "info":
		return 1
	case "warn":
		return 2
	case "error":
		return 3
	}
	return 1
}

func (l *Logger) log(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	e := Entry{At: time.Now(), Level: strings.ToUpper(level), Msg: msg}

	l.mu2.Lock()
	l.ring = append(l.ring, e)
	if len(l.ring) > ringSize {
		l.ring = l.ring[len(l.ring)-ringSize:]
	}
	l.mu2.Unlock()

	l.mu.Lock()
	defer l.mu.Unlock()
	if rank := levelRank(level); rank < levelRank(l.min) {
		return
	}
	day := e.At.Format("2006-01-02")
	if l.file == nil || l.day != day {
		if l.file != nil {
			_ = l.file.Close()
		}
		f, err := os.OpenFile(filepath.Join(l.dir, day+".log"),
			os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		l.file, l.day = f, day
	}
	fmt.Fprintf(l.file, "[%s] [%s] %s\n", e.At.Format(timeLayout), e.Level, msg)
}

func (l *Logger) Infof(format string, args ...any)  { l.log("info", format, args...) }
func (l *Logger) Warnf(format string, args ...any)  { l.log("warn", format, args...) }
func (l *Logger) Errorf(format string, args ...any) { l.log("error", format, args...) }

// Tail returns the newest entries (optionally filtered by minimum level).
func (l *Logger) Tail(minLevel string, limit int) []Entry {
	l.mu2.Lock()
	defer l.mu2.Unlock()
	out := []Entry{}
	min := levelRank(minLevel)
	for _, e := range l.ring {
		if levelRank(strings.ToLower(e.Level)) < min {
			continue
		}
		out = append(out, e)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// sweep removes log files older than keepDays.
func (l *Logger) sweep() {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	cutoff := time.Now().AddDate(0, 0, -keepDays)
	names := []string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		day := strings.TrimSuffix(name, ".log")
		t, err := time.ParseInLocation("2006-01-02", day, time.Local)
		if err != nil {
			continue
		}
		if t.Before(cutoff) {
			_ = os.Remove(filepath.Join(l.dir, name))
		}
	}
}

// Close flushes the current file.
func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
}
