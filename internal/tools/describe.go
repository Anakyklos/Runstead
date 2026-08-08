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
		{
			Name:    ToolWriteFile,
			Summary: "Write a UTF-8 text file inside the workspace. Stale-state protected: expected_before_hash must be the sha256 reported by read_file for an existing file, or \"absent\" for a new file.",
			Arguments: []ArgumentSpec{
				{Name: "path", Type: "string", Required: true, Note: "relative path inside the workspace; the parent directory must exist"},
				{Name: "content", Type: "string", Required: true, Note: "UTF-8 file content"},
				{Name: "expected_before_hash", Type: "string", Required: true, Note: "sha256 of the current file content from read_file, or \"absent\" for a new file"},
			},
			ReadOnly: false,
		},
		{
			Name:    ToolApplyPatch,
			Summary: "Apply a strict unified diff to one existing file inside the workspace. Stale-state protected: expected_before_hash must match the file's current sha256 from read_file.",
			Arguments: []ArgumentSpec{
				{Name: "path", Type: "string", Required: true, Note: "relative path of an existing regular file"},
				{Name: "patch", Type: "string", Required: true, Note: "strict unified diff: --- / +++ headers matching path, @@ -S,C +S,C @@ hunks, context/removal/addition lines"},
				{Name: "expected_before_hash", Type: "string", Required: true, Note: "sha256 of the current file content from read_file"},
			},
			ReadOnly: false,
		},
	}
}
