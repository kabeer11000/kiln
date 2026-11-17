# kiln on AWS: one-time setup per team account.
# Creates only things that cost $0 (or cents) while idle: ECS cluster, security group, IAM roles,
# a log group, an S3 bucket for artifacts and messages (objects expire), an SSM parameter with
# the settings kiln reads, and an optional budget alert.
# No NAT gateway, no load balancer, no always-on compute.

terraform {
  required_version = ">= 1.6"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0"
    }
  }
}

variable "region" {
  type    = string
  default = "us-east-1"
}

variable "stack" {
  description = "Name of this kiln install. Matches aws.stack in kiln.yaml."
  type        = string
  default     = "kiln"
}

variable "log_retention_days" {
  type    = number
  default = 7
}

variable "artifact_retention_days" {
  description = "Run artifacts and messages in S3 are deleted after this many days."
  type        = number
  default     = 30
}

variable "budget_usd" {
  description = "Monthly budget alert for kiln-tagged spend."
  type        = number
  default     = 5
}

variable "alert_email" {
  description = "Email for budget alerts. Empty = no budget alert."
  type        = string
  default     = ""
}

provider "aws" {
  region = var.region
  default_tags {
    tags = { project = "kiln", kiln-stack = var.stack }
  }
}

data "aws_caller_identity" "me" {}

# Default VPC public subnets: tasks get a public IP for outbound traffic, so no NAT gateway (~$32/month).
data "aws_vpc" "default" {
  default = true
}

data "aws_subnets" "default" {
  filter {
    name   = "vpc-id"
    values = [data.aws_vpc.default.id]
  }
  filter {
    name   = "default-for-az"
    values = ["true"]
  }
}

resource "aws_ecs_cluster" "this" {
  name = "kiln-${var.stack}"
  setting {
    name  = "containerInsights"
    value = "disabled" # costs money per task; kiln tracks what it needs itself
  }
}

resource "aws_ecs_cluster_capacity_providers" "this" {
  cluster_name       = aws_ecs_cluster.this.name
  capacity_providers = ["FARGATE", "FARGATE_SPOT"]
}

resource "aws_security_group" "tasks" {
  name        = "kiln-${var.stack}-tasks"
  description = "kiln tasks: outbound only, nothing can connect in"
  vpc_id      = data.aws_vpc.default.id

  egress {
    from_port        = 0
    to_port          = 0
    protocol         = "-1"
    cidr_blocks      = ["0.0.0.0/0"]
    ipv6_cidr_blocks = ["::/0"]
  }
}

resource "aws_cloudwatch_log_group" "this" {
  name              = "/kiln/${var.stack}"
  retention_in_days = var.log_retention_days
}

locals {
  secrets_prefix = "/kiln/${var.stack}/secrets/"
  secrets_arn    = "arn:aws:ssm:${var.region}:${data.aws_caller_identity.me.account_id}:parameter/kiln/${var.stack}/secrets/*"
}

# Used by ECS to pull images, write logs and inject secrets. Tasks themselves get no IAM role,
# so the agent has no AWS permissions at all.
resource "aws_iam_role" "execution" {
  name = "kiln-${var.stack}-execution"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ecs-tasks.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
}

resource "aws_iam_role_policy_attachment" "execution" {
  role       = aws_iam_role.execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

resource "aws_iam_role_policy" "execution_secrets" {
  name = "read-kiln-secrets"
  role = aws_iam_role.execution.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["ssm:GetParameters"]
      Resource = local.secrets_arn
    }]
  })
}

# Attach this to the IAM users/groups/SSO roles of people who run kiln.
resource "aws_iam_policy" "users" {
  name        = "kiln-${var.stack}-users"
  description = "Run and manage kiln tasks on the ${var.stack} stack"
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect   = "Allow"
        Action   = ["ecs:RegisterTaskDefinition", "ecs:DeregisterTaskDefinition", "ecs:DescribeTasks", "ecs:TagResource"]
        Resource = "*"
      },
      {
        Effect    = "Allow"
        Action    = ["ecs:RunTask", "ecs:StopTask"]
        Resource  = "*"
        Condition = { ArnEquals = { "ecs:cluster" = aws_ecs_cluster.this.arn } }
      },
      {
        Effect   = "Allow"
        Action   = ["iam:PassRole"]
        Resource = aws_iam_role.execution.arn
      },
      {
        Effect   = "Allow"
        Action   = ["logs:FilterLogEvents", "logs:GetLogEvents", "logs:StartLiveTail"]
        Resource = ["${aws_cloudwatch_log_group.this.arn}", "${aws_cloudwatch_log_group.this.arn}:*"]
      },
      {
        Effect   = "Allow"
        Action   = ["ssm:GetParameter", "ssm:GetParameters", "ssm:PutParameter"]
        Resource = [aws_ssm_parameter.config.arn, local.secrets_arn]
      },
      {
        Effect   = "Allow"
        Action   = ["ssm:GetParameter"]
        Resource = aws_ssm_parameter.signer.arn
      },
      {
        Effect   = "Allow"
        Action   = ["s3:GetObject", "s3:PutObject"]
        Resource = "${aws_s3_bucket.runs.arn}/runs/*"
      },
      {
        Effect   = "Allow"
        Action   = ["s3:ListBucket"]
        Resource = aws_s3_bucket.runs.arn
      },
    ]
  })
}

# Everything kiln needs to start a task. kiln reads this, so kiln.yaml only says `backend: aws`.
resource "aws_ssm_parameter" "config" {
  name = "/kiln/${var.stack}/config"
  type = "String"
  value = jsonencode({
    cluster            = aws_ecs_cluster.this.name
    subnets            = data.aws_subnets.default.ids
    security_groups    = [aws_security_group.tasks.id]
    execution_role_arn = aws_iam_role.execution.arn
    log_group          = aws_cloudwatch_log_group.this.name
    secrets_prefix     = local.secrets_prefix
    bucket             = aws_s3_bucket.runs.bucket
    signer_parameter   = aws_ssm_parameter.signer.name
  })
}

# Per-run artifacts, monitor uploads and the message inbox. Tasks have no AWS credentials;
# kiln hands each task presigned URLs for its own run's objects only.
resource "aws_s3_bucket" "runs" {
  bucket        = "kiln-${var.stack}-${data.aws_caller_identity.me.account_id}-${var.region}"
  force_destroy = true
}

resource "aws_s3_bucket_public_access_block" "runs" {
  bucket                  = aws_s3_bucket.runs.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_lifecycle_configuration" "runs" {
  bucket = aws_s3_bucket.runs.id
  rule {
    id     = "expire-runs"
    status = "Enabled"
    filter {
      prefix = "runs/"
    }
    expiration {
      days = var.artifact_retention_days
    }
  }
}

# Signs the presigned URLs. Team members' own credentials are often short-lived (SSO, MFA
# sessions), and a URL dies with the credentials that signed it, so a run longer than the
# session would lose its uploads. This key can only read/write run objects in the bucket.
# Its secret is in the terraform state: keep the state private.
resource "aws_iam_user" "signer" {
  name = "kiln-${var.stack}-signer"
}

resource "aws_iam_user_policy" "signer" {
  name = "run-objects-only"
  user = aws_iam_user.signer.name
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["s3:GetObject", "s3:PutObject"]
      Resource = "${aws_s3_bucket.runs.arn}/runs/*"
    }]
  })
}

resource "aws_iam_access_key" "signer" {
  user = aws_iam_user.signer.name
}

resource "aws_ssm_parameter" "signer" {
  name = "/kiln/${var.stack}/signer"
  type = "SecureString"
  value = jsonencode({
    access_key_id     = aws_iam_access_key.signer.id
    secret_access_key = aws_iam_access_key.signer.secret
  })
}

# Alerts only: AWS Budgets cannot stop spend. kiln's per-run caps do that.
# Filters on the project=kiln tag, which must be activated as a cost allocation tag
# (Billing > Cost allocation tags) before it shows spend.
resource "aws_budgets_budget" "kiln" {
  count        = var.alert_email == "" ? 0 : 1
  name         = "kiln-${var.stack}"
  budget_type  = "COST"
  limit_amount = tostring(var.budget_usd)
  limit_unit   = "USD"
  time_unit    = "MONTHLY"

  cost_filter {
    name   = "TagKeyValue"
    values = ["user:project$kiln"]
  }

  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 80
    threshold_type             = "PERCENTAGE"
    notification_type          = "ACTUAL"
    subscriber_email_addresses = [var.alert_email]
  }

  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 100
    threshold_type             = "PERCENTAGE"
    notification_type          = "FORECASTED"
    subscriber_email_addresses = [var.alert_email]
  }
}

output "cluster" {
  value = aws_ecs_cluster.this.name
}

output "users_policy_arn" {
  description = "Attach to everyone who runs kiln."
  value       = aws_iam_policy.users.arn
}

output "kiln_yaml" {
  value = "backend: aws\naws: {region: ${var.region}, stack: ${var.stack}}"
}
