package config

import "testing"

func FuzzLoadFromBytes(f *testing.F) {
	// Seed corpus: valid configs
	f.Add([]byte(`
auth:
  enabled: false
routes:
  - path_prefix: "/api"
    backend: "http://localhost:3000"
`))
	f.Add([]byte(`
server:
  port: 9090
auth:
  enabled: true
  jwt_secret: "secret"
  issuer: "iss"
  audience: "aud"
routes:
  - path_prefix: "/api/v1"
    backend: "https://backend:3000"
    strip_prefix: true
    methods: ["GET"]
    timeout_ms: 5000
`))

	// Edge cases
	f.Add([]byte(``))
	f.Add([]byte(`routes: []`))
	f.Add([]byte(`server: { port: 0 }`))
	f.Add([]byte(`auth: { enabled: false }
routes:
  - path_prefix: "/"
    backend: "http://localhost:3000"
`))

	f.Fuzz(func(t *testing.T, data []byte) {
		// LoadFromBytes must never panic regardless of input.
		cfg, err := LoadFromBytes(data)
		if err != nil {
			return
		}
		// If parsing succeeded, verify invariants that validation should enforce.
		if cfg.Server.Port < 0 || cfg.Server.Port > 65535 {
			t.Errorf("invalid port escaped validation: %d", cfg.Server.Port)
		}
		if cfg.RateLimit.RequestsPerSecond < 0 {
			t.Errorf("negative rps escaped validation: %f", cfg.RateLimit.RequestsPerSecond)
		}
		if cfg.RateLimit.BurstSize < 0 {
			t.Errorf("negative burst escaped validation: %d", cfg.RateLimit.BurstSize)
		}
	})
}
