provider "aws" {
  region  = var.region
  profile = var.aws_profile

  # Defense in depth — anyone setting these env vars wrong gets
  # rejected. Belt + suspenders for R-E2E-L2-1 ("us-east-2 only").
  default_tags {
    tags = {
      Project              = "knative-agents"
      "knative-agents-e2e" = "infra"
      ManagedBy            = "terraform"
    }
  }
}
