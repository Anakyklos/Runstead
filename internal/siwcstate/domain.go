// Package siwcstate resolves a previously registered SIWC state domain.
// Registration is intentionally absent until authenticated credential setup
// exists; provider declaration fields alone are not proof of account identity.
package siwcstate

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/RenyEnnos/Runstead/internal/provider"
	_ "modernc.org/sqlite"
)

const (
	LocatorVersion   = 1
	ManifestVersion  = 1
	LocatorFile      = "siwc-locator.json"
	ManifestFile     = "siwc-manifest.json"
	DomainMarkerKey  = "siwc_state_domain_v1" // Reserved for writes by future authenticated registration.
	maxMetadataBytes = 16 << 10
)

var (
	ErrDomainUnavailable = errors.New("SIWC state domain unavailable")
	ErrDivergentPath     = errors.New("SIWC state directory override diverges from registered domain")
	ErrIdentityMismatch  = errors.New("SIWC provider identity does not match registered domain")
	ErrUnsafePath        = errors.New("unsafe SIWC state path")
	ErrDatabaseBusy      = errors.New("SIWC database has active or unreconciled sidecars")
)

// Locator is deliberately read-only in this stage. A future authenticated
// registration flow owns creation and replacement of this record.
type Locator struct {
	Version      int    `json:"version"`
	CanonicalDir string `json:"canonical_dir"`
	DomainID     string `json:"domain_id"`
}

// Manifest contains only the opaque, non-secret provider identity fields.
type Manifest struct {
	Version           int                   `json:"version"`
	WireContract      provider.WireContract `json:"wire_contract"`
	AccountBinding    string                `json:"account_binding"`
	CredentialBinding string                `json:"credential_binding"`
	BehaviorDigest    string                `json:"behavior_digest"`
	ProviderID        string                `json:"provider_id"`
	Model             string                `json:"model"`
	ConfigIdentity    string                `json:"config_identity"`
}

// Options supplies the trusted locator location, an optional already
// validated v2 provider identity, and operator overrides that must agree with
// the registered canonical path.
type Options struct {
	LocatorPath string
	Identity    *provider.Identity
	Overrides   []string
}

// Domain is a validated, existing state root. Resolve never creates files.
type Domain struct {
	Dir      string
	Identity Manifest
}

// ManifestFromIdentity is intentionally private to the package. There is no
// public API that turns untrusted provider configuration into a registration.
func manifestFromIdentity(identity provider.Identity) Manifest {
	return Manifest{
		Version: ManifestVersion, WireContract: identity.WireContract,
		AccountBinding: identity.AccountBinding, CredentialBinding: identity.CredentialBinding,
		BehaviorDigest: identity.BehaviorDigest, ProviderID: identity.ProviderID,
		Model: identity.Model, ConfigIdentity: identity.ConfigIdentity,
	}
}

// DefaultLocatorPath derives only the location at which a registration may be
// discovered. HOME/XDG values never establish or alter provider identity. A
// changed discovery root with no locator fails closed and is never initialized.
func DefaultLocatorPath(home, xdgStateHome string) (string, error) {
	base := strings.TrimSpace(xdgStateHome)
	if base != "" {
		if !filepath.IsAbs(base) {
			return "", fmt.Errorf("%w: XDG_STATE_HOME must be absolute", ErrUnsafePath)
		}
	} else {
		base = strings.TrimSpace(home)
		if base == "" || !filepath.IsAbs(base) {
			return "", fmt.Errorf("%w: HOME must be an absolute path", ErrUnsafePath)
		}
		base = filepath.Join(base, ".local", "state")
	}
	return filepath.Join(filepath.Clean(base), "runstead", "siwc", LocatorFile), nil
}

// Resolve validates an existing locator, manifest and initialized Runstead
// SQLite database, then checks every explicit state-dir override. It does not
// create, migrate, repair or rewrite any state. Callers may open the validated
// DB only after this function succeeds.
func Resolve(options Options) (Domain, error) {
	if strings.TrimSpace(options.LocatorPath) == "" {
		return Domain{}, fmt.Errorf("%w: locator path is required", ErrDomainUnavailable)
	}
	locatorPath, err := existingPrivateFile(options.LocatorPath)
	if err != nil {
		return Domain{}, fmt.Errorf("%w: locator is missing or unsafe: %w", ErrDomainUnavailable, err)
	}
	if _, err := existingPrivateDir(filepath.Dir(locatorPath)); err != nil {
		return Domain{}, fmt.Errorf("%w: locator directory is missing or unsafe: %w", ErrDomainUnavailable, err)
	}
	var locator Locator
	if err := readStrictJSON(locatorPath, &locator); err != nil {
		return Domain{}, fmt.Errorf("%w: invalid locator", ErrDomainUnavailable)
	}
	if locator.Version != LocatorVersion || locator.CanonicalDir == "" || locator.DomainID == "" {
		return Domain{}, fmt.Errorf("%w: unsupported or incomplete locator", ErrDomainUnavailable)
	}
	canonicalDir, err := existingPrivateDir(locator.CanonicalDir)
	if err != nil {
		return Domain{}, fmt.Errorf("%w: canonical directory is missing or unsafe: %w", ErrDomainUnavailable, err)
	}
	if canonicalDir != filepath.Clean(locator.CanonicalDir) {
		return Domain{}, fmt.Errorf("%w: canonical directory is not normalized", ErrDomainUnavailable)
	}
	manifestPath := filepath.Join(canonicalDir, ManifestFile)
	manifestFile, err := existingPrivateFile(manifestPath)
	if err != nil {
		return Domain{}, fmt.Errorf("%w: manifest is missing or unsafe: %w", ErrDomainUnavailable, err)
	}
	var manifest Manifest
	if err := readStrictJSON(manifestFile, &manifest); err != nil || !validManifest(manifest) {
		return Domain{}, fmt.Errorf("%w: invalid manifest", ErrDomainUnavailable)
	}
	if locator.DomainID != manifest.ConfigIdentity {
		return Domain{}, fmt.Errorf("%w: locator and manifest disagree", ErrDomainUnavailable)
	}
	if options.Identity != nil && !matchesIdentity(manifest, *options.Identity) {
		return Domain{}, ErrIdentityMismatch
	}
	for _, override := range options.Overrides {
		if strings.TrimSpace(override) == "" {
			return Domain{}, fmt.Errorf("%w: empty override", ErrDivergentPath)
		}
		resolved, resolveErr := normalizeOverride(override, canonicalDir)
		if resolveErr != nil {
			return Domain{}, resolveErr
		}
		if resolved != canonicalDir {
			return Domain{}, ErrDivergentPath
		}
	}
	dbPath := filepath.Join(canonicalDir, "runstead.db")
	if _, err := existingPrivateFile(dbPath); err != nil {
		return Domain{}, fmt.Errorf("%w: database is missing or unsafe: %w", ErrDomainUnavailable, err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := rejectExistingSidecar(dbPath + suffix); err != nil {
			return Domain{}, fmt.Errorf("%w: SQLite sidecar requires reconciliation: %w", ErrDomainUnavailable, err)
		}
	}
	if err := verifyExistingRunsteadDB(dbPath, manifest.ConfigIdentity); err != nil {
		return Domain{}, fmt.Errorf("%w: database is not an initialized Runstead store", ErrDomainUnavailable)
	}
	return Domain{Dir: canonicalDir, Identity: manifest}, nil
}

func rejectExistingSidecar(path string) error {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return ErrUnsafePath
	}
	return ErrDatabaseBusy
}

func validManifest(manifest Manifest) bool {
	if manifest.Version != ManifestVersion || manifest.WireContract != provider.WireResponsesSIWCV1 ||
		!validBinding(manifest.AccountBinding) || !validBinding(manifest.CredentialBinding) ||
		manifest.AccountBinding == manifest.CredentialBinding || manifest.ProviderID == "" ||
		manifest.Model == "" || !validDigest(manifest.BehaviorDigest) || !validConfigIdentity(manifest.ConfigIdentity) {
		return false
	}
	return manifest.ConfigIdentity == providerV2Identity(manifest.BehaviorDigest, manifest.AccountBinding, manifest.CredentialBinding)
}

func matchesIdentity(manifest Manifest, identity provider.Identity) bool {
	return manifestFromIdentity(identity) == manifest
}

func validBinding(value string) bool {
	if !strings.HasPrefix(value, "hmac-sha256:v1:") || len(value) != len("hmac-sha256:v1:")+64 {
		return false
	}
	for _, c := range strings.TrimPrefix(value, "hmac-sha256:v1:") {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func validDigest(value string) bool {
	return len(value) == len("sha256:")+64 && strings.HasPrefix(value, "sha256:") && isLowerHex(value[len("sha256:"):])
}

func validConfigIdentity(value string) bool {
	return len(value) == len("provider.v2:sha256:")+64 && strings.HasPrefix(value, "provider.v2:sha256:") && isLowerHex(value[len("provider.v2:sha256:"):])
}

func isLowerHex(value string) bool {
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func providerV2Identity(behavior, account, credential string) string {
	// Keep the framing synchronized with provider.siwcConfigIdentity.
	sum := sha256.Sum256([]byte(behavior + "\x00" + account + "\x00" + credential))
	return "provider.v2:sha256:" + hex.EncodeToString(sum[:])
}

func normalizeOverride(value, canonicalDir string) (string, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return "", fmt.Errorf("%w: cannot normalize override", ErrDivergentPath)
	}
	if !filepath.IsAbs(raw) {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("%w: cannot normalize override", ErrDivergentPath)
		}
		raw = cwd + string(filepath.Separator) + raw
	}
	absolute := filepath.Clean(raw)
	if absolute == canonicalDir {
		if err := rejectSymlinkComponents(raw); err != nil {
			return "", fmt.Errorf("%w: override path is unsafe", ErrUnsafePath)
		}
		if _, err := existingPrivateDir(absolute); err != nil {
			return "", fmt.Errorf("%w: override path is unsafe", ErrUnsafePath)
		}
	}
	return absolute, nil
}

func existingPrivateDir(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", ErrUnsafePath
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return "", err
	}
	abs := filepath.Clean(path)
	info, err := os.Lstat(abs)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		return "", ErrUnsafePath
	}
	return abs, nil
}

func existingPrivateFile(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", ErrUnsafePath
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return "", err
	}
	abs := filepath.Clean(path)
	info, err := os.Lstat(abs)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		return "", ErrUnsafePath
	}
	return abs, nil
}

func rejectSymlinkComponents(path string) error {
	if !filepath.IsAbs(path) {
		return ErrUnsafePath
	}
	volume := filepath.VolumeName(path)
	remainder := strings.TrimPrefix(path, volume)
	current := volume + string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(remainder, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		if component == ".." {
			current = filepath.Dir(current)
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafePath
		}
	}
	return nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	current, err := user.Current()
	if err != nil || current.Uid == "" {
		return false
	}
	// FileInfo.Sys exposes platform-specific ownership metadata. Support common
	// Unix stat structs without a compile-time dependency on one OS; platforms
	// that do not expose an owner fail closed.
	sys := reflect.ValueOf(info.Sys())
	if !sys.IsValid() {
		return false
	}
	if sys.Kind() == reflect.Pointer {
		sys = sys.Elem()
	}
	if !sys.IsValid() || sys.Kind() != reflect.Struct {
		return false
	}
	uid := sys.FieldByName("Uid")
	return uid.IsValid() && fmt.Sprint(uid.Interface()) == current.Uid
}

func readStrictJSON(path string, target any) (resultErr error) {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil && resultErr == nil {
			resultErr = fmt.Errorf("close metadata file: %w", closeErr)
		}
	}()
	data, err := io.ReadAll(io.LimitReader(file, maxMetadataBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxMetadataBytes {
		return errors.New("metadata unreadable or too large")
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("metadata has trailing content")
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("metadata has trailing content")
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 16 {
		return errors.New("metadata nesting too deep")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("invalid object key")
			}
			if _, exists := seen[name]; exists {
				return fmt.Errorf("duplicate key %q", name)
			}
			seen[name] = struct{}{}
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return errors.New("invalid JSON delimiter")
	}
}

func verifyExistingRunsteadDB(path, expectedDomainID string) (resultErr error) {
	fileURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String() + "?mode=ro&immutable=1"
	db, err := sql.Open("sqlite", fileURL)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil && resultErr == nil {
			resultErr = fmt.Errorf("close read-only SIWC database: %w", closeErr)
		}
	}()
	db.SetMaxOpenConns(1)
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version < 1 {
		return errors.New("missing Runstead schema version")
	}
	var required int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('tasks', 'events', 'meta')").Scan(&required); err != nil || required != 3 {
		return errors.New("required Runstead tables are missing")
	}
	var domainID string
	if err := db.QueryRow("SELECT value FROM meta WHERE key = ?", DomainMarkerKey).Scan(&domainID); err != nil || domainID != expectedDomainID {
		return errors.New("SIWC database domain binding is missing or mismatched")
	}
	var integrity string
	if err := db.QueryRow("PRAGMA quick_check").Scan(&integrity); err != nil || integrity != "ok" {
		return errors.New("SQLite integrity check failed")
	}
	return nil
}
