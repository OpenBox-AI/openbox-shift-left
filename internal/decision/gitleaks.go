package decision

import (
	"sort"
	"strings"
	"sync"

	"github.com/zricethezav/gitleaks/v8/detect"
	"github.com/zricethezav/gitleaks/v8/report"
)

// What it does NOT replace, deliberately:
//   - The generic `secret_assignment` value-group pattern, because gitleaks
//     reports

var gitleaksDetector = sync.OnceValue(func() *detect.Detector {
	d, err := detect.NewDetectorDefaultConfig()
	if err != nil {
		return nil
	}
	d.Redact = 0
	d.MaxDecodeDepth = 0
	d.MaxArchiveDepth = 0
	return d
})

const minGitleaksSecretLen = 8

// redactGitleaks categories are gitleaks rule IDS, content-free by
// construction; they name the rule, never the matched text.
func redactGitleaks(d *detect.Detector, text string, catSet map[string]struct{}) string {
	if d == nil || text == "" {
		return text
	}
	out := text
	all := findings(d, text)
	sort.SliceStable(all, func(i, j int) bool {
		return len(secretText(all[i])) > len(secretText(all[j]))
	})
	for _, f := range all {
		secret := secretText(f)
		if len(secret) < minGitleaksSecretLen {
			continue
		}
		if strings.Contains(secret, redactedPrefix) {
			continue
		}
		if !strings.Contains(out, secret) {
			continue
		}
		catSet[f.RuleID] = struct{}{}
		out = strings.ReplaceAll(out, secret, placeholder(f.RuleID))
	}
	return out
}

func secretText(f report.Finding) string {
	if f.Secret != "" {
		return f.Secret
	}
	return f.Match
}

func findings(d *detect.Detector, text string) (out []report.Finding) {
	defer func() {
		if r := recover(); r != nil {
			out = nil
		}
	}()
	return d.DetectString(text)
}
