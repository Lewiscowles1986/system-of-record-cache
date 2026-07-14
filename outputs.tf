output "cache_endpoint_address" {
  value       = aws_elasticache_serverless_cache.this.endpoint[0].address
  description = "The address of the serverless ElastiCache connection endpoint"
}

output "cache_endpoint_port" {
  value       = aws_elasticache_serverless_cache.this.endpoint[0].port
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
  value       = aws_elasticache_user.read_only.user_name
  description = "The username of the read-only Redis user"
}

output "read_write_user_name" {
  value       = aws_elasticache_user.read_write.user_name
  description = "The username of the read-write Redis user"
}

output "user_group_id" {
  value       = aws_elasticache_user_group.this.user_group_id
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
