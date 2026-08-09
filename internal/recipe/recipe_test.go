package recipe_test

import (
	"strings"
	"testing"

	"github.com/RenyEnnos/Runstead/internal/recipe"
)

func TestCatalogParsesAndNormalizesRecipes(t *testing.T) {
	catalog, err := recipe.ParseCatalog([]byte(`[
		{"id":"test","executable":"go","argv":["test","./..."],"capabilities":["execute_repository_code","temporary_files"]},
		{"id":"vet","executable":"go","argv":["vet","./..."],"working_directory":"sub","timeout_nanos":5000000000,"capabilities":["execute_repository_code"],"allowed_environment":["GOCACHE"]}
	]`))
	if err != nil {
		t.Fatalf("ParseCatalog() error = %v", err)
	}
	if catalog.Len() != 2 {
		t.Fatalf("catalog len = %d, want 2", catalog.Len())
	}
	test, ok := catalog.Get("test")
	if !ok {
		t.Fatal("recipe test missing")
	}
	if test.Executable != "go" || len(test.Argv) != 2 || test.Argv[0] != "test" {
		t.Fatalf("test recipe = %+v", test)
	}
	if test.Timeout() != recipe.DefaultTimeout {
		t.Fatalf("default timeout not applied: %v", test.Timeout())
	}
	if test.OutputLimits.MaxStdoutBytes != recipe.DefaultMaxStdoutBytes {
		t.Fatalf("default stdout limit not applied")
	}
	vet, _ := catalog.Get("vet")
	if vet.WorkingDirectory != "sub" || vet.Timeout() != 5_000_000_000 {
		t.Fatalf("vet recipe normalization wrong: %+v", vet)
	}
	if len(vet.AllowedEnvironment) != 1 || vet.AllowedEnvironment[0] != "GOCACHE" {
		t.Fatalf("allowed environment = %v", vet.AllowedEnvironment)
	}
}

func TestCatalogRejectsInvalidRecipes(t *testing.T) {
	cases := []string{
		`[{"id":"","executable":"go"}]`,                                                // empty id
		`[{"id":"x","executable":""}]`,                                                 // empty executable
		`[{"id":"x","executable":"go","working_directory":"../up"}]`,                   // traversal cwd
		`[{"id":"x","executable":"go","working_directory":"/abs"}]`,                    // absolute cwd
		`[{"id":"x","executable":"go","capabilities":["bogus"]}]`,                      // unknown capability
		`[{"id":"x","executable":"go","output_limits":{"max_stdout_bytes":99999999}}]`, // over cap
		`[{"id":"x","executable":"go","allowed_environment":["OPENAI_API_KEY"]}]`,      // credential name refused
		`[{"id":"x","executable":"go"},{"id":"x","executable":"go"}]`,                  // duplicate id
	}
	for _, raw := range cases {
		if _, err := recipe.ParseCatalog([]byte(raw)); err == nil {
			t.Fatalf("catalog %s must be rejected", raw)
		}
	}
}

func TestBuildEnvironmentAllowlist(t *testing.T) {
	parent := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/user",
		"GOCACHE=/cache",
		"OPENAI_API_KEY=sk-secret",
		"OMNIROUTE_API_KEY=secret",
		"AUTHORIZATION=Bearer abc",
		"SESSION_COOKIE=abc123",
		"CHATGPT_ACCESS_TOKEN=tok",
		"TOKEN=secret",
		"RUNSTEAD_MARKER=visible",
	}
	selected := recipe.Recipe{
		ID:                 "test",
		Executable:         "go",
		AllowedEnvironment: []string{"GOCACHE", "HOME", "OPENAI_API_KEY", "OMNIROUTE_API_KEY", "AUTHORIZATION", "SESSION_COOKIE", "CHATGPT_ACCESS_TOKEN", "TOKEN", "RUNSTEAD_MARKER"},
	}
	env := recipe.BuildEnvironment(parent, selected)
	envMap := make(map[string]string, len(env))
	for _, entry := range env {
		parts := strings.SplitN(entry, "=", 2)
		envMap[parts[0]] = parts[1]
	}
	// The allowlist passes allowed non-credential names and PATH.
	if envMap["GOCACHE"] != "/cache" {
		t.Fatalf("GOCACHE not inherited: %v", envMap)
	}
	if envMap["HOME"] != "/home/user" {
		t.Fatalf("HOME not inherited: %v", envMap)
	}
	if envMap["RUNSTEAD_MARKER"] != "visible" {
		t.Fatalf("allowed marker not inherited: %v", envMap)
	}
	if envMap["PATH"] == "" {
		t.Fatalf("PATH must always be passed: %v", envMap)
	}
	if envMap["RUNSTEAD_RECIPE_ID"] != "test" {
		t.Fatalf("recipe id marker missing: %v", envMap)
	}
	// Credential-shaped names are never inherited even when explicitly listed.
	for _, name := range []string{"OPENAI_API_KEY", "OMNIROUTE_API_KEY", "AUTHORIZATION", "SESSION_COOKIE", "CHATGPT_ACCESS_TOKEN", "TOKEN"} {
		if _, ok := envMap[name]; ok {
			t.Fatalf("credential-shaped env %q must never be inherited: %v", name, envMap)
		}
	}
	// Unlisted parent variables are not inherited.
	if _, ok := envMap["HOME"]; !ok {
		t.Fatal("HOME was listed, should be inherited")
	}
	// A variable that is neither PATH nor allowed nor credential is absent by
	// default; verify with a fresh recipe that lists nothing.
	empty := recipe.BuildEnvironment(parent, recipe.Recipe{ID: "x", Executable: "go"})
	for _, entry := range empty {
		if strings.HasPrefix(entry, "HOME=") || strings.HasPrefix(entry, "GOCACHE=") {
			t.Fatalf("unlisted parent env leaked: %v", empty)
		}
	}
}

func TestIsCredentialNameCoversFixtures(t *testing.T) {
	for _, name := range []string{
		"OPENAI_API_KEY", "OMNIROUTE_API_KEY", "AUTHORIZATION", "TOKEN",
		"SESSION_COOKIE", "CHATGPT_ACCESS_TOKEN", "CLIENT_SECRET", "PASSWORD",
		"API_KEY", "SESSION_ID",
	} {
		env := recipe.BuildEnvironment([]string{name + "=x"}, recipe.Recipe{ID: "x", Executable: "go", AllowedEnvironment: []string{name}})
		for _, entry := range env {
			if strings.HasPrefix(entry, name+"=") {
				t.Fatalf("credential fixture %q leaked into child env: %v", name, env)
			}
		}
	}
}

func TestRecipeSpecAndParseRoundTrip(t *testing.T) {
	// policy-level helpers are in the policy package; here we verify the
	// recipe normalization keeps ids stable for policy keys.
	catalog, err := recipe.ParseCatalog([]byte(`[{"id":"test","executable":"go","capabilities":["execute_repository_code"]}]`))
	if err != nil {
		t.Fatal(err)
	}
	ids := catalog.IDs()
	if len(ids) != 1 || ids[0] != "test" {
		t.Fatalf("ids = %v", ids)
	}
}
