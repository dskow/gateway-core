// Package routing provides shared route-matching helpers used by multiple
// gateway packages (proxy, ratelimit, auth).
package routing

import "strings"

// MatchesPrefix checks if path matches prefix with boundary enforcement.
// The path must either equal the prefix, the prefix must end with "/",
// or the character after the prefix in path must be "/".
func MatchesPrefix(path, prefix string) bool {
	if prefix == "" {
		return false
	}
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	if len(path) == len(prefix) {
		return true
	}
	if prefix[len(prefix)-1] == '/' {
		return true
	}
	return path[len(prefix)] == '/'
}
