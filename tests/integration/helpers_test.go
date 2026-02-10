//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	gatewayURL = "http://localhost:8080"
	jwtSecret  = "integration-test-secret-key-32chars!!"
	jwtIssuer  = "https://auth.example.com"
	jwtAud     = "api-gateway"
)

var httpClient = &http.Client{Timeout: 10 * time.Second}

func TestMain(m *testing.M) {
	// Write .env for docker-compose
	if err := os.WriteFile("../../.env", []byte("JWT_SECRET="+jwtSecret+"\n"), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write .env: %v\n", err)
		os.Exit(1)
	}

	// Start docker-compose stack
	up := exec.Command("docker", "compose",
		"-f", "../../docker-compose.yml",
		"-f", "../../docker-compose.integration.yaml",
		"up", "--build", "-d",
	)
	up.Dir = "."
	up.Stdout = os.Stdout
	up.Stderr = os.Stderr
	if err := up.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "docker compose up failed: %v\n", err)
		os.Exit(1)
	}

	// Wait for gateway to become ready
	if err := waitForGateway(gatewayURL+"/health", 60*time.Second); err != nil {
		fmt.Fprintf(os.Stderr, "gateway not ready: %v\n", err)
		// Print logs for debugging
		logs := exec.Command("docker", "compose",
			"-f", "../../docker-compose.yml",
			"-f", "../../docker-compose.integration.yaml",
			"logs",
		)
		logs.Stdout = os.Stderr
		logs.Stderr = os.Stderr
		logs.Run()
		teardown()
		os.Exit(1)
	}

	code := m.Run()

	teardown()
	os.Exit(code)
}

func teardown() {
	down := exec.Command("docker", "compose",
		"-f", "../../docker-compose.yml",
		"-f", "../../docker-compose.integration.yaml",
		"down", "-v",
	)
	down.Stdout = os.Stdout
	down.Stderr = os.Stderr
	down.Run()
}

func waitForGateway(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("gateway not ready after %v", timeout)
}

func generateJWT(sub, scope string, expiry time.Duration) string {
	claims := jwt.MapClaims{
		"sub":   sub,
		"iss":   jwtIssuer,
		"aud":   jwtAud,
		"exp":   time.Now().Add(expiry).Unix(),
		"scope": scope,
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := token.SignedString([]byte(jwtSecret))
	if err != nil {
		panic(fmt.Sprintf("generateJWT: %v", err))
	}
	return s
}

func httpDo(method, url string, body io.Reader, headers map[string]string) (*http.Response, []byte, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp, data, err
}

func httpGet(url string, headers map[string]string) (*http.Response, []byte, error) {
	return httpDo("GET", url, nil, headers)
}

func authHeader(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

func parseJSON(t *testing.T, data []byte) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("failed to parse JSON %q: %v", string(data), err)
	}
	return m
}

func assertStatusCode(t *testing.T, resp *http.Response, expected int) {
	t.Helper()
	if resp.StatusCode != expected {
		t.Errorf("expected status %d, got %d", expected, resp.StatusCode)
	}
}

func assertErrorCode(t *testing.T, body []byte, expected string) {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("failed to parse error response: %v\nbody: %s", err, string(body))
	}
	code, ok := m["error_code"].(string)
	if !ok {
		t.Fatalf("error_code field missing or not a string in %s", string(body))
	}
	if code != expected {
		t.Errorf("expected error_code %q, got %q", expected, code)
	}
}

func assertHeader(t *testing.T, resp *http.Response, key, expected string) {
	t.Helper()
	got := resp.Header.Get(key)
	if got != expected {
		t.Errorf("expected header %s=%q, got %q", key, expected, got)
	}
}

func assertHeaderPresent(t *testing.T, resp *http.Response, key string) {
	t.Helper()
	if resp.Header.Get(key) == "" {
		t.Errorf("expected header %s to be present", key)
	}
}

func assertBodyContains(t *testing.T, body []byte, substr string) {
	t.Helper()
	if !strings.Contains(string(body), substr) {
		t.Errorf("expected body to contain %q, got %q", substr, string(body))
	}
}
