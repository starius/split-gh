// Package wire defines what the gh shim in the agent qube and split-gh serve
// in gh-auth send each other.
//
// The shim sends POST /gh with one Request as JSON. A refusal is an HTTP
// error with the reason as text. Otherwise the answer is 200 with frames:
// gh's stdout ('o') and stderr ('e') as they come, then its exit status
// ('x').
package wire

// Request is the body of POST /gh.
type Request struct {
	V      int      `json:"v"`
	Argv   []string `json:"argv"`
	Repo   string   `json:"repo"`
	Branch string   `json:"branch"`
}
