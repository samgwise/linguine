package main

import (
	"strings"
	"testing"
)

func TestAdminRevokeKeyRequiresID(t *testing.T) {
	err := adminRevokeKey("", nil)
	if err == nil || !strings.Contains(err.Error(), "--id is required") {
		t.Errorf("missing --id: got %v", err)
	}
}

func TestPrintVersion(t *testing.T) {
	var buf strings.Builder
	printVersion(&buf)
	if got := strings.TrimSpace(buf.String()); got != version {
		t.Errorf("printVersion = %q, want %q", got, version)
	}
}
