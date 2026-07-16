package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/redis/go-redis/v9"
	tc "github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Shared seed values
const (
	companyID = "comp-test-123"
	accountID = "acc-test-456"
	userID    = "user-john-doe"
	email     = "john@example.com"
)

var (
	compPayload = map[string]interface{}{
		"name":   "Go Test Corp",
		"active": true,
	}
	userPayload = map[string]interface{}{
		"username": "johndoe",
		"email":    email,
	}
)

func TestReaderHandlers_Redis(t *testing.T) {
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
	store = &RedisReader{client: rdb}

	// Set globals
	vendor = "testvnd"

	// Init test schemas & OTel tracer
	os.Setenv("SCHEMAS_DIR", "../../schemas")
	loadSchemas()
	initTracer(ctx)

	// 4. Seed test data for 'companies' resource
	compBytes, _ := json.Marshal(compPayload)
	compHashKey := fmt.Sprintf("companies:%s", companyID)
	rdb.HSet(ctx, compHashKey, map[string]interface{}{
		"secondary_index": accountID,
		"payload":         string(compBytes),
	})
	rdb.Expire(ctx, compHashKey, 10*time.Second)

	compIndexKey := fmt.Sprintf("companies:index:account_id:%s", accountID)
	rdb.Set(ctx, compIndexKey, companyID, 10*time.Second)

	// Seed invalid data to test schema validation error (RFC 9457)
	badCompHashKey := "companies:bad-company-123"
	rdb.HSet(ctx, badCompHashKey, map[string]interface{}{
		"payload": `{"name": 12345, "active": "not-bool"}`,
	})
	rdb.Expire(ctx, badCompHashKey, 10*time.Second)

	// 5. Seed test data for 'users' resource
	userBytes, _ := json.Marshal(userPayload)
	userHashKey := fmt.Sprintf("users:%s", userID)
	rdb.HSet(ctx, userHashKey, map[string]interface{}{
		"secondary_index": email,
		"payload":         string(userBytes),
	})
	rdb.Expire(ctx, userHashKey, 10*time.Second)

	userIndexKey := fmt.Sprintf("users:index:email:%s", email)
	rdb.Set(ctx, userIndexKey, userID, 10*time.Second)

	// 6. Run test suite
	runTestSuite(t)
}

func TestReaderHandlers_DynamoDB(t *testing.T) {
	ctx := context.Background()

	// 1. Start generic container for DynamoDB Local
	dbContainer, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{
		ContainerRequest: tc.ContainerRequest{
			Image:        "amazon/dynamodb-local:latest",
			ExposedPorts: []string{"8000/tcp"},
			WaitingFor:   wait.ForListeningPort("8000/tcp"),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("Failed to start DynamoDB container: %v", err)
	}
	defer func() {
		if err := tc.TerminateContainer(dbContainer); err != nil {
			t.Errorf("Failed to terminate container: %v", err)
		}
	}()

	// 2. Retrieve connection address
	mappedPort, err := dbContainer.MappedPort(ctx, "8000")
	if err != nil {
		t.Fatalf("Failed to get mapped port: %v", err)
	}
	host, err := dbContainer.Host(ctx)
	if err != nil {
		t.Fatalf("Failed to get host: %v", err)
	}
	endpoint := fmt.Sprintf("http://%s:%s", host, mappedPort.Port())

	// 3. Initialize DynamoDB Client
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("dummy", "dummy", "")),
		config.WithEndpointResolverWithOptions(aws.EndpointResolverWithOptionsFunc(
			func(service, region string, options ...interface{}) (aws.Endpoint, error) {
				return aws.Endpoint{
					URL:           endpoint,
					SigningRegion: region,
				}, nil
			},
		)),
	)
	if err != nil {
		t.Fatalf("Failed to load AWS configuration: %v", err)
	}

	dbClient := dynamodb.NewFromConfig(cfg)

	// Create test table
	tableName := "shared-store-test"
	err = createTableIfNotExist(ctx, dbClient, tableName)
	if err != nil {
		t.Fatalf("Failed to create test table: %v", err)
	}

	store = &DynamoDBReader{
		client:    dbClient,
		tableName: tableName,
	}

	// Set globals
	vendor = "testvnd"

	// Init test schemas & OTel tracer
	os.Setenv("SCHEMAS_DIR", "../../schemas")
	loadSchemas()
	initTracer(ctx)

	// 4. Seed test data for 'companies' resource
	compBytes, _ := json.Marshal(compPayload)
	_, err = dbClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(tableName),
		Item: map[string]types.AttributeValue{
			"PK":      &types.AttributeValueMemberS{Value: "companies:" + companyID},
			"payload": &types.AttributeValueMemberS{Value: string(compBytes)},
			"GSI1PK":  &types.AttributeValueMemberS{Value: "companies:index:account_id:" + accountID},
		},
	})
	if err != nil {
		t.Fatalf("Failed to seed company in DynamoDB: %v", err)
	}

	// Seed invalid data to test schema validation error (RFC 9457)
	_, err = dbClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(tableName),
		Item: map[string]types.AttributeValue{
			"PK":      &types.AttributeValueMemberS{Value: "companies:bad-company-123"},
			"payload": &types.AttributeValueMemberS{Value: `{"name": 12345, "active": "not-bool"}`},
		},
	})
	if err != nil {
		t.Fatalf("Failed to seed invalid company in DynamoDB: %v", err)
	}

	// 5. Seed test data for 'users' resource
	userBytes, _ := json.Marshal(userPayload)
	_, err = dbClient.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(tableName),
		Item: map[string]types.AttributeValue{
			"PK":      &types.AttributeValueMemberS{Value: "users:" + userID},
			"payload": &types.AttributeValueMemberS{Value: string(userBytes)},
			"GSI1PK":  &types.AttributeValueMemberS{Value: "users:index:email:" + email},
		},
	})
	if err != nil {
		t.Fatalf("Failed to seed user in DynamoDB: %v", err)
	}

	// 6. Run test suite
	runTestSuite(t)
}

func runTestSuite(t *testing.T) {
	// health check test
	t.Run("Get Health successfully", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/health", nil)
		rec := httptest.NewRecorder()
		handleHealth(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", rec.Code)
		}
	})

	// tests for primary lookup
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

	// tests for secondary index lookup
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

	t.Run("Get Company with Accept format 1", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/companies/"+companyID, nil)
		req.SetPathValue("resource", "companies")
		req.SetPathValue("id", companyID)
		req.Header.Set("Accept", "application/json+vnd+testvnd/companiesv1")
		rec := httptest.NewRecorder()

		handleGetPrimary(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", rec.Code)
		}
		if rec.Header().Get("Content-Type") != "application/json+vnd+testvnd/companiesv1" {
			t.Errorf("Expected Content-Type 'application/json+vnd+testvnd/companiesv1', got %q", rec.Header().Get("Content-Type"))
		}
	})

	t.Run("Get Company with Accept format 2", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/companies/"+companyID, nil)
		req.SetPathValue("resource", "companies")
		req.SetPathValue("id", companyID)
		req.Header.Set("Accept", "application/json+vnd.testvnd.companies.v1")
		rec := httptest.NewRecorder()

		handleGetPrimary(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", rec.Code)
		}
		if rec.Header().Get("Content-Type") != "application/json+vnd.testvnd.companies.v1" {
			t.Errorf("Expected Content-Type 'application/json+vnd.testvnd.companies.v1', got %q", rec.Header().Get("Content-Type"))
		}
	})

	t.Run("Get Company with Accept application/json", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/companies/"+companyID, nil)
		req.SetPathValue("resource", "companies")
		req.SetPathValue("id", companyID)
		req.Header.Set("Accept", "application/json")
		rec := httptest.NewRecorder()

		handleGetPrimary(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", rec.Code)
		}
		if rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("Expected Content-Type 'application/json', got %q", rec.Header().Get("Content-Type"))
		}
	})

	t.Run("Get Corrupted Company fails schema validation (HTTP 500 RFC 9457)", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/companies/bad-company-123", nil)
		req.SetPathValue("resource", "companies")
		req.SetPathValue("id", "bad-company-123")
		rec := httptest.NewRecorder()

		handleGetPrimary(rec, req)

		if rec.Code != http.StatusInternalServerError {
			t.Errorf("Expected status 500, got %d", rec.Code)
		}
		if rec.Header().Get("Content-Type") != "application/problem+json" {
			t.Errorf("Expected Content-Type 'application/problem+json', got %q", rec.Header().Get("Content-Type"))
		}

		var pd ProblemDetails
		if err := json.Unmarshal(rec.Body.Bytes(), &pd); err != nil {
			t.Fatalf("Failed to parse ProblemDetails: %v", err)
		}
		if pd.Title != "Cache Data Corrupted" {
			t.Errorf("Expected title 'Cache Data Corrupted', got %q", pd.Title)
		}
		if len(pd.Errors) == 0 {
			t.Error("Expected error details in ProblemDetails, got empty list")
		}
	})

	t.Run("Get OpenAPI Specification successfully", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/openapi.json", nil)
		rec := httptest.NewRecorder()

		handleOpenAPI(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", rec.Code)
		}
		
		var spec map[string]interface{}
		if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
			t.Fatalf("Failed to parse OpenAPI JSON: %v", err)
		}

		if spec["openapi"] != "3.0.3" {
			t.Errorf("Expected openapi version '3.0.3', got %v", spec["openapi"])
		}

		paths := spec["paths"].(map[string]interface{})
		if paths["/{resource}/{id}"] == nil {
			t.Error("Expected path '/{resource}/{id}' to be defined")
		}
	})

	t.Run("Get OpenAPI Specification in Yolo Mode", func(t *testing.T) {
		os.Setenv("YOLO_MODE", "true")
		defer os.Unsetenv("YOLO_MODE")

		req := httptest.NewRequest("GET", "/openapi.json", nil)
		rec := httptest.NewRecorder()

		handleOpenAPI(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", rec.Code)
		}

		var spec map[string]interface{}
		json.Unmarshal(rec.Body.Bytes(), &spec)
		info := spec["info"].(map[string]interface{})
		if !strings.Contains(info["title"].(string), "YOLO Mode") {
			t.Errorf("Expected YOLO Mode title, got %v", info["title"])
		}
	})

	t.Run("Memory Leak Test", func(t *testing.T) {
		runReq := func() {
			req := httptest.NewRequest("GET", "/companies/"+companyID, nil)
			req.SetPathValue("resource", "companies")
			req.SetPathValue("id", companyID)
			rec := httptest.NewRecorder()
			handleGetPrimary(rec, req)
		}

		// Warmup
		for i := 0; i < 100; i++ {
			runReq()
		}

		// Baseline memory after GC
		runtime.GC()
		var baseline runtime.MemStats
		runtime.ReadMemStats(&baseline)

		// Exercise the HTTP lookup path repeatedly
		iterations := 1000
		for i := 0; i < iterations; i++ {
			runReq()
		}

		// Final memory after GC
		runtime.GC()
		var final runtime.MemStats
		runtime.ReadMemStats(&final)

		growth := int64(final.HeapAlloc) - int64(baseline.HeapAlloc)
		limit := int64(512 * 1024) // 512 KB heap allocation growth limit

		t.Logf("Memory Leak Test -> Baseline: %d KB, Final: %d KB, Growth: %d KB", baseline.HeapAlloc/1024, final.HeapAlloc/1024, growth/1024)
		if growth > limit {
			t.Errorf("Potential memory leak: Heap grew by %d KB over %d iterations (limit: %d KB)", growth/1024, iterations, limit/1024)
		}
	})
}
