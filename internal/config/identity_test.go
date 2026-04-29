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
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Bare `~` resolves to home, but home is a directory — should fail with
	// "not a regular file". This confirms expansion happened (otherwise we'd
	// see "not found").
	_, err := ParseIdentity("~")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("expected 'not a regular file', got %q", err.Error())
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
	if err := os.Chmod(keyPath, 0000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(keyPath, 0600) // restore so TempDir cleanup can remove it
	})

	_, err := ParseIdentity(keyPath)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "not readable") {
		t.Errorf("expected 'not readable', got %q", err.Error())
	}
}
