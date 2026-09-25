package handler

import (
	"testing"
)

func TestKeyPrefix(t *testing.T) {
	if got := keyPrefix("tk-3f2a9c7e", 8); got != "tk-3f2a9" {
		t.Errorf("keyPrefix=%q want %q", got, "tk-3f2a9")
	}
	if got := keyPrefix("abc", 8); got != "abc" {
		t.Errorf("short key should be returned as-is, got %q", got)
	}
}
