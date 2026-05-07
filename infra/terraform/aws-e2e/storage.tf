# Artifact bucket: cloud-init payloads, test logs from interrupted Spot
# instances, manifest tarballs. 7-day expiration keeps cost negligible.

resource "aws_s3_bucket" "artifacts" {
  bucket = "knative-agents-e2e-artifacts-${var.region}"
  tags = {
    "knative-agents-e2e" = "infra"
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "artifacts" {
  bucket = aws_s3_bucket.artifacts.id
  rule {
    id     = "expire-7d"
    status = "Enabled"
    expiration {
      days = 7
    }
    abort_incomplete_multipart_upload {
      days_after_initiation = 1
    }
    filter {}
  }
}

resource "aws_s3_bucket_public_access_block" "artifacts" {
  bucket                  = aws_s3_bucket.artifacts.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# ECR repos: one per image we ship to L2.

resource "aws_ecr_repository" "operator" {
  name                 = "knative-agents/operator"
  image_tag_mutability = "MUTABLE"
}

resource "aws_ecr_repository" "agent" {
  name                 = "knative-agents/agent"
  image_tag_mutability = "MUTABLE"
}

resource "aws_ecr_repository" "ebpf_loader" {
  name                 = "knative-agents/ebpf-loader"
  image_tag_mutability = "MUTABLE"
}

resource "aws_ecr_repository" "secret_proxy" {
  name                 = "knative-agents/secret-proxy"
  image_tag_mutability = "MUTABLE"
}

resource "aws_ecr_repository" "fake_llm" {
  name                 = "knative-agents/fake-llm"
  image_tag_mutability = "MUTABLE"
}

resource "aws_ecr_repository" "fake_gateway" {
  name                 = "knative-agents/fake-gateway"
  image_tag_mutability = "MUTABLE"
}

resource "aws_ecr_lifecycle_policy" "trim" {
  for_each = toset([
    aws_ecr_repository.operator.name,
    aws_ecr_repository.agent.name,
    aws_ecr_repository.ebpf_loader.name,
    aws_ecr_repository.secret_proxy.name,
    aws_ecr_repository.fake_llm.name,
    aws_ecr_repository.fake_gateway.name,
  ])
  repository = each.value
  policy = jsonencode({
    rules = [{
      rulePriority = 1
      description  = "Keep most recent 20 images; expire older."
      selection = {
        tagStatus   = "any"
        countType   = "imageCountMoreThan"
        countNumber = 20
      }
      action = { type = "expire" }
    }]
  })
}
