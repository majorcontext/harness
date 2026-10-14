package protocol

// Workspace change scopes: the merge base with the default branch, or HEAD.
const (
	ScopeBranch      = "branch"
	ScopeUncommitted = "uncommitted"
)

// EngineGit is the Engine of a git work tree.
const EngineGit = "git"

// WorkspaceChanges is the diff of a work tree. Files is complete. Patch ends at
// the last whole file that fits its cap, and Truncated reports a cut. HeadRev is
// the zero Rev, which JSON omits, when the head has no commit.
type WorkspaceChanges struct {
	Dir       string        `json:"dir"`
	Scope     string        `json:"scope"`
	Engine    string        `json:"engine"`
	Branch    string        `json:"branch"`
	HeadRev   Rev           `json:"head_rev,omitzero"`
	Base      *BaseRef      `json:"base,omitempty"`
	Files     []ChangedFile `json:"files"`
	Patch     string        `json:"patch"`
	Truncated bool          `json:"truncated"`
}

// BaseRef is the merge base of the branch scope: the default branch and the revision.
type BaseRef struct {
	Ref string `json:"ref"`
	Rev Rev    `json:"rev,omitzero"`
}

// ChangedFile is one file of a diff. Status is added, deleted, modified, or
// renamed. A Large untracked file has no counts and no hunk.
type ChangedFile struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary"`
	Large     bool   `json:"large"`
}
