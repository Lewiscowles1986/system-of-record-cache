variable "name" {
  type        = string
  description = "Name prefix for resources"
}

variable "environment" {
  type        = string
  description = "Deployment environment name (e.g., dev, prod)"
}

variable "subnet_ids" {
  type        = list(string)
  description = "List of VPC subnet IDs where the serverless cache should be provisioned"
}

variable "vpc_id" {
  type        = string
  default     = null
  description = "VPC ID. If not provided, it will be automatically looked up using the subnet IDs."
}

variable "security_group_ids" {
  type        = list(string)
  default     = null
  description = "Optional list of existing Security Group IDs to associate with ElastiCache. If provided, the module will not create a new security group."
}

variable "allowed_security_group_ids" {
  type        = list(string)
  default     = []
  description = "List of security groups that should be granted inbound access to the Redis cache (port 6379)"
}

variable "kms_key_arn" {
  type        = string
  default     = null
  description = "Optional customer-managed KMS Key ARN for encrypting the cache. If null, uses the default AWS-managed service key."
}

variable "engine" {
  type        = string
  default     = "redis"
  description = "The engine for the serverless cache. Valid values: redis, valkey, memcached."
}

variable "major_engine_version" {
  type        = string
  default     = "7"
  description = "Major version of the engine. IAM Auth requires Redis/Valkey version 7 or above."
}

variable "max_storage_gb" {
  type        = number
  default     = 10
  description = "Maximum storage limit for the serverless cache in GB."
}

variable "max_ecpu_per_second" {
  type        = number
  default     = 5000
  description = "Maximum ECPU limit per second."
}

# ==========================================
# BYO-RABBITMQ BRIDGE CONFIGURATION
# ==========================================

variable "rabbitmq_host" {
  type        = string
  default     = null
  description = "Host address of the existing RabbitMQ broker (enables bridge if set)."
}

variable "rabbitmq_port" {
  type        = number
  default     = 5672
  description = "Port number of the existing RabbitMQ broker."
}

variable "rabbitmq_username" {
  type        = string
  default     = null
  description = "Username for the existing RabbitMQ broker."
}

variable "rabbitmq_password_secret_arn" {
  type        = string
  default     = null
  description = "AWS Secrets Manager Secret ARN containing the password for RabbitMQ (providing this implicitly enables the bridge)."
}

variable "bridge_runtime" {
  type        = string
  default     = "lambda"
  description = "Runtime environment for the bridge. Must be either 'lambda' or 'ecs'."
  validation {
    condition     = contains(["lambda", "ecs"], var.bridge_runtime)
    error_message = "bridge_runtime must be either 'lambda' or 'ecs'."
  }
}

variable "bridge_ecs_cluster_arn" {
  type        = string
  default     = null
  description = "The ARN of the ECS cluster where the bridge Fargate service should run (required if bridge_runtime = 'ecs')."
}

variable "bridge_image" {
  type        = string
  default     = "redis-rabbitmq-bridge:latest"
  description = "The Docker image for the bridge consumer (used when bridge_runtime = 'ecs')."
}
