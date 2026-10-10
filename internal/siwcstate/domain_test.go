package siwcstate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/state"
)

func testIdentity() provider.Identity {
	identity := provider.Identity{
		WireContract:      provider.WireResponsesSIWCV1,
		AccountBinding:    "hmac-sha256:v1:" + strings.Repeat("a", 64),
		CredentialBinding: "hmac-sha256:v1:" + strings.Repeat("b", 64),
		BehaviorDigest:    "sha256:" + strings.Repeat("c", 64),
		ProviderID:        "siwc-openai",
		ProtocolFamily:    provider.FamilyOpenAICompatible,
		Model:             "gpt-test",
		ProfileVersion:    "siwc-profile-v1",
		AdapterVersion:    "responses-siwc-v1-unavailable",
	}
	sum := sha256.Sum256([]byte(identity.BehaviorDigest + "\x00" + identity.AccountBinding + "\x00" + identity.CredentialBinding))
	identity.ConfigIdentity = "provider.v2:sha256:" + hex.EncodeToString(sum[:])
	return identity
}

func registerFixture(t *testing.T, locatorPath, stateDir string, identity provider.Identity) {
	registerFixtureWithBinding(t, locatorPath, stateDir, identity, true)
}

func registerFixtureWithoutBinding(t *testing.T, locatorPath, stateDir string, identity provider.Identity) {
	registerFixtureWithBinding(t, locatorPath, stateDir, identity, false)
}

func registerFixtureWithBinding(t *testing.T, locatorPath, stateDir string, identity provider.Identity, bind bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(locatorPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(locatorPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := state.Open(state.Options{Path: filepath.Join(stateDir, "runstead.db")})
	if err != nil {
		t.Fatal(err)
	}
	if bind {
		if _, err := store.DB().Exec("INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", DomainMarkerKey, identity.ConfigIdentity); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	writeJSON := func(path string, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manifest := manifestFromIdentity(identity)
	writeJSON(locatorPath, Locator{Version: LocatorVersion, CanonicalDir: stateDir, DomainID: identity.ConfigIdentity})
	writeJSON(filepath.Join(stateDir, ManifestFile), manifest)
}

func TestResolveRegisteredDomainAndEquivalentOverride(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "canonical")
	locator := filepath.Join(root, "locator", LocatorFile)
	identity := testIdentity()
	registerFixture(t, locator, stateDir, identity)
	if err := os.Mkdir(filepath.Join(stateDir, "alias"), 0o700); err != nil {
		t.Fatal(err)
	}

	domain, err := Resolve(Options{LocatorPath: locator, Identity: &identity, Overrides: []string{stateDir + string(filepath.Separator) + "alias" + string(filepath.Separator) + ".."}})
	if err != nil {
		t.Fatal(err)
	}
	if domain.Dir != stateDir {
		t.Fatalf("resolved directory = %q, want %q", domain.Dir, stateDir)
	}
}

func TestInitializePublishesOnlyLockedAndMarkedDomain(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "new-state")
	locator := filepath.Join(base, "xdg", "runstead", "siwc", LocatorFile)
	identity := testIdentity()
	if err := Initialize(locator, stateDir, identity); err != nil {
		t.Fatal(err)
	}
	domain, err := Resolve(Options{LocatorPath: locator})
	if err != nil {
		t.Fatal(err)
	}
	if domain.Dir != stateDir || domain.Identity.ConfigIdentity != identity.ConfigIdentity {
		t.Fatalf("resolved domain=%#v", domain)
	}
	lockInfo, err := os.Stat(filepath.Join(stateDir, lockFile))
	if err != nil || lockInfo.Mode().Perm() != 0o600 {
		t.Fatalf("lock marker stat=%v err=%v", lockInfo, err)
	}
	if err := Initialize(locator, stateDir, identity); err == nil {
		t.Fatal("reinitialized an existing domain")
	}
}

func TestResolveRejectsLegacyDatabaseWithoutSIWCDomainBinding(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "legacy-state")
	locator := filepath.Join(root, "locator", LocatorFile)
	identity := testIdentity()
	registerFixtureWithoutBinding(t, locator, stateDir, identity)
	before, err := os.ReadFile(filepath.Join(stateDir, "runstead.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Resolve(Options{LocatorPath: locator, Identity: &identity})
	if !errors.Is(err, ErrDomainUnavailable) {
		t.Fatalf("legacy database error = %v, want ErrDomainUnavailable", err)
	}
	after, err := os.ReadFile(filepath.Join(stateDir, "runstead.db"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("legacy DB changed after rejected resolution: err=%v", err)
	}
}

func TestResolveDoesNotCreateOrModifyDomainFiles(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "canonical")
	locator := filepath.Join(root, "locator", LocatorFile)
	identity := testIdentity()
	registerFixture(t, locator, stateDir, identity)
	before, err := directorySnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(Options{LocatorPath: locator, Identity: &identity}); err != nil {
		t.Fatal(err)
	}
	after, err := directorySnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Fatalf("resolver changed files:\nbefore=%v\nafter=%v", before, after)
	}
}

func directorySnapshot(root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			paths = append(paths, path)
		}
		return nil
	})
	return paths, err
}

func TestResolveRejectsDivergentOverridesWithoutCreatingFiles(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "canonical")
	locator := filepath.Join(root, "locator", LocatorFile)
	identity := testIdentity()
	registerFixture(t, locator, stateDir, identity)
	divergent := filepath.Join(root, "alternate")

	_, err := Resolve(Options{LocatorPath: locator, Identity: &identity, Overrides: []string{divergent}})
	if !errors.Is(err, ErrDivergentPath) {
		t.Fatalf("error = %v, want ErrDivergentPath", err)
	}
	if _, err := os.Stat(divergent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("divergent path was created or changed: %v", err)
	}
}

func TestResolveFailsClosedForMissingOrCorruptDomainParts(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "canonical")
	locator := filepath.Join(root, "locator", LocatorFile)
	identity := testIdentity()
	registerFixture(t, locator, stateDir, identity)

	t.Run("missing locator", func(t *testing.T) {
		_, err := Resolve(Options{LocatorPath: filepath.Join(root, "missing", LocatorFile), Identity: &identity})
		if !errors.Is(err, ErrDomainUnavailable) {
			t.Fatalf("error = %v, want ErrDomainUnavailable", err)
		}
	})
	t.Run("missing manifest", func(t *testing.T) {
		if err := os.Remove(filepath.Join(stateDir, ManifestFile)); err != nil {
			t.Fatal(err)
		}
		_, err := Resolve(Options{LocatorPath: locator, Identity: &identity})
		if !errors.Is(err, ErrDomainUnavailable) {
			t.Fatalf("error = %v, want ErrDomainUnavailable", err)
		}
	})
	t.Run("missing database", func(t *testing.T) {
		registerFixture(t, locator, stateDir, identity)
		if err := os.Remove(filepath.Join(stateDir, "runstead.db")); err != nil {
			t.Fatal(err)
		}
		_, err := Resolve(Options{LocatorPath: locator, Identity: &identity})
		if !errors.Is(err, ErrDomainUnavailable) {
			t.Fatalf("error = %v, want ErrDomainUnavailable", err)
		}
	})
	t.Run("tampered manifest", func(t *testing.T) {
		registerFixture(t, locator, stateDir, identity)
		if err := os.WriteFile(filepath.Join(stateDir, ManifestFile), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Resolve(Options{LocatorPath: locator, Identity: &identity})
		if !errors.Is(err, ErrDomainUnavailable) {
			t.Fatalf("error = %v, want ErrDomainUnavailable", err)
		}
	})
	t.Run("corrupted database is not repaired", func(t *testing.T) {
		registerFixture(t, locator, stateDir, identity)
		dbPath := filepath.Join(stateDir, "runstead.db")
		corrupt := []byte("not a sqlite database")
		if err := os.WriteFile(dbPath, corrupt, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Resolve(Options{LocatorPath: locator, Identity: &identity})
		if !errors.Is(err, ErrDomainUnavailable) {
			t.Fatalf("error = %v, want ErrDomainUnavailable", err)
		}
		got, err := os.ReadFile(dbPath)
		if err != nil || string(got) != string(corrupt) {
			t.Fatalf("corrupted DB changed during resolution: data=%q err=%v", got, err)
		}
	})
}

func TestResolveRejectsIdentityMismatch(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "canonical")
	locator := filepath.Join(root, "locator", LocatorFile)
	identity := testIdentity()
	registerFixture(t, locator, stateDir, identity)
	other := identity
	other.Model = "different-model"

	_, err := Resolve(Options{LocatorPath: locator, Identity: &other})
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("error = %v, want ErrIdentityMismatch", err)
	}
}

func TestResolveRejectsDatabaseHardlinkAlias(t *testing.T) {
	root := t.TempDir()
	identity := testIdentity()
	stateA := filepath.Join(root, "domain-a")
	stateB := filepath.Join(root, "domain-b")
	locatorA := filepath.Join(root, "locator-a", LocatorFile)
	locatorB := filepath.Join(root, "locator-b", LocatorFile)
	registerFixture(t, locatorA, stateA, identity)
	registerFixture(t, locatorB, stateB, identity)
	if err := os.Remove(filepath.Join(stateB, "runstead.db")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(stateA, "runstead.db"), filepath.Join(stateB, "runstead.db")); err != nil {
		t.Fatalf("create database hardlink alias: %v", err)
	}
	for name, locator := range map[string]string{"first domain": locatorA, "second domain": locatorB} {
		t.Run(name, func(t *testing.T) {
			if _, err := Resolve(Options{LocatorPath: locator, Identity: &identity}); !errors.Is(err, ErrDomainUnavailable) {
				t.Fatalf("Resolve() with hardlinked database = %v, want ErrDomainUnavailable", err)
			}
		})
	}
}

func TestResolveRejectsHardlinkedSQLiteSidecar(t *testing.T) {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		t.Run(suffix, func(t *testing.T) {
			root := t.TempDir()
			identity := testIdentity()
			stateDir := filepath.Join(root, "domain")
			locator := filepath.Join(root, "locator", LocatorFile)
			registerFixture(t, locator, stateDir, identity)
			sidecar := filepath.Join(stateDir, "runstead.db"+suffix)
			if err := os.WriteFile(sidecar, []byte("sidecar"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(sidecar, filepath.Join(root, "sidecar-alias")); err != nil {
				t.Fatalf("create sidecar hardlink alias: %v", err)
			}
			if _, err := Resolve(Options{LocatorPath: locator, Identity: &identity}); !errors.Is(err, ErrDomainUnavailable) {
				t.Fatalf("Resolve() with hardlinked sidecar = %v, want ErrDomainUnavailable", err)
			}
		})
	}
}

func TestResolveRejectsSymlinksAndWeakPermissions(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "canonical")
	locator := filepath.Join(root, "locator", LocatorFile)
	identity := testIdentity()
	registerFixture(t, locator, stateDir, identity)

	link := filepath.Join(root, "state-link")
	if err := os.Symlink(stateDir, link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(locator, []byte(`{"version":1,"canonical_dir":"`+link+`","domain_id":"`+identity.ConfigIdentity+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(Options{LocatorPath: locator, Identity: &identity}); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlinked domain error = %v, want ErrUnsafePath", err)
	}

	registerFixture(t, locator, stateDir, identity)
	if err := os.Chmod(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(Options{LocatorPath: locator, Identity: &identity}); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("permissive domain error = %v, want ErrUnsafePath", err)
	}
}

func TestResolveRejectsSymlinkPathAlias(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "canonical")
	locator := filepath.Join(root, "locator", LocatorFile)
	identity := testIdentity()
	registerFixture(t, locator, stateDir, identity)
	link := filepath.Join(root, "state-link")
	if err := os.Symlink(stateDir, link); err != nil {
		t.Fatal(err)
	}
	alias := root + string(filepath.Separator) + "state-link" + string(filepath.Separator) + ".." + string(filepath.Separator) + "canonical"
	_, err := Resolve(Options{LocatorPath: locator, Identity: &identity, Overrides: []string{alias}})
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("symlink path alias error = %v, want ErrUnsafePath", err)
	}
}

func TestResolveRejectsDuplicateLocatorKeys(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "canonical")
	locator := filepath.Join(root, "locator", LocatorFile)
	identity := testIdentity()
	registerFixture(t, locator, stateDir, identity)
	data, err := os.ReadFile(locator)
	if err != nil {
		t.Fatal(err)
	}
	duplicated := strings.Replace(string(data), `"version":1`, `"version":1,"version":1`, 1)
	if err := os.WriteFile(locator, []byte(duplicated), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(Options{LocatorPath: locator, Identity: &identity}); !errors.Is(err, ErrDomainUnavailable) {
		t.Fatalf("duplicate-key locator error = %v, want ErrDomainUnavailable", err)
	}
}

func TestResolveAllowsSQLiteSidecarsReadOnlyForLockedRecovery(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, "canonical")
	locator := filepath.Join(root, "locator", LocatorFile)
	identity := testIdentity()
	registerFixture(t, locator, stateDir, identity)
	wal := filepath.Join(stateDir, "runstead.db-wal")
	if err := os.WriteFile(wal, []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(Options{LocatorPath: locator, Identity: &identity}); err != nil {
		t.Fatalf("read-only resolver error = %v", err)
	}
	if _, err := os.Stat(wal); err != nil {
		t.Fatalf("resolver modified or removed WAL sidecar: %v", err)
	}
}

func TestDefaultLocatorPathUsesHomeAndXDGOnlyForDiscovery(t *testing.T) {
	home := t.TempDir()
	xdg := filepath.Join(t.TempDir(), "state")
	path, err := DefaultLocatorPath(home, xdg)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(xdg, "runstead", "siwc", LocatorFile)
	if path != want {
		t.Fatalf("locator path = %q, want %q", path, want)
	}
	changed, err := DefaultLocatorPath(t.TempDir(), filepath.Join(t.TempDir(), "other"))
	if err != nil {
		t.Fatal(err)
	}
	if changed == path {
		t.Fatal("changed XDG/HOME unexpectedly resolved the previous locator")
	}
}
