# Default User (Required by ElastiCache, but disabled for security)
resource "aws_elasticache_user" "default" {
  user_id       = "${var.name}-${var.environment}-default"
  user_name     = "default"
  engine        = "REDIS"
  access_string = "off ~* -@all"
  authentication_mode {
    type = "no-password-required"
  }
}

# Read-Only User (IAM Authenticated)
resource "aws_elasticache_user" "read_only" {
  user_id       = "${var.name}-${var.environment}-ro"
  user_name     = "read-only"
  engine        = "REDIS"
  access_string = "on ~* -@all +@read"
  authentication_mode {
    type = "iam"
  }
}

# Read-Write User (IAM Authenticated)
resource "aws_elasticache_user" "read_write" {
  user_id       = "${var.name}-${var.environment}-rw"
  user_name     = "read-write"
  engine        = "REDIS"
  access_string = "on ~* +@all"
  authentication_mode {
    type = "iam"
  }
}

# ElastiCache User Group
resource "aws_elasticache_user_group" "this" {
  engine        = "REDIS"
  user_group_id = "${var.name}-${var.environment}-ug"
  user_ids = [
    aws_elasticache_user.default.user_id,
    aws_elasticache_user.read_only.user_id,
    aws_elasticache_user.read_write.user_id
  ]
}

# IAM Policy for Read-Only Access
resource "aws_iam_policy" "read_only" {
  name        = "${var.name}-${var.environment}-ro-policy"
  description = "Allows read-only connection access to the ${var.name} ElastiCache Serverless cluster"

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = "elasticache:Connect"
        Resource = [
          aws_elasticache_user.read_only.arn,
          aws_elasticache_serverless_cache.this.arn
        ]
      }
    ]
  })
}

# IAM Policy for Read-Write Access
resource "aws_iam_policy" "read_write" {
  name        = "${var.name}-${var.environment}-rw-policy"
  description = "Allows read-write connection access to the ${var.name} ElastiCache Serverless cluster"

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = "elasticache:Connect"
        Resource = [
          aws_elasticache_user.read_write.arn,
          aws_elasticache_serverless_cache.this.arn
        ]
      }
    ]
  })
}
