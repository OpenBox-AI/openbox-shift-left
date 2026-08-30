// Package aivss holds the default aivss risk posture the CLI supplies when
// registering a developer agent via POST /agent/create.
//
//	final aivss_score. A high aivss_score is the safe end.
//	decision_criticality, adaptability) descend (1 = highest risk) while
//	model_robustness ascends (5 = highest risk); it is the odd one out.
//	The values below were chosen with that per-field direction in mind.
package aivss

// Base group is one aivss parameter group.
type Base struct {
	AttackVector       int `json:"attack_vector"`
	AttackComplexity   int `json:"attack_complexity"`
	PrivilegesRequired int `json:"privileges_required"`
	UserInteraction    int `json:"user_interaction"`
	Scope              int `json:"scope"`
}

type AISpecific struct {
	ModelRobustness     int `json:"model_robustness"`
	DataSensitivity     int `json:"data_sensitivity"`
	EthicalImpact       int `json:"ethical_impact"`
	DecisionCriticality int `json:"decision_criticality"`
	Adaptability        int `json:"adaptability"`
}

type Impact struct {
	ConfidentialityImpact int `json:"confidentiality_impact"`
	IntegrityImpact       int `json:"integrity_impact"`
	AvailabilityImpact    int `json:"availability_impact"`
	SafetyImpact          int `json:"safety_impact"`
}

// Config is the full aivss_config payload (all fields required by the
// backend).
type Config struct {
	BaseSecurity Base       `json:"base_security"`
	AISpecific   AISpecific `json:"ai_specific"`
	Impact       Impact     `json:"impact"`
}

// DefaultDeveloperProfile is the accepted risk posture for a developer coding
// agent: capable (shell/file/MCP, code that ships) but human-supervised and
// observe-only by default.
func DefaultDeveloperProfile() Config {
	return Config{
		BaseSecurity: Base{
			AttackVector:       2,
			AttackComplexity:   2,
			PrivilegesRequired: 2,
			UserInteraction:    2,
			Scope:              2,
		},
		AISpecific: AISpecific{
			ModelRobustness:     2,
			DataSensitivity:     4,
			EthicalImpact:       2,
			DecisionCriticality: 3,
			Adaptability:        4,
		},
		Impact: Impact{
			ConfidentialityImpact: 3,
			IntegrityImpact:       3,
			AvailabilityImpact:    2,
			SafetyImpact:          1,
		},
	}
}

type bound struct {
	min, max int
}

// Validate re-checks every integer against the backend's @Min/@Max bounds so a
// bad edit fails locally (with a clear field name) instead of as an opaque 400
// from agent/create.
func (c Config) Validate() (field string, ok bool) {
	checks := []struct {
		name string
		val  int
		b    bound
	}{
		{"base_security.attack_vector", c.BaseSecurity.AttackVector, bound{1, 4}},
		{"base_security.attack_complexity", c.BaseSecurity.AttackComplexity, bound{1, 2}},
		{"base_security.privileges_required", c.BaseSecurity.PrivilegesRequired, bound{1, 3}},
		{"base_security.user_interaction", c.BaseSecurity.UserInteraction, bound{1, 2}},
		{"base_security.scope", c.BaseSecurity.Scope, bound{1, 2}},
		{"ai_specific.model_robustness", c.AISpecific.ModelRobustness, bound{1, 5}},
		{"ai_specific.data_sensitivity", c.AISpecific.DataSensitivity, bound{1, 5}},
		{"ai_specific.ethical_impact", c.AISpecific.EthicalImpact, bound{1, 5}},
		{"ai_specific.decision_criticality", c.AISpecific.DecisionCriticality, bound{1, 5}},
		{"ai_specific.adaptability", c.AISpecific.Adaptability, bound{1, 5}},
		{"impact.confidentiality_impact", c.Impact.ConfidentialityImpact, bound{1, 5}},
		{"impact.integrity_impact", c.Impact.IntegrityImpact, bound{1, 5}},
		{"impact.availability_impact", c.Impact.AvailabilityImpact, bound{1, 5}},
		{"impact.safety_impact", c.Impact.SafetyImpact, bound{1, 5}},
	}
	for _, c := range checks {
		if c.val < c.b.min || c.val > c.b.max {
			return c.name, false
		}
	}
	return "", true
}
