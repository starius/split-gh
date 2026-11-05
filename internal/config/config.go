// Package config reads gh-auth's ~/.config/split-gh/config.json.
package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	Qubes         map[string]Qube `json:"qubes"`
	ReadTokenFile string          `json:"read_token_file"`
}

// Qube holds what one agent qube may do.
type Qube struct {
	// Port is where split-gh serve answers this qube, on 127.0.0.1. The dom0
	// policy lets only this qube connect to it, with qubes.ConnectTCP.
	Port int `json:"port"`
	// PasswordFile holds the password the qube sends with every request.
	PasswordFile string `json:"password_file"`
	// ReadRepos are the GitHub OWNER/REPO patterns the qube may read, with
	// gh and with git fetch.
	ReadRepos []string `json:"read_repos"`
	// LocalRepos are repositories in gh-auth, relative to the home directory
	// ("lnd" or "~/lnd"), that the qube may send patches for.
	LocalRepos []string `json:"local_repos"`
}

// Load reads the config. Unknown fields are errors, so a typo can't
// silently drop a setting.
func Load(home string) (Config, error) {
	var c Config
	b, err := os.ReadFile(filepath.Join(home, ".config/split-gh/config.json"))
	if err != nil {
		return c, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	err = d.Decode(&c)
	return c, err
}

// Path expands a leading ~/ in a configured file name.
func Path(home, p string) string {
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// AllowsLocal reports whether rel, a clean path relative to the home
// directory, is one of the qube's local repositories.
func (q Qube) AllowsLocal(rel string) bool {
	for _, r := range q.LocalRepos {
		if filepath.Clean(strings.TrimPrefix(r, "~/")) == rel {
			return true
		}
	}
	return false
}
