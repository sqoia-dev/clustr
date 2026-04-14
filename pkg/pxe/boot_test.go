package pxe

import (
	"strings"
	"testing"
)

func TestGenerateDiskBootScript_UsesSanboot(t *testing.T) {
	script, err := GenerateDiskBootScript("node207")
	if err != nil {
		t.Fatalf("GenerateDiskBootScript returned error: %v", err)
	}
	out := string(script)

	if !strings.Contains(out, "sanboot --no-describe --drive 0x80") {
		t.Errorf("disk boot script missing sanboot command; got:\n%s", out)
	}

	// Ensure bare "exit" line is not present — it causes SeaBIOS PXE loop.
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "exit" {
			t.Errorf("disk boot script must not contain bare 'exit' line (SeaBIOS loop); got line: %q", line)
		}
	}

	if !strings.Contains(out, "node207") {
		t.Errorf("disk boot script should include hostname 'node207'; got:\n%s", out)
	}
}
