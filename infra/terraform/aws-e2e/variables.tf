variable "aws_profile" {
  description = "AWS CLI profile to use; pinned to 'stigen' per memory/aws_e2e_account.md."
  type        = string
  default     = "stigen"
  validation {
    condition     = var.aws_profile == "stigen"
    error_message = "Only the stigen sandbox account is supported; other profiles are blocked at the variable level."
  }
}

variable "region" {
  description = "AWS region; locked to us-east-2 per memory/aws_e2e_account.md."
  type        = string
  default     = "us-east-2"
  validation {
    condition     = var.region == "us-east-2"
    error_message = "Only us-east-2 is supported. Switching is a deliberate spec change."
  }
}

variable "monthly_budget_usd" {
  description = "AWS Budget cap for tag knative-agents-e2e (USD/month). Spec default $50."
  type        = number
  default     = 50
}

variable "active_l2_instance_cap" {
  description = "Pre-flight refuses new L2 runs if this many active instances exist."
  type        = number
  default     = 3
}

variable "github_repository" {
  description = "github_owner/repo for GHA OIDC trust. Format: owner/repo."
  type        = string
  default     = "stigen/knative-agents"
}

variable "instance_type" {
  description = "Bare-metal instance type for L2. Locked to c6gd.metal (only path to /dev/kvm at lowest Spot price)."
  type        = string
  default     = "c6gd.metal"
  validation {
    condition     = var.instance_type == "c6gd.metal"
    error_message = "Only c6gd.metal is supported (cheapest bare-metal with Graviton arm64)."
  }
}
