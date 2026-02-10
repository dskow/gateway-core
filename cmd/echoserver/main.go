// Package main provides a simple echo server for testing the gateway.
// It returns request details as JSON, useful for verifying routing,
// header injection, and prefix stripping.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	port := flag.Int("port", 3001, "port to listen on")
	name := flag.String("name", "echo", "service name")
	flag.Parse()

	if p := os.Getenv("PORT"); p != "" {
		fmt.Sscanf(p, "%d", port)
	}
	if n := os.Getenv("SERVICE_NAME"); n != "" {
		*name = n
	}

	// /__status/{code} returns an arbitrary HTTP status code.
	// Useful for testing error handling, retries, and metrics.
	// Example: GET /__status/503 → 503 Service Unavailable
	http.HandleFunc("/__status/", func(w http.ResponseWriter, r *http.Request) {
		codeStr := strings.TrimPrefix(r.URL.Path, "/__status/")
		code, err := strconv.Atoi(codeStr)
		if err != nil || code < 100 || code > 599 {
			code = 500
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"service":        *name,
			"requested_code": code,
			"message":        http.StatusText(code),
		})
	})

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]interface{}{
			"service":     *name,
			"method":      r.Method,
			"path":        r.URL.Path,
			"query":       r.URL.RawQuery,
			"headers":     flattenHeaders(r.Header),
			"remote_addr": r.RemoteAddr,
			"timestamp":   time.Now().UTC().Format(time.RFC3339),
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(resp)
	})

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("%s listening on %s", *name, addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

func flattenHeaders(h http.Header) map[string]string {
	flat := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) == 1 {
			flat[k] = v[0]
		} else {
			b, _ := json.Marshal(v)
			flat[k] = string(b)
		}
	}
	return flat
}
