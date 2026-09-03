package main

import (
	"strings"
	"testing"
)

func TestRunExportRejectsUnknownFormatBeforeLoadingConfiguration(t *testing.T) {
	err := runExport([]string{"--format", "html", "--output", "unused"})
	if err == nil || !strings.Contains(err.Error(), "unsupported export format") {
		t.Fatalf("runExport error = %v", err)
	}
}
