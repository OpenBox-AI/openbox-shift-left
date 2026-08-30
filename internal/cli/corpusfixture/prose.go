package corpusfixture

import "strings"

// proseSentence is the unit SyntheticProse repeats.
//
// Pure ASCII, no digits, no path or address shape: filler that tripped one of
// Scan's own value patterns would make every regenerated fixture fail the gate
// that admits it.
const proseSentence = "This paragraph is synthetic fixture prose. " +
	"It stands in for recorded prompt text so the committed corpus carries no real content, " +
	"while the exchange keeps the rune geometry the recording had. "

// SyntheticProse returns exactly n runes of deterministic filler.
//
// Prefix-stable across lengths, which is what lets a checker re-derive the
// filler for an observed length and compare for equality instead of matching a
// pattern that drifts away from the generator.
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
