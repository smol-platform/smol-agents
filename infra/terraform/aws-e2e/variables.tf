variable "aws_profile" {
  description = "AWS CLI profile name pointing at the sandbox account; required."
  type        = string
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

# Bring-your-own-VPC: when set, the module reuses an existing VPC +
# public subnet instead of creating its own. Useful when the
# sandbox's IGW quota is already saturated, or when sharing
# networking with other workloads. Both must be set together.
variable "vpc_id" {
  description = "Existing VPC ID to reuse. Empty means create a new VPC + IGW + subnet."
  type        = string
  default     = ""
}

variable "subnet_id" {
  description = "Existing public subnet ID inside vpc_id. Required when vpc_id is set."
  type        = string
  default     = ""
  validation {
    condition     = (var.subnet_id == "") == (var.vpc_id == "")
    error_message = "vpc_id and subnet_id must both be set or both empty."
  }
}
