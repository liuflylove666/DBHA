package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyRequiresPersistentServerStateDirectory(t *testing.T) {
	if err := validateApplyPrerequisites(true, "", true, true); err == nil || !strings.Contains(err.Error(), "--state-dir") {
		t.Fatalf("missing state directory accepted: %v", err)
	}
	if err := validateApplyPrerequisites(true, filepath.Join(t.TempDir(), "missing"), true, true); err == nil {
		t.Fatal("missing mounted state directory accepted")
	}
	if err := validateApplyPrerequisites(true, t.TempDir(), true, true); err != nil {
		t.Fatalf("writable state directory rejected: %v", err)
	}
	if err := validateApplyPrerequisites(false, "", false, false); err != nil {
		t.Fatalf("dry run unexpectedly requires state directory: %v", err)
	}
}
