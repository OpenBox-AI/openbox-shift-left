package securityreport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/observation"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/runfs"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/targetposture"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/securityskill"
)

type descriptor struct {
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	Bytes     int    `json:"bytes"`
	SHA256    string `json:"sha256"`
}

type manifest struct {
	Schema      string       `json:"schema"`
	PackSchema  string       `json:"pack_schema"`
	Payloads    []descriptor `json:"payloads"`
	PackDigest  string       `json:"pack_digest"`
	FinalizedAt string       `json:"finalized_at"`
}

var reportInventory = []struct {
	Path      string
	MediaType string
}{
	{"observation/run.json", "application/json"},
	{"observation/backend.json", "application/json"},
	{"observation/openshell.jsonl", "application/x-ndjson"},
	{"observation/effects.json", "application/json"},
	{"observation/behavior.json", "application/json"},
	{"observation/coverage.json", "application/json"},
	{"observation/manifest.json", "application/json"},
	{"analysis.json", "application/json"},
	{"standards.json", "application/json"},
	{"recommendation-catalog.json", "application/json"},
	{"target-posture.json", "application/json"},
	{"report.json", "application/json"},
	{"report.md", "text/markdown"},
	{"report.sarif", "application/sarif+json"},
}

// Finalize performs the only online Phase 4 step after Prepare has completed.
// Capture is GET-only; sealing never invokes a model, host skill, project, or
// OpenBox write route.
func Finalize(ctx context.Context, prepared *Prepared, input RuntimeInput, dependencies Dependencies) (Result, error) {
	if prepared == nil {
		return Result{}, errors.New("security report: offline preparation is required")
	}
	capture := dependencies.Capture
	if capture == nil {
		capture = targetposture.Capture
	}
	posture, err := capture(ctx, targetposture.Config{
		BackendURL: input.BackendURL, ControlToken: input.ControlToken,
		AgentID: prepared.AgentID, OrganizationID: prepared.OrganizationID,
		PackDigest: prepared.PackDigest,
		Catalog:    targetposture.Identity{Version: RecommendationVersion, Digest: RecommendationDigest},
		HTTP:       input.HTTP, ProxyConfigured: input.ProxyConfigured, Now: input.Now,
	})
	if err != nil {
		return Result{}, err
	}
	if err := validatePosture(posture, prepared); err != nil {
		return Result{}, err
	}
	projections, postureBytes, err := buildProjections(prepared, posture)
	if err != nil {
		return Result{}, err
	}
	_, catalogBytes, err := loadCatalog()
	if err != nil {
		return Result{}, err
	}
	files := make(map[string][]byte, len(reportInventory))
	for _, name := range []string{"run.json", "backend.json", "openshell.jsonl", "effects.json", "behavior.json", "coverage.json"} {
		files["observation/"+name] = append([]byte(nil), prepared.Observation.Payloads[name]...)
	}
	files["observation/manifest.json"] = append([]byte(nil), prepared.Observation.Manifest...)
	files["analysis.json"] = append([]byte(nil), prepared.CandidateBytes...)
	files["standards.json"] = append([]byte(nil), prepared.StandardsBytes...)
	files["recommendation-catalog.json"] = catalogBytes
	files["target-posture.json"] = postureBytes
	files["report.json"] = projections.JSON
	files["report.md"] = projections.Markdown
	files["report.sarif"] = projections.SARIF
	packDigest, err := sealPack(prepared.OutputPath, files, posture.CaptureWindow.CompletedAt)
	if err != nil {
		return Result{}, err
	}
	return Result{Output: prepared.OutputPath, PackDigest: packDigest}, nil
}

func sealPack(output string, files map[string][]byte, finalizedAt string) (digest string, err error) {
	if _, err := time.Parse(time.RFC3339Nano, finalizedAt); err != nil {
		return "", errors.New("security report: invalid finalization timestamp")
	}
	if _, err := resolveAbsentOutput(output); err != nil {
		return "", err
	}
	descriptors := make([]descriptor, 0, len(reportInventory))
	for _, expected := range reportInventory {
		content, ok := files[expected.Path]
		if !ok {
			return "", fmt.Errorf("security report: missing pack payload %s", expected.Path)
		}
		descriptors = append(descriptors, descriptor{Path: expected.Path, MediaType: expected.MediaType, Bytes: len(content), SHA256: artifact.DigestBytes(content).String()})
	}
	if len(files) != len(reportInventory) {
		return "", errors.New("security report: pack payload set is not exact")
	}
	descriptorBytes, err := artifact.CanonicalJSON(descriptors)
	if err != nil {
		return "", err
	}
	digest = artifact.DigestBytes(descriptorBytes).String()
	manifestBytes, err := artifact.CanonicalJSON(manifest{Schema: ManifestSchema, PackSchema: Schema, Payloads: descriptors, PackDigest: digest, FinalizedAt: finalizedAt})
	if err != nil {
		return "", err
	}
	if err := validatePhaseFourSchema(ManifestSchema, manifestBytes); err != nil {
		return "", err
	}
	parent := filepath.Dir(output)
	staging, err := os.MkdirTemp(parent, "."+filepath.Base(output)+".security-report-")
	if err != nil {
		return "", fmt.Errorf("security report: create same-parent staging directory: %w", err)
	}
	rollback := true
	defer func() {
		if rollback {
			_ = os.Chmod(filepath.Join(staging, "observation"), 0o700)
			_ = os.Chmod(staging, 0o700)
			cleanupErr := os.RemoveAll(staging)
			if cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("security report: clean exact staging directory: %w", cleanupErr))
			}
		}
	}()
	if err := os.Chmod(staging, 0o700); err != nil {
		return "", err
	}
	observationDirectory := filepath.Join(staging, "observation")
	if err := os.Mkdir(observationDirectory, 0o700); err != nil {
		return "", err
	}
	for _, expected := range reportInventory {
		if err := writePrivateFile(filepath.Join(staging, filepath.FromSlash(expected.Path)), files[expected.Path]); err != nil {
			return "", err
		}
	}
	if err := writePrivateFile(filepath.Join(staging, "manifest.json"), manifestBytes); err != nil {
		return "", err
	}
	for _, expected := range reportInventory {
		if err := os.Chmod(filepath.Join(staging, filepath.FromSlash(expected.Path)), 0o400); err != nil {
			return "", err
		}
	}
	if err := os.Chmod(filepath.Join(staging, "manifest.json"), 0o400); err != nil {
		return "", err
	}
	if err := os.Chmod(observationDirectory, 0o500); err != nil {
		return "", err
	}
	if err := os.Chmod(staging, 0o500); err != nil {
		return "", err
	}
	if err := syncDirectory(observationDirectory); err != nil {
		return "", err
	}
	if err := syncDirectory(staging); err != nil {
		return "", err
	}
	if err := runfs.PublishDirectoryNoReplace(staging, output); err != nil {
		return "", fmt.Errorf("security report: publish sealed pack: %w", err)
	}
	rollback = false
	return digest, nil
}

func writePrivateFile(path string, content []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	written, writeErr := file.Write(content)
	if writeErr == nil && written != len(content) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	return errors.Join(writeErr, closeErr)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil && !strings.Contains(strings.ToLower(err.Error()), "invalid argument") && !strings.Contains(strings.ToLower(err.Error()), "not supported") {
		return err
	}
	return nil
}

// Verify reopens and independently reconstructs a sealed report pack.
func Verify(path string) (*VerifiedPack, error) {
	root, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := verifyDirectory(root, 0o500); err != nil {
		return nil, err
	}
	if err := exactEntries(root, []string{"analysis.json", "manifest.json", "observation", "recommendation-catalog.json", "report.json", "report.md", "report.sarif", "standards.json", "target-posture.json"}); err != nil {
		return nil, err
	}
	observationRoot := filepath.Join(root, "observation")
	if err := verifyDirectory(observationRoot, 0o500); err != nil {
		return nil, err
	}
	if err := exactEntries(observationRoot, []string{"backend.json", "behavior.json", "coverage.json", "effects.json", "manifest.json", "openshell.jsonl", "run.json"}); err != nil {
		return nil, err
	}
	files := make(map[string][]byte, len(reportInventory))
	for _, expected := range reportInventory {
		content, err := runfs.ReadPrivateFile(filepath.Join(root, filepath.FromSlash(expected.Path)), 0o400, 72<<20)
		if err != nil {
			return nil, fmt.Errorf("security report: read %s: %w", expected.Path, err)
		}
		files[expected.Path] = content
	}
	manifestBytes, err := runfs.ReadPrivateFile(filepath.Join(root, "manifest.json"), 0o400, 1<<20)
	if err != nil {
		return nil, err
	}
	canonicalManifest, err := artifact.CanonicalizeJSON(manifestBytes)
	if err != nil || !bytes.Equal(canonicalManifest, manifestBytes) {
		return nil, errors.New("security report: manifest is not canonical JSON")
	}
	var document manifest
	if err := decodeExactJSON(manifestBytes, &document); err != nil || document.Schema != ManifestSchema || document.PackSchema != Schema || len(document.Payloads) != len(reportInventory) {
		return nil, errors.New("security report: manifest identity or shape is invalid")
	}
	if err := validatePhaseFourSchema(ManifestSchema, manifestBytes); err != nil {
		return nil, err
	}
	if _, err := time.Parse(time.RFC3339Nano, document.FinalizedAt); err != nil {
		return nil, errors.New("security report: manifest finalization time is invalid")
	}
	for index, expected := range reportInventory {
		descriptor := document.Payloads[index]
		content := files[expected.Path]
		if descriptor.Path != expected.Path || descriptor.MediaType != expected.MediaType || descriptor.Bytes != len(content) || descriptor.SHA256 != artifact.DigestBytes(content).String() {
			return nil, fmt.Errorf("security report: manifest descriptor mismatch for %s", expected.Path)
		}
	}
	descriptorBytes, err := artifact.CanonicalJSON(document.Payloads)
	if err != nil || document.PackDigest != artifact.DigestBytes(descriptorBytes).String() {
		return nil, errors.New("security report: pack digest mismatch")
	}
	pack, err := observation.Read(observationRoot)
	if err != nil {
		return nil, fmt.Errorf("security report: embedded observation: %w", err)
	}
	candidate, standards, issues, err := validateCandidate(pack, files["analysis.json"])
	if err != nil {
		return nil, err
	}
	bundle, bundleFiles, err := securityskill.Load()
	if err != nil || bundle.Digest != candidate.Skill.Digest || !bytes.Equal(standards, bundleFiles["references/standards.json"]) || !bytes.Equal(files["standards.json"], standards) {
		return nil, errors.New("security report: embedded standards or skill identity drifted")
	}
	_, catalogBytes, err := loadCatalog()
	if err != nil || !bytes.Equal(files["recommendation-catalog.json"], catalogBytes) {
		return nil, errors.New("security report: embedded recommendation catalog drifted")
	}
	var posture targetposture.Posture
	canonicalPosture, err := artifact.CanonicalizeJSON(files["target-posture.json"])
	if err != nil || !bytes.Equal(canonicalPosture, files["target-posture.json"]) || decodeExactJSON(files["target-posture.json"], &posture) != nil {
		return nil, errors.New("security report: target posture is invalid")
	}
	packDigest, _ := pack.PackDigest()
	var run struct {
		AgentID        string `json:"agent_id"`
		OrganizationID string `json:"organization_id"`
	}
	if json.Unmarshal(pack.Payloads["run.json"], &run) != nil {
		return nil, errors.New("security report: embedded target identity is invalid")
	}
	prepared := &Prepared{Observation: pack, Candidate: candidate, CandidateBytes: files["analysis.json"], StandardsBytes: standards, PackDigest: packDigest, AgentID: run.AgentID, OrganizationID: run.OrganizationID, Issues: issues}
	if err := validatePosture(&posture, prepared); err != nil {
		return nil, err
	}
	projections, _, err := buildProjections(prepared, &posture)
	if err != nil || !bytes.Equal(projections.JSON, files["report.json"]) || !bytes.Equal(projections.Markdown, files["report.md"]) || !bytes.Equal(projections.SARIF, files["report.sarif"]) {
		return nil, errors.New("security report: projections cannot be reconstructed exactly")
	}
	return &VerifiedPack{Root: root, PackDigest: document.PackDigest, Files: files, Projection: projections}, nil
}

func validatePosture(posture *targetposture.Posture, prepared *Prepared) error {
	if posture == nil || prepared == nil || posture.Schema != targetposture.Schema || posture.ReadContract != targetposture.ReadContract || posture.Catalog.Version != RecommendationVersion || posture.Catalog.Digest != RecommendationDigest {
		return errors.New("security report: target posture identity is invalid")
	}
	if posture.Observation.PackDigest != prepared.PackDigest || posture.Observation.AgentID != prepared.AgentID || posture.Observation.OrganizationID != prepared.OrganizationID || posture.Agent.ID != prepared.AgentID || posture.Agent.OrganizationID != prepared.OrganizationID {
		return errors.New("security report: target posture does not reconcile with the observation")
	}
	if posture.CaptureWindow.Passes != 2 {
		return errors.New("security report: target posture lacks two-pass stability")
	}
	started, startErr := time.Parse(time.RFC3339Nano, posture.CaptureWindow.StartedAt)
	completed, completeErr := time.Parse(time.RFC3339Nano, posture.CaptureWindow.CompletedAt)
	if startErr != nil || completeErr != nil || completed.Before(started) {
		return errors.New("security report: target posture capture window is invalid")
	}
	wantPermissions := append([]string(nil), observation.RequiredPermissions...)
	sort.Strings(wantPermissions)
	if !equalStringSlices(posture.Permissions, wantPermissions) {
		return errors.New("security report: target posture permission authority drifted")
	}
	seams := []targetposture.Seam{posture.Seams.Guardrail, posture.Seams.Policy, posture.Seams.BehaviorRule, posture.Seams.ApprovalRequirement, posture.Seams.SDKIntegration}
	for _, seam := range seams {
		if seam.Status != "observed" || seam.Permission == "" || seam.Route == "" {
			return errors.New("security report: target posture contains an unavailable required read seam")
		}
	}
	if !sortedUniqueGuardrails(posture.Guardrails) || !sortedUniquePolicies(posture.Policies) || !sortedUniqueBehavior(posture.BehaviorRules) {
		return errors.New("security report: target posture controls are duplicate or not sorted")
	}
	if posture.CurrentPolicyID != "" {
		found := false
		for _, policy := range posture.Policies {
			found = found || (policy.ID == posture.CurrentPolicyID && policy.Current)
		}
		if !found {
			return errors.New("security report: target posture current policy is unresolved")
		}
	}
	if posture.GuardrailAggregate == nil || posture.BehaviorRuleAggregate == nil || posture.GuardrailAggregate.Count < 0 || posture.BehaviorRuleAggregate.Count < 0 {
		return errors.New("security report: aggregate control identities are missing")
	}
	return nil
}

func verifyDirectory(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != mode {
		return errors.New("security report: pack directory mode or type is invalid")
	}
	return nil
}

func exactEntries(path string, expected []string) error {
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != len(expected) {
		return errors.New("security report: pack file set is not exact")
	}
	want := append([]string(nil), expected...)
	sort.Strings(want)
	for index, entry := range entries {
		if entry.Name() != want[index] || entry.Type()&os.ModeSymlink != 0 {
			return errors.New("security report: pack contains an unexpected entry")
		}
	}
	return nil
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func sortedUniqueGuardrails(values []targetposture.Guardrail) bool {
	for index, value := range values {
		if value.ID == "" || value.VersionHash == "" || (index > 0 && values[index-1].ID >= value.ID) {
			return false
		}
	}
	return true
}

func sortedUniquePolicies(values []targetposture.Policy) bool {
	for index, value := range values {
		if value.ID == "" || value.VersionHash == "" || (index > 0 && values[index-1].ID >= value.ID) {
			return false
		}
	}
	return true
}

func sortedUniqueBehavior(values []targetposture.BehaviorRule) bool {
	for index, value := range values {
		if value.ID == "" || value.VersionHash == "" || value.BaseRuleID == "" || value.Trigger == "" || value.Verdict == "" || (index > 0 && values[index-1].ID >= value.ID) {
			return false
		}
	}
	return true
}
