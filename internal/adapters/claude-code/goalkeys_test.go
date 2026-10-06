package claudecode

import "testing"

// coreGoalKeys is core's stringifySignalArgs preference order. It takes the
// first of these it finds in a signal's signal_args and treats the value as the
// session's goal text.
var coreGoalKeys = []string{"prompt", "message", "input", "text", "content"}

// TestNoSignalMetadataKeyIsACoreGoalKey guards the mechanism behind the v1.9
// ordering hazard, from the side that can actually see the keys.
//
// Since v1.9 the client projects a signal's whole metadata map into
// signal_args. On a core WITHOUT the source-and-name goal gate, a projected key
// named one of coreGoalKeys becomes the session goal — and every later action is
// then judged against it. The gate is what makes the projection safe, so this
// test does not prevent a defect on a gated core; it bounds the blast radius of
// running a 1.9 client against an ungated one, which is exactly the window the
// rollout ordering exists to keep shut.
//
// The client has a matching test (TestSignalArgsProjectionDoesNotUseCoreGoalKeys)
// but it can only see signalDetailKeyFor's outputs. The metadata keys are the
// adapter's, and the adapter is the only package that can enumerate them — so
// the pair of tests is the whole guard, and this is the half that covers the
// larger surface.
//
// Driven off signalCases(), which is also what conformance C52 iterates, so a
// class added to the mapper without a case here fails that count first.
func TestNoSignalMetadataKeyIsACoreGoalKey(t *testing.T) {
	m := testMapper()
	m.CaptureContent = true // the widest key set: gated content keys included

	checked := 0
	for _, tc := range signalCases() {
		ev, ok := m.Map(tc.hook, tc.ev)
		if !ok {
			t.Fatalf("%s: Map ok=false", tc.name)
		}
		checked++
		for _, gk := range coreGoalKeys {
			if v, present := ev.Metadata[gk]; present {
				t.Errorf("%s: metadata carries %q = %v. Since v1.9 that key is projected into "+
					"signal_args, where an ungated core reads it as the session's goal text. "+
					"Rename it to something structural.", tc.name, gk, v)
			}
		}
	}
	if checked != 21 {
		t.Errorf("checked %d signal classes, want 21; signalCases() and this guard have diverged", checked)
	}
}
