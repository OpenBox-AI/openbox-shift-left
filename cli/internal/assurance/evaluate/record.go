package evaluate

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
)

type phaseEntry struct {
	Phase string    `json:"phase"`
	At    time.Time `json:"at"`
}

type logRecord struct {
	SHA256    string `json:"sha256"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated"`
}

type cleanupRecord struct {
	SandboxDeleteAttempted   bool `json:"sandbox_delete_attempted"`
	SandboxAbsent            bool `json:"sandbox_absent"`
	RegistryTagRemoved       bool `json:"registry_tag_removed"`
	RegistryContainerRemoved bool `json:"registry_container_removed"`
	RegistryContainerAbsent  bool `json:"registry_container_absent"`
	RegistryVolumeRemoved    bool `json:"registry_volume_removed"`
	RegistryVolumeAbsent     bool `json:"registry_volume_absent"`
	OllamaModelUnloaded      bool `json:"ollama_model_unloaded"`
}

type executionRecord struct {
	Schema       string    `json:"schema"`
	EvaluationID string    `json:"evaluation_id"`
	AgentID      string    `json:"agent_id"`
	StartedAt    time.Time `json:"started_at"`
	CompletedAt  time.Time `json:"completed_at"`
	DurationMS   int64     `json:"duration_ms"`
	Image        struct {
		Requested          string `json:"requested"`
		LocalID            string `json:"local_id"`
		Platform           string `json:"platform"`
		ManifestDigest     string `json:"manifest_digest,omitempty"`
		PublishedReference string `json:"published_reference,omitempty"`
		ImmutableReference string `json:"immutable_reference,omitempty"`
		WorkingDir         string `json:"working_dir"`
	} `json:"image"`
	Argv             []string `json:"argv"`
	EnvironmentNames []string `json:"environment_names"`
	OpenShell        struct {
		CLIVersion     string `json:"cli_version"`
		GatewayVersion string `json:"gateway_version"`
		DriverVersion  string `json:"driver_version"`
		Provider       string `json:"provider"`
	} `json:"openshell"`
	Inference struct {
		Provider    string `json:"provider"`
		Model       string `json:"model"`
		ModelDigest string `json:"model_digest"`
	} `json:"inference"`
	Phases []phaseEntry `json:"phases"`
	Core   struct {
		ValidationAttempts   int    `json:"validation_attempts"`
		ValidationSuccesses  int    `json:"validation_successes"`
		MatchingValidations  int    `json:"matching_agent_validations"`
		GovernanceEvents     int    `json:"evaluation_governance_events"`
		LastValidationStatus int    `json:"last_validation_status,omitempty"`
		AuthorizationClass   string `json:"authorization_class,omitempty"`
	} `json:"core"`
	Effects struct {
		SafeSinkAttempts int `json:"safe_sink_attempts"`
		SafeSinkMatching int `json:"safe_sink_matching"`
	} `json:"effects"`
	CommandExitCode    *int   `json:"command_exit_code,omitempty"`
	ExitClassification string `json:"exit_classification"`
	Error              string `json:"error,omitempty"`
	Logs               struct {
		ProcessStdout logRecord `json:"process_stdout"`
		ProcessStderr logRecord `json:"process_stderr"`
		OpenShell     logRecord `json:"openshell"`
	} `json:"logs"`
	CoverageLimitations []string      `json:"coverage_limitations"`
	Cleanup             cleanupRecord `json:"cleanup"`
}

func digestRecord(content []byte, truncated bool) logRecord {
	digest := sha256.Sum256(content)
	return logRecord{SHA256: "sha256:" + hex.EncodeToString(digest[:]), Bytes: len(content), Truncated: truncated}
}
