package capdict

// Translator returns a closure that looks up the supplied capability
// id in `d` and returns the description on a hit, or the empty
// string on a miss.
//
// The shape matches the M9.4.b `approval.CapabilityTranslator` seam
// (`func(string) string`) so the production wiring can pass the
// returned closure directly into `approval.RenderApprovalCard` — Go's
// assignability rules allow a value of unnamed-func type to satisfy
// a parameter of named-func type with the same underlying signature.
//
// A nil dictionary returns a non-nil closure that always returns the
// empty string — same degradation shape the card renderer expects
// when the dictionary is not yet wired (the dictionary-not-loaded
// fallback path stays a legitimate operator state, e.g. during
// initial boot before the loader has run).
//
// Concurrent invocation is safe by the never-mutated-after-construction
// invariant on the underlying [Dictionary.caps] map (NOT by a
// runtime lock). The `-race` tests exercise concurrent READS only —
// the race detector cannot prove the no-mutation invariant because
// there is no mutation to race against; the invariant is upheld by
// the unexported-map design plus the absence of any setter on
// [Dictionary].
func Translator(d *Dictionary) func(string) string {
	return func(id string) string {
		desc, _ := d.Translate(id)
		return desc
	}
}
