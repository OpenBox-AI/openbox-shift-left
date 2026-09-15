package observation

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/artifact"
	"github.com/openbox-ai/openbox-shift-left/cli/internal/assurance/runfs"
)

// The evidence payload is the sandbox's own typed isolation record. It
// replaced openshell.jsonl, which was the gateway's log text wrapped one line
// per JSON object — evidence that had to be parsed back out of prose, and whose
// model-route claim rested on substring matching.
var payloadOrder = []string{"run.json", "backend.json", "sandbox-evidence.json", "effects.json", "behavior.json", "coverage.json"}

type Pack struct {
	Payloads map[string][]byte
	Manifest []byte
}

type PackInput struct {
	ExecutionJSON   []byte
	SandboxEvidence []byte
	Snapshot        *Snapshot
	Backend         *Result
	Window          Window
	Effects         map[string]any
	FinalizedAt     time.Time
}

func Assemble(input PackInput) (*Pack, error) {
	if input.Snapshot == nil || input.Backend == nil || len(input.ExecutionJSON) == 0 || input.FinalizedAt.IsZero() {
		return nil, errors.New("observation: incomplete pack input")
	}
	var execution map[string]any
	if err := json.Unmarshal(input.ExecutionJSON, &execution); err != nil {
		return nil, errors.New("observation: invalid execution record")
	}
	execution["schema"] = RunSchema
	delete(execution, "backend_url")
	execution["backend"] = input.Snapshot.Backend
	execution["organization_id"] = input.Backend.OrganizationID
	execution["selected_session_id"] = input.Backend.Session.ID
	execution["collection_window"] = map[string]any{
		"started_at": input.Window.StartedAt.UTC().Format(time.RFC3339Nano),
		"deadline":   input.Window.Deadline.UTC().Format(time.RFC3339Nano),
	}
	runBytes, err := artifact.CanonicalJSON(execution)
	if err != nil {
		return nil, err
	}
	backendBytes, err := artifact.CanonicalJSON(map[string]any{
		"schema": BackendSchema, "source_contract": DashboardActivityContract, "entries": input.Backend.Entries,
	})
	if err != nil {
		return nil, err
	}
	evidenceBytes, evidenceObserved, err := canonicalSandboxEvidence(input.SandboxEvidence)
	if err != nil {
		return nil, err
	}
	effects := input.Effects
	if effects == nil {
		effects = map[string]any{}
	}
	effects["schema"] = EffectsSchema
	modelEffect, _ := effects["model_route"].(map[string]any)
	if modelEffect == nil {
		modelEffect = map[string]any{}
	}
	// The model route is no longer observable. Its only evidence was a
	// substring in the gateway's log, and the lane no longer reads that log —
	// nor should it, since a log line is not a receipt. It stays `missing`
	// until the model relay issues one of its own, which is honest: missing
	// means nobody looked successfully, never that no call was made.
	modelEffect["status"] = "missing"
	modelEffect["matching_receipts"] = 0
	effects["model_route"] = modelEffect
	effectsBytes, err := artifact.CanonicalJSON(effects)
	if err != nil {
		return nil, err
	}
	behaviorEntries := behaviorFromInput(input, evidenceObserved, effects)
	behaviorBytes, err := artifact.CanonicalJSON(map[string]any{"schema": BehaviorSchema, "entries": behaviorEntries})
	if err != nil {
		return nil, err
	}
	coverageBytes, err := artifact.CanonicalJSON(map[string]any{
		"schema": CoverageSchema,
		"channels": []any{
			map[string]any{"id": "coverage:backend_lifecycle", "name": "backend_lifecycle", "status": "observed", "authority": "backend", "records": len(input.Backend.Events)},
			map[string]any{"id": "coverage:sandbox_isolation", "name": "sandbox_isolation", "status": statusForEvidence(evidenceObserved), "authority": "sandbox", "records": evidenceRecordCount(evidenceObserved)},
			map[string]any{"id": "coverage:safe_sink", "name": "safe_sink", "status": effectStatus(effects, "safe_sink"), "authority": "independent_receipt"},
			map[string]any{"id": "coverage:retrieval_poison", "name": "retrieval_poison", "status": "missing", "authority": "independent_receipt"},
			map[string]any{"id": "coverage:model_route", "name": "model_route", "status": effectStatus(effects, "model_route"), "authority": "model_receipt"},
			map[string]any{"id": "coverage:signed_request_attribution", "name": "signed_request_attribution", "status": "unsupported", "authority": "backend"},
		},
		"truncated": false, "contradictions": []any{},
	})
	if err != nil {
		return nil, err
	}
	payloads := map[string][]byte{
		"run.json": runBytes, "backend.json": backendBytes, "sandbox-evidence.json": evidenceBytes,
		"effects.json": effectsBytes, "behavior.json": behaviorBytes, "coverage.json": coverageBytes,
	}
	descriptors := make([]map[string]any, 0, len(payloadOrder))
	for _, name := range payloadOrder {
		media := "application/json"
		descriptors = append(descriptors, map[string]any{"path": name, "media_type": media, "bytes": len(payloads[name]), "sha256": artifact.DigestBytes(payloads[name]).String()})
	}
	descriptorBytes, err := artifact.CanonicalJSON(descriptors)
	if err != nil {
		return nil, err
	}
	manifest, err := artifact.CanonicalJSON(map[string]any{
		"schema": ManifestSchema, "pack_schema": Schema, "payloads": descriptors,
		"pack_digest":  artifact.DigestBytes(descriptorBytes).String(),
		"finalized_at": input.FinalizedAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return nil, err
	}
	pack := &Pack{Payloads: payloads, Manifest: manifest}
	if err := Validate(pack); err != nil {
		return nil, err
	}
	return pack, nil
}

// canonicalSandboxEvidence canonicalizes the provider's isolation record.
//
// Absent evidence is represented as an explicit empty object rather than an
// empty file, because the pack must distinguish "the provider recorded
// nothing" from "this payload was never written". The first is a real
// observation; the second is a broken pack.
func canonicalSandboxEvidence(content []byte) ([]byte, bool, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		canonical, err := artifact.CanonicalJSON(map[string]any{"schema": SandboxEvidenceSchema, "observed": false})
		return canonical, false, err
	}
	if bytes.Contains(content, []byte("obx_")) || bytes.Contains(content, []byte("-----BEGIN PRIVATE KEY-----")) {
		return nil, false, errors.New("observation: sandbox evidence contains credential material")
	}
	var decoded any
	if err := json.Unmarshal(content, &decoded); err != nil {
		return nil, false, errors.New("observation: sandbox evidence is not valid JSON")
	}
	canonical, err := artifact.CanonicalJSON(map[string]any{
		"schema": SandboxEvidenceSchema, "observed": true, "evidence": decoded,
	})
	return canonical, true, err
}

func statusForEvidence(observed bool) string {
	if observed {
		return "observed"
	}
	return "missing"
}

func evidenceRecordCount(observed bool) int {
	if observed {
		return 1
	}
	return 0
}

func behaviorFromInput(input PackInput, evidenceObserved bool, effects map[string]any) []map[string]any {
	entries := make([]map[string]any, 0, len(input.Backend.Events)+3)
	for _, event := range input.Backend.Events {
		entries = append(entries, map[string]any{
			"id": event.ID, "type": event.Type,
			"timestamp": event.CreatedAt.UTC().Format(time.RFC3339Nano), "authority": "backend",
			"correlation": map[string]any{"agent_id": event.AgentID, "session_id": event.SessionID, "run_id": event.RunID},
			"source":      map[string]any{"file": "backend.json", "response_ordinal": event.SourceOrdinal, "record_ordinal": event.SourceRecord},
		})
	}
	if safe, ok := effects["safe_sink"].(map[string]any); ok && effectStatus(effects, "safe_sink") == "observed" {
		entries = append(entries, map[string]any{
			"id": "effect:safe_sink:" + input.Window.EvaluationID, "type": "SafeEffect", "timestamp": safe["matched_at"], "authority": "independent_receipt",
			"correlation": map[string]any{"run_id": input.Window.EvaluationID}, "source": map[string]any{"file": "effects.json", "record": "safe_sink"},
		})
	}
	// One entry, not one per log line: the sandbox reports its isolation
	// decisions as a single typed record, so the behavior index cites that
	// record rather than a line number inside prose.
	if evidenceObserved {
		entries = append(entries, map[string]any{
			"id": "sandbox_evidence:" + input.Window.EvaluationID, "type": "SandboxIsolation", "authority": "sandbox",
			"correlation": map[string]any{"run_id": input.Window.EvaluationID},
			"source":      map[string]any{"file": "sandbox-evidence.json", "record": "evidence"},
		})
	}
	authorityOrder := map[string]int{"backend": 0, "independent_receipt": 1, "model_receipt": 2, "sandbox": 3}
	sort.SliceStable(entries, func(i, j int) bool {
		leftTime, _ := entries[i]["timestamp"].(string)
		rightTime, _ := entries[j]["timestamp"].(string)
		if leftTime == "" {
			leftTime = "~"
		}
		if rightTime == "" {
			rightTime = "~"
		}
		if leftTime != rightTime {
			return leftTime < rightTime
		}
		leftAuthority, _ := entries[i]["authority"].(string)
		rightAuthority, _ := entries[j]["authority"].(string)
		if authorityOrder[leftAuthority] != authorityOrder[rightAuthority] {
			return authorityOrder[leftAuthority] < authorityOrder[rightAuthority]
		}
		return entries[i]["id"].(string) < entries[j]["id"].(string)
	})
	return entries
}

func openShellTimestamp(message string) (string, string) {
	if !strings.HasPrefix(message, "[") {
		return "", ""
	}
	end := strings.IndexByte(message, ']')
	if end < 2 {
		return "", ""
	}
	raw := message[1:end]
	wholeText, fractionText, _ := strings.Cut(raw, ".")
	seconds, err := strconv.ParseInt(wholeText, 10, 64)
	if err != nil || len(fractionText) > 9 {
		return raw, ""
	}
	fractionText += strings.Repeat("0", 9-len(fractionText))
	nanoseconds, err := strconv.ParseInt(fractionText, 10, 64)
	if err != nil {
		return raw, ""
	}
	return raw, time.Unix(seconds, nanoseconds).UTC().Format(time.RFC3339Nano)
}

func Validate(pack *Pack) error {
	if pack == nil || len(pack.Payloads) != len(payloadOrder) {
		return errors.New("observation: invalid payload set")
	}
	for _, name := range payloadOrder {
		content, ok := pack.Payloads[name]
		if !ok {
			return fmt.Errorf("observation: missing %s", name)
		}
		if name == "openshell.jsonl" {
			continue
		}
		canonical, err := artifact.CanonicalizeJSON(content)
		if err != nil || !bytes.Equal(canonical, content) {
			return fmt.Errorf("observation: %s is not canonical", name)
		}
	}
	if err := validatePackSchemas(pack); err != nil {
		return err
	}
	var backend struct {
		SourceContract string  `json:"source_contract"`
		Entries        []Entry `json:"entries"`
	}
	if json.Unmarshal(pack.Payloads["backend.json"], &backend) != nil || backend.SourceContract != DashboardActivityContract {
		return errors.New("observation: invalid backend payload")
	}
	for index, entry := range backend.Entries {
		body, err := base64.StdEncoding.DecodeString(entry.BodyBase64)
		activity := strings.Contains(entry.Path, "/sessions")
		validRepresentation := entry.Representation == "backend_response" || entry.Representation == "dashboard_public_projection"
		if err != nil || entry.Ordinal != index+1 || entry.Method != "GET" || !validRepresentation || activity != (entry.Representation == "dashboard_public_projection") || len(body) != entry.BodyBytes || artifact.DigestBytes(body).String() != entry.SHA256 || !json.Valid(body) || rejectCredentialMaterial(body) != nil {
			return errors.New("observation: backend evidence reconciliation failed")
		}
	}
	// The evidence payload is one canonical document, verified as such. There
	// are no per-line records to reconcile any more, which removes the only
	// place this validator parsed provider prose.
	evidenceCanonical, evidenceErr := artifact.CanonicalizeJSON(pack.Payloads["sandbox-evidence.json"])
	if evidenceErr != nil || !bytes.Equal(evidenceCanonical, pack.Payloads["sandbox-evidence.json"]) {
		return errors.New("observation: sandbox evidence is not canonical")
	}
	var evidenceDocument struct {
		Schema   string `json:"schema"`
		Observed bool   `json:"observed"`
	}
	if json.Unmarshal(pack.Payloads["sandbox-evidence.json"], &evidenceDocument) != nil || evidenceDocument.Schema != SandboxEvidenceSchema {
		return errors.New("observation: sandbox evidence reconciliation failed")
	}
	evidenceObserved := evidenceDocument.Observed

	var effects struct {
		SafeSink struct {
			Status           string `json:"status"`
			EvaluationID     string `json:"evaluation_id"`
			Attempts         int    `json:"attempts"`
			MatchingReceipts int    `json:"matching_receipts"`
			MatchedAt        string `json:"matched_at"`
		} `json:"safe_sink"`
		Retrieval struct {
			Status           string `json:"status"`
			MatchingReceipts int    `json:"matching_receipts"`
		} `json:"retrieval_poison"`
		Model struct {
			Status           string `json:"status"`
			Model            string `json:"model"`
			MatchingReceipts int    `json:"matching_receipts"`
		} `json:"model_route"`
		Core struct {
			Status              string `json:"status"`
			MatchingValidations int    `json:"matching_validations"`
			GovernanceEvents    int    `json:"governance_events"`
		} `json:"core_relay"`
	}
	if json.Unmarshal(pack.Payloads["effects.json"], &effects) != nil ||
		(effects.SafeSink.Status == "observed") != (effects.SafeSink.MatchingReceipts > 0) || effects.SafeSink.MatchingReceipts > effects.SafeSink.Attempts ||
		(effects.Retrieval.Status == "observed") != (effects.Retrieval.MatchingReceipts > 0) ||
		(effects.Model.Status == "observed") != (effects.Model.MatchingReceipts == 1) ||
		(effects.Core.Status == "observed") != (effects.Core.MatchingValidations > 0 && effects.Core.GovernanceEvents > 0) {
		return errors.New("observation: contradictory effect receipts")
	}
	// A model-route observation would now need a receipt from the model relay.
	// Until that exists the status is `missing`, and claiming `observed`
	// without a citable receipt is refused rather than trusted.
	if effects.Model.Status == "observed" {
		return errors.New("observation: model receipt has no authority to resolve against")
	}
	var run struct {
		EvaluationID string          `json:"evaluation_id"`
		Backend      BackendIdentity `json:"backend"`
	}
	if json.Unmarshal(pack.Payloads["run.json"], &run) != nil || run.EvaluationID == "" || effects.SafeSink.EvaluationID != run.EvaluationID || run.Backend.URL != ExactBackendURL || run.Backend.APIContract != DashboardActivityContract {
		return errors.New("observation: effect correlation does not match the run")
	}

	latestEvents := map[string]Event{}
	for _, entry := range backend.Entries {
		if !strings.Contains(entry.Path, "/logs/chronological?") {
			continue
		}
		body, _ := base64.StdEncoding.DecodeString(entry.BodyBase64)
		data, decodeErr := decodeEnvelope(body)
		if decodeErr != nil {
			return errors.New("observation: chronological behavior source is invalid")
		}
		items, _, _, _, pageErr := decodePage(data, pageFromPath(entry.Path), []string{"merkle_root", "event_count", "attestation"})
		if pageErr != nil {
			return errors.New("observation: chronological behavior page is invalid")
		}
		for recordOrdinal, raw := range items {
			event, eventErr := decodeEvent(raw)
			if eventErr != nil {
				return errors.New("observation: chronological behavior record is invalid")
			}
			event.SourceOrdinal = entry.Ordinal
			event.SourceRecord = recordOrdinal
			latestEvents[event.ID] = event
		}
	}
	events := make([]Event, 0, len(latestEvents))
	for _, event := range latestEvents {
		events = append(events, event)
	}
	var effectsMap map[string]any
	if json.Unmarshal(pack.Payloads["effects.json"], &effectsMap) != nil {
		return errors.New("observation: invalid effect behavior source")
	}
	expectedBehavior, canonicalErr := artifact.CanonicalJSON(map[string]any{
		"schema":  BehaviorSchema,
		"entries": behaviorFromInput(PackInput{Backend: &Result{Events: events}, Window: Window{EvaluationID: run.EvaluationID}}, evidenceObserved, effectsMap),
	})
	if canonicalErr != nil || !bytes.Equal(expectedBehavior, pack.Payloads["behavior.json"]) {
		return errors.New("observation: behavior index cannot be reconstructed exactly")
	}
	expectedCoverage, canonicalErr := artifact.CanonicalJSON(map[string]any{
		"schema": CoverageSchema,
		"channels": []any{
			map[string]any{"id": "coverage:backend_lifecycle", "name": "backend_lifecycle", "status": "observed", "authority": "backend", "records": len(events)},
			map[string]any{"id": "coverage:sandbox_isolation", "name": "sandbox_isolation", "status": statusForEvidence(evidenceObserved), "authority": "sandbox", "records": evidenceRecordCount(evidenceObserved)},
			map[string]any{"id": "coverage:safe_sink", "name": "safe_sink", "status": effects.SafeSink.Status, "authority": "independent_receipt"},
			map[string]any{"id": "coverage:retrieval_poison", "name": "retrieval_poison", "status": effects.Retrieval.Status, "authority": "independent_receipt"},
			map[string]any{"id": "coverage:model_route", "name": "model_route", "status": effects.Model.Status, "authority": "model_receipt"},
			map[string]any{"id": "coverage:signed_request_attribution", "name": "signed_request_attribution", "status": "unsupported", "authority": "backend"},
		},
		"truncated": false, "contradictions": []any{},
	})
	if canonicalErr != nil || !bytes.Equal(expectedCoverage, pack.Payloads["coverage.json"]) {
		return errors.New("observation: coverage index cannot be reconstructed exactly")
	}
	var manifest struct {
		Schema     string `json:"schema"`
		PackSchema string `json:"pack_schema"`
		Payloads   []struct {
			Path      string `json:"path"`
			MediaType string `json:"media_type"`
			SHA256    string `json:"sha256"`
			Bytes     int    `json:"bytes"`
		} `json:"payloads"`
		PackDigest string `json:"pack_digest"`
	}
	if json.Unmarshal(pack.Manifest, &manifest) != nil || manifest.Schema != ManifestSchema || manifest.PackSchema != Schema || len(manifest.Payloads) != len(payloadOrder) {
		return errors.New("observation: invalid manifest")
	}
	descriptors := make([]map[string]any, 0, len(payloadOrder))
	for index, descriptor := range manifest.Payloads {
		content := pack.Payloads[payloadOrder[index]]
		wantMedia := "application/json"
		if descriptor.Path == "openshell.jsonl" {
			wantMedia = "application/x-ndjson"
		}
		if descriptor.Path != payloadOrder[index] || descriptor.MediaType != wantMedia || descriptor.Bytes != len(content) || descriptor.SHA256 != artifact.DigestBytes(content).String() {
			return errors.New("observation: manifest payload mismatch")
		}
		descriptors = append(descriptors, map[string]any{"path": descriptor.Path, "media_type": descriptor.MediaType, "bytes": descriptor.Bytes, "sha256": descriptor.SHA256})
	}
	descriptorBytes, err := artifact.CanonicalJSON(descriptors)
	if err != nil || manifest.PackDigest != artifact.DigestBytes(descriptorBytes).String() {
		return errors.New("observation: pack digest mismatch")
	}
	canonicalManifest, err := artifact.CanonicalizeJSON(pack.Manifest)
	if err != nil || !bytes.Equal(canonicalManifest, pack.Manifest) {
		return errors.New("observation: manifest is not canonical")
	}
	return nil
}

func validatePackSchemas(pack *Pack) error {
	for _, document := range []struct {
		identifier string
		content    []byte
	}{
		{identifier: ManifestSchema, content: pack.Manifest},
		{identifier: RunSchema, content: pack.Payloads["run.json"]},
		{identifier: BackendSchema, content: pack.Payloads["backend.json"]},
		{identifier: EffectsSchema, content: pack.Payloads["effects.json"]},
		{identifier: BehaviorSchema, content: pack.Payloads["behavior.json"]},
		{identifier: CoverageSchema, content: pack.Payloads["coverage.json"]},
	} {
		if err := validateSchema(document.identifier, document.content); err != nil {
			return err
		}
	}
	return validateSchema(SandboxEvidenceSchema, pack.Payloads["sandbox-evidence.json"])
}

// PackDigest returns the manifest-declared digest after successful validation.
func (pack *Pack) PackDigest() (string, error) {
	if pack == nil {
		return "", errors.New("observation: nil pack")
	}
	var manifest struct {
		PackDigest string `json:"pack_digest"`
	}
	if err := json.Unmarshal(pack.Manifest, &manifest); err != nil || manifest.PackDigest == "" {
		return "", errors.New("observation: invalid manifest pack digest")
	}
	return manifest.PackDigest, nil
}

// Read opens an immutable exact-file observation transaction and performs
// semantic reconciliation before returning it.
func Read(path string) (*Pack, error) {
	payloads, manifest, err := runfs.ReadObservation(path)
	if err != nil {
		return nil, err
	}
	pack := &Pack{Payloads: payloads, Manifest: manifest}
	if err := Validate(pack); err != nil {
		return nil, err
	}
	return pack, nil
}

func statusForCount(count int) string {
	if count > 0 {
		return "observed"
	}
	return "missing"
}
func effectStatus(effects map[string]any, name string) string {
	value, ok := effects[name].(map[string]any)
	if ok {
		switch count := value["matching_receipts"].(type) {
		case int:
			if count > 0 {
				return "observed"
			}
		case float64:
			if count > 0 {
				return "observed"
			}
		case json.Number:
			if parsed, err := count.Int64(); err == nil && parsed > 0 {
				return "observed"
			}
		}
	}
	return "missing"
}

func pageFromPath(path string) int {
	marker := "page="
	index := strings.Index(path, marker)
	if index < 0 {
		return -1
	}
	value := path[index+len(marker):]
	if end := strings.IndexByte(value, '&'); end >= 0 {
		value = value[:end]
	}
	page, err := strconv.Atoi(value)
	if err != nil {
		return -1
	}
	return page
}
