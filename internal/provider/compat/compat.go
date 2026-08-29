// Package compat composes the supported compatibility protocol adapters
// behind the provider-neutral contract of #79. Selecting an adapter by
// ProtocolFamily belongs HERE, in the composition/configuration layer; the
// agent loop only ever depends on provider.Client and the provider-neutral
// contracts (#14/#86).
package compat

import (
	"context"
	"fmt"

	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/provider/anthropiccompat"
	"github.com/RenyEnnos/Runstead/internal/provider/googlecompat"
	"github.com/RenyEnnos/Runstead/internal/provider/openaicompat"
)

// AdapterVersion identifies this compatibility composition surface. The value
// lives in internal/provider (provider.CompatAdapterVersion) so adapters and
// the provider-neutral contract share one version identity without an import
// cycle. Bump the constant when the adapter set or its behavior changes
// meaningfully.
const AdapterVersion = provider.CompatAdapterVersion

// SecretResolver turns a non-secret provider.SecretRef into the actual
// authentication material at dispatch time. It mirrors the per-adapter
// resolver seam; the composition layer passes the same resolver to whichever
// family adapter is selected. Implementations must never log or persist
// resolved values.
type SecretResolver func(ctx context.Context, reference provider.SecretRef) (string, error)

// EnvSecretResolver builds the default operator resolver: the SecretRef names
// an environment variable that holds the credential. The reference itself is
// never secret; the resolved value is delivered only to the adapter that
// needs it and is never persisted.
func EnvSecretResolver(lookup func(string) (string, bool)) SecretResolver {
	if lookup == nil {
		lookup = func(key string) (string, bool) { return "", false }
	}
	return func(ctx context.Context, reference provider.SecretRef) (string, error) {
		name := string(reference)
		if name == "" {
			return "", fmt.Errorf("secret reference is empty")
		}
		value, ok := lookup(name)
		if !ok {
			return "", fmt.Errorf("secret reference %q is not available in the operator environment", name)
		}
		return value, nil
	}
}

// NewSelectsAndBuilds one configured provider endpoint into the protocol
// adapter for its family. Exactly one adapter is instantiated per call; there
// is no router, fallback, load balancing or automatic selection anywhere in
// this function. An unknown or invalid family refuses before any dispatch.
func New(resolved provider.Resolved, resolver SecretResolver) (provider.Client, error) {
	family := resolved.ProtocolFamily
	switch family {
	case provider.FamilyOpenAICompatible:
		return openaicompat.New(resolved, openaicompat.SecretResolver(resolver), openaicompat.Options{})
	case provider.FamilyAnthropicCompatible:
		return anthropiccompat.New(resolved, anthropiccompat.SecretResolver(resolver), anthropiccompat.Options{})
	case provider.FamilyGoogleCompatible:
		return googlecompat.New(resolved, googlecompat.SecretResolver(resolver), googlecompat.Options{})
	default:
		return nil, fmt.Errorf("compat: no adapter is registered for protocol family %q", string(family))
	}
}
