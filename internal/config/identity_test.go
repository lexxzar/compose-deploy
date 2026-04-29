package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseIdentity_Empty(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"tab and newline", "\t\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseIdentity(tt.in)
			if err == nil {
				t.Fatalf("expected error for %q, got nil", tt.in)
			}
			if !strings.Contains(err.Error(), "path is empty") {
				t.Errorf("expected 'path is empty', got %q", err.Error())
			}
		})
	}
}

func TestParseIdentity_TildeExpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	keyPath := filepath.Join(home, "id_test")
	if err := os.WriteFile(keyPath, []byte("dummy"), 0600); err != nil {
		t.Fatalf("failed to write key file: %v", err)
	}

	got, err := ParseIdentity("~/id_test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != keyPath {
		t.Errorf("ParseIdentity(~/id_test) = %q, want %q", got, keyPath)
	}
}

func TestParseIdentity_BareTilde(t *testing.T) {
	// Bare `~` is rejected explicitly (a bare home directory is never a key
	// file, and supporting it adds expansion code that always fails the
	// subsequent regular-file check). Only `~/<path>` is supported.
	_, err := ParseIdentity("~")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "only ~/ is supported") {
		t.Errorf("expected 'only ~/ is supported', got %q", err.Error())
	}
}

func TestParseIdentity_TildeUserRejected(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"~root", "~root"},
		{"~user/key", "~someuser/.ssh/id_rsa"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseIdentity(tt.in)
			if err == nil {
				t.Fatalf("expected error for %q, got nil", tt.in)
			}
			if !strings.Contains(err.Error(), "only ~/ is supported") {
				t.Errorf("expected 'only ~/ is supported', got %q", err.Error())
			}
		})
	}
}

func TestParseIdentity_NotFound(t *testing.T) {
	_, err := ParseIdentity("/nonexistent/path/to/key")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected 'not found', got %q", err.Error())
	}
}

func TestParseIdentity_Directory(t *testing.T) {
	dir := t.TempDir()
	_, err := ParseIdentity(dir)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("expected 'not a regular file', got %q", err.Error())
	}
}

func TestParseIdentity_ValidFile(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id_rsa")
	if err := os.WriteFile(keyPath, []byte("dummy key"), 0600); err != nil {
		t.Fatalf("failed to write key file: %v", err)
	}
	got, err := ParseIdentity(keyPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != keyPath {
		t.Errorf("ParseIdentity(%q) = %q, want %q", keyPath, got, keyPath)
	}
}

func TestParseIdentity_RelativePath(t *testing.T) {
	// Create a file in the cwd and reference by relative name. Match must be
	// preserved (no absolutization to match `ssh -i` semantics).
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(cwd); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	if err := os.WriteFile("rel_key", []byte("k"), 0600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	got, err := ParseIdentity("rel_key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "rel_key" {
		t.Errorf("expected relative path passthrough %q, got %q", "rel_key", got)
	}
}

func TestParseIdentity_TrimsWhitespace(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	if err := os.WriteFile(keyPath, []byte("k"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ParseIdentity("  " + keyPath + "  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != keyPath {
		t.Errorf("got %q, want %q", got, keyPath)
	}
}

func TestParseIdentity_HomeUnsetError(t *testing.T) {
	// On unix, os.UserHomeDir() returns an error when HOME is unset. Clear
	// HOME (and Windows-equivalents) to exercise the "cannot resolve home
	// directory" branch.
	t.Setenv("HOME", "")
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", "")
	}

	_, err := ParseIdentity("~/foo")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "cannot resolve home") {
		t.Errorf("expected 'cannot resolve home', got %q", err.Error())
	}
}

func TestParseIdentity_StatPermissionError(t *testing.T) {
	// Cover the non-IsNotExist branch of os.Stat by placing the candidate
	// path inside a directory we cannot traverse (chmod 0000). Skip on
	// windows (chmod semantics differ) and on root (chmod 0000 doesn't
	// restrict access).
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0000 not meaningful on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 0000 doesn't restrict access")
	}

	dir := t.TempDir()
	subDir := filepath.Join(dir, "blocked")
	if err := os.Mkdir(subDir, 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Register cleanup BEFORE mutating mode (mirror the safety pattern used
	// in TestParseIdentity_Unreadable).
	t.Cleanup(func() {
		_ = os.Chmod(subDir, 0700) // restore so TempDir cleanup can remove it
	})
	if err := os.Chmod(subDir, 0000); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err := ParseIdentity(filepath.Join(subDir, "key"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Either branch is acceptable: on macOS/Linux, os.Stat on a path inside
	// a 0000-mode directory typically returns EACCES (handled by the
	// "cannot stat" branch). It must NOT be the IsNotExist branch.
	if strings.Contains(err.Error(), "not found") {
		t.Errorf("got 'not found' (IsNotExist branch), want 'cannot stat': %q", err.Error())
	}
	if !strings.Contains(err.Error(), "cannot stat") {
		t.Errorf("expected 'cannot stat', got %q", err.Error())
	}
}

func TestParseIdentity_Symlink_ToFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require elevation on windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real_key")
	if err := os.WriteFile(target, []byte("k"), 0600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	link := filepath.Join(dir, "link_key")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	got, err := ParseIdentity(link)
	if err != nil {
		t.Fatalf("expected symlink-to-file to be accepted, got error: %v", err)
	}
	if got != link {
		t.Errorf("got %q, want %q (symlink path preserved)", got, link)
	}
}

func TestParseIdentity_Symlink_ToDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require elevation on windows")
	}
	dir := t.TempDir()
	targetDir := filepath.Join(dir, "real_dir")
	if err := os.Mkdir(targetDir, 0700); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	link := filepath.Join(dir, "link_to_dir")
	if err := os.Symlink(targetDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := ParseIdentity(link)
	if err == nil {
		t.Fatal("expected error for symlink-to-directory, got nil")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("expected 'not a regular file', got %q", err.Error())
	}
}

func TestParseIdentity_Unreadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0000 not meaningful on windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 0000 doesn't restrict access")
	}
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "unreadable")
	if err := os.WriteFile(keyPath, []byte("k"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Register the cleanup BEFORE the chmod so a panic between the two
	// statements still restores 0600 (otherwise t.TempDir() can't clean up
	// the unreadable file).
	t.Cleanup(func() {
		_ = os.Chmod(keyPath, 0600) // restore so TempDir cleanup can remove it
	})
	if err := os.Chmod(keyPath, 0000); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	_, err := ParseIdentity(keyPath)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not readable") {
		t.Errorf("expected 'not readable', got %q", err.Error())
	}
}
