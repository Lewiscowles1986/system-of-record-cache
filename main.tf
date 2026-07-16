data "aws_subnet" "selected" {
  count = var.vpc_id == null ? 1 : 0
  id    = var.subnet_ids[0]
}

locals {
  use_dynamodb        = var.storage_backend == "dynamodb"
  use_elasticache     = var.storage_backend == "elasticache"
  vpc_id              = var.vpc_id != null ? var.vpc_id : data.aws_subnet.selected[0].vpc_id
  dynamodb_table_name = var.dynamodb_table_name != null ? var.dynamodb_table_name : "${var.name}-${var.environment}"
  security_group_ids  = local.use_elasticache ? (var.security_group_ids != null ? var.security_group_ids : [aws_security_group.cache[var.name].id]) : []
  bridge_enabled      = var.rabbitmq_password_secret_arn != null
}

# Create security group if not provided
resource "aws_security_group" "cache" {
  for_each    = (var.security_group_ids == null && local.use_elasticache) ? { (var.name) = true } : {}
  name        = "${each.key}-${var.environment}-cache-sg"
  description = "Security group for ElastiCache Serverless key-value store"
  vpc_id      = local.vpc_id

  tags = {
    Name        = "${each.key}-${var.environment}-cache-sg"
    Environment = var.environment
  }
}

# Add ingress rules for allowed client security groups
resource "aws_vpc_security_group_ingress_rule" "allowed_ingress" {
  for_each                     = (var.security_group_ids == null && local.use_elasticache) ? toset(var.allowed_security_group_ids) : []
  security_group_id            = aws_security_group.cache[var.name].id
  referenced_security_group_id = each.key
  from_port                    = 6379
  to_port                      = 6379
  ip_protocol                  = "tcp"
  description                  = "Allow inbound Redis traffic from authorized client security groups"
}

# Add ingress rule for bridge if bridge is enabled
resource "aws_vpc_security_group_ingress_rule" "bridge_ingress" {
  for_each                     = (var.security_group_ids == null && local.bridge_enabled) ? { "bridge" = true } : {}
  security_group_id            = aws_security_group.cache[var.name].id
  referenced_security_group_id = local.bridge_security_group_id
  from_port                    = 6379
  to_port                      = 6379
  ip_protocol                  = "tcp"
  description                  = "Allow inbound Redis traffic from legacy event bridge"
}

# ElastiCache Serverless Cache
resource "aws_elasticache_serverless_cache" "this" {
  for_each             = local.use_elasticache ? { (var.name) = true } : {}
  engine               = var.engine
  name                 = "${each.key}-${var.environment}"
  major_engine_version = var.major_engine_version
  user_group_id        = aws_elasticache_user_group.this["group"].user_group_id
  subnet_ids           = var.subnet_ids
  security_group_ids   = local.security_group_ids
  kms_key_id           = var.kms_key_arn

  cache_usage_limits {
    data_storage {
      maximum = var.max_storage_gb
      unit    = "GB"
    }
    ecpu_per_second {
      maximum = var.max_ecpu_per_second
    }
  }

  tags = {
    Name        = "${each.key}-${var.environment}"
    Environment = var.environment
  }
}

# DynamoDB Serverless Key-Value Store
resource "aws_dynamodb_table" "this" {
  for_each     = local.use_dynamodb ? { (local.dynamodb_table_name) = true } : {}
  name         = each.key
  billing_mode = "PAY_PER_REQUEST"
  hash_key     = "PK"

  attribute {
    name = "PK"
    type = "S"
  }

  attribute {
    name = "GSI1PK"
    type = "S"
  }

  global_secondary_index {
    name            = "GSI1"
    hash_key        = "GSI1PK"
    projection_type = "ALL"
  }

  ttl {
    attribute_name = "ttl"
    enabled        = true
  }

  stream_enabled   = local.bridge_enabled
  stream_view_type = local.bridge_enabled ? "NEW_IMAGE" : null

  tags = {
    Name        = each.key
    Environment = var.environment
  }
}
