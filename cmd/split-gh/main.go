// Command split-gh gives agents in an untrusted qube read access to GitHub
// through the authenticated qube.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"split-gh/internal/client"
	"split-gh/internal/serve"
)

const usage = `usage:
  in the agent qube:
    gh ...                       (symlink) read GitHub through gh-auth
  in gh-auth:
    split-gh serve               answer the agent qubes until Ctrl-C`

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
	case "serve":
		os.Exit(serve.Main(args[1:]))
	}
	fmt.Fprintln(os.Stderr, usage)
	os.Exit(2)
}
