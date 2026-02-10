package routing

import "testing"

func TestMatchesPrefix(t *testing.T) {
	tests := []struct {
		path   string
		prefix string
		want   bool
	}{
		{"/api/users/123", "/api/users", true},
		{"/api/users", "/api/users", true},
		{"/api/", "/api/", true},
		{"/api/test", "/api/", true},
		{"/api.evil.com/steal", "/api", false},
		{"/api-extended", "/api", false},
		{"/apiary", "/api", false},
		{"/api", "/api", true},
		{"/api/test", "/api", true},
		{"/other", "/api", false},
	}

	for _, tt := range tests {
		t.Run(tt.path+"_vs_"+tt.prefix, func(t *testing.T) {
			got := MatchesPrefix(tt.path, tt.prefix)
			if got != tt.want {
				t.Errorf("MatchesPrefix(%q, %q) = %v, want %v", tt.path, tt.prefix, got, tt.want)
			}
		})
	}
}
