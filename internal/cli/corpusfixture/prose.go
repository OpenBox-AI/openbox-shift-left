package corpusfixture

import "strings"

const proseSentence = "This paragraph is synthetic fixture prose. " +
	"It stands in for recorded prompt text so the committed corpus carries no real content, " +
	"while the exchange keeps the rune geometry the recording had. "

// SyntheticProse returns exactly n runes of deterministic filler.
func SyntheticProse(n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(n)
	for b.Len() < n {
		remaining := n - b.Len()
		if remaining >= len(proseSentence) {
			b.WriteString(proseSentence)
			continue
		}
		b.WriteString(proseSentence[:remaining])
	}
	return b.String()
}
