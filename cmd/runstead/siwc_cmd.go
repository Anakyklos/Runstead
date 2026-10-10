package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/RenyEnnos/Runstead/internal/provider"
	"github.com/RenyEnnos/Runstead/internal/provider/compat"
	"github.com/RenyEnnos/Runstead/internal/siwcauth"
	"github.com/RenyEnnos/Runstead/internal/siwcstate"
)

func siwcCommand(ctx context.Context, args []string, out, errOut io.Writer) int {
	if len(args) == 0 || isHelp(args[0]) {
		printSIWCHelp(out)
		return exitSuccess
	}
	if runtime.GOOS != "linux" {
		fmt.Fprintln(errOut, "siwc: credential custody and callback are supported only on Linux")
		return exitUnavailable
	}
	switch args[0] {
	case "login":
		return siwcLogin(ctx, args[1:], out, errOut)
	case "models":
		return siwcModels(ctx, args[1:], out, errOut)
	case "logout":
		return siwcLogout(ctx, args[1:], out, errOut)
	case "setup":
		return siwcSetup(ctx, args[1:], out, errOut)
	case "status":
		return siwcStatus(args[1:], out, errOut)
	default:
		fmt.Fprintf(errOut, "siwc: unknown subcommand %q\n", args[0])
		printSIWCHelp(errOut)
		return exitUsage
	}
}

func openSIWCStore(create bool) (*siwcauth.Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, errors.New("cannot resolve home directory")
	}
	path, err := siwcauth.DefaultStorePath(home, os.Getenv("XDG_CONFIG_HOME"))
	if err != nil {
		return nil, err
	}
	return siwcauth.OpenStore(path, create)
}

func siwcLogin(ctx context.Context, args []string, out, errOut io.Writer) int {
	flags := flag.NewFlagSet("siwc login", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	clientID := ""
	flags.StringVar(&clientID, "client-id", "", "issued client ID for reauthorization; omit for a new account registration")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(errOut, "siwc login: invalid flags")
		return exitUsage
	}
	store, err := openSIWCStore(true)
	if err != nil {
		fmt.Fprintf(errOut, "siwc login: protected credential store unavailable: %v\n", err)
		return exitUnavailable
	}
	reg, err := siwcauth.Login(ctx, store, nil, clientID, siwcauth.OpenSystemBrowser)
	if err != nil {
		fmt.Fprintf(errOut, "siwc login: sign-in failed: %v\n", err)
		return exitUnavailable
	}
	fmt.Fprintf(out, "SIWC credentials stored for issued client %q; inference remains unavailable.\n", reg.ClientID)
	return exitSuccess
}

func siwcModels(ctx context.Context, args []string, out, errOut io.Writer) int {
	flags := flag.NewFlagSet("siwc models", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	clientID := ""
	flags.StringVar(&clientID, "client-id", "", "issued client ID to query")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || clientID == "" {
		fmt.Fprintln(errOut, "siwc models: --client-id is required")
		return exitUsage
	}
	store, err := openSIWCStore(false)
	if err != nil {
		fmt.Fprintln(errOut, "siwc models: protected credential store unavailable")
		return exitUnavailable
	}
	models, err := store.Catalog(ctx, nil, siwcauth.OpenAIEndpoints(), clientID)
	if err != nil {
		fmt.Fprintf(errOut, "siwc models: catalog unavailable: %v\n", err)
		return exitUnavailable
	}
	for _, model := range models {
		fmt.Fprintf(out, "%q\t%q\n", model.Slug, model.DisplayName)
	}
	fmt.Fprintln(errOut, "Catalog visibility is display metadata and does not establish usage eligibility; Responses inference remains unavailable.")
	return exitSuccess
}

func siwcLogout(ctx context.Context, args []string, out, errOut io.Writer) int {
	flags := flag.NewFlagSet("siwc logout", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	clientID := ""
	flags.StringVar(&clientID, "client-id", "", "issued client ID to revoke")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || clientID == "" {
		fmt.Fprintln(errOut, "siwc logout: --client-id is required")
		return exitUsage
	}
	store, err := openSIWCStore(false)
	if err != nil {
		fmt.Fprintln(errOut, "siwc logout: protected credential store unavailable")
		return exitUnavailable
	}
	if err := store.RevokeAndClear(ctx, nil, siwcauth.OpenAIEndpoints(), clientID); err != nil {
		fmt.Fprintf(errOut, "siwc logout: revocation was not confirmed; local credentials were retained: %v\n", err)
		return exitUnavailable
	}
	fmt.Fprintln(out, "SIWC renewable session revoked and local tokens cleared; registration identity retained.")
	return exitSuccess
}

func siwcStatus(args []string, out, errOut io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintln(errOut, "siwc status: unexpected arguments")
		return exitUsage
	}
	store, err := openSIWCStore(false)
	if err != nil {
		fmt.Fprintln(errOut, "siwc status: protected credential store unavailable")
		return exitUnavailable
	}
	registrations, err := store.Registrations()
	if err != nil {
		fmt.Fprintf(errOut, "siwc status: cannot read registrations: %v\n", err)
		return exitUnavailable
	}
	for _, reg := range registrations {
		state := "signed out"
		if loaded, err := store.Load(reg.ClientID); err == nil {
			state = "authenticated"
			if !loaded.ExpiresAt.After(time.Now()) {
				state = "expired; sign-in required"
			}
		} else if errors.Is(err, siwcauth.ErrRefreshUncertain) {
			state = "refresh uncertain; sign-in required"
		} else if !errors.Is(err, os.ErrNotExist) {
			state = "unavailable"
		}
		fmt.Fprintf(out, "client=%q subject_binding=%q state=%q model=%q scopes=%q\n", reg.ClientID, shortHash(reg.Subject), state, reg.Model, strings.Join(reg.Scopes, " "))
	}
	fmt.Fprintln(out, "SIWC credentials and model catalog do not enable inference in this stage.")
	return exitSuccess
}

func siwcSetup(ctx context.Context, args []string, out, errOut io.Writer) int {
	return siwcSetupWithEndpoints(ctx, args, out, errOut, siwcauth.OpenAIEndpoints())
}

func siwcSetupWithEndpoints(ctx context.Context, args []string, out, errOut io.Writer, endpoints siwcauth.Endpoints) int {
	return siwcSetupWithEndpointsHook(ctx, args, out, errOut, endpoints, nil)
}

func siwcSetupWithEndpointsHook(ctx context.Context, args []string, out, errOut io.Writer, endpoints siwcauth.Endpoints, afterStage func() error) int {
	return siwcSetupWithLifecycleHooks(ctx, args, out, errOut, endpoints, afterStage, nil)
}

func siwcSetupWithLifecycleHooks(ctx context.Context, args []string, out, errOut io.Writer, endpoints siwcauth.Endpoints, afterStage, afterProviderPublish func() error) int {
	flags := flag.NewFlagSet("siwc setup", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	clientID, model, providerID, stateDir, providersPath := "", "", "siwc-openai", "", ""
	flags.StringVar(&clientID, "client-id", "", "issued client ID for the authenticated account")
	flags.StringVar(&model, "model", "", "exact model slug selected from this account's visible catalog")
	flags.StringVar(&providerID, "provider-id", providerID, "stable provider ID")
	flags.StringVar(&stateDir, "state-dir", "", "new absolute SIWC state directory; it must not already exist")
	flags.StringVar(&providersPath, "providers", "", "new provider configuration output path")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || clientID == "" || model == "" || stateDir == "" || providersPath == "" {
		fmt.Fprintln(errOut, "siwc setup: --client-id, --model, --state-dir and --providers are required")
		return exitUsage
	}
	if !filepath.IsAbs(stateDir) || !filepath.IsAbs(providersPath) || filepath.Clean(stateDir) != stateDir || filepath.Clean(providersPath) != providersPath {
		fmt.Fprintln(errOut, "siwc setup: state and provider paths must be normalized absolute paths")
		return exitUsage
	}
	if err := validateSIWCProviderDestination(providersPath); err != nil {
		fmt.Fprintln(errOut, "siwc setup: provider output path is unavailable or unsafe")
		return exitUnavailable
	}
	store, err := openSIWCStore(false)
	if err != nil {
		fmt.Fprintln(errOut, "siwc setup: protected credential store unavailable")
		return exitUnavailable
	}
	models, reg, err := store.CatalogSnapshot(ctx, nil, endpoints, clientID)
	if err != nil {
		fmt.Fprintf(errOut, "siwc setup: account catalog unavailable: %v\n", err)
		return exitUnavailable
	}
	if err := store.SetModel(clientID, model, models); err != nil {
		if errors.Is(err, siwcauth.ErrSignedOut) || errors.Is(err, siwcauth.ErrRefreshUncertain) {
			fmt.Fprintln(errOut, "siwc setup: authenticated registration changed during catalog lookup")
			return exitUnavailable
		}
		fmt.Fprintf(errOut, "siwc setup: selected model is not visible in this account catalog\n")
		return exitUsage
	}
	// Keep the token snapshot from the catalog request; SetModel must not
	// replace it with a later reauthorization that raced the request.
	reg.Model = model
	accountBinding, credentialBinding, err := store.Bindings(reg.Public(), false)
	if err != nil {
		fmt.Fprintf(errOut, "siwc setup: verified credential binding unavailable\n")
		return exitUnavailable
	}
	config := provider.Config{DocumentVersion: 2, WireContract: provider.WireResponsesSIWCV1, AccountBinding: accountBinding, CredentialBinding: credentialBinding, ProviderID: providerID, ProtocolFamily: provider.FamilyOpenAICompatible, BaseURL: siwcauth.Resource, Model: model, Auth: provider.SecretRef("siwc:" + clientID), AuthRequirement: provider.AuthReferenceRequired, ConfigVersion: "siwc-stage4-v1", Profile: provider.CapabilityProfile{ProfileVersion: "v1", Capabilities: provider.Capabilities{provider.CapabilityTextTurn: true, provider.CapabilityRunsteadProtocol: true}, RouteSafety: provider.SafeRouteSafety(), MaxRequestBytes: 1 << 20, MaxResponseBytes: 4 << 20}}
	registry, err := provider.NewRegistry(config)
	if err != nil {
		fmt.Fprintf(errOut, "siwc setup: provider contract refused\n")
		return exitUsage
	}
	resolved, err := registry.Resolve(providerID, provider.RequiredCapabilities(), provider.SafeRouteSafety())
	if err != nil {
		fmt.Fprintf(errOut, "siwc setup: provider contract refused\n")
		return exitUsage
	}
	identity := provider.IdentityFromResolved(*resolved, compat.AdapterVersion)
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(errOut, "siwc setup: home directory unavailable")
		return exitUnavailable
	}
	locator, err := siwcstate.DefaultLocatorPath(home, os.Getenv("XDG_STATE_HOME"))
	if err != nil {
		fmt.Fprintln(errOut, "siwc setup: SIWC locator unavailable")
		return exitUnavailable
	}
	if err := store.WithActiveRegistration(reg, func() error {
		staged, err := stageSIWCProviderFile(providersPath, config)
		if err != nil {
			return fmt.Errorf("provider configuration preparation failed: %w", err)
		}
		if afterStage != nil {
			if err := afterStage(); err != nil {
				return errors.Join(fmt.Errorf("provider configuration staging interrupted: %w", err), staged.Cleanup())
			}
		}
		publish := func() (func() error, error) {
			rollback, err := staged.Publish()
			if err == nil && afterProviderPublish != nil {
				err = afterProviderPublish()
			}
			return rollback, err
		}
		initializeErr := siwcstate.InitializeWithPublisher(locator, stateDir, identity, publish)
		if initializeErr != nil {
			initializeErr = fmt.Errorf("SIWC domain initialization refused: %w", initializeErr)
		}
		return errors.Join(initializeErr, staged.Cleanup())
	}); err != nil {
		fmt.Fprintf(errOut, "siwc setup: authenticated setup commit refused: %v\n", err)
		return exitUnavailable
	}
	fmt.Fprintf(out, "SIWC domain initialized for provider %q and selected model %q. Stage 5 Responses inference is still unavailable.\n", providerID, model)
	return exitSuccess
}

func writeSIWCProviderFile(path string, c provider.Config) error {
	staged, err := stageSIWCProviderFile(path, c)
	if err != nil {
		return err
	}
	rollback, err := staged.Publish()
	if err != nil {
		if rollback != nil {
			err = errors.Join(err, rollback())
		}
	}
	return errors.Join(err, staged.Cleanup())
}

type stagedSIWCProviderFile struct {
	tempPath  string
	finalPath string
	fileInfo  os.FileInfo
	published bool
}

func stageSIWCProviderFile(path string, c provider.Config) (*stagedSIWCProviderFile, error) {
	if err := validateSIWCOutputParent(path); err != nil {
		return nil, err
	}
	if err := validateSIWCProviderDestination(path); err != nil {
		return nil, err
	}
	doc := map[string]any{"version": 2, "providers": []any{map[string]any{
		"wire_contract": c.WireContract, "account_binding": c.AccountBinding, "credential_binding": c.CredentialBinding, "provider_id": c.ProviderID, "protocol_family": c.ProtocolFamily, "base_url": c.BaseURL, "model": c.Model, "auth_ref": c.Auth, "auth_requirement": c.AuthRequirement, "options": map[string]string{}, "config_version": c.ConfigVersion,
		"profile": map[string]any{"profile_version": c.Profile.ProfileVersion, "capabilities": []string{string(provider.CapabilityTextTurn), string(provider.CapabilityRunsteadProtocol)}, "route_safety": map[string]string{"attempt_accounting": "single", "single_attempt": "guaranteed", "internal_retries": "disabled", "cooldown_replay": "disabled", "account_pooling": "disabled", "automatic_fallback": "disabled", "combo_routing": "disabled"}, "max_request_bytes": c.Profile.MaxRequestBytes, "max_response_bytes": c.Profile.MaxResponseBytes},
	}}}
	encoded, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, errors.New("cannot encode provider configuration")
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".runstead-siwc-provider-*.stage")
	if err != nil {
		return nil, errors.New("cannot create provider configuration staging file")
	}
	tempPath := file.Name()
	stage := &stagedSIWCProviderFile{tempPath: tempPath, finalPath: path}
	removeTemp := func() error { return removeSIWCOutputFile(tempPath) }
	if err := file.Chmod(0o600); err != nil {
		return nil, errors.Join(errors.New("cannot protect provider configuration staging file"), file.Close(), removeTemp())
	}
	if _, err = file.Write(encoded); err != nil {
		return nil, errors.Join(errors.New("cannot write provider configuration staging file"), file.Close(), removeTemp())
	}
	if err = file.Sync(); err != nil {
		return nil, errors.Join(errors.New("cannot sync provider configuration staging file"), file.Close(), removeTemp())
	}
	if err := file.Close(); err != nil {
		return nil, errors.Join(errors.New("cannot close provider configuration staging file"), removeTemp())
	}
	stage.fileInfo, err = os.Lstat(tempPath)
	if err != nil || !stage.fileInfo.Mode().IsRegular() || stage.fileInfo.Mode().Perm() != 0o600 || !siwcOutputOwnedByCurrentUser(stage.fileInfo) {
		return nil, errors.Join(errors.New("provider configuration staging file is unsafe"), removeTemp())
	}
	if err := syncSIWCOutputDirectory(filepath.Dir(path)); err != nil {
		return nil, errors.Join(err, removeTemp())
	}
	return stage, nil
}

func (s *stagedSIWCProviderFile) Publish() (func() error, error) {
	rollback := s.Rollback
	if err := os.Link(s.tempPath, s.finalPath); err != nil {
		return rollback, errors.New("provider configuration path already exists or is unsafe")
	}
	s.published = true
	info, err := os.Lstat(s.finalPath)
	if err != nil || !os.SameFile(s.fileInfo, info) || info.Mode().Perm() != 0o600 || !siwcOutputOwnedByCurrentUser(info) {
		return rollback, errors.New("published provider configuration identity is unsafe")
	}
	if err := syncSIWCOutputDirectory(filepath.Dir(s.finalPath)); err != nil {
		return rollback, errors.New("cannot sync provider configuration directory")
	}
	if err := os.Remove(s.tempPath); err != nil {
		return rollback, errors.New("cannot finalize provider configuration staging file")
	}
	s.tempPath = ""
	return rollback, nil
}

func (s *stagedSIWCProviderFile) Rollback() error {
	var result error
	if s.published {
		current, err := os.Lstat(s.finalPath)
		if err == nil && os.SameFile(s.fileInfo, current) {
			if err := os.Remove(s.finalPath); err != nil {
				result = errors.Join(result, err)
			} else {
				result = errors.Join(result, syncSIWCOutputDirectory(filepath.Dir(s.finalPath)))
			}
		} else if err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, err)
		}
		s.published = false
	}
	if s.tempPath != "" {
		if err := os.Remove(s.tempPath); err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, err)
		} else {
			s.tempPath = ""
		}
	}
	return result
}

func (s *stagedSIWCProviderFile) Cleanup() error {
	if s.tempPath != "" {
		err := removeSIWCOutputFile(s.tempPath)
		if err != nil {
			return err
		}
		s.tempPath = ""
	}
	return nil
}

func removeSIWCOutputFile(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func validateSIWCProviderDestination(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return errors.New("provider configuration destination already exists")
	} else if !os.IsNotExist(err) {
		return errors.New("provider configuration destination is unsafe")
	}
	return nil
}

func syncSIWCOutputDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return errors.New("cannot open provider configuration directory")
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil || closeErr != nil {
		return errors.New("cannot sync provider configuration directory")
	}
	return nil
}

func siwcOutputOwnedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
}

func validateSIWCOutputParent(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("provider configuration path must be a normalized absolute path")
	}
	parent := filepath.Dir(path)
	current := string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(filepath.Clean(parent), current), current) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("provider configuration directory is unsafe")
		}
	}
	return nil
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func printSIWCHelp(out io.Writer) {
	fmt.Fprintln(out, "Usage: runstead siwc <login|status|models|setup|logout>")
	fmt.Fprintln(out, "  login              authenticate through official OAuth + loopback PKCE")
	fmt.Fprintln(out, "  status              list local registrations without credentials")
	fmt.Fprintln(out, "  models --client-id  fetch the account-scoped visible model catalog")
	fmt.Fprintln(out, "  setup --client-id ID --model SLUG --state-dir PATH --providers PATH  create one offline SIWC domain")
	fmt.Fprintln(out, "  logout --client-id  revoke and clear one renewable session")
	fmt.Fprintln(out, "Credentials and model catalog do not enable Responses inference in Stage 4.")
}
