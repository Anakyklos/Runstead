//go:build linux

package siwcauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testRegistration(store *Store, t *testing.T) Registration {
	t.Helper()
	host, err := store.HostID(true)
	if err != nil {
		t.Fatal(err)
	}
	return Registration{Issuer: Issuer, Subject: "offline-subject", ClientID: "offline-issued-client", HostID: host, Email: "private@example.invalid", Scopes: append([]string(nil), requiredScopes[:]...), ExpiresAt: time.Now().Add(time.Hour), tokens: TokenSet{AccessToken: "access-secret-fixture", RefreshToken: "refresh-secret-fixture", IDToken: "id-token-secret-fixture", TokenType: "Bearer"}}
}

func TestStorePrivateAtomicCustodyAndVerifiedBindingLookup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "config", "runstead", "siwc")
	store, err := OpenStore(root, true)
	if err != nil {
		t.Fatal(err)
	}
	reg := testRegistration(store, t)
	if err := store.Save(reg); err != nil {
		t.Fatal(err)
	}
	account, credential, err := store.Bindings(reg.Public(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyBindings(account, credential); err != nil {
		t.Fatal(err)
	}
	if err := store.VerifyBindings(account, credential+"x"); err == nil {
		t.Fatal("accepted nonmatching credential binding")
	}
	loaded, err := store.Load(reg.ClientID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Subject != reg.Subject || loaded.Public().ClientID != reg.ClientID {
		t.Fatalf("registration mismatch: %#v", loaded.Public())
	}
	if got := loaded.String() + " " + loaded.GoString() + " " + loaded.Public().Subject; containsSecret(got) {
		t.Fatalf("credential leaked through formatting: %q", got)
	}
	files, err := os.ReadDir(filepath.Join(root, "registrations"))
	if err != nil || len(files) != 1 {
		t.Fatalf("registration files=%v err=%v", files, err)
	}
	for _, path := range []string{root, filepath.Join(root, "registrations"), filepath.Join(root, "binding.key"), filepath.Join(root, "host-id"), filepath.Join(root, "registrations", files[0].Name())} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o700)
		if !info.IsDir() {
			want = 0o600
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s mode=%#o want %#o", path, info.Mode().Perm(), want)
		}
	}
	if _, err := os.Stat(filepath.Join(t.TempDir(), "runstead.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("credential custody created SQLite state")
	}
}

func TestWithActiveRegistrationRejectsChangedSessionAtCommitBoundary(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "siwc"), true)
	if err != nil {
		t.Fatal(err)
	}
	expected := testRegistration(store, t)
	if err := store.Save(expected); err != nil {
		t.Fatal(err)
	}
	called := false
	if err := store.WithActiveRegistration(expected, func() error { called = true; return nil }); err != nil || !called {
		t.Fatalf("active unchanged registration commit: called=%v err=%v", called, err)
	}
	changed := expected
	changed.tokens.AccessToken = "new-session-token"
	if err := store.Save(changed); err != nil {
		t.Fatal(err)
	}
	called = false
	if err := store.WithActiveRegistration(expected, func() error { called = true; return nil }); !errors.Is(err, ErrIdentityChanged) || called {
		t.Fatalf("changed session commit: called=%v err=%v, want identity changed before callback", called, err)
	}
}

func TestIssuedClientIDCannotBeReboundToAnotherSubject(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "siwc"), true)
	if err != nil {
		t.Fatal(err)
	}
	first := testRegistration(store, t)
	if err := store.Save(first); err != nil {
		t.Fatal(err)
	}
	changed := first
	changed.Subject = "different-subject"
	if err := store.Save(changed); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("rebound client ID err=%v, want identity changed", err)
	}
	loaded, err := store.Load(first.ClientID)
	if err != nil || loaded.Subject != first.Subject {
		t.Fatalf("original binding changed: subject=%q err=%v", loaded.Subject, err)
	}
}

func TestDistinctRegistrationsWithSameEmailRemainSeparate(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "siwc"), true)
	if err != nil {
		t.Fatal(err)
	}
	first := testRegistration(store, t)
	if err := store.Save(first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.ClientID = "another-issued-client"
	second.Subject = "another-subject"
	if err := store.Save(second); err != nil {
		t.Fatal(err)
	}
	registrations, err := store.Registrations()
	if err != nil || len(registrations) != 2 {
		t.Fatalf("registrations=%d err=%v", len(registrations), err)
	}
	for _, want := range []Registration{first, second} {
		got, err := store.Load(want.ClientID)
		if err != nil || got.Subject != want.Subject || got.ClientID != want.ClientID {
			t.Fatalf("client %q resolved to subject %q, err=%v", want.ClientID, got.Subject, err)
		}
	}
}

func TestRefreshIntentSurvivesFailureAndPreventsReplay(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "runstead", "siwc"), true)
	if err != nil {
		t.Fatal(err)
	}
	reg := testRegistration(store, t)
	reg.ExpiresAt = time.Now().Add(time.Second)
	if err := store.Save(reg); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	_, err = store.refresh(reg.ClientID, time.Now().Add(time.Minute), func(Registration) (Registration, error) {
		calls.Add(1)
		return Registration{}, errors.New("simulated lost response")
	})
	if err == nil {
		t.Fatal("refresh failure not returned")
	}
	if _, err := store.Load(reg.ClientID); !errors.Is(err, ErrRefreshUncertain) {
		t.Fatalf("Load error=%v want uncertain", err)
	}
	_, err = store.refresh(reg.ClientID, time.Now().Add(time.Minute), func(Registration) (Registration, error) { calls.Add(1); return Registration{}, nil })
	if !errors.Is(err, ErrRefreshUncertain) || calls.Load() != 1 {
		t.Fatalf("second refresh err=%v calls=%d", err, calls.Load())
	}
}

func TestExplicitSuccessfulLoginClearsRefreshUncertainty(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "siwc"), true)
	if err != nil {
		t.Fatal(err)
	}
	reg := testRegistration(store, t)
	reg.ExpiresAt = time.Now().Add(time.Second)
	if err := store.Save(reg); err != nil {
		t.Fatal(err)
	}
	_, err = store.refresh(reg.ClientID, time.Now().Add(time.Minute), func(Registration) (Registration, error) {
		return Registration{}, errors.New("simulated lost refresh response")
	})
	if err == nil {
		t.Fatal("expected refresh uncertainty")
	}
	if _, err := store.Load(reg.ClientID); !errors.Is(err, ErrRefreshUncertain) {
		t.Fatalf("pre-login Load err=%v", err)
	}
	reg.tokens = TokenSet{AccessToken: "fresh-access", RefreshToken: "fresh-refresh", IDToken: "fresh-id", TokenType: "Bearer"}
	reg.ExpiresAt = time.Now().Add(time.Hour)
	if err := store.Save(reg); err != nil {
		t.Fatalf("explicitly authenticated replacement could not clear uncertainty: %v", err)
	}
	loaded, err := store.Load(reg.ClientID)
	if err != nil || loaded.tokens.RefreshToken != "fresh-refresh" {
		t.Fatalf("replacement credential unavailable: err=%v", err)
	}
}

func TestRefreshProcessHelper(t *testing.T) {
	mode := os.Getenv("RUNSTEAD_SIWC_REFRESH_HELPER")
	if mode == "" {
		return
	}
	store, err := OpenStore(os.Getenv("RUNSTEAD_SIWC_REFRESH_STORE"), false)
	if err != nil {
		t.Fatal(err)
	}
	clientID := os.Getenv("RUNSTEAD_SIWC_REFRESH_CLIENT")
	_, err = store.refresh(clientID, time.Now().Add(5*time.Minute), func(current Registration) (Registration, error) {
		if mode == "crash" {
			if err := os.WriteFile(os.Getenv("RUNSTEAD_SIWC_REFRESH_READY"), []byte("pending"), 0o600); err != nil {
				t.Fatal(err)
			}
			select {}
		}
		file, err := os.OpenFile(os.Getenv("RUNSTEAD_SIWC_REFRESH_CALLS"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fmt.Fprintln(file, current.tokens.RefreshToken)
		_ = file.Close()
		time.Sleep(200 * time.Millisecond)
		return Registration{Issuer: current.Issuer, Subject: current.Subject, ClientID: current.ClientID, HostID: current.HostID, Email: current.Email, Scopes: current.Scopes, ExpiresAt: time.Now().Add(time.Hour), Model: current.Model, tokens: TokenSet{AccessToken: "rotated-access", RefreshToken: "rotated-refresh", IDToken: current.tokens.IDToken, TokenType: "Bearer"}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRefreshSerializesAcrossProcessesAndSkipsFreshSession(t *testing.T) {
	root := filepath.Join(t.TempDir(), "credentials", "siwc")
	store, err := OpenStore(root, true)
	if err != nil {
		t.Fatal(err)
	}
	reg := testRegistration(store, t)
	reg.ExpiresAt = time.Now().Add(-time.Minute)
	if err := store.Save(reg); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(t.TempDir(), "calls")
	start := func() *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRefreshProcessHelper$")
		cmd.Env = append(os.Environ(), "RUNSTEAD_SIWC_REFRESH_HELPER=rotate", "RUNSTEAD_SIWC_REFRESH_STORE="+root, "RUNSTEAD_SIWC_REFRESH_CLIENT="+reg.ClientID, "RUNSTEAD_SIWC_REFRESH_CALLS="+calls)
		return cmd
	}
	a, b := start(), start()
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(); err != nil {
		_ = a.Process.Kill()
		t.Fatal(err)
	}
	if err := a.Wait(); err != nil {
		_ = b.Process.Kill()
		t.Fatal(err)
	}
	if err := b.Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "\n"); got != 1 {
		t.Fatalf("physical refresh callbacks=%d, want one; values=%q", got, string(data))
	}
	updated, err := store.Load(reg.ClientID)
	if err != nil || updated.tokens.RefreshToken != "rotated-refresh" {
		t.Fatalf("refreshed session=%v err=%v", updated.Public(), err)
	}
}

func TestRefreshCrashLeavesDurableUncertainMarkerAcrossProcesses(t *testing.T) {
	root := filepath.Join(t.TempDir(), "credentials", "siwc")
	store, err := OpenStore(root, true)
	if err != nil {
		t.Fatal(err)
	}
	reg := testRegistration(store, t)
	reg.ExpiresAt = time.Now().Add(-time.Minute)
	if err := store.Save(reg); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "refresh-ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestRefreshProcessHelper$")
	cmd.Env = append(os.Environ(), "RUNSTEAD_SIWC_REFRESH_HELPER=crash", "RUNSTEAD_SIWC_REFRESH_STORE="+root, "RUNSTEAD_SIWC_REFRESH_CLIENT="+reg.ClientID, "RUNSTEAD_SIWC_REFRESH_READY="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("refresh process did not persist intent")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if _, err := store.Load(reg.ClientID); !errors.Is(err, ErrRefreshUncertain) {
		t.Fatalf("load after process crash=%v, want uncertain", err)
	}
	var calls atomic.Int32
	_, err = store.refresh(reg.ClientID, time.Now().Add(time.Minute), func(Registration) (Registration, error) { calls.Add(1); return Registration{}, nil })
	if !errors.Is(err, ErrRefreshUncertain) || calls.Load() != 0 {
		t.Fatalf("post-crash refresh err=%v callback-count=%d", err, calls.Load())
	}
}

func TestStoreRefusesSymlinkAndRelaxedPermissions(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(filepath.Join(link, "siwc"), true); err == nil {
		t.Fatal("accepted symlink custody path")
	}
	shared := filepath.Join(base, "shared")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(shared, false); err == nil {
		t.Fatal("accepted non-private credential directory")
	}
}

func TestStoreRejectsCorruptAndHardlinkedCredentialRecords(t *testing.T) {
	root := filepath.Join(t.TempDir(), "siwc")
	store, err := OpenStore(root, true)
	if err != nil {
		t.Fatal(err)
	}
	reg := testRegistration(store, t)
	if err := store.Save(reg); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(root, "registrations", registrationID(reg.Public())+".json")
	contents, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(record, append(contents[:len(contents)-1], []byte(`,"version":1}`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(reg.ClientID); err == nil {
		t.Fatal("accepted duplicate-key/corrupt credential JSON")
	}
	if err := os.WriteFile(record, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(record, filepath.Join(root, "credential-alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(reg.ClientID); err == nil {
		t.Fatal("accepted a hardlinked credential record")
	}
}

func TestRevocationFailureRetainsLocalCredentials(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "siwc"), true)
	if err != nil {
		t.Fatal(err)
	}
	reg := testRegistration(store, t)
	if err := store.Save(reg); err != nil {
		t.Fatal(err)
	}
	issuer := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/openid-configuration" {
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "revocation_endpoint": issuer + "/revoke"})
			return
		}
		if r.URL.Path == "/revoke" {
			http.Error(w, "offline fixture", http.StatusServiceUnavailable)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	issuer = server.URL
	endpoints := Endpoints{Issuer: issuer, Discovery: issuer + "/.well-known/openid-configuration"}
	if err := store.RevokeAndClear(context.Background(), server.Client(), endpoints, reg.ClientID); err == nil {
		t.Fatal("reported revocation success after issuer failure")
	}
	loaded, err := store.Load(reg.ClientID)
	if err != nil || loaded.tokens.RefreshToken != reg.tokens.RefreshToken {
		t.Fatalf("failed revocation did not retain credential for explicit retry: err=%v", err)
	}
}

func containsSecret(value string) bool {
	return strings.Contains(value, "access-secret-fixture") || strings.Contains(value, "refresh-secret-fixture") || strings.Contains(value, "id-token-secret-fixture")
}
