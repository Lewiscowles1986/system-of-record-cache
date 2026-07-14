package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	tc "github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

func TestReaderHandlers(t *testing.T) {
	ctx := context.Background()

	// 1. Start ephemeral Redis container using Testcontainers
	redisContainer, err := tcredis.Run(ctx, "redis:7.0-alpine")
	if err != nil {
		t.Fatalf("Failed to start Redis container: %v", err)
	}
	defer func() {
		if err := tc.TerminateContainer(redisContainer); err != nil {
			t.Errorf("Failed to terminate container: %v", err)
		}
	}()

	// 2. Retrieve connection address
	connStr, err := redisContainer.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("Failed to get container connection string: %v", err)
	}

	// 3. Initialize global Redis client
	opts, err := redis.ParseURL(connStr)
	if err != nil {
		t.Fatalf("Failed to parse Redis connection string: %v", err)
	}
	rdb = redis.NewClient(opts)

	// Set globals
	vendor = "testvnd"

	// 4. Seed test data for 'companies' resource
	companyID := "comp-test-123"
	accountID := "acc-test-456"
	compPayload := map[string]interface{}{
		"name":   "Go Test Corp",
		"active": true,
	}
	compBytes, _ := json.Marshal(compPayload)

	compHashKey := fmt.Sprintf("companies:%s", companyID)
	rdb.HSet(ctx, compHashKey, map[string]interface{}{
		"secondary_index": accountID,
		"payload":         string(compBytes),
	})
	rdb.Expire(ctx, compHashKey, 10*time.Second)

	compIndexKey := fmt.Sprintf("companies:index:account_id:%s", accountID)
	rdb.Set(ctx, compIndexKey, companyID, 10*time.Second)

	// 5. Seed test data for 'users' resource (proving dynamic schema capability)
	userID := "user-john-doe"
	email := "john@example.com"
	userPayload := map[string]interface{}{
		"username": "johndoe",
		"email":    email,
	}
	userBytes, _ := json.Marshal(userPayload)

	userHashKey := fmt.Sprintf("users:%s", userID)
	rdb.HSet(ctx, userHashKey, map[string]interface{}{
		"secondary_index": email,
		"payload":         string(userBytes),
	})
	rdb.Expire(ctx, userHashKey, 10*time.Second)

	userIndexKey := fmt.Sprintf("users:index:email:%s", email)
	rdb.Set(ctx, userIndexKey, userID, 10*time.Second)

	// 6. Run tests for primary lookup
	t.Run("Get Company successfully (primary)", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/companies/"+companyID, nil)
		req.SetPathValue("resource", "companies")
		req.SetPathValue("id", companyID)
		rec := httptest.NewRecorder()

		handleGetPrimary(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", rec.Code)
		}

		expectedContentType := "application/json+vnd+testvnd/companiesv1"
		if rec.Header().Get("Content-Type") != expectedContentType {
			t.Errorf("Expected content-type %q, got %q", expectedContentType, rec.Header().Get("Content-Type"))
		}

		var resp map[string]interface{}
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp["name"] != "Go Test Corp" {
			t.Errorf("Expected name 'Go Test Corp', got '%v'", resp["name"])
		}
	})

	t.Run("Get User successfully (primary dynamic resource)", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/users/"+userID, nil)
		req.SetPathValue("resource", "users")
		req.SetPathValue("id", userID)
		rec := httptest.NewRecorder()

		handleGetPrimary(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", rec.Code)
		}

		expectedContentType := "application/json+vnd+testvnd/usersv1"
		if rec.Header().Get("Content-Type") != expectedContentType {
			t.Errorf("Expected content-type %q, got %q", expectedContentType, rec.Header().Get("Content-Type"))
		}

		var resp map[string]interface{}
		json.Unmarshal(rec.Body.Bytes(), &resp)
		if resp["username"] != "johndoe" {
			t.Errorf("Expected username 'johndoe', got '%v'", resp["username"])
		}
	})

	// 7. Run tests for secondary index lookup
	t.Run("Get Company successfully (secondary index)", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/companies/by-account_id/"+accountID, nil)
		req.SetPathValue("resource", "companies")
		req.SetPathValue("by_index", "by-account_id")
		req.SetPathValue("value", accountID)
		rec := httptest.NewRecorder()

		handleGetSecondary(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", rec.Code)
		}

		expectedContentType := "application/json+vnd+testvnd/companiesv1"
		if rec.Header().Get("Content-Type") != expectedContentType {
			t.Errorf("Expected content-type %q, got %q", expectedContentType, rec.Header().Get("Content-Type"))
		}
	})

	t.Run("Get User successfully (secondary index email)", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/users/by-email/"+email, nil)
		req.SetPathValue("resource", "users")
		req.SetPathValue("by_index", "by-email")
		req.SetPathValue("value", email)
		rec := httptest.NewRecorder()

		handleGetSecondary(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", rec.Code)
		}

		expectedContentType := "application/json+vnd+testvnd/usersv1"
		if rec.Header().Get("Content-Type") != expectedContentType {
			t.Errorf("Expected content-type %q, got %q", expectedContentType, rec.Header().Get("Content-Type"))
		}
	})

	t.Run("Get Company secondary index not found", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/companies/by-account_id/non-existent", nil)
		req.SetPathValue("resource", "companies")
		req.SetPathValue("by_index", "by-account_id")
		req.SetPathValue("value", "non-existent")
		rec := httptest.NewRecorder()

		handleGetSecondary(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Errorf("Expected status 404, got %d", rec.Code)
		}
	})

	t.Run("Invalid secondary route format", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/companies/invalid-prefix/value", nil)
		req.SetPathValue("resource", "companies")
		req.SetPathValue("by_index", "invalid-prefix")
		req.SetPathValue("value", "value")
		rec := httptest.NewRecorder()

		handleGetSecondary(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("Expected status 400, got %d", rec.Code)
		}
	})
}
