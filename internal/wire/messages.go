// Package wire defines the versioned qrexec protocol messages and framing.
package wire

// Request is the Read service header.
type Request struct {
	V      int      `json:"v"`
	Argv   []string `json:"argv"`
	Repo   string   `json:"repo"`
	Branch string   `json:"branch"`
}

// UploadPackRequest and UploadPackResponse describe the phase 2 handshake.
type UploadPackRequest struct {
	V    int    `json:"v"`
	Repo string `json:"repo"`
}
type UploadPackResponse struct {
	OK    bool   `json:"ok,omitempty"`
	Error string `json:"error,omitempty"`
}

// SendRequest and SendResponse describe phase 3's bundle and result.
type SendRequest struct {
	V            int      `json:"v"`
	RepoPath     string   `json:"repo_path"`
	WorktreePath string   `json:"worktree_path"`
	Branch       string   `json:"branch"`
	Tip          string   `json:"tip"`
	GitHub       []string `json:"github"`
	PushTo       string   `json:"push_to"`
	BundleSize   int64    `json:"bundle_size"`
}
type SendResponse struct {
	WorktreePath   string   `json:"worktree_path,omitempty"`
	Branch         string   `json:"branch,omitempty"`
	SignedTip      string   `json:"signed_tip,omitempty"`
	Commits        int      `json:"commits,omitempty"`
	WorktreeStatus string   `json:"worktree_status,omitempty"`
	PushCommand    string   `json:"push_command,omitempty"`
	Warnings       []string `json:"warnings,omitempty"`
	Error          string   `json:"error,omitempty"`
}
