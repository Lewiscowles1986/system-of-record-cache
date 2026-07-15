# Default User (Required by ElastiCache, but disabled for security)
resource "aws_elasticache_user" "default" {
  for_each      = local.use_elasticache ? { "default" = true } : {}
  user_id       = "${var.name}-${var.environment}-${each.key}"
  user_name     = each.key
  engine        = "REDIS"
  access_string = "off ~* -@all"
  authentication_mode {
    type = "no-password-required"
  }
}

# Read-Only User (IAM Authenticated)
resource "aws_elasticache_user" "read_only" {
  for_each      = local.use_elasticache ? { "read-only" = true } : {}
  user_id       = "${var.name}-${var.environment}-ro"
  user_name     = each.key
  engine        = "REDIS"
  access_string = "on ~* -@all +@read"
  authentication_mode {
    type = "iam"
  }
}

# Read-Write User (IAM Authenticated)
resource "aws_elasticache_user" "read_write" {
  for_each      = local.use_elasticache ? { "read-write" = true } : {}
  user_id       = "${var.name}-${var.environment}-rw"
  user_name     = each.key
  engine        = "REDIS"
  access_string = "on ~* +@all"
  authentication_mode {
    type = "iam"
  }
}

# ElastiCache User Group
resource "aws_elasticache_user_group" "this" {
  for_each      = local.use_elasticache ? { "group" = true } : {}
  engine        = "REDIS"
  user_group_id = "${var.name}-${var.environment}-ug"
  user_ids = [
    aws_elasticache_user.default["default"].user_id,
    aws_elasticache_user.read_only["read-only"].user_id,
    aws_elasticache_user.read_write["read-write"].user_id
  ]
}

# IAM Policy for Read-Only Access
resource "aws_iam_policy" "read_only" {
  name        = "${var.name}-${var.environment}-ro-policy"
  description = "Allows read-only access to the ${var.name} store (Redis or DynamoDB)"

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = local.use_dynamodb ? [
      {
        Effect = "Allow"
        Action = [
          "dynamodb:GetItem",
          "dynamodb:BatchGetItem",
          "dynamodb:Query"
        ]
        Resource = [
          aws_dynamodb_table.this[local.dynamodb_table_name].arn,
          "${aws_dynamodb_table.this[local.dynamodb_table_name].arn}/index/*"
        ]
      }
    ] : [
      {
        Effect = "Allow"
        Action = "elasticache:Connect"
        Resource = [
          aws_elasticache_user.read_only["read-only"].arn,
          aws_elasticache_serverless_cache.this[var.name].arn
        ]
      }
    ]
  })
}

# IAM Policy for Read-Write Access
resource "aws_iam_policy" "read_write" {
  name        = "${var.name}-${var.environment}-rw-policy"
  description = "Allows read-write access to the ${var.name} store (Redis or DynamoDB)"

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = local.use_dynamodb ? [
      {
        Effect = "Allow"
        Action = [
          "dynamodb:GetItem",
          "dynamodb:BatchGetItem",
          "dynamodb:Query",
          "dynamodb:PutItem",
          "dynamodb:UpdateItem",
          "dynamodb:DeleteItem",
          "dynamodb:BatchWriteItem"
        ]
        Resource = [
          aws_dynamodb_table.this[local.dynamodb_table_name].arn,
          "${aws_dynamodb_table.this[local.dynamodb_table_name].arn}/index/*"
        ]
      }
    ] : [
      {
        Effect = "Allow"
        Action = "elasticache:Connect"
        Resource = [
          aws_elasticache_user.read_write["read-write"].arn,
          aws_elasticache_serverless_cache.this[var.name].arn
        ]
      }
    ]
  })
}
