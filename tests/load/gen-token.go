//go:build ignore

package main

import (
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func main() {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		secret = "integration-test-secret-key-32chars!!"
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub":   "loadtest-user",
		"iss":   "https://auth.example.com",
		"aud":   "api-gateway",
		"exp":   time.Now().Add(2 * time.Hour).Unix(),
		"scope": "read write",
	})
	s, err := token.SignedString([]byte(secret))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(s)
}
