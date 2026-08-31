package composition

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

const ProfileSchemaVersion = 1

// Profile is operator-owned runtime composition. It contains no executable
// code and no trusted-kernel replacement fields.
type Profile struct {
	Version        int          `json:"version"`
	ProfileID      string       `json:"profile_id"`
	ProfileVersion string       `json:"profile_version"`
	ProviderID     string       `json:"provider_id,omitempty"`
	Packages       []PackageRef `json:"packages"`
	RecipeIDs      []string     `json:"recipe_ids,omitempty"`
}

// PackageRef selects one exact package version. A missing version is never
// interpreted as "latest" or a built-in default.
type PackageRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

func (p Profile) Validate() error {
	if p.Version != ProfileSchemaVersion {
		return compositionError(ErrorInvalidProfile, ErrInvalidProfile, "version", "unsupported profile schema version %d (supported: %d)", p.Version, ProfileSchemaVersion)
	}
	if strings.TrimSpace(p.ProfileID) == "" {
		return compositionError(ErrorInvalidProfile, ErrInvalidProfile, "profile_id", "profile_id must not be empty")
	}
	if strings.TrimSpace(p.ProfileVersion) == "" {
		return compositionError(ErrorInvalidProfile, ErrInvalidProfile, "profile_version", "profile_version must not be empty")
	}
	if strings.TrimSpace(p.ProviderID) != p.ProviderID {
		return compositionError(ErrorInvalidProfile, ErrInvalidProfile, "provider_id", "provider_id must not have surrounding whitespace")
	}
	if len(p.Packages) == 0 {
		return compositionError(ErrorInvalidProfile, ErrInvalidProfile, "packages", "at least one capability package is required")
	}
	seen := make(map[string]struct{}, len(p.Packages))
	for index, ref := range p.Packages {
		if strings.TrimSpace(ref.ID) == "" {
			return compositionError(ErrorInvalidProfile, ErrInvalidProfile, fmt.Sprintf("packages[%d].id", index), "package id must not be empty")
		}
		if strings.TrimSpace(ref.ID) != ref.ID {
			return compositionError(ErrorInvalidProfile, ErrInvalidProfile, fmt.Sprintf("packages[%d].id", index), "package id must not have surrounding whitespace")
		}
		if strings.TrimSpace(ref.Version) == "" {
			return compositionError(ErrorInvalidProfile, ErrInvalidProfile, fmt.Sprintf("packages[%d].version", index), "package version must be explicit")
		}
		if strings.TrimSpace(ref.Version) != ref.Version {
			return compositionError(ErrorInvalidProfile, ErrInvalidProfile, fmt.Sprintf("packages[%d].version", index), "package version must not have surrounding whitespace")
		}
		key := ref.ID + "\x00" + ref.Version
		if _, exists := seen[key]; exists {
			return compositionError(ErrorDuplicatePackage, ErrDuplicatePackage, fmt.Sprintf("packages[%d]", index), "package %q@%q appears more than once", ref.ID, ref.Version)
		}
		seen[key] = struct{}{}
	}
	seenRecipes := make(map[string]struct{}, len(p.RecipeIDs))
	for index, id := range p.RecipeIDs {
		if strings.TrimSpace(id) == "" || strings.TrimSpace(id) != id {
			return compositionError(ErrorInvalidProfile, ErrInvalidProfile, fmt.Sprintf("recipe_ids[%d]", index), "recipe id must be non-empty and trimmed")
		}
		if _, exists := seenRecipes[id]; exists {
			return compositionError(ErrorInvalidProfile, ErrInvalidProfile, fmt.Sprintf("recipe_ids[%d]", index), "recipe id %q appears more than once", id)
		}
		seenRecipes[id] = struct{}{}
	}
	return nil
}

// ParseProfile strictly decodes exactly one Profile document. Unknown fields,
// duplicate object keys and trailing content fail closed.
func ParseProfile(data []byte) (Profile, error) {
	if err := rejectDuplicateKeys(data); err != nil {
		return Profile{}, compositionError(ErrorInvalidProfile, ErrInvalidProfile, "document", "%v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var profile Profile
	if err := decoder.Decode(&profile); err != nil {
		return Profile{}, compositionError(ErrorInvalidProfile, ErrInvalidProfile, "document", "decode failed")
	}
	if err := ensureEOF(decoder); err != nil {
		return Profile{}, compositionError(ErrorInvalidProfile, ErrInvalidProfile, "document", "%v", err)
	}
	if err := profile.Validate(); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

func LoadProfile(path string) (Profile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, compositionError(ErrorInvalidProfile, ErrInvalidProfile, "file", "profile unavailable")
	}
	return ParseProfile(data)
}

func ensureEOF(decoder *json.Decoder) error {
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected content after JSON document")
		}
		return fmt.Errorf("trailing content after JSON document")
	}
	return nil
}
