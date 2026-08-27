package provider

// Identity is the sanitized, provider-neutral execution identity of one
// configured provider endpoint, derived from a validated Resolved value. It
// exists so the runtime can persist and inspect who executed a task without
// ever transporting wire types, credentials or raw configuration values
// (#14). Identity contains no secret material by construction: it reuses
// Config.Sanitized for the configuration identity and never renders option
// values, authentication material or request bodies.
type Identity struct {
	ProviderID     string
	ProtocolFamily ProtocolFamily
	Model          string
	ConfigIdentity string
	ProfileVersion string
	AdapterVersion string
}

// IdentityFromResolved derives the sanitized identity of a resolved provider
// plus the adapter implementation version that will execute it. The
// configuration identity is Config.Sanitized() (safe for any validated
// configuration). adapterVersion is operator-visible implementation evidence
// (for example "openaicompat v0.1") and never contains secrets.
func IdentityFromResolved(resolved Resolved, adapterVersion string) Identity {
	return Identity{
		ProviderID:     resolved.ProviderID,
		ProtocolFamily: resolved.ProtocolFamily,
		Model:          resolved.Model,
		ConfigIdentity: resolved.ConfigIdentity,
		ProfileVersion: resolved.Profile.ProfileVersion,
		AdapterVersion: adapterVersion,
	}
}

// Empty reports whether no provider identity is attached.
func (i Identity) Empty() bool { return i.ProviderID == "" }
