package compat

// Issue #14 (Part 6): provider/wire neutrality regression. The agent loop,
// the account governor, the tool registry and the verifier must never import
// or reference the protocol-family adapters or their wire vocabulary. Family
// dispatch belongs exclusively to the composition layer
// (internal/provider/compat); the runtime depends only on the provider-neutral
// contract of #79. This is a simple structural check over the package
// sources, deliberately not a runtime architecture.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// neutralRoots are the runtime packages that must stay provider-neutral.
var neutralRoots = []string{
	"../../agent",
	"../../governor",
	"../../tools",
	"../../verifier",
}

// adapterImports are the composition/adapter packages the neutral runtime
// must never import, together with wire-vocabulary fragments that must never
// appear in its sources.
var forbiddenImports = []string{
	"internal/provider/openaicompat",
	"internal/provider/anthropiccompat",
	"internal/provider/googlecompat",
	"internal/provider/omniroute",
	"internal/provider/compat",
}

var forbiddenFragments = []string{
	"chat/completions",
	"generateContent",
	"anthropic-version",
	"x-api-key",
	"openaicompat",
	"anthropiccompat",
	"googlecompat",
	"omniroute",
	"OpenAI",
	"Anthropic",
	"Gemini",
	// Operational profiles are provider-layer metadata consumed by the
	// composition root and inspection; the neutral runtime must never branch
	// on profile state (#91).
	"OperationalProfile",
	"ProfileUpdate",
	"SaveOperationalProfile",
	"LoadOperationalProfile",
}

func TestNeutralRuntimeNeverDependsOnWireAdapters(t *testing.T) {
	for _, root := range neutralRoots {
		root := root
		absolute, err := filepath.Abs(root)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(filepath.Base(absolute), func(t *testing.T) {
			var files []string
			err := filepath.Walk(absolute, func(path string, info os.FileInfo, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
					return nil
				}
				files = append(files, path)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(files) == 0 {
				t.Fatalf("no source files under %s", absolute)
			}
			for _, file := range files {
				content, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				imports := parseImports(t, file)
				for _, forbidden := range forbiddenImports {
					for _, imported := range imports {
						if imported == forbidden {
							t.Fatalf("%s imports forbidden adapter package %q; the %s runtime must stay provider-neutral", filepath.Base(file), forbidden, filepath.Base(absolute))
						}
					}
				}
				text := string(content)
				for _, fragment := range forbiddenFragments {
					if strings.Contains(text, fragment) {
						t.Fatalf("%s contains forbidden wire vocabulary %q; provider details must not enter the %s runtime", filepath.Base(file), fragment, filepath.Base(absolute))
					}
				}
			}
		})
	}
}

func parseImports(t *testing.T, path string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var imports []string
	for _, spec := range file.Imports {
		imports = append(imports, strings.Trim(spec.Path.Value, `"`))
	}
	return imports
}
