terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.60"
    }
    archive = {
      source  = "hashicorp/archive"
      version = "~> 2.4"
    }
  }

  # Backend points at S3 in the stigen account / us-east-2. The
  # bucket is created OUT-OF-BAND once (chicken-and-egg), then this
  # block stores subsequent state there. To bootstrap from scratch:
  #
  #   1. Comment out this block
  #   2. terraform apply (creates the bucket)
  #   3. Uncomment, terraform init -migrate-state
  backend "s3" {
    bucket         = "knative-agents-e2e-tfstate-us-east-2"
    key            = "aws-e2e/terraform.tfstate"
    region         = "us-east-2"
    profile        = "stigen"
    dynamodb_table = "knative-agents-e2e-tfstate-locks"
    encrypt        = true
  }
}
