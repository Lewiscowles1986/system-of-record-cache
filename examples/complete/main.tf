provider "aws" {
  region = "us-east-1"
}

# Mock networking setup for complete context
resource "aws_vpc" "main" {
  cidr_block = "10.0.0.0/16"
  tags = {
    Name = "shared-store-vpc"
  }
}

resource "aws_subnet" "private_1" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.0.1.0/24"
  availability_zone = "us-east-1a"
}

resource "aws_subnet" "private_2" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.0.2.0/24"
  availability_zone = "us-east-1b"
}

# Client ECS Fargate Task Security Group (allowed client)
resource "aws_security_group" "ecs_client" {
  name        = "ecs-service-client-sg"
  description = "Security group for ECS tasks accessing key-value store"
  vpc_id      = aws_vpc.main.id
}

# Instantiating the shared key-value store module
module "shared_store" {
  source = "../../"

  name        = "shared-store"
  environment = "staging"

  vpc_id     = aws_vpc.main.id
  subnet_ids = [aws_subnet.private_1.id, aws_subnet.private_2.id]

  # Allow ECS tasks in this security group to reach Redis on 6379
  allowed_security_group_ids = [aws_security_group.ecs_client.id]

  # Enable transition bridge implicitly by providing the Secret ARN
  rabbitmq_host                = "rabbitmq.internal.myorg.com"
  rabbitmq_port                = 15672 # Using RabbitMQ HTTP Management API port
  rabbitmq_username            = "store-publisher"
  rabbitmq_password_secret_arn = "arn:aws:secretsmanager:us-east-1:123456789012:secret:rabbitmq-publish-pwd-xyz"
  bridge_runtime               = "lambda"

  # Optional limits for storage and ECPUs (Auto-scaling limits)
  max_storage_gb      = 20
  max_ecpu_per_second = 10000
}

# Output useful outputs for referencing
output "connection_endpoint" {
  value = module.shared_store.cache_endpoint_address
}

output "read_only_iam_policy" {
  value = module.shared_store.read_only_policy_arn
}

output "read_write_iam_policy" {
  value = module.shared_store.read_write_policy_arn
}
