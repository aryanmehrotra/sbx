package execd

import (
	"fmt"
	"path"
	"strings"
)

// globMatch matches a search pattern against a file's base name, which is all upstream ever
// matches it against (it walks the tree and tests info.Name()). Upstream uses a doublestar
// matcher; against a single name with no separators that reduces to path.Match with every
// "**" meaning "*", and with leading "**/" segments matching zero directories - so "**",
// "*.txt" and "**/*.txt" all behave as they do upstream.
func globMatch(pattern, name string) bool {
	ok, err := path.Match(simplifyGlob(pattern), name)
	return err == nil && ok
}

func simplifyGlob(pattern string) string {
	for strings.HasPrefix(pattern, "**/") {
		pattern = strings.TrimPrefix(pattern, "**/")
	}

	for strings.Contains(pattern, "**") {
		pattern = strings.ReplaceAll(pattern, "**", "*")
	}

	return pattern
}

// validGlob rejects a malformed pattern up front, so a bad "[" answers 400 instead of an empty
// result that looks like "nothing matched".
func validGlob(pattern string) error {
	if _, err := path.Match(simplifyGlob(pattern), ""); err != nil {
		return fmt.Errorf("invalid pattern %q: %v; patterns are globs like *.txt or **", pattern, err)
	}

	return nil
}
