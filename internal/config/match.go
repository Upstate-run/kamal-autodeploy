package config

import (
	"errors"
	"path"
	"strings"
)

// Path patterns are slash-separated globs relative to the repository root:
//
//	**        any number of path segments (including none)
//	*, ?, []  within one segment (path.Match)
//	!pattern  negation: a later negation removes a match made by an earlier pattern
//
// A pattern without a slash or ** matches that exact path only (it is not a basename match),
// so `README.md` means the root README. Use `**/README.md` for every README.

// ValidPattern rejects patterns that could never match or that escape the repository.
func ValidPattern(p string) error {
	p = strings.TrimPrefix(p, "!")
	switch {
	case p == "":
		return errors.New("empty pattern")
	case strings.HasPrefix(p, "/"):
		return errors.New("patterns are relative to the repository root; drop the leading /")
	case strings.Contains(p, "\\"):
		return errors.New("use / as the separator")
	case strings.ContainsAny(p, "\x00\n\r"):
		return errors.New("control characters are not allowed")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." || seg == "" {
			return errors.New("segments may not be empty, . or ..")
		}
		if seg != "**" {
			if _, err := path.Match(seg, ""); err != nil {
				return errors.New("malformed glob")
			}
		}
	}
	return nil
}

// Match reports whether file is selected by the ordered pattern list (last match wins).
func Match(patterns []string, file string) bool {
	matched := false
	for _, p := range patterns {
		neg := strings.HasPrefix(p, "!")
		if neg {
			p = p[1:]
		}
		if globMatch(strings.Split(p, "/"), strings.Split(file, "/")) {
			matched = !neg
		}
	}
	return matched
}

func globMatch(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			for i := 0; i <= len(segs); i++ {
				if globMatch(rest, segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], segs[0])
		if err != nil || !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}
