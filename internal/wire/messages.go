// Package wire defines what the agent qube and split-gh serve in gh-auth
// send each other, apart from git's own protocol.
//
// The gh shim sends POST /gh with one Request as JSON. A refusal is an HTTP
// error with the reason as text. Otherwise the answer is 200 with frames:
// gh's stdout ('o') and stderr ('e') as they come, then its exit status
// ('x').
//
// Before git fetches from GitHub, git-remote-split-gh asks
// GET FetchesPath+"OWNER/REPO" whether serve fetches that repository for
// the qube. The answer is 200 with Yes or No, as a line of text. With Yes,
// git fetches through serve; with No, straight from GitHub, without a
// token. serve doesn't ask GitHub.
package wire

// Request is the body of POST /gh.
type Request struct {
	V      int      `json:"v"`
	Argv   []string `json:"argv"`
	Repo   string   `json:"repo"`
	Branch string   `json:"branch"`
}

// The question before a git fetch, and its answers.
const (
	FetchesPath = "/fetches/"
	Yes         = "yes"
	No          = "no"
)
