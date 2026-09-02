package snapshot

import (
	"errors"
	"path"
	"runtime"
	"sort"
	"strings"
)

// Rule is one normalized source-selection rule projected into the public
// project-model contract.
type Rule struct {
	ID      string `json:"id"`
	Source  string `json:"source"`
	Action  string `json:"action"`
	Pattern string `json:"pattern"`
}

// PathClass is the closed omission vocabulary of openbox.project-model/v1.
type PathClass string

const (
	PathClassVCS             PathClass = "vcs"
	PathClassAuditOutput     PathClass = "audit_output"
	PathClassCache           PathClass = "cache"
	PathClassSecret          PathClass = "secret"
	PathClassSocket          PathClass = "socket"
	PathClassFIFO            PathClass = "fifo"
	PathClassDevice          PathClass = "device"
	PathClassExternalSymlink PathClass = "external_symlink"
	PathClassIgnored         PathClass = "ignored"
)

// Omission records only paths and counts, never omitted file contents.
type Omission struct {
	PathClass         PathClass `json:"pathClass"`
	RuleID            string    `json:"ruleId"`
	Count             int64     `json:"count"`
	Examples          []string  `json:"examples"`
	ExamplesTruncated bool      `json:"examplesTruncated"`
}

type boundaryExclusion struct {
	class  PathClass
	ruleID string
}

type selectedSource struct {
	entries   []Entry
	rules     []Rule
	omissions []Omission
}

type exclusionMatch struct {
	class  PathClass
	ruleID string
}

type omissionObservation struct {
	path  string
	match exclusionMatch
}

var builtInRules = []Rule{
	{ID: "builtin-vcs", Source: "built_in", Action: "exclude", Pattern: "**/{.git,.hg,.svn}/**"},
	{ID: "builtin-audit-output", Source: "built_in", Action: "exclude", Pattern: "**/.openbox/audit/**"},
	{ID: "builtin-inspection-output", Source: "built_in", Action: "exclude", Pattern: "**/.openbox/inspect/**"},
	{ID: "builtin-cache", Source: "built_in", Action: "exclude", Pattern: "**/{node_modules,.cache,__pycache__,.pytest_cache,.mypy_cache,.ruff_cache,.tox,.venv,venv,.next,.turbo,coverage}/**"},
	{ID: "builtin-secret", Source: "built_in", Action: "exclude", Pattern: "recognized-secret-path/v1"},
	{ID: "builtin-socket", Source: "built_in", Action: "exclude", Pattern: "kind=socket"},
	{ID: "builtin-fifo", Source: "built_in", Action: "exclude", Pattern: "kind=fifo"},
	{ID: "builtin-device", Source: "built_in", Action: "exclude", Pattern: "kind=device-or-unsupported-special"},
	{ID: "builtin-external-symlink", Source: "built_in", Action: "exclude", Pattern: "kind=external-or-broken-symlink"},
	{ID: "builtin-external-mount", Source: "built_in", Action: "exclude", Pattern: "kind=external_mount"},
}

func (project *Project) selectDefault() (selectedSource, error) {
	return project.selection(false)
}

func (project *Project) selection(dependencies bool) (selectedSource, error) {
	entries, observations, err := selectEntriesWithPolicy(project, dependencies)
	if err != nil {
		return selectedSource{}, err
	}
	rules := append([]Rule(nil), builtInRules...)
	if dependencies {
		rules = append(rules, Rule{ID: "trusted-testbed-dependencies", Source: "profile_include", Action: "include", Pattern: "**/node_modules/**"})
	}
	boundaryPaths := make([]string, 0, len(project.excluded))
	for relative := range project.excluded {
		boundaryPaths = append(boundaryPaths, relative)
	}
	sort.Strings(boundaryPaths)
	for _, relative := range boundaryPaths {
		boundary := project.excluded[relative]
		rules = append(rules, Rule{ID: boundary.ruleID, Source: "built_in", Action: "exclude", Pattern: relative})
	}
	omissions, err := normalizeOmissions(observations)
	if err != nil {
		return selectedSource{}, err
	}
	return selectedSource{entries: entries, rules: rules, omissions: omissions}, nil
}

func (project *Project) matchPath(relative string) (exclusionMatch, bool) {
	return project.matchPathWithDependencies(relative, false)
}

func (project *Project) matchPathWithDependencies(relative string, dependencies bool) (exclusionMatch, bool) {
	if match, ok := project.matchBoundary(relative); ok {
		return match, true
	}
	base := path.Base(relative)
	if defaultAuditPath(relative) {
		return exclusionMatch{class: PathClassAuditOutput, ruleID: "builtin-audit-output"}, true
	}
	if defaultInspectionPath(relative) {
		return exclusionMatch{class: PathClassAuditOutput, ruleID: "builtin-inspection-output"}, true
	}
	if recognizedSecretPath(relative) {
		return exclusionMatch{class: PathClassSecret, ruleID: "builtin-secret"}, true
	}
	return matchDefaultBase(base, dependencies)
}

func (project *Project) matchBoundary(relative string) (exclusionMatch, bool) {
	for boundaryPath, boundary := range project.excluded {
		if selectionPathEqual(relative, boundaryPath) || selectionPathHasPrefix(relative, boundaryPath+"/") {
			return exclusionMatch{class: boundary.class, ruleID: boundary.ruleID}, true
		}
	}
	return exclusionMatch{}, false
}

func matchDefaultBase(base string, dependencies bool) (exclusionMatch, bool) {
	switch strings.ToLower(base) {
	case ".git", ".hg", ".svn":
		return exclusionMatch{class: PathClassVCS, ruleID: "builtin-vcs"}, true
	case "node_modules":
		if dependencies {
			return exclusionMatch{}, false
		}
		return exclusionMatch{class: PathClassCache, ruleID: "builtin-cache"}, true
	case ".cache", "__pycache__", ".pytest_cache", ".mypy_cache", ".ruff_cache", ".tox", ".venv", "venv", ".next", ".turbo", "coverage":
		return exclusionMatch{class: PathClassCache, ruleID: "builtin-cache"}, true
	}
	if recognizedSecretBase(base) {
		return exclusionMatch{class: PathClassSecret, ruleID: "builtin-secret"}, true
	}
	return exclusionMatch{}, false
}

func defaultAuditPath(relative string) bool {
	components := strings.Split(strings.ToLower(relative), "/")
	return len(components) >= 2 && components[len(components)-2] == ".openbox" && components[len(components)-1] == "audit"
}

func defaultInspectionPath(relative string) bool {
	components := strings.Split(strings.ToLower(relative), "/")
	return len(components) >= 2 && components[len(components)-2] == ".openbox" && components[len(components)-1] == "inspect"
}

func selectionPathEqual(left, right string) bool {
	if runtime.GOOS == "darwin" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func selectionPathHasPrefix(value, prefix string) bool {
	if runtime.GOOS == "darwin" {
		return strings.HasPrefix(strings.ToLower(value), strings.ToLower(prefix))
	}
	return strings.HasPrefix(value, prefix)
}

func matchEntry(entry Entry) (exclusionMatch, bool) {
	switch entry.Kind {
	case KindExternalSymlink, KindBrokenSymlink:
		return exclusionMatch{class: PathClassExternalSymlink, ruleID: "builtin-external-symlink"}, true
	case KindSocket:
		return exclusionMatch{class: PathClassSocket, ruleID: "builtin-socket"}, true
	case KindFIFO:
		return exclusionMatch{class: PathClassFIFO, ruleID: "builtin-fifo"}, true
	case KindDevice, KindOther:
		return exclusionMatch{class: PathClassDevice, ruleID: "builtin-device"}, true
	case KindExternalMount:
		return exclusionMatch{class: PathClassIgnored, ruleID: "builtin-external-mount"}, true
	default:
		return exclusionMatch{}, false
	}
}

func recognizedSecretBase(base string) bool {
	lower := strings.ToLower(base)
	if lower == ".env" || strings.HasPrefix(lower, ".env.") {
		return true
	}
	switch lower {
	case ".npmrc", ".pypirc", ".netrc", "credentials.json", "secrets.json", "secrets.yaml", "secrets.yml", "service-account.json", "service-account-key.json", "service_account.json", "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519":
		return true
	}
	for _, suffix := range []string{".pem", ".key", ".p12", ".pfx", ".jks"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

func recognizedSecretPath(relative string) bool {
	lower := strings.ToLower(relative)
	for _, suffix := range []string{
		".aws/credentials",
		".docker/config.json",
		".kube/config",
		".config/gcloud/application_default_credentials.json",
		".config/gh/hosts.yml",
	} {
		if lower == suffix || strings.HasSuffix(lower, "/"+suffix) {
			return true
		}
	}
	return false
}

func normalizeOmissions(observations []omissionObservation) ([]Omission, error) {
	type key struct {
		class  PathClass
		ruleID string
	}
	grouped := make(map[key]*Omission)
	order := make([]key, 0)
	for _, observation := range observations {
		itemKey := key{class: observation.match.class, ruleID: observation.match.ruleID}
		item, ok := grouped[itemKey]
		if !ok {
			item = &Omission{
				PathClass: observation.match.class,
				RuleID:    observation.match.ruleID,
				Examples:  make([]string, 0, 16),
			}
			grouped[itemKey] = item
			order = append(order, itemKey)
		}
		if item.Count == maxContractInteger {
			return nil, errors.New("snapshot: omission count exceeds the v1 integer bound")
		}
		item.Count++
		if observation.match.class == PathClassSecret {
			item.ExamplesTruncated = true
			continue
		}
		if len(item.Examples) < 16 {
			item.Examples = append(item.Examples, observation.path)
		} else {
			item.ExamplesTruncated = true
		}
	}
	sort.Slice(order, func(left, right int) bool {
		if order[left].class != order[right].class {
			return order[left].class < order[right].class
		}
		return order[left].ruleID < order[right].ruleID
	})
	omissions := make([]Omission, 0, len(order))
	for _, itemKey := range order {
		omissions = append(omissions, *grouped[itemKey])
	}
	return omissions, nil
}
