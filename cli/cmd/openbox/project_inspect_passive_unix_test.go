//go:build darwin || linux

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/model"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/sdkdesc"
)

func TestInspectProjectPassivelyIsDeterministicAndDoesNotExecuteOrLeak(t *testing.T) {
	projectRoot := filepath.Join(t.TempDir(), "fixture")
	if err := os.Mkdir(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	const secret = "openbox-se02-08-secret-canary"
	files := map[string][]byte{
		"package.json": []byte(`{"main":"src/index.ts","scripts":{"postinstall":"node scripts/must-not-run.js"},"dependencies":{"@openbox-ai/openbox-mastra-sdk":"1.0.0"}}`),
		"src/index.ts": []byte(`import { withOpenBox } from "@openbox-ai/openbox-mastra-sdk";
const concealed = "openbox-se02-08-secret-canary";
const endpoint = "https://user:password@example.invalid/private?token=hidden";
export const agent = createTool({ id: "recording-tool" });
`),
		"scripts/must-not-run.js": []byte(`require("fs").writeFileSync(process.env.OPENBOX_EXEC_SENTINEL, "node-script-ran")`),
		"unsafe.py": []byte(`from pathlib import Path
Path(__import__("os").environ["OPENBOX_EXEC_SENTINEL"]).write_text("python-import-ran")
`),
		".env":   []byte("OPENBOX_API_KEY=" + secret + "\n"),
		".npmrc": []byte("//registry.invalid/:_authToken=" + secret + "\n"),
	}
	for relative, content := range files {
		full := filepath.Join(projectRoot, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	runtimeTemp := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(runtimeTemp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", runtimeTemp)
	execSentinel := filepath.Join(t.TempDir(), "executed")
	t.Setenv("OPENBOX_EXEC_SENTINEL", execSentinel)
	tripwireBin := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(tripwireBin, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"git", "node", "npm", "npx", "pnpm", "yarn", "python", "python3"} {
		script := []byte("#!/bin/sh\nprintf invoked > \"$OPENBOX_EXEC_SENTINEL\"\nexit 97\n")
		if err := os.WriteFile(filepath.Join(tripwireBin, name), script, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", tripwireBin)
	t.Setenv("OPENBOX_API_KEY", secret)
	t.Setenv("AWS_SECRET_ACCESS_KEY", secret)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	connected := make(chan struct{}, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = connection.Close()
			connected <- struct{}{}
		}
	}()
	proxy := "http://" + listener.Addr().String()
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy", "OPENBOX_URL", "OPENBOX_BACKEND_URL"} {
		t.Setenv(name, proxy)
	}

	before := sourceState(t, projectRoot)
	outputBoundary := filepath.Join(t.TempDir(), "inspection-output")
	first, err := inspectProjectPassively(projectRoot, outputBoundary)
	if err != nil {
		t.Fatal(err)
	}
	second, err := inspectProjectPassively(projectRoot, outputBoundary)
	if err != nil {
		t.Fatal(err)
	}
	a, stdout, stderr := testApp(nil)
	if code := a.run([]string{"project", "inspect", projectRoot, "--output", outputBoundary}); code != exitOK {
		t.Fatalf("real CLI exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for name, want := range map[string][]byte{
		"project-snapshot.json": first.project.SnapshotManifest().Bytes(),
		"project-model.json":    first.project.ProjectModel().Bytes(),
		"sdk-coverage.json":     first.coverage.SDKCoverage.Bytes(),
	} {
		got, readErr := os.ReadFile(filepath.Join(outputBoundary, name))
		if readErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("real CLI artifact %s differs from in-memory bytes: %v", name, readErr)
		}
	}
	if _, err := os.Lstat(filepath.Join(outputBoundary, "manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("standalone inspection published manifest.json: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-connected:
		t.Fatal("passive inspection connected to the loopback network trap")
	case <-time.After(20 * time.Millisecond):
	}
	if _, err := os.Lstat(execSentinel); !os.IsNotExist(err) {
		t.Fatalf("project code or executable ran: %v", err)
	}
	if after := sourceState(t, projectRoot); !bytes.Equal(before, after) {
		t.Fatalf("source state changed\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if entries, err := os.ReadDir(runtimeTemp); err != nil || len(entries) != 0 {
		t.Fatalf("temporary inspection residue = %v, %v", entries, err)
	}

	firstObjects := []struct {
		name   string
		bytes  []byte
		digest string
	}{
		{name: "project-snapshot", bytes: first.project.SnapshotManifest().Bytes(), digest: first.project.SnapshotManifest().Digest().String()},
		{name: "project-model", bytes: first.project.ProjectModel().Bytes(), digest: first.project.ProjectModel().Digest().String()},
		{name: "sdk-coverage", bytes: first.coverage.SDKCoverage.Bytes(), digest: first.coverage.SDKCoverage.Digest().String()},
	}
	secondDigests := []string{
		second.project.SnapshotManifest().Digest().String(),
		second.project.ProjectModel().Digest().String(),
		second.coverage.SDKCoverage.Digest().String(),
	}
	for index, object := range firstObjects {
		if object.digest != secondDigests[index] {
			t.Fatalf("%s digest changed: %s != %s", object.name, object.digest, secondDigests[index])
		}
		if bytes.Contains(object.bytes, []byte(secret)) || bytes.Contains(bytes.ToLower(object.bytes), []byte("password")) || bytes.Contains(object.bytes, []byte("token=hidden")) {
			t.Fatalf("%s retained a secret or URL credential component: %s", object.name, object.bytes)
		}
	}
	if first.coverage.Guidance.Status != sdkdesc.ReadinessNotRunnable ||
		!reflect.DeepEqual(first.coverage.Guidance, second.coverage.Guidance) {
		t.Fatalf("guidance = %#v / %#v", first.coverage.Guidance, second.coverage.Guidance)
	}
	joinedGuidance := first.coverage.Guidance.Summary + strings.Join(first.coverage.Guidance.Actions, "\n")
	if strings.Contains(joinedGuidance, secret) {
		t.Fatal("readiness guidance retained a secret")
	}
}

func TestInspectProjectPassivelyReportsUnknownGitState(t *testing.T) {
	projectRoot := filepath.Join(t.TempDir(), "fixture")
	if err := os.MkdirAll(filepath.Join(projectRoot, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "package.json"), []byte(`{"dependencies":{"@openbox-ai/openbox-mastra-sdk":"1.0.0"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runtimeTemp := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(runtimeTemp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", runtimeTemp)
	before := sourceState(t, projectRoot)
	result, err := inspectProjectPassively(projectRoot, filepath.Join(t.TempDir(), "output"))
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Project struct {
			Git struct {
				Present bool    `json:"present"`
				Head    *string `json:"head"`
				Dirty   *bool   `json:"dirty"`
			} `json:"git"`
		} `json:"project"`
		Uncertainties []struct {
			Subject string `json:"subject"`
		} `json:"uncertainties"`
	}
	if err := json.Unmarshal(result.project.ProjectModel().Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if !document.Project.Git.Present || document.Project.Git.Head != nil || document.Project.Git.Dirty != nil ||
		len(document.Uncertainties) == 0 || document.Uncertainties[len(document.Uncertainties)-1].Subject != "git-status" {
		t.Fatalf("Git state was not explicitly unknown: %#v", document)
	}
	if after := sourceState(t, projectRoot); !bytes.Equal(before, after) {
		t.Fatalf("Git-root source state changed\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if entries, readErr := os.ReadDir(runtimeTemp); readErr != nil || len(entries) != 0 {
		t.Fatalf("temporary inspection residue = %v, %v", entries, readErr)
	}
}

func TestVerifyProjectIdentityRejectsGitMarkerChange(t *testing.T) {
	root := filepath.Join(t.TempDir(), "fixture")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := model.CollectProjectIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := verifyProjectIdentity(root, identity); err == nil || !strings.Contains(err.Error(), "Git marker changed") {
		t.Fatalf("error = %v", err)
	}
}

func sourceState(t *testing.T, root string) []byte {
	t.Helper()
	records := make([]string, 0)
	err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		detail := ""
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			detail, err = os.Readlink(current)
		case info.Mode().IsRegular():
			content, readErr := os.ReadFile(current)
			if readErr != nil {
				return readErr
			}
			detail = fmt.Sprintf("%x", sha256.Sum256(content))
		}
		if err != nil {
			return err
		}
		records = append(records, fmt.Sprintf("%s\t%s\t%04o\t%d\t%s", filepath.ToSlash(relative), info.Mode().Type(), info.Mode().Perm(), info.Size(), detail))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(records)
	return []byte(strings.Join(records, "\n"))
}
