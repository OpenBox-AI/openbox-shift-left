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

var payloadOrder = []string{"run.json", "backend.json", "openshell.jsonl", "effects.json", "behavior.json", "coverage.json"}

type Pack struct {
	Payloads map[string][]byte
	Manifest []byte
}

type PackInput struct {
	ExecutionJSON []byte
	OpenShellLog  []byte
	Snapshot      *Snapshot
	Backend       *Result
	Window        Window
	Effects       map[string]any
	FinalizedAt   time.Time
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
	expectedModel := ""
	if model, ok := input.Effects["model_route"].(map[string]any); ok {
		expectedModel, _ = model["model"].(string)
	}
	openshellBytes, openshellRecords, modelRoute, err := canonicalOpenShell(input.OpenShellLog, expectedModel)
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
	modelEffect["status"] = "missing"
	modelEffect["matching_receipts"] = modelRoute.Count
	if modelRoute.Count == 1 {
		modelEffect["status"] = "observed"
		modelEffect["source"] = map[string]any{"file": "openshell.jsonl", "record_ordinal": modelRoute.Ordinal}
		modelEffect["observed_at"] = modelRoute.Timestamp
	}
	effects["model_route"] = modelEffect
	effectsBytes, err := artifact.CanonicalJSON(effects)
	if err != nil {
		return nil, err
	}
	behaviorEntries := behaviorFromInput(input, openshellRecords, effects, modelRoute)
	behaviorBytes, err := artifact.CanonicalJSON(map[string]any{"schema": BehaviorSchema, "entries": behaviorEntries})
	if err != nil {
		return nil, err
	}
	coverageBytes, err := artifact.CanonicalJSON(map[string]any{
		"schema": CoverageSchema,
		"channels": []any{
			map[string]any{"id": "coverage:backend_lifecycle", "name": "backend_lifecycle", "status": "observed", "authority": "backend", "records": len(input.Backend.Events)},
			map[string]any{"id": "coverage:openshell_runtime", "name": "openshell_runtime", "status": statusForCount(len(openshellRecords)), "authority": "openshell", "records": len(openshellRecords)},
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
		"run.json": runBytes, "backend.json": backendBytes, "openshell.jsonl": openshellBytes,
		"effects.json": effectsBytes, "behavior.json": behaviorBytes, "coverage.json": coverageBytes,
	}
	descriptors := make([]map[string]any, 0, len(payloadOrder))
	for _, name := range payloadOrder {
		media := "application/json"
		if name == "openshell.jsonl" {
			media = "application/x-ndjson"
		}
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

type modelRouteObservation struct {
	Count, Ordinal int
	Timestamp      string
}

type openShellRecord struct {
	Ordinal           int    `json:"ordinal"`
	Source            string `json:"source"`
	Channel           string `json:"channel"`
	OriginalTimestamp string `json:"original_timestamp"`
	Timestamp         string `json:"timestamp"`
	Message           string `json:"message"`
	Canonical         []byte `json:"-"`
}

func canonicalOpenShell(content []byte, expectedModel string) ([]byte, []openShellRecord, modelRouteObservation, error) {
	lines := bytes.Split(content, []byte("\n"))
	var output bytes.Buffer
	records := make([]openShellRecord, 0, len(lines))
	var route modelRouteObservation
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		if strings.Contains(string(line), "obx_") || strings.Contains(string(line), "-----BEGIN PRIVATE KEY-----") {
			return nil, nil, route, errors.New("observation: OpenShell log contains credential material")
		}
		ordinal := len(records) + 1
		message := string(line)
		originalTimestamp, timestamp := openShellTimestamp(message)
		channel := "unknown"
		parts := strings.Split(message, "] [")
		if len(parts) > 1 {
			channel = strings.TrimPrefix(parts[1], "[")
		}
		record, err := artifact.CanonicalJSON(map[string]any{"ordinal": ordinal, "source": "openshell", "channel": channel, "original_timestamp": originalTimestamp, "timestamp": timestamp, "message": message})
		if err != nil {
			return nil, nil, route, err
		}
		records = append(records, openShellRecord{Ordinal: ordinal, Source: "openshell", Channel: channel, OriginalTimestamp: originalTimestamp, Timestamp: timestamp, Message: message, Canonical: record})
		output.Write(record)
		output.WriteByte('\n')
		if expectedModel != "" && strings.Contains(message, "API:INFERENCE") && strings.Contains(message, "Success "+expectedModel+" via ") && strings.Contains(message, "POST\\u{20}/v1/chat/completions") {
			route.Count++
			route.Ordinal = ordinal
			route.Timestamp = timestamp
		}
	}
	return output.Bytes(), records, route, nil
}

func behaviorFromInput(input PackInput, openshell []openShellRecord, effects map[string]any, modelRoute modelRouteObservation) []map[string]any {
	entries := make([]map[string]any, 0, len(input.Backend.Events)+len(openshell)+2)
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
	if modelRoute.Count == 1 {
		entries = append(entries, map[string]any{
			"id": "model_route:" + input.Window.EvaluationID, "type": "ModelInvocation", "timestamp": modelRoute.Timestamp, "authority": "model_receipt",
			"correlation": map[string]any{"run_id": input.Window.EvaluationID}, "source": map[string]any{"file": "effects.json", "record": "model_route"},
		})
	}
	for _, record := range openshell {
		entry := map[string]any{
			"id":   fmt.Sprintf("openshell:%d:%s", record.Ordinal, artifact.DigestBytes(record.Canonical).String()),
			"type": "OpenShellObservation", "authority": "openshell",
			"correlation": map[string]any{"run_id": input.Window.EvaluationID},
			"source":      map[string]any{"file": "openshell.jsonl", "record_ordinal": record.Ordinal},
		}
		if record.Timestamp != "" {
			entry["timestamp"] = record.Timestamp
		}
		entries = append(entries, entry)
	}
	authorityOrder := map[string]int{"backend": 0, "independent_receipt": 1, "model_receipt": 2, "openshell": 3}
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
	openshellLines := bytes.Split(bytes.TrimSuffix(pack.Payloads["openshell.jsonl"], []byte("\n")), []byte("\n"))
	for index, line := range openshellLines {
		canonical, err := artifact.CanonicalizeJSON(line)
		var record struct {
			Ordinal int    `json:"ordinal"`
			Source  string `json:"source"`
		}
		if err != nil || !bytes.Equal(canonical, line) || json.Unmarshal(line, &record) != nil || record.Ordinal != index+1 || record.Source != "openshell" {
			return errors.New("observation: OpenShell record reconciliation failed")
		}
	}
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
			Source           struct {
				File          string `json:"file"`
				RecordOrdinal int    `json:"record_ordinal"`
			} `json:"source"`
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
	if effects.Model.Status == "observed" {
		if effects.Model.Source.File != "openshell.jsonl" || effects.Model.Source.RecordOrdinal < 1 || effects.Model.Source.RecordOrdinal > len(openshellLines) || !bytes.Contains(openshellLines[effects.Model.Source.RecordOrdinal-1], []byte("API:INFERENCE")) || !bytes.Contains(openshellLines[effects.Model.Source.RecordOrdinal-1], []byte("Success "+effects.Model.Model+" via ")) {
			return errors.New("observation: model receipt does not resolve to its OpenShell authority")
		}
	}
	var run struct {
		EvaluationID string          `json:"evaluation_id"`
		Backend      BackendIdentity `json:"backend"`
	}
	if json.Unmarshal(pack.Payloads["run.json"], &run) != nil || run.EvaluationID == "" || effects.SafeSink.EvaluationID != run.EvaluationID || run.Backend.URL != ExactBackendURL || run.Backend.APIContract != DashboardActivityContract {
		return errors.New("observation: effect correlation does not match the run")
	}

	openshellRecords := make([]openShellRecord, 0, len(openshellLines))
	for _, line := range openshellLines {
		var record openShellRecord
		if json.Unmarshal(line, &record) != nil {
			return errors.New("observation: invalid OpenShell behavior source")
		}
		record.Canonical = append([]byte(nil), line...)
		openshellRecords = append(openshellRecords, record)
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
	modelRoute := modelRouteObservation{Count: effects.Model.MatchingReceipts, Ordinal: effects.Model.Source.RecordOrdinal}
	if modelRoute.Count == 1 {
		if modelRoute.Ordinal < 1 || modelRoute.Ordinal > len(openshellRecords) {
			return errors.New("observation: model behavior source is invalid")
		}
		modelRoute.Timestamp = openshellRecords[modelRoute.Ordinal-1].Timestamp
	}
	expectedBehavior, canonicalErr := artifact.CanonicalJSON(map[string]any{
		"schema":  BehaviorSchema,
		"entries": behaviorFromInput(PackInput{Backend: &Result{Events: events}, Window: Window{EvaluationID: run.EvaluationID}}, openshellRecords, effectsMap, modelRoute),
	})
	if canonicalErr != nil || !bytes.Equal(expectedBehavior, pack.Payloads["behavior.json"]) {
		return errors.New("observation: behavior index cannot be reconstructed exactly")
	}
	expectedCoverage, canonicalErr := artifact.CanonicalJSON(map[string]any{
		"schema": CoverageSchema,
		"channels": []any{
			map[string]any{"id": "coverage:backend_lifecycle", "name": "backend_lifecycle", "status": "observed", "authority": "backend", "records": len(events)},
			map[string]any{"id": "coverage:openshell_runtime", "name": "openshell_runtime", "status": statusForCount(len(openshellRecords)), "authority": "openshell", "records": len(openshellRecords)},
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
	lines := bytes.Split(bytes.TrimSuffix(pack.Payloads["openshell.jsonl"], []byte("\n")), []byte("\n"))
	for index, line := range lines {
		if err := validateSchema(OpenShellRecordSchema, line); err != nil {
			return fmt.Errorf("observation: OpenShell record %d: %w", index+1, err)
		}
	}
	return nil
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
