package composition

import (
	"fmt"
	"sort"
	"strings"

	"github.com/RenyEnnos/Runstead/internal/tools"
)

// PackageKind identifies how a package is supplied. M10 only supports
// metadata for built-in packages compiled into the binary.
type PackageKind string

const PackageKindBuiltin PackageKind = "builtin"

// CapabilityPackage is declarative metadata only. It contains no executable
// callbacks, commands, credentials or authority replacement hooks.
type CapabilityPackage struct {
	ID                       string       `json:"id"`
	Version                  string       `json:"version"`
	Provenance               string       `json:"provenance"`
	Kind                     PackageKind  `json:"kind"`
	RuntimeCompatibility     string       `json:"runtime_compatibility"`
	Actions                  []string     `json:"actions"`
	Capabilities             []string     `json:"capabilities"`
	WorkspaceRequirements    []string     `json:"workspace_requirements"`
	NetworkRequirements      []string     `json:"network_requirements"`
	EffectClass              string       `json:"effect_class"`
	RecoveryClass            string       `json:"recovery_class"`
	EvidenceRequirements     []string     `json:"evidence_requirements"`
	VerificationRequirements []string     `json:"verification_requirements"`
	ApprovalBoundary         string       `json:"approval_boundary"`
	MaxOutputBytes           int          `json:"max_output_bytes"`
	Dependencies             []PackageRef `json:"dependencies,omitempty"`
	Conflicts                []PackageRef `json:"conflicts,omitempty"`
}

// PackageRegistry is an immutable-by-convention registry of exact package
// identities. It stores no executable extension code.
type PackageRegistry struct {
	packages map[string]CapabilityPackage
}

func packageKey(id, version string) string { return id + "\x00" + version }

// NewPackageRegistry creates a typed registry for built-in/static package
// metadata. Callers cannot replace an existing identity silently.
func NewPackageRegistry(packages ...CapabilityPackage) (PackageRegistry, error) {
	registry := PackageRegistry{packages: make(map[string]CapabilityPackage, len(packages))}
	for index, pkg := range packages {
		if err := validatePackage(pkg, index); err != nil {
			return PackageRegistry{}, err
		}
		key := packageKey(pkg.ID, pkg.Version)
		if _, exists := registry.packages[key]; exists {
			return PackageRegistry{}, compositionError(ErrorInvalidPackage, ErrInvalidPackage, fmt.Sprintf("packages[%d]", index), "duplicate package %q@%q", pkg.ID, pkg.Version)
		}
		registry.packages[key] = clonePackage(pkg)
	}
	return registry, nil
}

// NewBuiltinRegistry returns the minimal compiled-in M10 package set.
func NewBuiltinRegistry() PackageRegistry {
	registry, err := NewPackageRegistry(
		CapabilityPackage{
			ID: "repo.read", Version: "1.0.0", Provenance: "runstead/builtin", Kind: PackageKindBuiltin,
			RuntimeCompatibility: "runstead-runtime.v1", Actions: []string{
				tools.ToolReadFile, tools.ToolListFiles, tools.ToolSearchText, tools.ToolGitStatus, tools.ToolGitDiff,
			}, Capabilities: []string{"read_workspace", "git_metadata"}, WorkspaceRequirements: []string{"workspace"},
			EffectClass: "read_only", RecoveryClass: "replay_safe", EvidenceRequirements: []string{"observation"},
			VerificationRequirements: []string{"runstead.verifier.v1"}, ApprovalBoundary: "none", MaxOutputBytes: 8 << 10,
		},
		CapabilityPackage{
			ID: "repo.write", Version: "1.0.0", Provenance: "runstead/builtin", Kind: PackageKindBuiltin,
			RuntimeCompatibility: "runstead-runtime.v1", Actions: []string{tools.ToolWriteFile, tools.ToolApplyPatch},
			Capabilities: []string{"write_workspace"}, WorkspaceRequirements: []string{"workspace"},
			EffectClass: "workspace_write", RecoveryClass: "reconcile_required", EvidenceRequirements: []string{"write_evidence"},
			VerificationRequirements: []string{"runstead.verifier.v1"}, ApprovalBoundary: "runstead.policy.v1", MaxOutputBytes: 8 << 10,
		},
		CapabilityPackage{
			ID: "process.recipes", Version: "1.0.0", Provenance: "runstead/builtin", Kind: PackageKindBuiltin,
			RuntimeCompatibility: "runstead-runtime.v1", Actions: []string{tools.ToolRunRecipe},
			Capabilities: []string{"execute_repository_code"}, WorkspaceRequirements: []string{"workspace"},
			EffectClass: "process", RecoveryClass: "human_review_required", EvidenceRequirements: []string{"process_evidence"},
			VerificationRequirements: []string{"runstead.verifier.v1"}, ApprovalBoundary: "runstead.policy.v1", MaxOutputBytes: 8 << 10,
		},
	)
	if err != nil {
		panic(err)
	}
	return registry
}

func (r PackageRegistry) Lookup(id, version string) (CapabilityPackage, bool) {
	pkg, ok := r.packages[packageKey(strings.TrimSpace(id), strings.TrimSpace(version))]
	if !ok {
		return CapabilityPackage{}, false
	}
	return clonePackage(pkg), true
}

func (r PackageRegistry) HasID(id string) bool {
	id = strings.TrimSpace(id)
	for key := range r.packages {
		if strings.HasPrefix(key, id+"\x00") {
			return true
		}
	}
	return false
}

func validatePackage(pkg CapabilityPackage, index int) error {
	path := fmt.Sprintf("packages[%d]", index)
	if strings.TrimSpace(pkg.ID) == "" || strings.TrimSpace(pkg.Version) == "" || strings.TrimSpace(pkg.Provenance) == "" {
		return compositionError(ErrorInvalidPackage, ErrInvalidPackage, path, "id, version and provenance are required")
	}
	if pkg.Kind != PackageKindBuiltin {
		return compositionError(ErrorInvalidPackage, ErrInvalidPackage, path, "unsupported package kind %q", pkg.Kind)
	}
	if strings.TrimSpace(pkg.RuntimeCompatibility) == "" {
		return compositionError(ErrorInvalidPackage, ErrInvalidPackage, path, "runtime compatibility is required")
	}
	if len(pkg.Actions) == 0 {
		return compositionError(ErrorInvalidPackage, ErrInvalidPackage, path, "at least one action is required")
	}
	if pkg.MaxOutputBytes <= 0 {
		return compositionError(ErrorInvalidPackage, ErrInvalidPackage, path, "max output bytes must be positive")
	}
	for _, field := range []struct {
		name   string
		values []string
	}{
		{name: "actions", values: pkg.Actions},
		{name: "capabilities", values: pkg.Capabilities},
		{name: "workspace_requirements", values: pkg.WorkspaceRequirements},
		{name: "network_requirements", values: pkg.NetworkRequirements},
		{name: "evidence_requirements", values: pkg.EvidenceRequirements},
		{name: "verification_requirements", values: pkg.VerificationRequirements},
	} {
		if err := validateStringSet(field.values); err != nil {
			return compositionError(ErrorInvalidPackage, ErrInvalidPackage, path+"."+field.name, "%v", err)
		}
	}
	for _, field := range []struct {
		name string
		refs []PackageRef
	}{
		{name: "dependencies", refs: pkg.Dependencies},
		{name: "conflicts", refs: pkg.Conflicts},
	} {
		seen := map[string]struct{}{}
		for _, ref := range field.refs {
			if strings.TrimSpace(ref.ID) == "" || strings.TrimSpace(ref.Version) == "" {
				return compositionError(ErrorInvalidPackage, ErrInvalidPackage, path+"."+field.name, "package references require exact id and version")
			}
			key := packageKey(ref.ID, ref.Version)
			if _, exists := seen[key]; exists {
				return compositionError(ErrorInvalidPackage, ErrInvalidPackage, path+"."+field.name, "duplicate package reference %q@%q", ref.ID, ref.Version)
			}
			seen[key] = struct{}{}
		}
	}
	return nil
}

func validateStringSet(values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return fmt.Errorf("values must be non-empty")
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("duplicate value %q", value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func clonePackage(pkg CapabilityPackage) CapabilityPackage {
	pkg.Actions = append([]string(nil), pkg.Actions...)
	pkg.Capabilities = append([]string(nil), pkg.Capabilities...)
	pkg.WorkspaceRequirements = append([]string(nil), pkg.WorkspaceRequirements...)
	pkg.NetworkRequirements = append([]string(nil), pkg.NetworkRequirements...)
	pkg.EvidenceRequirements = append([]string(nil), pkg.EvidenceRequirements...)
	pkg.VerificationRequirements = append([]string(nil), pkg.VerificationRequirements...)
	pkg.Dependencies = append([]PackageRef(nil), pkg.Dependencies...)
	pkg.Conflicts = append([]PackageRef(nil), pkg.Conflicts...)
	return pkg
}

func sortedStrings(values []string) []string {
	copy := append([]string(nil), values...)
	sort.Strings(copy)
	return copy
}
