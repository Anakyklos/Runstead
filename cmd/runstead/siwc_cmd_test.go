package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/provider"
)

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
