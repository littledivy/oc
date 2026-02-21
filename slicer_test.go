package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOutline_NoExtension_DoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "LICENSE")
	content := "MIT License\n\nPermission is hereby granted...\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	outline := Outline(path)
	if strings.TrimSpace(outline) != "" {
		t.Fatalf("expected empty outline for extensionless file, got %q", outline)
	}
}
