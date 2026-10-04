package protocol

// Workspace change scopes: the merge base with the default branch, or HEAD.
const (
	ScopeBranch      = "branch"
	ScopeUncommitted = "uncommitted"
)

// WorkspaceChanges is the diff of a git work tree. Files is complete. Patch
// ends at the last whole file that fits its cap, and Truncated reports a cut.
// Head is empty on a branch with no commit.
type WorkspaceChanges struct {
	Dir       string        `json:"dir"`
	Scope     string        `json:"scope"`
	Branch    string        `json:"branch"`
	Head      string        `json:"head"`
	Base      *BaseRef      `json:"base,omitempty"`
	Files     []ChangedFile `json:"files"`
	Patch     string        `json:"patch"`
	Truncated bool          `json:"truncated"`
}

// BaseRef is the merge base of the branch scope: the default branch and the commit.
type BaseRef struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
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
