package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// DefaultLogDir returns the default log directory (~/.cdeploy/logs/).
func DefaultLogDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".cdeploy", "logs")
	}
	return filepath.Join(home, ".cdeploy", "logs")
}

// Logger manages a timestamped log file.
type Logger struct {
	file *os.File
	path string
}

// NewLogger creates a new log file in the given directory.
// If logDir is empty, DefaultLogDir() is used.
func NewLogger(logDir string) (*Logger, error) {
	if logDir == "" {
		logDir = DefaultLogDir()
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating log directory: %w", err)
	}

	hostname, _ := os.Hostname()
	timestamp := time.Now().Format("2006-01-02-15_04_05")
	filename := fmt.Sprintf("cdeploy_on_%s_%s.log", hostname, timestamp)
	path := filepath.Join(logDir, filename)

	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("creating log file: %w", err)
	}

	return &Logger{file: f, path: path}, nil
}

// Writer returns the log file as an io.Writer.
func (l *Logger) Writer() io.Writer {
	return l.file
}

// Path returns the full path of the log file.
func (l *Logger) Path() string {
	return l.path
}

// Close closes the log file.
func (l *Logger) Close() error {
	return l.file.Close()
}
