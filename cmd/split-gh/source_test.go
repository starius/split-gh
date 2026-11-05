package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The code is reviewed by reading it, so it must not hold characters that
// render invisibly or reorder text. Write them as Go escapes instead.
func TestSourcesAreASCII(t *testing.T) {
	err := filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		b, err := os.ReadFile(path)
		for i, c := range b {
			if c > 0x7e || c < 0x20 && c != '\n' && c != '\t' {
				t.Errorf("%s: byte 0x%02x at offset %d", path, c, i)
				break
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
