package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunInitTrayEmitsNonTerminalAutostartEntry(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := runInit([]string{"tray"}, &stdout, &stderr); err != nil {
		t.Fatalf("runInit(tray) error = %v", err)
	}
	output := stdout.String()
	for _, want := range []string{"[Desktop Entry]", "Exec=%h/.local/bin/plan-usage tray", "Terminal=false"} {
		if !strings.Contains(output, want) {
			t.Fatalf("init tray output missing %q:\n%s", want, output)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("init tray wrote stderr: %s", stderr.String())
	}
}
