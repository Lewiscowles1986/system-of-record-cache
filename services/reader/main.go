package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/awslabs/aws-lambda-go-api-proxy/handlerfunc"
	"github.com/redis/go-redis/v9"
	"github.com/santhosh-tekuri/jsonschema/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// ErrNotFound is returned when a record is not found in the store
var ErrNotFound = errors.New("record not found")

// StoreReader abstracts storage read operations
type StoreReader interface {
	GetPrimary(ctx context.Context, resource, id string) (string, error)
	GetSecondary(ctx context.Context, resource, indexName, value string) (string, error)
	Close() error
}

// RedisReader implements StoreReader for Redis
type RedisReader struct {
	client *redis.Client
}

func (r *RedisReader) GetPrimary(ctx context.Context, resource, id string) (string, error) {
	key := fmt.Sprintf("%s:%s", resource, id)
	val, err := r.client.HGet(ctx, key, "payload").Result()
	if err == redis.Nil {
		return "", ErrNotFound
	}
	return val, err
}

func (r *RedisReader) GetSecondary(ctx context.Context, resource, indexName, value string) (string, error) {
	indexKey := fmt.Sprintf("%s:index:%s:%s", resource, indexName, value)
	primaryVal, err := r.client.Get(ctx, indexKey).Result()
	if err == redis.Nil {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	primaryKey := fmt.Sprintf("%s:%s", resource, primaryVal)
	val, err := r.client.HGet(ctx, primaryKey, "payload").Result()
	if err == redis.Nil {
		return "", ErrNotFound
	}
	return val, err
}

func (r *RedisReader) Close() error {
	return r.client.Close()
}

// DynamoDBReader implements StoreReader for DynamoDB
type DynamoDBReader struct {
	client    *dynamodb.Client
	tableName string
}

func (d *DynamoDBReader) GetPrimary(ctx context.Context, resource, id string) (string, error) {
	pk := fmt.Sprintf("%s:%s", resource, id)
	out, err := d.client.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(d.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: pk},
		},
	})
	if err != nil {
		return "", err
	}
	if out.Item == nil {
		return "", ErrNotFound
	}
	payloadAttr, exists := out.Item["payload"]
	if !exists {
		return "", fmt.Errorf("payload attribute not found")
	}
	payloadS, ok := payloadAttr.(*types.AttributeValueMemberS)
	if !ok {
		return "", fmt.Errorf("payload is not a string")
	}
	return payloadS.Value, nil
}

func (d *DynamoDBReader) GetSecondary(ctx context.Context, resource, indexName, value string) (string, error) {
	gsi1pk := fmt.Sprintf("%s:index:%s:%s", resource, indexName, value)
	out, err := d.client.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(d.tableName),
		IndexName:              aws.String("GSI1"),
		KeyConditionExpression: aws.String("GSI1PK = :gsi1pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":gsi1pk": &types.AttributeValueMemberS{Value: gsi1pk},
		},
	})
	if err != nil {
		return "", err
	}
	if len(out.Items) == 0 {
		return "", ErrNotFound
	}
	item := out.Items[0]
	payloadAttr, exists := item["payload"]
	if !exists {
		return "", fmt.Errorf("payload attribute not found in index item")
	}
	payloadS, ok := payloadAttr.(*types.AttributeValueMemberS)
	if !ok {
		return "", fmt.Errorf("payload is not a string")
	}
	return payloadS.Value, nil
}

func (d *DynamoDBReader) Close() error {
	return nil
}

// createTableIfNotExist creates a DynamoDB table if it doesn't exist (primarily for local dev emulators)
func createTableIfNotExist(ctx context.Context, client *dynamodb.Client, tableName string) error {
	_, err := client.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(tableName),
	})
	if err == nil {
		return nil
	}

	_, err = client.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   aws.String(tableName),
		BillingMode: types.BillingModePayPerRequest,
		KeySchema: []types.KeySchemaElement{
			{
				AttributeName: aws.String("PK"),
				KeyType:       types.KeyTypeHash,
			},
		},
		AttributeDefinitions: []types.AttributeDefinition{
			{
				AttributeName: aws.String("PK"),
				AttributeType: types.ScalarAttributeTypeS,
			},
			{
				AttributeName: aws.String("GSI1PK"),
				AttributeType: types.ScalarAttributeTypeS,
			},
		},
		GlobalSecondaryIndexes: []types.GlobalSecondaryIndex{
			{
				IndexName: aws.String("GSI1"),
				KeySchema: []types.KeySchemaElement{
					{
						AttributeName: aws.String("GSI1PK"),
						KeyType:       types.KeyTypeHash,
					},
				},
				Projection: &types.Projection{
					ProjectionType: types.ProjectionTypeAll,
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create DynamoDB table %s: %w", tableName, err)
	}

	slog.Info("DynamoDB table created successfully", slog.String("table", tableName))
	return nil
}

var (
	rdb             *redis.Client // Maintained for backwards compatibility / direct tests
	store           StoreReader
	vendor          string
	lambdaProxy     *handlerfunc.HandlerFuncAdapter
	compiledSchemas = make(map[string]*jsonschema.Schema)
	rawSchemas      = make(map[string]interface{})
	tracer          trace.Tracer
)

// ProblemDetails implements RFC 9457 for structured error handling
type ProblemDetails struct {
	Type     string   `json:"type"`
	Title    string   `json:"title"`
	Status   int      `json:"status"`
	Detail   string   `json:"detail"`
	Instance string   `json:"instance"`
	Errors   []string `json:"errors,omitempty"`
}

func writeProblemDetails(w http.ResponseWriter, r *http.Request, title string, status int, detail string, errs []string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	pd := ProblemDetails{
		Type:     fmt.Sprintf("https://problems.myorg.com/%s", strings.ReplaceAll(strings.ToLower(title), " ", "-")),
		Title:    title,
		Status:   status,
		Detail:   detail,
		Instance: r.URL.Path,
		Errors:   errs,
	}
	_ = json.NewEncoder(w).Encode(pd)
}

func initTracer() *sdktrace.TracerProvider {
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	tracer = otel.Tracer("shared-store-reader")
	return tp
}

func loadSchemas() {
	if os.Getenv("YOLO_MODE") == "true" {
		slog.Info("Yolo mode is active, bypassing schema loading.")
		return
	}

	schemasDir := getEnv("SCHEMAS_DIR", "./schemas")
	if _, err := os.Stat(schemasDir); os.IsNotExist(err) {
		slog.Warn("Schemas directory not found", slog.String("dir", schemasDir))
		return
	}
	files, err := os.ReadDir(schemasDir)
	if err != nil {
		slog.Error("Failed to read schemas directory", slog.String("dir", schemasDir), slog.Any("error", err))
		return
	}
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		filePath := filepath.Join(schemasDir, file.Name())
		data, err := os.ReadFile(filePath)
		if err != nil {
			slog.Error("Failed to read schema file", slog.String("file", file.Name()), slog.Any("error", err))
			continue
		}

		name := strings.TrimSuffix(file.Name(), ".json")

		compiler := jsonschema.NewCompiler()
		err = compiler.AddResource(file.Name(), bytes.NewReader(data))
		if err != nil {
			slog.Error("Failed to add resource to compiler", slog.String("file", file.Name()), slog.Any("error", err))
			continue
		}
		schema, err := compiler.Compile(file.Name())
		if err != nil {
			slog.Error("Failed to compile schema", slog.String("file", file.Name()), slog.Any("error", err))
			continue
		}
		compiledSchemas[name] = schema

		var raw interface{}
		if err := json.Unmarshal(data, &raw); err == nil {
			rawSchemas[name] = raw
		}
		slog.Info("Successfully loaded and compiled schema", slog.String("name", name))
	}
}

func negotiateContentType(acceptHeader, queryParamVal, resource string) (string, string) {
	version := queryParamVal
	if version == "" {
		version = "v1"
	}

	defaultContentType := fmt.Sprintf("application/json+vnd+%s/%s%s", vendor, resource, version)
	defaultSchemaKey := fmt.Sprintf("%s.%s", resource, version)

	if acceptHeader == "" || acceptHeader == "*/*" {
		return defaultContentType, defaultSchemaKey
	}

	parts := strings.Split(acceptHeader, ",")
	for _, part := range parts {
		part = strings.TrimSpace(strings.Split(part, ";")[0])
		if part == "application/json" {
			return "application/json", resource
		}

		prefix1 := fmt.Sprintf("application/json+vnd+%s/", vendor)
		if strings.HasPrefix(part, prefix1) {
			resVer := strings.TrimPrefix(part, prefix1)
			if strings.HasPrefix(resVer, resource) {
				ver := strings.TrimPrefix(resVer, resource)
				if ver != "" {
					return part, fmt.Sprintf("%s.%s", resource, ver)
				}
			}
		}

		prefix2 := fmt.Sprintf("application/json+vnd.%s.%s.", vendor, resource)
		if strings.HasPrefix(part, prefix2) {
			ver := strings.TrimPrefix(part, prefix2)
			if ver != "" {
				return part, fmt.Sprintf("%s.%s", resource, ver)
			}
		}
	}

	// Double check if application/json is listed as a secondary fallback
	for _, part := range parts {
		part = strings.TrimSpace(strings.Split(part, ";")[0])
		if part == "application/json" {
			return "application/json", resource
		}
	}

	return defaultContentType, defaultSchemaKey
}

func validatePayload(ctx context.Context, schemaKey, payload string) error {
	if os.Getenv("YOLO_MODE") == "true" {
		return nil
	}

	schema, ok := compiledSchemas[schemaKey]
	if !ok {
		baseKey := strings.Split(schemaKey, ".")[0]
		schema, ok = compiledSchemas[baseKey]
		if !ok {
			slog.DebugContext(ctx, "No schema found for validation, bypassing", slog.String("key", schemaKey))
			return nil
		}
	}

	var v interface{}
	if err := json.Unmarshal([]byte(payload), &v); err != nil {
		return fmt.Errorf("payload is not valid JSON: %w", err)
	}

	if err := schema.Validate(v); err != nil {
		return err
	}
	return nil
}

func main() {
	// 1. Initialize structured logger
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	storeType := getEnv("STORE_TYPE", "redis")
	vendor = getEnv("VENDOR_NAME", "myorg")
	apiPort := getEnv("API_PORT", "8080")

	ctx := context.Background()

	// 2. Initialize OpenTelemetry tracing
	tp := initTracer()
	defer func() {
		_ = tp.Shutdown(ctx)
	}()

	// 3. Load JSON Schemas
	loadSchemas()

	if storeType == "dynamodb" {
		slog.Info("Initializing DynamoDB store reader backend...")
		tableName := getEnv("DYNAMODB_TABLE_NAME", "shared-store")
		endpoint := os.Getenv("DYNAMODB_ENDPOINT")

		var cfg aws.Config
		var err error

		if endpoint != "" {
			slog.Info("DynamoDB using local endpoint override", slog.String("endpoint", endpoint))
			cfg, err = config.LoadDefaultConfig(ctx,
				config.WithRegion(getEnv("AWS_REGION", "us-east-1")),
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
		} else {
			cfg, err = config.LoadDefaultConfig(ctx,
				config.WithRegion(getEnv("AWS_REGION", "us-east-1")),
			)
		}

		if err != nil {
			slog.Error("Unable to load AWS config", slog.Any("error", err))
			os.Exit(1)
		}

		dbClient := dynamodb.NewFromConfig(cfg)

		if endpoint != "" {
			if err := createTableIfNotExist(ctx, dbClient, tableName); err != nil {
				slog.Warn("Local table check/creation failed", slog.Any("error", err))
			}
		}

		store = &DynamoDBReader{
			client:    dbClient,
			tableName: tableName,
		}
		slog.Info("DynamoDB backend initialized", slog.String("table", tableName))

	} else {
		slog.Info("Initializing Redis store reader backend...")
		redisHost := getEnv("REDIS_HOST", "localhost")
		redisPort := getEnv("REDIS_PORT", "6379")
		redisDBStr := getEnv("REDIS_DB", "0")
		redisSSLStr := getEnv("REDIS_SSL", "false")
		redisUsername := os.Getenv("REDIS_USERNAME")
		redisPassword := os.Getenv("REDIS_PASSWORD")

		redisDB, err := strconv.Atoi(redisDBStr)
		if err != nil {
			redisDB = 0
		}

		redisSSL, err := strconv.ParseBool(redisSSLStr)
		if err != nil {
			redisSSL = false
		}

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
			slog.Info("Connecting to Redis with TLS/SSL enabled...")
		} else {
			slog.Info("Connecting to Redis (TLS disabled)...")
		}

		rdb = redis.NewClient(rdbOpts)

		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := rdb.Ping(pingCtx).Err(); err != nil {
			slog.Warn("Failed to connect to Redis on startup", slog.Any("error", err))
		} else {
			slog.Info("Connected to Redis successfully.")
		}

		store = &RedisReader{client: rdb}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /openapi.json", handleOpenAPI)
	mux.HandleFunc("GET /{resource}/{id}", handleGetPrimary)
	mux.HandleFunc("GET /{resource}/{by_index}/{value}", handleGetSecondary)

	if os.Getenv("AWS_LAMBDA_FUNCTION_NAME") != "" {
		slog.Info("Running Go Reader in AWS Lambda mode...")
		lambdaProxy = handlerfunc.New(mux.ServeHTTP)
		lambda.Start(lambdaProxy.ProxyWithContext)
	} else {
		serverAddr := fmt.Sprintf(":%s", apiPort)
		slog.Info("Dynamic Reader API service listening", slog.String("addr", serverAddr))
		if err := http.ListenAndServe(serverAddr, mux); err != nil {
			slog.Error("Server failed to start", slog.Any("error", err))
			os.Exit(1)
		}
	}
}

// handleGetPrimary handles GET /{resource}/{id}
func handleGetPrimary(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "handleGetPrimary")
	defer span.End()

	resource := r.PathValue("resource")
	id := r.PathValue("id")

	span.SetAttributes(
		attribute.String("http.method", r.Method),
		attribute.String("http.route", "/{resource}/{id}"),
		attribute.String("resource", resource),
		attribute.String("id", id),
	)

	if resource == "" || id == "" {
		writeProblemDetails(w, r, "Bad Request", http.StatusBadRequest, "Missing resource or id parameter", nil)
		return
	}

	payload, err := store.GetPrimary(ctx, resource, id)
	if err == ErrNotFound {
		writeProblemDetails(w, r, "Resource Not Found", http.StatusNotFound, fmt.Sprintf("%s record not found", resource), nil)
		return
	} else if err != nil {
		slog.ErrorContext(ctx, "Failed to retrieve primary key", slog.String("resource", resource), slog.String("id", id), slog.Any("error", err))
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		writeProblemDetails(w, r, "Internal Server Error", http.StatusInternalServerError, "Failed to fetch record from store", nil)
		return
	}

	// Content Negotiation
	acceptHeader := r.Header.Get("Accept")
	queryParamVal := r.URL.Query().Get("v")
	contentType, schemaKey := negotiateContentType(acceptHeader, queryParamVal, resource)

	// Runtime validation
	if err := validatePayload(ctx, schemaKey, payload); err != nil {
		slog.ErrorContext(ctx, "Cache payload schema validation failed",
			slog.String("resource", resource),
			slog.String("id", id),
			slog.String("schemaKey", schemaKey),
			slog.Any("error", err))
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		errMsgs := []string{err.Error()}
		writeProblemDetails(w, r, "Cache Data Corrupted", http.StatusInternalServerError,
			"The data retrieved from upstream store failed internal schema validation constraints", errMsgs)
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(payload))
}

// handleGetSecondary handles GET /{resource}/by-{index}/{value}
func handleGetSecondary(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "handleGetSecondary")
	defer span.End()

	resource := r.PathValue("resource")
	byIndex := r.PathValue("by_index")
	value := r.PathValue("value")

	span.SetAttributes(
		attribute.String("http.method", r.Method),
		attribute.String("http.route", "/{resource}/{by_index}/{value}"),
		attribute.String("resource", resource),
		attribute.String("by_index", byIndex),
		attribute.String("value", value),
	)

	if resource == "" || byIndex == "" || value == "" {
		writeProblemDetails(w, r, "Bad Request", http.StatusBadRequest, "Missing parameters", nil)
		return
	}

	if !strings.HasPrefix(byIndex, "by-") {
		writeProblemDetails(w, r, "Bad Request", http.StatusBadRequest, "Invalid routing format, expected /by-{index}/", nil)
		return
	}
	indexName := strings.TrimPrefix(byIndex, "by-")

	payload, err := store.GetSecondary(ctx, resource, indexName, value)
	if err == ErrNotFound {
		writeProblemDetails(w, r, "Resource Not Found", http.StatusNotFound, fmt.Sprintf("%s index for %s not found", resource, indexName), nil)
		return
	} else if err != nil {
		slog.ErrorContext(ctx, "Failed to retrieve secondary key",
			slog.String("resource", resource),
			slog.String("index", indexName),
			slog.String("value", value),
			slog.Any("error", err))
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		writeProblemDetails(w, r, "Internal Server Error", http.StatusInternalServerError, "Failed to fetch record from store", nil)
		return
	}

	// Content Negotiation
	acceptHeader := r.Header.Get("Accept")
	queryParamVal := r.URL.Query().Get("v")
	contentType, schemaKey := negotiateContentType(acceptHeader, queryParamVal, resource)

	// Runtime validation
	if err := validatePayload(ctx, schemaKey, payload); err != nil {
		slog.ErrorContext(ctx, "Cache payload index schema validation failed",
			slog.String("resource", resource),
			slog.String("index", indexName),
			slog.String("value", value),
			slog.String("schemaKey", schemaKey),
			slog.Any("error", err))
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())

		errMsgs := []string{err.Error()}
		writeProblemDetails(w, r, "Cache Data Corrupted", http.StatusInternalServerError,
			"The data retrieved from upstream store failed internal schema validation constraints", errMsgs)
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(payload))
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"UP"}`))
}

func handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if os.Getenv("YOLO_MODE") == "true" {
		_, _ = fmt.Fprint(w, `{"openapi":"3.0.3","info":{"title":"Shared Store Reader API (YOLO Mode)","version":"1.0.0"},"paths":{"/{resource}/{id}":{"get":{"summary":"Primary Lookup (Yolo Mode)","responses":{"200":{"description":"Success","content":{"application/json":{}}}}}},"/{resource}/by-{index}/{value}":{"get":{"summary":"Secondary Lookup (Yolo Mode)","responses":{"200":{"description":"Success","content":{"application/json":{}}}}}}}}`)
		return
	}

	type Component struct {
		Schemas map[string]interface{} `json:"schemas"`
	}
	type ResponseContent struct {
		Schema map[string]interface{} `json:"schema"`
	}
	type Response struct {
		Description string                     `json:"description"`
		Content     map[string]ResponseContent `json:"content"`
	}
	type PathItem struct {
		Summary    string              `json:"summary"`
		Parameters []interface{}       `json:"parameters"`
		Responses  map[string]Response `json:"responses"`
	}
	type OpenAPI struct {
		OpenAPI    string              `json:"openapi"`
		Info       map[string]string   `json:"info"`
		Paths      map[string]PathItem `json:"paths"`
		Components Component           `json:"components"`
	}

	spec := OpenAPI{
		OpenAPI: "3.0.3",
		Info: map[string]string{
			"title":       "Shared Store Reader API",
			"version":     "1.0.0",
			"description": "Dynamic HTTP API gateway for high-performance key-value retrieval",
		},
		Paths:      make(map[string]PathItem),
		Components: Component{Schemas: make(map[string]interface{})},
	}

	resourcesSet := make(map[string]bool)
	for key, schemaObj := range rawSchemas {
		spec.Components.Schemas[key] = schemaObj
		res := strings.Split(key, ".")[0]
		resourcesSet[res] = true
	}

	var resources []string
	for res := range resourcesSet {
		resources = append(resources, res)
	}

	resourceParam := map[string]interface{}{
		"name":     "resource",
		"in":       "path",
		"required": true,
		"schema": map[string]interface{}{
			"type": "string",
			"enum": resources,
		},
	}

	idParam := map[string]interface{}{
		"name":     "id",
		"in":       "path",
		"required": true,
		"schema": map[string]string{
			"type": "string",
		},
	}

	indexParam := map[string]interface{}{
		"name":     "by_index",
		"in":       "path",
		"required": true,
		"schema": map[string]string{
			"type": "string",
		},
	}

	valueParam := map[string]interface{}{
		"name":     "value",
		"in":       "path",
		"required": true,
		"schema": map[string]string{
			"type": "string",
		},
	}

	contentPrimary := make(map[string]ResponseContent)
	contentSecondary := make(map[string]ResponseContent)

	for key := range rawSchemas {
		parts := strings.Split(key, ".")
		res := parts[0]
		ver := "v1"
		if len(parts) > 1 {
			ver = parts[1]
		}

		ct1 := fmt.Sprintf("application/json+vnd+%s/%s%s", vendor, res, ver)
		ct2 := fmt.Sprintf("application/json+vnd.%s.%s.%s", vendor, res, ver)

		refMap := map[string]interface{}{"$ref": fmt.Sprintf("#/components/schemas/%s", key)}
		contentPrimary[ct1] = ResponseContent{Schema: refMap}
		contentPrimary[ct2] = ResponseContent{Schema: refMap}
		contentSecondary[ct1] = ResponseContent{Schema: refMap}
		contentSecondary[ct2] = ResponseContent{Schema: refMap}
	}

	contentPrimary["application/json"] = ResponseContent{Schema: map[string]interface{}{"type": "object"}}
	contentSecondary["application/json"] = ResponseContent{Schema: map[string]interface{}{"type": "object"}}

	spec.Paths["/{resource}/{id}"] = PathItem{
		Summary:    "Primary Key Lookup",
		Parameters: []interface{}{resourceParam, idParam},
		Responses: map[string]Response{
			"200": {
				Description: "Successful dynamic lookup",
				Content:     contentPrimary,
			},
			"500": {
				Description: "Cache data invalid (RFC 9457 Problem Details)",
				Content: map[string]ResponseContent{
					"application/problem+json": {},
				},
			},
		},
	}

	spec.Paths["/{resource}/{by_index}/{value}"] = PathItem{
		Summary:    "Secondary Index Lookup",
		Parameters: []interface{}{resourceParam, indexParam, valueParam},
		Responses: map[string]Response{
			"200": {
				Description: "Successful secondary lookup",
				Content:     contentSecondary,
			},
			"500": {
				Description: "Cache data invalid (RFC 9457 Problem Details)",
				Content: map[string]ResponseContent{
					"application/problem+json": {},
				},
			},
		},
	}

	_ = json.NewEncoder(w).Encode(spec)
}

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultVal
}
