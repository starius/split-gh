// Command split-gh gives agents in an untrusted qube read access to GitHub
// through the authenticated qube, and hands their commits over as patch
// files for the user to review, apply with git am, and push.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"split-gh/internal/apply"
	"split-gh/internal/client"
	"split-gh/internal/serve"
)

const usage = `usage:
  in the agent qube:
    gh ...                       (symlink) read GitHub through gh-auth
    split-gh send [--base COMMIT]
                                 hand this branch's commits over as a patch
  in gh-auth:
    split-gh serve               answer the agent qubes until Ctrl-C
    split-gh diff PATCH          compare the branch with a patch from the agent
    split-gh apply [--force] PATCH
                                 add the patch's commits to the branch with
                                 git am, which signs if your config says to`

func main() {
	if filepath.Base(os.Args[0]) == "gh" {
		os.Exit(client.Main(os.Args[1:]))
	}
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	switch args[0] {
	case "send":
		os.Exit(client.Send(args[1:]))
	case "serve":
		os.Exit(serve.Main(args[1:]))
	case "diff":
		os.Exit(apply.Diff(args[1:]))
	case "apply":
		os.Exit(apply.Apply(args[1:]))
	}
	fmt.Fprintln(os.Stderr, usage)
	os.Exit(2)
}
