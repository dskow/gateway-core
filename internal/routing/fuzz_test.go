package routing

import "testing"

func FuzzMatchesPrefix(f *testing.F) {
	// Seed corpus from existing test cases
	f.Add("/api/users/123", "/api/users")
	f.Add("/api.evil.com/steal", "/api")
	f.Add("/apiary", "/api")
	f.Add("", "")
	f.Add("/", "/")
	f.Add("/api", "/api")
	f.Add("/api/", "/api/")
	f.Add("/api/test", "/api/")
	f.Add("/api-extended", "/api")

	f.Fuzz(func(t *testing.T, path, prefix string) {
		// Must never panic.
		result := MatchesPrefix(path, prefix)

		// If it matches and path is longer than prefix, verify the boundary
		// enforcement invariant: prefix ends with '/' OR path[len(prefix)] == '/'.
		if result && len(path) > len(prefix) && len(prefix) > 0 {
			if prefix[len(prefix)-1] != '/' && path[len(prefix)] != '/' {
				t.Errorf("MatchesPrefix(%q, %q) = true but boundary not enforced", path, prefix)
			}
		}
	})
}
