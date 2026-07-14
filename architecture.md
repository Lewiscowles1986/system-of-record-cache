# Architecture Diagram: Shared Key-Value Store & API Gateway

This document provides a visual representation and detailed explanation of the shared high-performance key-value store architecture powered by AWS ElastiCache Serverless (Redis).

## Architecture Diagram

```mermaid
flowchart TB
    %% Nodes & Styling
    classDef client fill:#e1f5fe,stroke:#03a9f4,stroke-width:2px,color:#01579b;
    classDef aws fill:#fff3e0,stroke:#ff9800,stroke-width:2px,color:#e65100;
    classDef security fill:#ffebee,stroke:#f44336,stroke-width:2px,color:#b71c1c;
    classDef redis fill:#e8f5e9,stroke:#4caf50,stroke-width:2px,color:#1b5e20;
    classDef external fill:#f3e5f5,stroke:#9c27b0,stroke-width:2px,color:#4a148c;

    subgraph Client_Space ["Client Layer"]
        SDK["Client SDKs<br/>(Python / TypeScript / Elixir)"]:::client
        Consumer["HTTP Client / Consumer"]:::client
    end

    subgraph AWS_Cloud ["AWS Cloud (VPC Boundary)"]
        
        subgraph IAM_Security ["IAM & Access Control"]
            IAM["AWS IAM Policies<br/>(Read-Only / Read-Write)"]:::security
            Secrets["AWS Secrets Manager<br/>(RabbitMQ Credentials)"]:::security
            ACLs["Redis ACL Groups<br/>(read-only / read-write / default:off)"]:::security
        end
        
        subgraph Data_Cache ["ElastiCache Serverless (Redis 7.0+)"]
            HashKeys["Primary Keys Hash Map<br/>(resource:id)"]:::redis
            IndexKeys["Secondary Indexes String Map<br/>(resource:index:field:val)"]:::redis
            Changelog["Write Log Stream<br/>(changelog)"]:::redis
        end

        subgraph Gateway ["API Gateway Layer"]
            GoReader["Go Reader Service<br/>(Dynamic HTTP API Gateway)"]:::aws
        end

        subgraph Event_Bridge ["Forwarding Bridge Runtime"]
            Bridge["RabbitMQ Event Bridge<br/>(AWS Lambda OR ECS Fargate)"]:::aws
        end
    end

    subgraph Legacy_Broker ["External Messaging"]
        Rabbit["RabbitMQ Exchange<br/>(HTTP Management API)"]:::external
    end

    %% Flows
    SDK -->|"1. SigV4 IAM Authentication"| IAM
    IAM -->|"2. Maps to"| ACLs
    
    %% SDK Writes
    SDK -->|"3a. Write Primary HSET & Secondary SET"| Data_Cache
    SDK -->|"3b. Append write event"| Changelog
    
    %% HTTP Reads
    Consumer -->|"4a. Request /{resource}/{id}"| GoReader
    Consumer -->|"4b. Request /{resource}/by-{index}/{val}"| GoReader
    
    GoReader -->|"5a. Authenticates with"| IAM
    GoReader -->|"5b. Direct HGET lookup"| HashKeys
    GoReader -->|"5c. Secondary Index lookup"| IndexKeys
    IndexKeys -.->|"Resolves to Primary ID"| HashKeys
    
    %% Event forwarding
    Bridge -->|6. Connects via IAM Auth| ACLs
    Bridge -->|7. XREAD batch / XDEL ack| Changelog
    Bridge -->|8. Fetch Credentials| Secrets
    Bridge -->|9. HTTP POST payload| Rabbit

    %% Connection descriptions
    style AWS_Cloud fill:#fafafa,stroke:#ccc,stroke-dasharray: 5 5;
    style IAM_Security fill:#fff9f9,stroke:#ffcdd2;
    style Data_Cache fill:#f1f8e9,stroke:#d0e7b5;
    style Gateway fill:#fffde7,stroke:#fff59d;
    style Event_Bridge fill:#f3e5f5,stroke:#e1bee7;
```

---

## Architectural Breakdown

### 1. Security & Identity Boundary
* **AWS IAM Authentication**: Connection passwords are generated on-the-fly via IAM SigV4 query authentication tokens (expiring after 15 minutes) in both client SDKs and services.
* **Redis ACL Control**:
  * `read-only`: Mapped to reader components; permissions restricted to reading hashes and keys (`+@read`).
  * `read-write`: Mapped to writer components; full operational permissions (`+@all`).
  * `default`: Explicitly disabled (`off`) for security compliance.
* **VPC Isolation**: The cache cluster and dynamic services are placed inside private subnets, with ingress strictly governed by security groups.

### 2. Cache Layout & Indexing Strategy
* **Primary Key Hash Map**: The actual JSON record payloads are stored in Redis Hashes under `{resource}:{id}` keys.
* **Secondary Index Map**: String keys mapped from `{resource}:index:{field}:{value}` containing the primary record identifier as their value. Lookups resolve this key first, then issue an `HGET` on the resolved primary key.
* **TTL Coherency**: Writer client SDKs atomically expire both primary and index keys using identical Time-To-Live parameters to avoid orphaned indexes.

### 3. Reading/Writing Data Flow
1. **Writing**: Client SDKs (available in [Python SDK](file:///Users/lewiscowles/Projects/dynamodb-shared-store/clients/python/store_client.py), [TypeScript SDK](file:///Users/lewiscowles/Projects/dynamodb-shared-store/clients/typescript/store_client.ts), and Elixir SDK) write directly to the primary and secondary cache keys.
2. **Dynamic Reading**: The [Go Reader Service](file:///Users/lewiscowles/Projects/dynamodb-shared-store/services/reader/main.go) serves as a high-performance HTTP gateway to resolve cache entries. It streams raw JSON values directly back to consumers, setting customized MIME content types matching `application/json+vnd+{vendor}/{resource}{version}` automatically.

### 4. Legacy Integration Bridge
* **RabbitMQ Event Bridge**: If configured via [bridge.tf](file:///Users/lewiscowles/Projects/dynamodb-shared-store/bridge.tf), the module deploys a worker (using either AWS Lambda or an ECS Fargate container) that periodically reads records from the `changelog` stream in Redis, checks credential access via Secrets Manager, and forwards those events onto external RabbitMQ broker exchanges before executing an acknowledging deletion (`XDEL`) on the stream.
