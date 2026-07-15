output "cache_endpoint_address" {
  value       = local.use_elasticache ? aws_elasticache_serverless_cache.this[var.name].endpoint[0].address : null
  description = "The address of the serverless ElastiCache connection endpoint"
}

output "cache_endpoint_port" {
  value       = local.use_elasticache ? aws_elasticache_serverless_cache.this[var.name].endpoint[0].port : null
  description = "The port of the serverless ElastiCache connection endpoint"
}

output "read_only_policy_arn" {
  value       = aws_iam_policy.read_only.arn
  description = "The ARN of the IAM policy granting read-only connection permissions"
}

output "read_write_policy_arn" {
  value       = aws_iam_policy.read_write.arn
  description = "The ARN of the IAM policy granting read-write connection permissions"
}

output "read_only_user_name" {
  value       = local.use_elasticache ? aws_elasticache_user.read_only["read-only"].user_name : null
  description = "The username of the read-only Redis user"
}

output "read_write_user_name" {
  value       = local.use_elasticache ? aws_elasticache_user.read_write["read-write"].user_name : null
  description = "The username of the read-write Redis user"
}

output "user_group_id" {
  value       = local.use_elasticache ? aws_elasticache_user_group.this["group"].user_group_id : null
  description = "The ElastiCache User Group ID associated with the cache"
}

output "bridge_enabled" {
  value       = local.bridge_enabled
  description = "Boolean flag indicating if the RabbitMQ forwarding bridge is active"
}

output "rabbitmq_host" {
  value       = var.rabbitmq_host
  description = "The host address of the integrated RabbitMQ broker"
}

output "dynamodb_table_name" {
  value       = local.use_dynamodb ? aws_dynamodb_table.this[local.dynamodb_table_name].name : null
  description = "The name of the DynamoDB table (when using dynamodb storage backend)"
}

output "dynamodb_table_arn" {
  value       = local.use_dynamodb ? aws_dynamodb_table.this[local.dynamodb_table_name].arn : null
  description = "The ARN of the DynamoDB table (when using dynamodb storage backend)"
}
