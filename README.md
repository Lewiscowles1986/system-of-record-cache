# Shared Key-Value Store & API Gateway (ElastiCache Serverless)

A reusable Terraform infrastructure module and SDK library suite designed to implement a high-performance, shared key-value store on AWS. It replaces table-copying and event-propagation bottlenecks with direct API-level reads, using **AWS ElastiCache Serverless (Redis 7.0+)** for zero-cluster-management auto-scaling and strict IAM/ACL security.

---

## Key Project Features

- **Decoupled Local Development**: Avoid heavy cloud emulators. The entire client/server stack runs against standard local Redis or Docker containers.
- **Auto-Scaling & Simplified Clustering**: Leverage ElastiCache Serverless to scale storage and compute automatically while abstracting sharding underneath a single endpoint.
- **Strict Role Separation**: Generate separate IAM policies for Read-Only and Read-Write users, utilizing Redis ACLs mapped to IAM authentication tokens.
- **Dynamic & Schema-Agnostic Go API**: The Go Reader API streams JSON payloads without parsing them, automatically setting custom MIME Content-Types on dynamic routes.
- **BYO-RabbitMQ Forwarding Bridge**: Setting the `rabbitmq_password_secret_arn` variable dynamically provisions an event forwarder that captures writes to Redis (via a `changelog` stream) and publishes them to your existing RabbitMQ broker exchanges using the HTTP Management API.

---

## Repository Layout

```
.
├── main.tf                 # Terraform: Serverless cache configuration
├── variables.tf            # Terraform: Inputs (KMS & VPC are optional)
├── outputs.tf              # Terraform: Connection endpoints, policy ARNs
├── iam.tf                  # Terraform: Redis Users, Group, and IAM Policies
├── bridge.tf               # Terraform: Redis-to-RabbitMQ forwarding bridge
├── justfile                # Command runner: installs and runs tests
├── README.md               # Root documentation
├── examples/
│   └── complete/
│       └── main.tf         # Terraform: Complete deployment reference
├── services/
│   └── reader/             # Go dynamic API server & testcontainers tests
│       ├── main.go         # Go: HTTP API endpoints and logic
│       └── main_test.go    # Go: Integration tests via Testcontainers
└── clients/
    ├── python/             # Python client SDK & tests
    ├── typescript/         # TypeScript client SDK & tests
    └── elixir/             # Elixir client SDK & tests
```

---

## Local Development & Testing

All clients and services run integration tests against ephemeral Redis instances started automatically via **Testcontainers** in Docker. Tests run in parallel using UUID-prefixed key namespaces to prevent collisions.

### Prerequisites
Make sure you have:
- Docker running locally
- [Just](https://github.com/casey/just) command runner (`brew install just`)
- Python 3.11+, Node 20+, Elixir 1.15+, and Go 1.22+

### Quick Start Commands
```bash
# 1. Install dependencies for all clients and services
just install

# 2. Run the complete integration test suite (Python, TypeScript, Elixir, Go)
just test
```

---

## Go Reader Service Endpoints

The Go Reader API operates as a high-performance proxy, dynamically matching any resource type and secondary index parameter:

- **Primary Key Lookup**: `GET /{resource}/{id}`
  - Resolves Redis key: `{resource}:{id}`
  - Content-Type: `application/json+vnd+{VENDOR}/{resource}{version}`
- **Secondary Index Lookup**: `GET /{resource}/by-{index}/{value}`
  - Resolves Redis key: `{resource}:index:{index}:{value}` to fetch primary ID, then gets the payload.
  - Content-Type: `application/json+vnd+{VENDOR}/{resource}{version}`
- **Health Check**: `GET /health`

---

## Terraform Deployment Quickstart

Deploying the store and securing it for downstream consumers:

```hcl
module "shared_store" {
  source = "git::https://github.com/your-org/dynamodb-shared-store.git//terraform"

  name        = "shared-store"
  environment = "production"
  subnet_ids  = ["subnet-123456", "subnet-789012"]

  # Restrict access to ECS cluster tasks security group
  allowed_security_group_ids = ["sg-client-tasks"]

  # Enable the RabbitMQ forwarding bridge by providing your secret ARN
  rabbitmq_host                = "rabbitmq.internal.myorg.com"
  rabbitmq_port                = 15672
  rabbitmq_username            = "store-publisher"
  rabbitmq_password_secret_arn = "arn:aws:secretsmanager:us-east-1:123456789012:secret:rabbitmq-publish-pwd-xyz"
  bridge_runtime               = "lambda" # Or "ecs" for continuous Fargate polling
}

# Downstream consumers are attached to the generated IAM policies:
# module.shared_store.read_only_policy_arn
# module.shared_store.read_write_policy_arn
```

---

## Client SDK Quickstarts

All SDKs implement a uniform, 4-method interface: `put`, `get`, `getBySecondary`, and `delete`.

### 1. Python SDK
```python
from store_client import SharedStoreClient

# Local Setup (TLS & IAM Auth disabled)
client = SharedStoreClient(host="localhost", port=6379)

# AWS Setup with IAM Auth
client = SharedStoreClient(
    host="your-cache-endpoint.cache.amazonaws.com",
    username="read-write",
    iam_auth=True,
    region="us-east-1"
)

# Put payload with 24h TTL
client.put("comp-123", "acc-456", {"name": "Test Company"}, ttl_seconds=86400)
```

### 2. TypeScript SDK
```typescript
import { SharedStoreClient } from "./store_client";

const client = new SharedStoreClient({
  host: "localhost",
  port: 6379,
});

await client.put("comp-123", "acc-456", { name: "Test Company" }, 86400);
const item = await client.getBySecondary("acc-456");
```

### 3. Elixir SDK
```elixir
{:ok, client} = SharedStoreClient.connect(host: "localhost", port: 6379)

:ok = SharedStoreClient.put(client, "comp-123", "acc-456", %{"name" => "Test"}, 86400)
{:ok, item} = SharedStoreClient.get(client, "comp-123")
```
