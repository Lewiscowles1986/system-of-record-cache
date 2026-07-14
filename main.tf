data "aws_subnet" "selected" {
  count = var.vpc_id == null ? 1 : 0
  id    = var.subnet_ids[0]
}

locals {
  vpc_id             = var.vpc_id != null ? var.vpc_id : data.aws_subnet.selected[0].vpc_id
  security_group_ids = var.security_group_ids != null ? var.security_group_ids : [aws_security_group.cache[0].id]
  bridge_enabled     = var.rabbitmq_password_secret_arn != null
}

# Create security group if not provided
resource "aws_security_group" "cache" {
  count       = var.security_group_ids == null ? 1 : 0
  name        = "${var.name}-${var.environment}-cache-sg"
  description = "Security group for ElastiCache Serverless key-value store"
  vpc_id      = local.vpc_id

  tags = {
    Name        = "${var.name}-${var.environment}-cache-sg"
    Environment = var.environment
  }
}

# Add ingress rules for allowed client security groups
resource "aws_vpc_security_group_ingress_rule" "allowed_ingress" {
  count                        = var.security_group_ids == null ? length(var.allowed_security_group_ids) : 0
  security_group_id            = aws_security_group.cache[0].id
  referenced_security_group_id = var.allowed_security_group_ids[count.index]
  from_port                    = 6379
  to_port                      = 6379
  ip_protocol                  = "tcp"
  description                  = "Allow inbound Redis traffic from authorized client security groups"
}

# Add ingress rule for bridge if bridge is enabled
resource "aws_vpc_security_group_ingress_rule" "bridge_ingress" {
  count                        = (var.security_group_ids == null && local.bridge_enabled) ? 1 : 0
  security_group_id            = aws_security_group.cache[0].id
  referenced_security_group_id = local.bridge_security_group_id
  from_port                    = 6379
  to_port                      = 6379
  ip_protocol                  = "tcp"
  description                  = "Allow inbound Redis traffic from legacy event bridge"
}

# ElastiCache Serverless Cache
resource "aws_elasticache_serverless_cache" "this" {
  engine               = var.engine
  name                 = "${var.name}-${var.environment}"
  major_engine_version = var.major_engine_version
  user_group_id        = aws_elasticache_user_group.this.user_group_id
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
    Name        = "${var.name}-${var.environment}"
    Environment = var.environment
  }
}
