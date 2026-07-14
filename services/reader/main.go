package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

var (
	rdb    *redis.Client
	vendor string
)

func main() {
	// 1. Read configuration from environment variables
	redisHost := getEnv("REDIS_HOST", "localhost")
	redisPort := getEnv("REDIS_PORT", "6379")
	redisDBStr := getEnv("REDIS_DB", "0")
	redisSSLStr := getEnv("REDIS_SSL", "false")
	redisUsername := os.Getenv("REDIS_USERNAME")
	redisPassword := os.Getenv("REDIS_PASSWORD")
	vendor = getEnv("VENDOR_NAME", "myorg")
	apiPort := getEnv("API_PORT", "8080")

	redisDB, err := strconv.Atoi(redisDBStr)
	if err != nil {
		redisDB = 0
	}

	redisSSL, err := strconv.ParseBool(redisSSLStr)
	if err != nil {
		redisSSL = false
	}

	// 2. Setup Redis client
	rdbOpts := &redis.Options{
		Addr:     fmt.Sprintf("%s:%s", redisHost, redisPort),
		DB:       redisDB,
		Username: redisUsername,
		Password: redisPassword,
	}

	if redisSSL {
		rdbOpts.TLSConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
		}
		log.Printf("Connecting to Redis with TLS/SSL enabled...")
	} else {
		log.Printf("Connecting to Redis (TLS disabled)...")
	}

	rdb = redis.NewClient(rdbOpts)

	// Ping connection
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Printf("Warning: Failed to connect to Redis on startup: %v", err)
	} else {
		log.Println("Connected to Redis successfully.")
	}

	// 3. Define dynamic routing using Go 1.22+ ServeMux
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /{resource}/{id}", handleGetPrimary)
	mux.HandleFunc("GET /{resource}/{by_index}/{value}", handleGetSecondary)

	serverAddr := fmt.Sprintf(":%s", apiPort)
	log.Printf("Dynamic Reader API service listening on %s...", serverAddr)
	if err := http.ListenAndServe(serverAddr, mux); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}

// handleGetPrimary handles GET /{resource}/{id}
func handleGetPrimary(w http.ResponseWriter, r *http.Request) {
	resource := r.PathValue("resource")
	id := r.PathValue("id")

	if resource == "" || id == "" {
		http.Error(w, `{"error":"Missing resource or id parameter"}`, http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	primaryKey := fmt.Sprintf("%s:%s", resource, id)

	// Fetch payload from Redis Hash
	payload, err := rdb.HGet(ctx, primaryKey, "payload").Result()
	if err == redis.Nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(fmt.Sprintf(`{"error":"%s record not found"}`, resource)))
		return
	} else if err != nil {
		log.Printf("Redis error: %v", err)
		http.Error(w, `{"error":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	// Construct dynamic Content-Type header
	version := r.URL.Query().Get("v")
	if version == "" {
		version = "v1"
	}
	contentType := fmt.Sprintf("application/json+vnd+%s/%s%s", vendor, resource, version)

	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(payload))
}

// handleGetSecondary handles GET /{resource}/by-{index}/{value}
func handleGetSecondary(w http.ResponseWriter, r *http.Request) {
	resource := r.PathValue("resource")
	byIndex := r.PathValue("by_index")
	value := r.PathValue("value")

	if resource == "" || byIndex == "" || value == "" {
		http.Error(w, `{"error":"Missing parameters"}`, http.StatusBadRequest)
		return
	}

	// Verify prefix matches "by-"
	if !strings.HasPrefix(byIndex, "by-") {
		http.Error(w, `{"error":"Invalid routing format, expected /by-{index}/"}`, http.StatusBadRequest)
		return
	}
	indexName := strings.TrimPrefix(byIndex, "by-")

	ctx := r.Context()
	indexKey := fmt.Sprintf("%s:index:%s:%s", resource, indexName, value)

	// Resolve primary key from the secondary index key
	primaryVal, err := rdb.Get(ctx, indexKey).Result()
	if err == redis.Nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(fmt.Sprintf(`{"error":"%s index for %s not found"}`, resource, indexName)))
		return
	} else if err != nil {
		log.Printf("Redis error: %v", err)
		http.Error(w, `{"error":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	// Fetch payload from Redis Hash using resolved primary key
	primaryKey := fmt.Sprintf("%s:%s", resource, primaryVal)
	payload, err := rdb.HGet(ctx, primaryKey, "payload").Result()
	if err == redis.Nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(fmt.Sprintf(`{"error":"%s payload not found"}`, resource)))
		return
	} else if err != nil {
		log.Printf("Redis error: %v", err)
		http.Error(w, `{"error":"Internal server error"}`, http.StatusInternalServerError)
		return
	}

	// Construct dynamic Content-Type header
	version := r.URL.Query().Get("v")
	if version == "" {
		version = "v1"
	}
	contentType := fmt.Sprintf("application/json+vnd+%s/%s%s", vendor, resource, version)

	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(payload))
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"UP"}`))
}

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultVal
}
