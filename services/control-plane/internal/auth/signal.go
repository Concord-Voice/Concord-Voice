package auth

// signalMatch implements symmetric absent-is-no-signal matching for optional
// session metadata, such as machine_id.
//
// Optional metadata has TWO meaningful Go states, not three. NULL and "" both
// mean absent. A DISPLAY reader renders absent as unknown. A symmetric equality
// reader treats absence as NO SIGNAL — never as a value or mismatch.
//
// known is true only when BOTH sides carry a value; ok is true when the signal
// is unknown OR the two sides agree. Grace recovery treats recorded IP and
// user-agent evidence differently: stored absence is no signal, but a stored
// value must match a presented value.
//
// This is for optional symmetric comparisons. A DISPLAY reader must NOT use it
// — it renders absent as "unknown" instead. Adding a nullable column? Pick one,
// deliberately.
//
// Introduced by #3290. Coalescing nullable columns to "" must not turn absence
// into a mismatch. This protects optional symmetric comparisons; grace recovery
// separately treats stored IP and user-agent values as evidence to be refuted.
func signalMatch(stored, presented string) (known, ok bool) {
	known = stored != "" && presented != ""
	return known, !known || stored == presented
}
