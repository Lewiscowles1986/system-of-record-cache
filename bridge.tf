locals {
  bridge_security_group_id = local.bridge_enabled ? try(aws_security_group.bridge[0].id, "") : ""
}

# Bridge network security group (only created if bridge is active)
resource "aws_security_group" "bridge" {
  count       = local.bridge_enabled ? 1 : 0
  name        = "${var.name}-${var.environment}-bridge-sg"
  description = "Security group for the RabbitMQ forwarder bridge"
  vpc_id      = local.vpc_id

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name        = "${var.name}-${var.environment}-bridge-sg"
    Environment = var.environment
  }
}

# ==========================================
# LAMBDA RUNTIME RESOURCES
# ==========================================

resource "aws_iam_role" "lambda_bridge" {
  count = (local.bridge_enabled && var.bridge_runtime == "lambda") ? 1 : 0
  name  = "${var.name}-${var.environment}-bridge-lambda-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Action = "sts:AssumeRole"
        Effect = "Allow"
        Principal = {
          Service = "lambda.amazonaws.com"
        }
      }
    ]
  })
}

resource "aws_iam_role_policy_attachment" "lambda_vpc" {
  count      = (local.bridge_enabled && var.bridge_runtime == "lambda") ? 1 : 0
  role       = aws_iam_role.lambda_bridge[0].name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaVPCAccessExecutionRole"
}

resource "aws_iam_role_policy" "lambda_cache_connect" {
  count = (local.bridge_enabled && var.bridge_runtime == "lambda" && local.use_elasticache) ? 1 : 0
  name  = "${var.name}-${var.environment}-bridge-lambda-cache-policy"
  role  = aws_iam_role.lambda_bridge[0].id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = "elasticache:Connect"
        Resource = [
          aws_elasticache_user.read_write["read-write"].arn,
          aws_elasticache_serverless_cache.this[var.name].arn
        ]
      }
    ]
  })
}

resource "aws_iam_role_policy" "lambda_dynamodb_stream_connect" {
  count = (local.bridge_enabled && var.bridge_runtime == "lambda" && local.use_dynamodb) ? 1 : 0
  name  = "${var.name}-${var.environment}-bridge-lambda-ddb-stream"
  role  = aws_iam_role.lambda_bridge[0].id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = [
          "dynamodb:GetRecords",
          "dynamodb:GetShardIterator",
          "dynamodb:DescribeStream",
          "dynamodb:ListStreams"
        ]
        Resource = [
          aws_dynamodb_table.this[local.dynamodb_table_name].stream_arn
        ]
      }
    ]
  })
}

# Grant Lambda permission to read from Secrets Manager
resource "aws_iam_role_policy" "lambda_bridge_secrets" {
  count = (local.bridge_enabled && var.bridge_runtime == "lambda") ? 1 : 0
  name  = "${var.name}-${var.environment}-bridge-lambda-secrets"
  role  = aws_iam_role.lambda_bridge[0].id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = "secretsmanager:GetSecretValue"
        Resource = var.rabbitmq_password_secret_arn
      }
    ]
  })
}

data "archive_file" "lambda_zip" {
  count       = (local.bridge_enabled && var.bridge_runtime == "lambda") ? 1 : 0
  type        = "zip"
  output_path = "${path.module}/lambda_bridge.zip"

  source {
    content  = <<EOF
import os
import json
import base64
import urllib.request
import redis
import boto3

def handler(event, context):
    # 1. Fetch RabbitMQ password from Secrets Manager
    secret_arn = os.environ['RABBITMQ_SECRET_ARN']
    sm_client = boto3.client('secretsmanager')
    resp = sm_client.get_secret_value(SecretId=secret_arn)
    password = resp['SecretString']

    # Prepare RabbitMQ request parameters
    rabbitmq_host = os.environ['RABBITMQ_HOST']
    rabbitmq_port = os.environ['RABBITMQ_PORT']
    rabbitmq_user = os.environ['RABBITMQ_USER']
    
    # Base64 encode credentials for basic authentication
    auth_str = f"{rabbitmq_user}:{password}"
    auth_encoded = base64.b64encode(auth_str.encode('utf-8')).decode('utf-8')
    headers = {
        'Content-Type': 'application/json',
        'Authorization': f"Basic {auth_encoded}"
    }

    # Handle DynamoDB Stream Trigger Event if present
    if event and 'Records' in event:
        print(f"Processing {len(event['Records'])} records from DynamoDB stream...")
        for record in event['Records']:
            if record.get('eventName') in ('INSERT', 'MODIFY'):
                ddb = record.get('dynamodb', {})
                new_image = ddb.get('NewImage', {})
                
                pk_attr = new_image.get('PK', {})
                pk_val = pk_attr.get('S', '')
                
                payload_attr = new_image.get('payload', {})
                payload_val = payload_attr.get('S', '{}')
                
                if not pk_val or not payload_val:
                    continue
                
                # Extract resource prefix from PK (e.g. companies:comp-123 -> companies)
                resource = pk_val.split(':')[0] if ':' in pk_val else 'unknown'
                
                # Post directly to RabbitMQ exchange HTTP API
                rabbitmq_url = f"http://{rabbitmq_host}:{rabbitmq_port}/api/exchanges/%2f/{resource}/publish"
                req_body = {
                    "properties": {},
                    "routing_key": resource,
                    "payload": payload_val,
                    "payload_encoding": "string"
                }
                req = urllib.request.Request(rabbitmq_url, data=json.dumps(req_body).encode('utf-8'), headers=headers, method='POST')
                try:
                    with urllib.request.urlopen(req) as f:
                        print(f"Forwarded DynamoDB stream event to RabbitMQ exchange {resource}. Status: {f.status}")
                except Exception as err:
                    print(f"Failed to forward DynamoDB stream event: {err}")
        return {"status": "success"}

    # Fallback: Redis Stream polling
    redis_host = os.environ.get('REDIS_HOST', '')
    if not redis_host:
        print("Redis host environment variable is empty. Bypassing Redis polling.")
        return {"status": "success"}

    redis_port = int(os.environ.get('REDIS_PORT', 6379))
    redis_user = os.environ.get('REDIS_USER', 'default')

    # Connect to Redis (SSL enabled)
    r = redis.Redis(host=redis_host, port=redis_port, username=redis_user, ssl=True, decode_responses=True)

    # Read up to 100 messages from Redis Write Stream 'changelog'
    stream_name = os.environ.get('REDIS_STREAM_NAME', 'changelog')
    try:
        events = r.xread({stream_name: '0'}, count=100)
    except Exception as e:
        print(f"Error reading from Redis stream: {e}")
        return {"status": "error"}

    if not events:
        print("No new events in Redis stream.")
        return {"status": "success"}

    # Iterate and publish Redis events
    for stream_key, messages in events:
        for msg_id, data in messages:
            resource = data.get("resource", "unknown")
            payload = data.get("payload", "{}")
            
            # Post directly to RabbitMQ exchange HTTP API
            rabbitmq_url = f"http://{rabbitmq_host}:{rabbitmq_port}/api/exchanges/%2f/{resource}/publish"
            req_body = {
                "properties": {},
                "routing_key": resource,
                "payload": payload,
                "payload_encoding": "string"
            }
            req = urllib.request.Request(rabbitmq_url, data=json.dumps(req_body).encode('utf-8'), headers=headers, method='POST')
            try:
                with urllib.request.urlopen(req) as f:
                    print(f"Forwarded Redis stream event {msg_id} to RabbitMQ. Status: {f.status}")
                # Acknowledge by deleting message from Redis stream
                r.xdel(stream_name, msg_id)
            except Exception as err:
                print(f"Failed to forward Redis stream message {msg_id} to RabbitMQ: {err}")

    return {"status": "success"}
EOF
    filename = "index.py"
  }
}

resource "aws_lambda_function" "bridge" {
  count            = (local.bridge_enabled && var.bridge_runtime == "lambda") ? 1 : 0
  filename         = data.archive_file.lambda_zip[0].output_path
  function_name    = "${var.name}-${var.environment}-bridge"
  role             = aws_iam_role.lambda_bridge[0].arn
  handler          = "index.handler"
  runtime          = "python3.11"
  source_code_hash = data.archive_file.lambda_zip[0].output_base64sha256

  vpc_config {
    subnet_ids         = var.subnet_ids
    security_group_ids = [aws_security_group.bridge[0].id]
  }

  environment {
    variables = {
      REDIS_HOST           = local.use_elasticache ? aws_elasticache_serverless_cache.this[var.name].endpoint[0].address : ""
      REDIS_PORT           = local.use_elasticache ? tostring(aws_elasticache_serverless_cache.this[var.name].endpoint[0].port) : ""
      REDIS_USER           = local.use_elasticache ? aws_elasticache_user.read_write["read-write"].user_name : ""
      REDIS_STREAM_NAME    = "changelog"
      RABBITMQ_HOST        = var.rabbitmq_host
      RABBITMQ_PORT        = tostring(var.rabbitmq_port)
      RABBITMQ_USER        = var.rabbitmq_username
      RABBITMQ_SECRET_ARN  = var.rabbitmq_password_secret_arn
    }
  }
}

# Run the Lambda function periodically (e.g. every minute) to forward writes (Redis only)
resource "aws_cloudwatch_event_rule" "every_minute" {
  count               = (local.bridge_enabled && var.bridge_runtime == "lambda" && local.use_elasticache) ? 1 : 0
  name                = "${var.name}-${var.environment}-bridge-trigger"
  description         = "Triggers the Redis-to-RabbitMQ bridge forwarder"
  schedule_expression = "rate(1 minute)"
}

resource "aws_cloudwatch_event_target" "trigger_bridge" {
  count     = (local.bridge_enabled && var.bridge_runtime == "lambda" && local.use_elasticache) ? 1 : 0
  rule      = aws_cloudwatch_event_rule.every_minute[0].name
  target_id = "bridge"
  arn       = aws_lambda_function.bridge[0].arn
}

resource "aws_lambda_permission" "allow_cloudwatch" {
  count         = (local.bridge_enabled && var.bridge_runtime == "lambda" && local.use_elasticache) ? 1 : 0
  statement_id  = "AllowExecutionFromCloudWatch"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.bridge[0].function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.every_minute[0].arn
}

# Dynamic event stream mapping for DynamoDB stream trigger (DynamoDB only)
resource "aws_lambda_event_source_mapping" "dynamodb_stream" {
  count             = (local.bridge_enabled && var.bridge_runtime == "lambda" && local.use_dynamodb) ? 1 : 0
  event_source_arn  = aws_dynamodb_table.this[local.dynamodb_table_name].stream_arn
  function_name     = aws_lambda_function.bridge[0].arn
  starting_position = "LATEST"
}

# ==========================================
# ECS FARGATE RUNTIME RESOURCES
# ==========================================

resource "aws_iam_role" "ecs_execution" {
  count = (local.bridge_enabled && var.bridge_runtime == "ecs") ? 1 : 0
  name  = "${var.name}-${var.environment}-ecs-exec-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Action = "sts:AssumeRole"
      Effect = "Allow"
      Principal = { Service = "ecs-tasks.amazonaws.com" }
    }]
  })
}

resource "aws_iam_role_policy_attachment" "ecs_execution" {
  count      = (local.bridge_enabled && var.bridge_runtime == "ecs") ? 1 : 0
  role       = aws_iam_role.ecs_execution[0].name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

resource "aws_iam_role" "ecs_task" {
  count = (local.bridge_enabled && var.bridge_runtime == "ecs") ? 1 : 0
  name  = "${var.name}-${var.environment}-ecs-task-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Action = "sts:AssumeRole"
      Effect = "Allow"
      Principal = { Service = "ecs-tasks.amazonaws.com" }
    }]
  })
}

resource "aws_iam_role_policy" "ecs_task_cache_connect" {
  count = (local.bridge_enabled && var.bridge_runtime == "ecs" && local.use_elasticache) ? 1 : 0
  name  = "${var.name}-${var.environment}-ecs-task-cache"
  role  = aws_iam_role.ecs_task[0].id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = "elasticache:Connect"
        Resource = [
          aws_elasticache_user.read_write["read-write"].arn,
          aws_elasticache_serverless_cache.this[var.name].arn
        ]
      }
    ]
  })
}

resource "aws_iam_role_policy" "ecs_task_dynamodb_stream_connect" {
  count = (local.bridge_enabled && var.bridge_runtime == "ecs" && local.use_dynamodb) ? 1 : 0
  name  = "${var.name}-${var.environment}-ecs-task-ddb-stream"
  role  = aws_iam_role.ecs_task[0].id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = [
          "dynamodb:GetRecords",
          "dynamodb:GetShardIterator",
          "dynamodb:DescribeStream",
          "dynamodb:ListStreams"
        ]
        Resource = [
          aws_dynamodb_table.this[local.dynamodb_table_name].stream_arn
        ]
      }
    ]
  })
}

# Grant ECS task permission to read from Secrets Manager
resource "aws_iam_role_policy" "ecs_task_secrets" {
  count = (local.bridge_enabled && var.bridge_runtime == "ecs") ? 1 : 0
  name  = "${var.name}-${var.environment}-bridge-ecs-secrets"
  role  = aws_iam_role.ecs_task[0].id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = "secretsmanager:GetSecretValue"
        Resource = var.rabbitmq_password_secret_arn
      }
    ]
  })
}

resource "aws_ecs_task_definition" "bridge" {
  count                    = (local.bridge_enabled && var.bridge_runtime == "ecs") ? 1 : 0
  family                   = "${var.name}-${var.environment}-bridge"
  network_mode             = "awsvpc"
  requires_compatibilities = ["FARGATE"]
  cpu                      = "256"
  memory                   = "512"
  execution_role_arn       = aws_iam_role.ecs_execution[0].arn
  task_role_arn            = aws_iam_role.ecs_task[0].arn

  container_definitions = jsonencode([{
    name      = "bridge"
    image     = var.bridge_image
    essential = true
    environment = [
      { name = "REDIS_HOST", value = local.use_elasticache ? aws_elasticache_serverless_cache.this[var.name].endpoint[0].address : "" },
      { name = "REDIS_PORT", value = local.use_elasticache ? tostring(aws_elasticache_serverless_cache.this[var.name].endpoint[0].port) : "" },
      { name = "REDIS_USER", value = local.use_elasticache ? aws_elasticache_user.read_write["read-write"].user_name : "" },
      { name = "REDIS_STREAM_NAME", value = "changelog" },
      { name = "RABBITMQ_HOST", value = var.rabbitmq_host },
      { name = "RABBITMQ_PORT", value = tostring(var.rabbitmq_port) },
      { name = "RABBITMQ_USER", value = var.rabbitmq_username },
      { name = "RABBITMQ_SECRET_ARN", value = var.rabbitmq_password_secret_arn },
      { name = "DYNAMODB_STREAM_ARN", value = local.use_dynamodb ? aws_dynamodb_table.this[local.dynamodb_table_name].stream_arn : "" }
    ]
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        "awslogs-group"         = "/ecs/${var.name}-${var.environment}-bridge"
        "awslogs-region"        = data.aws_region.current.name
        "awslogs-stream-prefix" = "bridge"
      }
    }
  }])
}

resource "aws_cloudwatch_log_group" "ecs" {
  count             = (local.bridge_enabled && var.bridge_runtime == "ecs") ? 1 : 0
  name              = "/ecs/${var.name}-${var.environment}-bridge"
  retention_in_days = 7
}

resource "aws_ecs_service" "bridge" {
  count           = (local.bridge_enabled && var.bridge_runtime == "ecs") ? 1 : 0
  name            = "${var.name}-${var.environment}-bridge"
  cluster         = var.bridge_ecs_cluster_arn
  task_definition = aws_ecs_task_definition.bridge[0].arn
  desired_count   = 1
  launch_type     = "FARGATE"

  network_configuration {
    subnets          = var.subnet_ids
    security_groups  = [aws_security_group.bridge[0].id]
    assign_public_ip = false
  }
}
