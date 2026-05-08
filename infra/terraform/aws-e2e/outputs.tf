output "vpc_id" {
  value = local.vpc_id
}

output "subnet_id" {
  value = local.subnet_id
}

output "security_group_id" {
  value = aws_security_group.l2.id
}

output "instance_profile" {
  value = aws_iam_instance_profile.l2.name
}

output "runner_role_arn" {
  description = "ARN to plug into .github/workflows/e2e.yml as STIGEN_AWS_ACCOUNT_ID derives from."
  value       = aws_iam_role.runner.arn
}

output "artifact_bucket" {
  value = aws_s3_bucket.artifacts.id
}

output "ecr_repos" {
  value = {
    operator     = aws_ecr_repository.operator.repository_url
    agent        = aws_ecr_repository.agent.repository_url
    ebpf_loader  = aws_ecr_repository.ebpf_loader.repository_url
    secret_proxy = aws_ecr_repository.secret_proxy.repository_url
    fake_llm     = aws_ecr_repository.fake_llm.repository_url
    fake_gateway = aws_ecr_repository.fake_gateway.repository_url
  }
}

# ecr_registry is the hostname-only form (e.g.
# `123456789.dkr.ecr.us-east-2.amazonaws.com`) that the L2 image
# build script + cloud-init template both want — they prepend
# `/knative-agents/<image>:<tag>` themselves. Derived by stripping
# the path off any one repo URL.
output "ecr_registry" {
  value = split("/", aws_ecr_repository.operator.repository_url)[0]
}

output "sweeper_lambda" {
  value = aws_lambda_function.sweeper.function_name
}

output "nuke_lambda" {
  value = aws_lambda_function.nuke.function_name
}
