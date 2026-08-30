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
