package main

import (
	"testing"
)

func TestSetupLogging(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "warning", "error", "unknown"} {
		t.Run(level, func(t *testing.T) {
			setupLogging(level)
		})
	}
}

func TestVersionDefined(t *testing.T) {
	if version == "" {
		t.Error("version should not be empty")
	}
}
