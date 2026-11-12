// Package glob matches slash-separated paths against globs in which a "**" segment spans folders.
package glob

import (
	"path"
	"strings"
)

// Match is path.Match per segment, where a "**" segment matches any number of segments, none included.
func Match(glob, file string) bool {
	return segments(strings.Split(glob, "/"), strings.Split(file, "/"))
}

func segments(glob, file []string) bool {
	for len(glob) > 0 {
		if glob[0] == "**" {
			for i := 0; i <= len(file); i++ {
				if segments(glob[1:], file[i:]) {
					return true
				}
			}
			return false
		}
		if len(file) == 0 {
			return false
		}
		if ok, _ := path.Match(glob[0], file[0]); !ok {
			return false
		}
		glob, file = glob[1:], file[1:]
	}
	return len(file) == 0
}

// Valid reports whether glob is non-empty and every segment is a well-formed path.Match pattern.
func Valid(glob string) bool {
	if glob == "" {
		return false
	}
	for _, seg := range strings.Split(glob, "/") {
		if _, err := path.Match(seg, ""); err != nil {
			return false
		}
	}
	return true
}
