package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
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

	log.Printf("DynamoDB table %s created successfully.", tableName)
	return nil
}

var (
	rdb         *redis.Client // Maintained for backwards compatibility / direct tests
	store       StoreReader
	vendor      string
	lambdaProxy *handlerfunc.HandlerFuncAdapter
)

func main() {
	storeType := getEnv("STORE_TYPE", "redis")
	vendor = getEnv("VENDOR_NAME", "myorg")
	apiPort := getEnv("API_PORT", "8080")

	ctx := context.Background()

	if storeType == "dynamodb" {
		log.Println("Initializing DynamoDB store reader backend...")
		tableName := getEnv("DYNAMODB_TABLE_NAME", "shared-store")
		endpoint := os.Getenv("DYNAMODB_ENDPOINT")

		var cfg aws.Config
		var err error

		if endpoint != "" {
			// Local development / emulator override
			log.Printf("DynamoDB using local endpoint override: %s", endpoint)
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
			log.Fatalf("Unable to load AWS config: %v", err)
		}

		dbClient := dynamodb.NewFromConfig(cfg)

		// Pre-create table if running against a local endpoint
		if endpoint != "" {
			if err := createTableIfNotExist(ctx, dbClient, tableName); err != nil {
				log.Printf("Warning: local table check/creation failed: %v", err)
			}
		}

		store = &DynamoDBReader{
			client:    dbClient,
			tableName: tableName,
		}
		log.Printf("DynamoDB backend initialized (Table: %s).", tableName)

	} else {
		log.Println("Initializing Redis store reader backend...")
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
			log.Printf("Connecting to Redis with TLS/SSL enabled...")
		} else {
			log.Printf("Connecting to Redis (TLS disabled)...")
		}

		rdb = redis.NewClient(rdbOpts)

		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := rdb.Ping(pingCtx).Err(); err != nil {
			log.Printf("Warning: Failed to connect to Redis on startup: %v", err)
		} else {
			log.Println("Connected to Redis successfully.")
		}

		store = &RedisReader{client: rdb}
	}

	// Dynamic routing using Go ServeMux
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /{resource}/{id}", handleGetPrimary)
	mux.HandleFunc("GET /{resource}/{by_index}/{value}", handleGetSecondary)

	if os.Getenv("AWS_LAMBDA_FUNCTION_NAME") != "" {
		log.Println("Running Go Reader in AWS Lambda mode...")
		lambdaProxy = handlerfunc.New(mux.ServeHTTP)
		lambda.Start(lambdaProxy.ProxyWithContext)
	} else {
		serverAddr := fmt.Sprintf(":%s", apiPort)
		log.Printf("Dynamic Reader API service listening on %s...", serverAddr)
		if err := http.ListenAndServe(serverAddr, mux); err != nil {
			log.Fatalf("Server failed to start: %v", err)
		}
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
	payload, err := store.GetPrimary(ctx, resource, id)
	if err == ErrNotFound {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(fmt.Sprintf(`{"error":"%s record not found"}`, resource)))
		return
	} else if err != nil {
		log.Printf("Store error: %v", err)
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
	payload, err := store.GetSecondary(ctx, resource, indexName, value)
	if err == ErrNotFound {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(fmt.Sprintf(`{"error":"%s payload not found"}`, resource)))
		return
	} else if err != nil {
		log.Printf("Store error: %v", err)
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
