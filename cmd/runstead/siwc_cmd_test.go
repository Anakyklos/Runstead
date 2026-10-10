package main

import (
	"bytes"
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
	"testing"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/siwcauth"
	"github.com/RenyEnnos/Runstead/internal/siwcstate"
)

func TestSIWCSetupProcessHelper(t *testing.T) {
	mode := os.Getenv("RUNSTEAD_SIWC_SETUP_HELPER")
	if mode == "" {
		return
	}
	endpoints := siwcauth.Endpoints{
		Issuer: siwcauth.Issuer, Discovery: os.Getenv("RUNSTEAD_SIWC_TEST_DISCOVERY"),
		Catalog: os.Getenv("RUNSTEAD_SIWC_TEST_CATALOG"),
	}
	args := []string{"--client-id", "issued-client", "--model", "gpt-test", "--state-dir", os.Getenv("RUNSTEAD_SIWC_TEST_STATE"), "--providers", os.Getenv("RUNSTEAD_SIWC_TEST_PROVIDERS")}
	var out, errOut bytes.Buffer
	code := siwcSetupWithEndpoints(context.Background(), args, &out, &errOut, endpoints)
	fmt.Fprintf(os.Stdout, "SETUP_EXIT=%d\n", code)
	if mode == "expect-success" && code != exitSuccess {
		t.Fatalf("setup failed: %d: %s", code, errOut.String())
	}
	if mode == "expect-refusal" && code == exitSuccess {
		t.Fatalf("setup unexpectedly committed: %s", out.String())
	}
}

func TestSIWCLogoutProcessHelper(t *testing.T) {
	if os.Getenv("RUNSTEAD_SIWC_LOGOUT_HELPER") != "1" {
		return
	}
	store, err := openSIWCStore(false)
	if err != nil {
		t.Fatal(err)
	}
	endpoints := siwcauth.Endpoints{
		Issuer: siwcauth.Issuer, Discovery: os.Getenv("RUNSTEAD_SIWC_TEST_DISCOVERY"),
	}
	if err := store.RevokeAndClear(context.Background(), nil, endpoints, "issued-client"); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(os.Stdout, "LOGOUT_COMMITTED")
}

func TestSIWCStatusDoesNotRenderCredentialMaterial(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	xdg := filepath.Join(base, "xdg")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	setSIWCEnv(t, home, xdg)
	identity := cliSIWCIdentity()
	seedSIWCCredential(t, home, identity)
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"siwc", "status"}, &out, &errOut); code != exitSuccess {
		t.Fatalf("status exit=%d stderr=%q", code, errOut.String())
	}
	for _, secret := range []string{"offline-access-fixture", "offline-refresh-fixture", "offline-id-fixture"} {
		if strings.Contains(out.String()+errOut.String(), secret) {
			t.Fatalf("status leaked credential %q", secret)
		}
	}
	if !strings.Contains(out.String(), "authenticated") || !strings.Contains(out.String(), "inference in this stage") {
		t.Fatalf("status output=%q", out.String())
	}
}

func TestSIWCCommandHelpDoesNotEnableInference(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(context.Background(), []string{"siwc", "--help"}, &out, &errOut); code != exitSuccess {
		t.Fatalf("help exit=%d stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Responses inference in Stage 4") || strings.Contains(out.String(), "responses API request") {
		t.Fatalf("unexpected SIWC help text: %q", out.String())
	}
}

func TestSIWCProviderConfigCreationRefusesSymlinksAndOverwrite(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	if err := writeSIWCProviderFile(filepath.Join(link, "providers.json"), provider.Config{}); err == nil {
		t.Fatal("wrote provider configuration through a symlinked parent")
	}
	path := filepath.Join(realDir, "providers.json")
	if err := writeSIWCProviderFile(path, provider.Config{}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("provider file permissions: info=%v err=%v", info, err)
	}
	if err := writeSIWCProviderFile(path, provider.Config{}); err == nil {
		t.Fatal("overwrote existing provider configuration")
	}
}

func TestSIWCSetupSerializesFinalCommitWithLogoutAcrossProcesses(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	xdg := filepath.Join(base, "xdg")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	setSIWCEnv(t, home, xdg)
	seedSIWCCredential(t, home, cliSIWCIdentity())
	stateDir := filepath.Join(base, "state")
	providers := filepath.Join(base, "providers.json")
	catalogStarted := make(chan struct{}, 1)
	allowCatalogResponse := make(chan struct{})
	logoutFinished := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/catalog":
			catalogStarted <- struct{}{}
			<-allowCatalogResponse
			_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-test","display_name":"fixture","visibility":"list"}]}`))
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": siwcauth.Issuer, "revocation_endpoint": "http://" + r.Host + "/revoke"})
		case "/revoke":
			logoutFinished <- struct{}{}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	// httptest binds loopback; its URL is an explicitly allowed offline OAuth
	// endpoint. Keep the fixture issuer fixed to the official registration.
	localBase := server.URL
	discovery := localBase + "/.well-known/openid-configuration"
	cliBinary := os.Args[0]
	commonEnv := []string{"HOME=" + home, "XDG_STATE_HOME=" + xdg, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"RUNSTEAD_SIWC_TEST_DISCOVERY=" + discovery, "RUNSTEAD_SIWC_TEST_CATALOG=" + localBase + "/catalog",
		"RUNSTEAD_SIWC_TEST_STATE=" + stateDir, "RUNSTEAD_SIWC_TEST_PROVIDERS=" + providers}
	setup := exec.Command(cliBinary, "-test.run=^TestSIWCSetupProcessHelper$")
	setup.Env = append(os.Environ(), commonEnv...)
	setup.Env = append(setup.Env, "RUNSTEAD_SIWC_SETUP_HELPER=expect-refusal")
	setupOutput := make(chan []byte, 1)
	go func() { output, _ := setup.CombinedOutput(); setupOutput <- output }()
	select {
	case <-catalogStarted:
	case <-time.After(5 * time.Second):
		_ = setup.Process.Kill()
		t.Fatal("setup process did not reach the local catalog")
	}
	logout := exec.Command(cliBinary, "-test.run=^TestSIWCLogoutProcessHelper$")
	logout.Env = append(os.Environ(), commonEnv...)
	logout.Env = append(logout.Env, "RUNSTEAD_SIWC_LOGOUT_HELPER=1")
	logoutOutput, err := logout.CombinedOutput()
	if err != nil || !strings.Contains(string(logoutOutput), "LOGOUT_COMMITTED") {
		close(allowCatalogResponse)
		t.Fatalf("separate logout process failed: %v: %s", err, logoutOutput)
	}
	select {
	case <-logoutFinished:
	case <-time.After(5 * time.Second):
		close(allowCatalogResponse)
		t.Fatal("logout did not revoke through the local endpoint")
	}
	close(allowCatalogResponse)
	select {
	case output := <-setupOutput:
		if !strings.Contains(string(output), "SETUP_EXIT=") || !strings.Contains(string(output), "SETUP_EXIT=3") {
			t.Fatalf("setup did not refuse after concurrent logout: %s", output)
		}
	case <-time.After(10 * time.Second):
		_ = setup.Process.Kill()
		t.Fatal("setup process did not finish after catalog response")
	}
	for _, path := range []string{stateDir, providers, filepath.Join(xdg, "runstead", "siwc", "siwc-locator.json")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("concurrently signed-out setup left artifact %s (err=%v)", path, err)
		}
	}
}

func TestSIWCSetupCommitsWithCurrentAuthenticatedRegistration(t *testing.T) {
	base := t.TempDir()
	home, xdg := filepath.Join(base, "home"), filepath.Join(base, "xdg")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	setSIWCEnv(t, home, xdg)
	seedSIWCCredential(t, home, cliSIWCIdentity())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/catalog" {
			_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-test","visibility":"list"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	endpoints := siwcauth.Endpoints{Issuer: siwcauth.Issuer, Discovery: server.URL + "/.well-known/openid-configuration", Catalog: server.URL + "/catalog"}
	stateDir, providers := filepath.Join(base, "state"), filepath.Join(base, "providers.json")
	args := []string{"--client-id", "issued-client", "--model", "gpt-test", "--state-dir", stateDir, "--providers", providers}
	var out, errOut bytes.Buffer
	if code := siwcSetupWithEndpoints(context.Background(), args, &out, &errOut, endpoints); code != exitSuccess {
		t.Fatalf("positive setup exit=%d stderr=%q", code, errOut.String())
	}
	for _, path := range []string{stateDir, providers, filepath.Join(xdg, "runstead", "siwc", "siwc-locator.json")} {
		if info, err := os.Stat(path); err != nil || (path == providers && info.Mode().Perm() != 0o600) {
			t.Fatalf("successful setup artifact %s info=%v err=%v", path, info, err)
		}
	}
}

func TestSIWCSetupPublishesProviderAndDomainTransactionally(t *testing.T) {
	base := t.TempDir()
	home, xdg := filepath.Join(base, "home"), filepath.Join(base, "xdg")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	setSIWCEnv(t, home, xdg)
	seedSIWCCredential(t, home, cliSIWCIdentity())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/catalog" {
			_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-test","visibility":"list"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	endpoints := siwcauth.Endpoints{Issuer: siwcauth.Issuer, Discovery: server.URL + "/.well-known/openid-configuration", Catalog: server.URL + "/catalog"}
	stateDir, providers := filepath.Join(base, "state"), filepath.Join(base, "providers.json")
	locator := filepath.Join(xdg, "runstead", "siwc", siwcstate.LocatorFile)
	args := []string{"--client-id", "issued-client", "--model", "gpt-test", "--state-dir", stateDir, "--providers", providers}
	runSetup := func(hook func() error) (int, string) {
		var out, errOut bytes.Buffer
		code := siwcSetupWithEndpointsHook(context.Background(), args, &out, &errOut, endpoints, hook)
		return code, errOut.String()
	}
	runSetupAfterProviderPublish := func(hook func() error) (int, string) {
		var out, errOut bytes.Buffer
		code := siwcSetupWithLifecycleHooks(context.Background(), args, &out, &errOut, endpoints, nil, hook)
		return code, errOut.String()
	}
	assertUnpublished := func() {
		t.Helper()
		for _, path := range []string{stateDir, filepath.Join(stateDir, "runstead.db"), locator} {
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("failed setup left discoverable artifact %s (err=%v)", path, err)
			}
		}
		staged, err := filepath.Glob(filepath.Join(base, ".runstead-siwc-provider-*.stage"))
		if err != nil || len(staged) != 0 {
			t.Fatalf("staging files were not cleaned: paths=%v err=%v", staged, err)
		}
	}

	// A destination present before setup is refused before any state or
	// locator is created, and its bytes and mode remain operator-owned.
	want := []byte("operator-owned provider configuration\n")
	if err := os.WriteFile(providers, want, 0o640); err != nil {
		t.Fatal(err)
	}
	if code, _ := runSetup(nil); code == exitSuccess {
		t.Fatal("setup accepted an existing provider output")
	}
	got, err := os.ReadFile(providers)
	info, statErr := os.Stat(providers)
	if err != nil || statErr != nil || string(got) != string(want) || info.Mode().Perm() != 0o640 {
		t.Fatalf("existing provider file changed: bytes=%q info=%v readErr=%v statErr=%v", got, info, err, statErr)
	}
	assertUnpublished()
	if err := os.Remove(providers); err != nil {
		t.Fatal(err)
	}

	// Simulate another writer winning the no-overwrite publication race after
	// the private output has been staged. Rollback must preserve that writer's
	// inode and remove the undiscoverable database/manifest/lock directory.
	raceBytes := []byte("concurrent operator file\n")
	code, message := runSetup(func() error { return os.WriteFile(providers, raceBytes, 0o640) })
	if code == exitSuccess || !strings.Contains(message, "initialization refused") {
		t.Fatalf("post-staging publication race was not refused: exit=%d stderr=%q", code, message)
	}
	got, err = os.ReadFile(providers)
	info, statErr = os.Stat(providers)
	if err != nil || statErr != nil || string(got) != string(raceBytes) || info.Mode().Perm() != 0o640 {
		t.Fatalf("racing provider file was changed: bytes=%q info=%v readErr=%v statErr=%v", got, info, err, statErr)
	}
	assertUnpublished()
	if err := os.Remove(providers); err != nil {
		t.Fatal(err)
	}

	// Force the locator destination to be claimed after provider publication.
	// Locator remains the discoverability boundary; failed publication must
	// remove only the provider inode created by this transaction.
	locatorBytes := []byte("operator-owned locator\n")
	code, message = runSetupAfterProviderPublish(func() error { return os.WriteFile(locator, locatorBytes, 0o640) })
	if code == exitSuccess || !strings.Contains(message, "initialization refused") {
		t.Fatalf("post-provider locator race was not refused: exit=%d stderr=%q", code, message)
	}
	got, err = os.ReadFile(locator)
	info, statErr = os.Stat(locator)
	if err != nil || statErr != nil || string(got) != string(locatorBytes) || info.Mode().Perm() != 0o640 {
		t.Fatalf("racing locator file was changed: bytes=%q info=%v readErr=%v statErr=%v", got, info, err, statErr)
	}
	for _, path := range []string{stateDir, providers, filepath.Join(stateDir, "runstead.db")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("failed final publication left artifact %s (err=%v)", path, err)
		}
	}
	if err := os.Remove(locator); err != nil {
		t.Fatal(err)
	}

	// The same authenticated setup can be retried after the unrelated output is
	// removed; it publishes a matching provider document and canonical domain.
	if code, message := runSetup(nil); code != exitSuccess {
		t.Fatalf("setup did not succeed after safe refusal: exit=%d stderr=%q", code, message)
	}
	var doc struct {
		Version   int `json:"version"`
		Providers []struct {
			ProviderID string `json:"provider_id"`
			Model      string `json:"model"`
			Account    string `json:"account_binding"`
			Credential string `json:"credential_binding"`
		} `json:"providers"`
	}
	data, err := os.ReadFile(providers)
	if err != nil || json.Unmarshal(data, &doc) != nil || doc.Version != 2 || len(doc.Providers) != 1 || doc.Providers[0].ProviderID != "siwc-openai" || doc.Providers[0].Model != "gpt-test" {
		t.Fatalf("provider config does not match successful domain: config=%+v err=%v", doc, err)
	}
	manifestData, err := os.ReadFile(filepath.Join(stateDir, siwcstate.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	var manifest siwcstate.Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	if doc.Providers[0].Account != manifest.AccountBinding || doc.Providers[0].Credential != manifest.CredentialBinding {
		t.Fatalf("provider and canonical domain bindings disagree: provider=%+v manifest=%+v", doc.Providers[0], manifest)
	}
}

func TestSIWCSetupPreservesUnexpectedDomainArtifactOnRollback(t *testing.T) {
	base := t.TempDir()
	home, xdg := filepath.Join(base, "home"), filepath.Join(base, "xdg")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	setSIWCEnv(t, home, xdg)
	seedSIWCCredential(t, home, cliSIWCIdentity())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/catalog" {
			_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-test","visibility":"list"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	endpoints := siwcauth.Endpoints{Issuer: siwcauth.Issuer, Discovery: server.URL + "/.well-known/openid-configuration", Catalog: server.URL + "/catalog"}
	stateDir, providers := filepath.Join(base, "new-state"), filepath.Join(base, "providers.json")
	locator := filepath.Join(xdg, "runstead", "siwc", siwcstate.LocatorFile)
	if _, err := os.Lstat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("test state directory must start absent: %v", err)
	}
	args := []string{"--client-id", "issued-client", "--model", "gpt-test", "--state-dir", stateDir, "--providers", providers}
	sentinelPath := filepath.Join(stateDir, "operator-owned-evidence")
	sentinel := []byte("preserve this unrelated operator artifact\n")
	injectedFailure := errors.New("injected failure before locator publication")
	var out, errOut bytes.Buffer
	code := siwcSetupWithLifecycleHooks(context.Background(), args, &out, &errOut, endpoints, nil, func() error {
		if err := os.WriteFile(sentinelPath, sentinel, 0o600); err != nil {
			return err
		}
		return injectedFailure
	})
	if code == exitSuccess || !strings.Contains(errOut.String(), injectedFailure.Error()) {
		t.Fatalf("injected pre-locator failure was not propagated: exit=%d stderr=%q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "preserve incomplete SIWC state") {
		t.Fatalf("partial-state preservation was not reported explicitly: %q", errOut.String())
	}
	got, err := os.ReadFile(sentinelPath)
	if err != nil || !bytes.Equal(got, sentinel) {
		t.Fatalf("operator sentinel was not preserved: bytes=%q err=%v", got, err)
	}
	for _, path := range []string{locator, providers, filepath.Join(stateDir, "runstead.db"), filepath.Join(stateDir, siwcstate.ManifestFile)} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("failed setup published or retained owned artifact %s (err=%v)", path, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(stateDir, ".siwc-domain-lock-v1")); err != nil {
		t.Fatalf("partial domain did not preserve the persistent lock marker with unexpected content: %v", err)
	}
	if _, err := siwcstate.Resolve(siwcstate.Options{LocatorPath: locator}); !errors.Is(err, siwcstate.ErrDomainUnavailable) {
		t.Fatalf("failed setup left an operational locator/domain: resolve error=%v", err)
	}

	// A subsequent setup attempt sees the retained unexpected content and fails
	// at exclusive directory creation without changing it.
	var retryOut, retryErr bytes.Buffer
	retryCode := siwcSetupWithLifecycleHooks(context.Background(), args, &retryOut, &retryErr, endpoints, nil, nil)
	if retryCode == exitSuccess || !strings.Contains(retryErr.String(), "cannot create requested SIWC state directory") {
		t.Fatalf("retry did not fail deterministically on preserved partial state: exit=%d stderr=%q", retryCode, retryErr.String())
	}
	got, err = os.ReadFile(sentinelPath)
	if err != nil || !bytes.Equal(got, sentinel) {
		t.Fatalf("retry changed preserved sentinel: bytes=%q err=%v", got, err)
	}
	if _, err := os.Lstat(locator); !os.IsNotExist(err) {
		t.Fatalf("retry published locator for incomplete state (err=%v)", err)
	}
}
