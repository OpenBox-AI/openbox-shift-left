//go:build darwin || linux

package securityskill

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPublisherUsesNoReplaceHardLinkAndExactTemporaryCleanup(t *testing.T) {
	_, files, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	script := filepath.Join(directory, "publish-candidate.sh")
	if err := os.WriteFile(script, files["scripts/publish-candidate.sh"], 0o700); err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(directory, ".openbox-security-analysis.tmp.first")
	target := filepath.Join(directory, "candidate.json")
	content := []byte(`{"result":"no_supported_issue"}`)
	if err := os.WriteFile(temporary, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(script, temporary, target).CombinedOutput(); err != nil {
		t.Fatalf("publish: %v: %s", err, output)
	}
	if got, _ := os.ReadFile(target); string(got) != string(content) {
		t.Fatalf("published bytes = %q", got)
	}
	if _, err := os.Lstat(temporary); !os.IsNotExist(err) {
		t.Fatalf("temporary remains: %v", err)
	}

	second := filepath.Join(directory, ".openbox-security-analysis.tmp.second")
	if err := os.WriteFile(second, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(script, second, target).CombinedOutput(); err == nil {
		t.Fatalf("overwrote target: %s", output)
	}
	if got, _ := os.ReadFile(target); string(got) != string(content) {
		t.Fatalf("existing target changed: %q", got)
	}
	if _, err := os.Lstat(second); !os.IsNotExist(err) {
		t.Fatalf("failed publication did not clean exact temp: %v", err)
	}
}
