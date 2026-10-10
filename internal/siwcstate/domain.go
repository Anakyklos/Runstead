// Package siwcstate resolves a previously registered SIWC state domain.
// Registration is intentionally absent until authenticated credential setup
// exists; provider declaration fields alone are not proof of account identity.
package siwcstate

import (
	"bytes"
	"context"
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
	"github.com/RenyEnnos/Runstead/internal/state"
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

type fileIdentity struct {
	device uint64
	inode  uint64
}

// Initialize creates one explicitly requested SIWC domain from an already
// derived v2 provider identity. Callers must verify that the identity's opaque
// account and credential bindings match protected authenticated custody
// before calling. The locator is published last, so partial initialization is
// never discoverable as an admitted domain.
func Initialize(locatorPath, canonicalDir string, identity provider.Identity) (resultErr error) {
	return InitializeWithPublisher(locatorPath, canonicalDir, identity, nil)
}

// InitializeWithPublisher initializes a new domain and calls publish only
// after its database and manifest are durable, but before the locator makes
// the domain discoverable. Failed initialization removes only artifacts whose
// identities were captured from this transaction. Unexpected entries are
// preserved and reported.
func InitializeWithPublisher(locatorPath, canonicalDir string, identity provider.Identity, publish func() (rollback func() error, err error)) (resultErr error) {
	if identity.WireContract != provider.WireResponsesSIWCV1 || !validBinding(identity.AccountBinding) || !validBinding(identity.CredentialBinding) || identity.AccountBinding == identity.CredentialBinding || !validDigest(identity.BehaviorDigest) || !validConfigIdentity(identity.ConfigIdentity) || identity.ConfigIdentity != providerV2Identity(identity.BehaviorDigest, identity.AccountBinding, identity.CredentialBinding) {
		return ErrIdentityMismatch
	}
	if !filepath.IsAbs(canonicalDir) || filepath.Clean(canonicalDir) != canonicalDir || !filepath.IsAbs(locatorPath) {
		return ErrUnsafePath
	}
	if _, err := os.Lstat(locatorPath); err == nil {
		return ErrDomainUnavailable
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrUnsafePath
	}
	if err := rejectSymlinkComponents(filepath.Dir(canonicalDir)); err != nil {
		return ErrUnsafePath
	}
	if err := os.Mkdir(canonicalDir, 0o700); err != nil {
		return fmt.Errorf("cannot create requested SIWC state directory: %w", err)
	}
	info, err := os.Lstat(canonicalDir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		return ErrUnsafePath
	}
	domainCommitted := false
	cleanup := domainInitializationCleanup{dir: canonicalDir, dirInfo: info}
	var lock *Lock
	defer func() {
		if resultErr != nil && !domainCommitted {
			if cleanupErr := cleanup.rollback(lock); cleanupErr != nil {
				resultErr = errors.Join(resultErr, cleanupErr)
			}
		}
		if lock != nil {
			if releaseErr := lock.Release(); releaseErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("release SIWC initialization lock: %w", releaseErr))
			}
		}
	}()
	domain := Domain{Dir: canonicalDir, Identity: manifestFromIdentity(identity)}
	lock, err = AcquireLock(context.Background(), domain)
	if err != nil {
		return err
	}
	lockInfo, err := lock.file.Stat()
	if err != nil {
		return fmt.Errorf("inspect SIWC lock identity: %w", err)
	}
	if lock.created {
		cleanup.add(filepath.Join(canonicalDir, lockFile), lockInfo)
	}
	dbPath := filepath.Join(canonicalDir, state.DefaultDBFile)
	dbFile, err := os.OpenFile(dbPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("create SIWC database exclusively: %w", err)
	}
	dbInfo, statErr := dbFile.Stat()
	closeErr := dbFile.Close()
	if statErr != nil || closeErr != nil || !validCreatedDomainFile(dbInfo) {
		return errors.Join(ErrUnsafePath, statErr, closeErr)
	}
	cleanup.add(dbPath, dbInfo)
	store, err := state.Open(state.Options{Path: dbPath})
	if err != nil {
		cleanup.captureSQLiteSidecars(dbPath)
		return fmt.Errorf("open SIWC initialization database: %w", err)
	}
	cleanup.captureSQLiteSidecars(dbPath)
	if _, err := store.DB().Exec(`INSERT INTO meta (key,value) VALUES (?,?)`, DomainMarkerKey, identity.ConfigIdentity); err != nil {
		if closeErr := store.Close(); closeErr != nil {
			return errors.Join(ErrDomainUnavailable, closeErr)
		}
		return ErrDomainUnavailable
	}
	if err := store.Close(); err != nil {
		return ErrDomainUnavailable
	}
	manifestPath := filepath.Join(canonicalDir, ManifestFile)
	manifestInfo, err := writeNewPrivateJSON(manifestPath, manifestFromIdentity(identity))
	if err != nil {
		return err
	}
	cleanup.add(manifestPath, manifestInfo)
	if err := ensurePrivateDirectory(filepath.Dir(locatorPath)); err != nil {
		return err
	}
	var rollback func() error
	if publish != nil {
		rollback, err = publish()
		if err != nil {
			if rollback != nil {
				resultErr = errors.Join(err, rollback())
			} else {
				resultErr = err
			}
			return resultErr
		}
	}
	locator := Locator{Version: LocatorVersion, CanonicalDir: canonicalDir, DomainID: identity.ConfigIdentity}
	if _, err := writeNewPrivateJSON(locatorPath, locator); err != nil {
		if rollback != nil {
			resultErr = errors.Join(err, rollback())
			return resultErr
		}
		return err
	}
	domainCommitted = true
	return nil
}

func ensurePrivateDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return ErrUnsafePath
	}
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(path), current), current) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return ErrUnsafePath
			}
			info, err = os.Lstat(current)
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return ErrUnsafePath
		}
	}
	return existingPrivateDirectoryOnly(path)
}

func existingPrivateDirectoryOnly(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		return ErrUnsafePath
	}
	return nil
}

func writeNewPrivateJSON(path string, value any) (os.FileInfo, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, ErrDomainUnavailable
	}
	dirPath := filepath.Dir(path)
	file, err := os.CreateTemp(dirPath, ".siwc-metadata-*.stage")
	if err != nil {
		return nil, ErrDomainUnavailable
	}
	tempPath := file.Name()
	cleanupTemp := func() error { return removeStagingFile(tempPath) }
	if err := file.Chmod(0o600); err != nil {
		return nil, errors.Join(ErrDomainUnavailable, file.Close(), cleanupTemp())
	}
	if _, err = file.Write(encoded); err != nil {
		return nil, errors.Join(ErrDomainUnavailable, file.Close(), cleanupTemp())
	}
	if err = file.Sync(); err != nil {
		return nil, errors.Join(ErrDomainUnavailable, file.Close(), cleanupTemp())
	}
	if err = file.Close(); err != nil {
		return nil, errors.Join(ErrDomainUnavailable, cleanupTemp())
	}
	info, err := os.Lstat(tempPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ownedByCurrentUser(info) {
		return nil, errors.Join(ErrUnsafePath, cleanupTemp())
	}
	if err := os.Link(tempPath, path); err != nil {
		return nil, errors.Join(ErrDomainUnavailable, cleanupTemp())
	}
	cleanupDestination := func() error {
		current, statErr := os.Lstat(path)
		if errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		if statErr != nil {
			return statErr
		}
		if !os.SameFile(info, current) {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		return syncMetadataDirectory(dirPath)
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, current) {
		return nil, errors.Join(ErrUnsafePath, cleanupDestination(), cleanupTemp())
	}
	dir, err := os.Open(dirPath)
	if err != nil {
		return nil, errors.Join(ErrDomainUnavailable, cleanupDestination(), cleanupTemp())
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil || closeErr != nil {
		return nil, errors.Join(ErrDomainUnavailable, syncErr, closeErr, cleanupDestination(), cleanupTemp())
	}
	if err := os.Remove(tempPath); err != nil {
		return nil, errors.Join(ErrDomainUnavailable, err, cleanupDestination(), cleanupTemp())
	}
	return info, nil
}

type domainInitializationCleanup struct {
	dir       string
	dirInfo   os.FileInfo
	artifacts []domainInitializationArtifact
}

type domainInitializationArtifact struct {
	path string
	info os.FileInfo
}

func (c *domainInitializationCleanup) add(path string, info os.FileInfo) {
	if info != nil {
		c.artifacts = append(c.artifacts, domainInitializationArtifact{path: path, info: info})
	}
}

func (c *domainInitializationCleanup) captureSQLiteSidecars(dbPath string) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		path := dbPath + suffix
		info, err := os.Lstat(path)
		if err == nil && validCreatedDomainFile(info) {
			c.add(path, info)
		}
	}
}

func (c *domainInitializationCleanup) rollback(lock *Lock) error {
	var cleanupErr error
	// Remove data while the exclusive lock is still held. The lock marker is
	// handled last and only when this transaction created its inode.
	for i := len(c.artifacts) - 1; i >= 0; i-- {
		artifact := c.artifacts[i]
		if filepath.Base(artifact.path) == lockFile {
			continue
		}
		if err := removeCreatedDomainArtifact(artifact); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if lock != nil && lock.created && cleanupErr == nil {
		for i := len(c.artifacts) - 1; i >= 0; i-- {
			artifact := c.artifacts[i]
			if filepath.Base(artifact.path) == lockFile {
				entries, readErr := os.ReadDir(c.dir)
				if readErr != nil {
					cleanupErr = errors.Join(cleanupErr, fmt.Errorf("preserve incomplete SIWC state: inspect remaining domain entries: %w", readErr))
				} else if len(entries) == 1 && entries[0].Name() == lockFile {
					if err := removeCreatedDomainArtifact(artifact); err != nil {
						cleanupErr = errors.Join(cleanupErr, err)
					}
				}
				break
			}
		}
	}
	current, err := os.Lstat(c.dir)
	if errors.Is(err, os.ErrNotExist) {
		return cleanupErr
	}
	if err != nil || !os.SameFile(c.dirInfo, current) || !current.IsDir() || current.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(current) {
		return errors.Join(cleanupErr, fmt.Errorf("preserve incomplete SIWC state: domain directory identity or permissions changed"))
	}
	if err := os.Remove(c.dir); err != nil {
		return errors.Join(cleanupErr, fmt.Errorf("preserve incomplete SIWC state: domain directory is not empty or cannot be removed: %w", err))
	}
	if err := syncMetadataDirectory(filepath.Dir(c.dir)); err != nil {
		return errors.Join(cleanupErr, fmt.Errorf("sync SIWC domain cleanup: %w", err))
	}
	return cleanupErr
}

func removeCreatedDomainArtifact(artifact domainInitializationArtifact) error {
	current, err := os.Lstat(artifact.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("preserve SIWC artifact %q: inspect: %w", filepath.Base(artifact.path), err)
	}
	if !os.SameFile(artifact.info, current) || !validCreatedDomainFile(current) {
		return fmt.Errorf("preserve SIWC artifact %q: identity changed", filepath.Base(artifact.path))
	}
	if err := os.Remove(artifact.path); err != nil {
		return fmt.Errorf("remove created SIWC artifact %q: %w", filepath.Base(artifact.path), err)
	}
	return syncMetadataDirectory(filepath.Dir(artifact.path))
}

func validCreatedDomainFile(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		return false
	}
	_, links, ok := extractFileIdentity(info)
	return ok && links == 1
}

func syncMetadataDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	return errors.Join(syncErr, closeErr)
}

func removeStagingFile(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
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

// Resolve performs a read-only preflight over an existing locator, manifest
// and initialized Runstead SQLite database, then checks explicit state-dir
// overrides. It never creates, migrates, repairs or rewrites state. Callers
// must acquire the domain lock and call Resolve again before opening the
// mutable store.
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
	_, dbIdentity, err := existingPrivateFileIdentity(dbPath)
	if err != nil {
		return Domain{}, fmt.Errorf("%w: database is missing or unsafe: %w", ErrDomainUnavailable, err)
	}
	sidecarIdentities := make(map[string]*fileIdentity, 3)
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		identity, exists, err := validateExistingSidecar(dbPath + suffix)
		if err != nil {
			return Domain{}, fmt.Errorf("%w: SQLite sidecar is unsafe: %w", ErrDomainUnavailable, err)
		}
		if exists {
			sidecarIdentities[dbPath+suffix] = identity
		}
	}
	// SQLite sidecars are expected after a process crash. This is an
	// immutable, read-only validation only; command callers must acquire the
	// domain lock and repeat Resolve before opening the mutable store.
	if err := verifyExistingRunsteadDB(dbPath, manifest.ConfigIdentity); err != nil {
		return Domain{}, fmt.Errorf("%w: database is not an initialized Runstead store", ErrDomainUnavailable)
	}
	// The immutable SQLite inspection above opens by pathname. Verify that the
	// path still names the unique inode we preflighted, and that each existing
	// sidecar is still the same unique inode. The caller repeats this whole
	// validation after taking the domain lock before opening mutable state.
	_, currentDBIdentity, err := existingPrivateFileIdentity(dbPath)
	if err != nil || currentDBIdentity != dbIdentity {
		return Domain{}, fmt.Errorf("%w: SQLite database identity changed during preflight", ErrDomainUnavailable)
	}
	for path, expected := range sidecarIdentities {
		current, exists, err := validateExistingSidecar(path)
		if err != nil || !exists || *current != *expected {
			return Domain{}, fmt.Errorf("%w: SQLite sidecar identity changed during preflight", ErrDomainUnavailable)
		}
	}
	return Domain{Dir: canonicalDir, Identity: manifest}, nil
}

func validateExistingSidecar(path string) (*fileIdentity, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		return nil, false, ErrUnsafePath
	}
	identity, links, ok := extractFileIdentity(info)
	if !ok || links != 1 {
		return nil, false, ErrUnsafePath
	}
	return &identity, true, nil
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
	clean, _, err := existingPrivateFileIdentity(path)
	return clean, err
}

func existingPrivateFileIdentity(path string) (string, fileIdentity, error) {
	if !filepath.IsAbs(path) {
		return "", fileIdentity{}, ErrUnsafePath
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return "", fileIdentity{}, err
	}
	abs := filepath.Clean(path)
	info, err := os.Lstat(abs)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		return "", fileIdentity{}, ErrUnsafePath
	}
	identity, links, ok := extractFileIdentity(info)
	if !ok || links != 1 {
		return "", fileIdentity{}, ErrUnsafePath
	}
	return abs, identity, nil
}

func extractFileIdentity(info os.FileInfo) (fileIdentity, uint64, bool) {
	sys := reflect.ValueOf(info.Sys())
	if sys.IsValid() && sys.Kind() == reflect.Pointer {
		sys = sys.Elem()
	}
	if !sys.IsValid() || sys.Kind() != reflect.Struct {
		return fileIdentity{}, 0, false
	}
	readUnsigned := func(name string) (uint64, bool) {
		field := sys.FieldByName(name)
		if !field.IsValid() {
			return 0, false
		}
		switch field.Kind() {
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			return field.Uint(), true
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			value := field.Int()
			if value < 0 {
				return 0, false
			}
			return uint64(value), true
		default:
			return 0, false
		}
	}
	device, deviceOK := readUnsigned("Dev")
	inode, inodeOK := readUnsigned("Ino")
	links, linksOK := readUnsigned("Nlink")
	return fileIdentity{device: device, inode: inode}, links, deviceOK && inodeOK && linksOK
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
