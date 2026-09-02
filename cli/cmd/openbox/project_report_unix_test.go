//go:build darwin || linux

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
)

func TestProjectReportAndProposeRenderOnlyVerifiedPack(t *testing.T) {
	root, _ := finalizedCLIVerificationPack(t)
	tests := []struct {
		args     []string
		required []string
		json     bool
	}{
		{args: []string{"project", "report", "--pack", root}, required: []string{"# OpenBox Project Assurance Report", "finding-ASI02-INDIRECT-EGRESS-001"}},
		{args: []string{"project", "report", "--pack", root, "--format", "json"}, required: []string{`"kind":"OpenBoxProjectAssuranceReport"`, `"severity":"unavailable"`}, json: true},
		{args: []string{"project", "report", "--pack", root, "--format", "sarif"}, required: []string{`"version":"2.1.0"`, `"level":"none"`}, json: true},
		{args: []string{"project", "propose", "--pack", root}, required: []string{`"kind":"OpenBoxProjectAssuranceProposal"`, `"candidateDocument":`, `input.activity_type == \"recordingTool\"`}, json: true},
		{args: []string{"project", "propose", "--pack", root, "--format", "markdown"}, required: []string{"# OpenBox Project Assurance Proposal", "## Candidate document", `input.activity_type == "recordingTool"`}},
	}
	for _, test := range tests {
		t.Run(strings.Join(test.args[1:3], "-"), func(t *testing.T) {
			a, stdout, stderr := testApp(nil)
			if code := a.run(test.args); code != exitOK {
				t.Fatalf("exit=%d stderr=%q", code, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr=%q", stderr.String())
			}
			if test.json && !json.Valid(stdout.Bytes()) {
				t.Fatalf("invalid JSON: %s", stdout.Bytes())
			}
			for _, required := range test.required {
				if !strings.Contains(stdout.String(), required) {
					t.Fatalf("output lacks %q: %s", required, stdout.String())
				}
			}
			if test.args[1] == "propose" && test.json {
				var envelope struct {
					CandidateDocument string `json:"candidateDocument"`
					Proposal          struct {
						Candidate struct {
							DocumentDigest artifact.ContentDigest `json:"documentDigest"`
						} `json:"candidate"`
					} `json:"proposal"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil ||
					envelope.Proposal.Candidate.DocumentDigest != artifact.DigestBytes([]byte(envelope.CandidateDocument)) {
					t.Fatalf("proposal candidate bytes are not digest-bound: %+v error=%v", envelope, err)
				}
			}
		})
	}
}

func TestProjectReportAndProposeRejectInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{"project", "report"},
		{"project", "report", "--pack", "", "--format", "json"},
		{"project", "report", "--pack", ".", "--format", "console"},
		{"project", "propose", "--pack", ".", "--format", "sarif"},
		{"project", "propose", "--pack", ".", "extra"},
	} {
		a, stdout, stderr := testApp(nil)
		if code := a.run(args); code != exitError || stdout.Len() != 0 || !strings.Contains(stderr.String(), "requires --pack DIR") {
			t.Fatalf("args=%v exit/output=%q/%q", args, stdout.String(), stderr.String())
		}
	}
	for _, args := range [][]string{{"project", "report", "--help"}, {"project", "propose", "--help"}} {
		a, stdout, stderr := testApp(nil)
		if code := a.run(args); code != exitOK || stdout.Len() != 0 || !strings.Contains(stderr.String(), "Usage: openbox project") {
			t.Fatalf("help args=%v exit/output=%q/%q", args, stdout.String(), stderr.String())
		}
	}
}

func TestProjectReportAndProposeHaveNoExecutionNetworkOrWriteAuthority(t *testing.T) {
	root, _ := finalizedCLIVerificationPack(t)
	packBefore := testTreeDigest(t, root)
	guard := t.TempDir()
	bin := filepath.Join(guard, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	executed := filepath.Join(guard, "must-not-execute")
	for _, name := range []string{"codex", "curl", "docker", "git", "node", "ollama"} {
		content := []byte("#!/bin/sh\n: > '" + executed + "'\nexit 99\n")
		if err := os.WriteFile(filepath.Join(bin, name), content, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	sentinel := filepath.Join(guard, "source-and-control-plane-sentinel")
	if err := os.WriteFile(sentinel, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := directoryNames(guard)
	if err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan bool, 1)
	proxyURL := "http://" + listener.Addr().String()
	t.Setenv("HTTP_PROXY", proxyURL)
	t.Setenv("HTTPS_PROXY", proxyURL)
	t.Setenv("ALL_PROXY", proxyURL)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			_ = connection.Close()
		}
		accepted <- acceptErr == nil
	}()

	for _, args := range [][]string{
		{"project", "report", "--pack", root, "--format", "json"},
		{"project", "propose", "--pack", root, "--format", "json"},
	} {
		a, _, stderr := testApp(nil)
		if code := a.run(args); code != exitOK || stderr.Len() != 0 {
			t.Fatalf("args=%v exit=%d stderr=%q", args, code, stderr.String())
		}
	}
	_ = listener.Close()
	if <-accepted {
		t.Fatal("report or propose made a network connection")
	}
	if _, err := os.Lstat(executed); !os.IsNotExist(err) {
		t.Fatalf("report or propose executed a child process: %v", err)
	}
	after, err := directoryNames(guard)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(sentinel)
	if err != nil || string(content) != "unchanged" || !reflect.DeepEqual(before, after) || packBefore != testTreeDigest(t, root) {
		t.Fatalf("guard changed: before=%v after=%v content=%q error=%v", before, after, content, err)
	}
}

func directoryNames(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name()
	}
	return names, nil
}

func testTreeDigest(t *testing.T, root string) [sha256.Size]byte {
	t.Helper()
	digest := sha256.New()
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(digest, "%s\x00%s\x00", relative, info.Mode())
		if entry.Type().IsRegular() {
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			_, _ = digest.Write(content)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))
	return result
}
