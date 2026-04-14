package logging

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestNewLogger_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLogger(dir)
	if err != nil {
		t.Fatalf("NewLogger() error: %v", err)
	}
	defer l.Close()

	if _, err := os.Stat(l.Path()); os.IsNotExist(err) {
		t.Fatalf("log file does not exist: %s", l.Path())
	}
}

func TestNewLogger_CreatesDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "logs")
	l, err := NewLogger(dir)
	if err != nil {
		t.Fatalf("NewLogger() error: %v", err)
	}
	defer l.Close()

	if _, err := os.Stat(dir); os.IsNotExist(err) {
		t.Fatalf("log directory was not created: %s", dir)
	}
}

func TestNewLogger_FileNaming(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLogger(dir)
	if err != nil {
		t.Fatalf("NewLogger() error: %v", err)
	}
	defer l.Close()

	filename := filepath.Base(l.Path())

	// Should match pattern: cdeploy_on_{hostname}_{timestamp}.log
	pattern := `^cdeploy_on_.+_\d{4}-\d{2}-\d{2}-\d{2}_\d{2}_\d{2}\.log$`
	matched, err := regexp.MatchString(pattern, filename)
	if err != nil {
		t.Fatalf("regexp error: %v", err)
	}
	if !matched {
		t.Errorf("filename %q does not match pattern %q", filename, pattern)
	}

	// Should contain hostname
	hostname, _ := os.Hostname()
	if !strings.Contains(filename, hostname) {
		t.Errorf("filename %q does not contain hostname %q", filename, hostname)
	}
}

func TestNewLogger_Writer(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLogger(dir)
	if err != nil {
		t.Fatalf("NewLogger() error: %v", err)
	}

	w := l.Writer()
	testData := "test log output\n"
	n, err := w.Write([]byte(testData))
	if err != nil {
		t.Fatalf("Write() error: %v", err)
	}
	if n != len(testData) {
		t.Errorf("Write() = %d bytes, want %d", n, len(testData))
	}

	l.Close()

	content, err := os.ReadFile(l.Path())
	if err != nil {
		t.Fatalf("ReadFile() error: %v", err)
	}
	if string(content) != testData {
		t.Errorf("file content = %q, want %q", string(content), testData)
	}
}

func TestNewLogger_DefaultDir(t *testing.T) {
	def := DefaultLogDir()
	if !strings.Contains(def, ".cdeploy") || !strings.Contains(def, "logs") {
		t.Errorf("DefaultLogDir() = %q, want path containing .cdeploy/logs", def)
	}
}

func TestLogger_Path(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLogger(dir)
	if err != nil {
		t.Fatalf("NewLogger() error: %v", err)
	}
	defer l.Close()

	if !strings.HasPrefix(l.Path(), dir) {
		t.Errorf("Path() = %q, want prefix %q", l.Path(), dir)
	}
}

func TestNewLogger_EmptyDir_UsesDefault(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)

	l, err := NewLogger("")
	if err != nil {
		t.Fatalf("NewLogger(\"\") error: %v", err)
	}
	defer l.Close()

	wantDir := filepath.Join(fakeHome, ".cdeploy", "logs")
	if !strings.HasPrefix(l.Path(), wantDir) {
		t.Errorf("Path() = %q, want prefix %q", l.Path(), wantDir)
	}
}

func TestNewLogger_InvalidDir(t *testing.T) {
	// Try to create a logger in an unwritable path
	_, err := NewLogger("/dev/null/impossible")
	if err == nil {
		t.Fatal("expected error for unwritable directory, got nil")
	}
	if !strings.Contains(err.Error(), "creating log directory") {
		t.Errorf("error = %q, want it to contain 'creating log directory'", err.Error())
	}
}
