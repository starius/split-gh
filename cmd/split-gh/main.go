package main

import (
	"fmt"
	"os"
	"path/filepath"
	"split-gh/internal/client"
	"split-gh/internal/serve"
)

func main() {
	if filepath.Base(os.Args[0]) == "gh" {
		os.Exit(client.Main(os.Args[1:]))
	}
	if len(os.Args) == 3 && os.Args[1] == "serve" && os.Args[2] == "read" {
		serve.Read(os.Stdin, os.Stdout)
		return
	}
	fmt.Fprintln(os.Stderr, "usage: split-gh serve read (or invoke as gh)")
	os.Exit(2)
}
