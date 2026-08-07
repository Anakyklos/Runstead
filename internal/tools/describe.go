package tools

// ArgumentSpec describes one typed tool argument for the deterministic system
// contract. It mirrors the validation implemented by ValidateArguments.
type ArgumentSpec struct {
	Name     string
	Type     string
	Required bool
	Note     string
}

// ToolSpec is the registry-owned, static description of one read-only tool.
type ToolSpec struct {
	Name      string
	Summary   string
	Arguments []ArgumentSpec
	ReadOnly  bool
}

// Describe returns the static read-only tool contract. The agent loop builds
// its system contract from this list, so tool surface and prompt never drift
// independently. Describe performs no execution and no workspace access.
func (r *Registry) Describe() []ToolSpec {
	return []ToolSpec{
		{
			Name:    ToolReadFile,
			Summary: "Read a UTF-8 text file inside the workspace.",
			Arguments: []ArgumentSpec{
				{Name: "path", Type: "string", Required: true, Note: "relative path inside the workspace"},
			},
			ReadOnly: true,
		},
		{
			Name:    ToolListFiles,
			Summary: "List one directory level inside the workspace.",
			Arguments: []ArgumentSpec{
				{Name: "path", Type: "string", Required: true, Note: "relative directory path inside the workspace"},
			},
			ReadOnly: true,
		},
		{
			Name:    ToolSearchText,
			Summary: "Search for fixed text inside the workspace.",
			Arguments: []ArgumentSpec{
				{Name: "query", Type: "string", Required: true, Note: "fixed text to search for"},
				{Name: "path", Type: "string", Required: true, Note: "relative path to search under"},
			},
			ReadOnly: true,
		},
		{
			Name:     ToolGitStatus,
			Summary:  "Show the repository working tree status.",
			ReadOnly: true,
		},
		{
			Name:     ToolGitDiff,
			Summary:  "Show unstaged working tree changes.",
			ReadOnly: true,
		},
	}
}
