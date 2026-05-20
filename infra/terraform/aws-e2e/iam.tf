# Two IAM roles:
#
# 1. smol-agents-e2e-runner — assumed by GHA via OIDC. Permissions
#    scoped tightly: RunInstances on c6gd.metal Spot only,
#    Terminate/Describe on tagged instances, SSM SendCommand, S3 +
#    ECR pulls.
#
# 2. smol-agents-e2e-l2 — instance profile attached to EC2.
#    Permissions: S3 GetObject for the artifact bucket + SSM agent.

# ---------------------------- runner role ----------------------------

data "aws_iam_policy_document" "gha_oidc_trust" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRoleWithWebIdentity"]
    principals {
      type        = "Federated"
      identifiers = [aws_iam_openid_connect_provider.github.arn]
    }
    condition {
      test     = "StringEquals"
      variable = "token.actions.githubusercontent.com:aud"
      values   = ["sts.amazonaws.com"]
    }
    condition {
      test     = "StringLike"
      variable = "token.actions.githubusercontent.com:sub"
      values   = ["repo:${var.github_repository}:*"]
    }
  }
}

resource "aws_iam_openid_connect_provider" "github" {
  url             = "https://token.actions.githubusercontent.com"
  client_id_list  = ["sts.amazonaws.com"]
  thumbprint_list = ["6938fd4d98bab03faadb97b34396831e3780aea1"]
}

resource "aws_iam_role" "runner" {
  name               = "smol-agents-e2e-runner"
  assume_role_policy = data.aws_iam_policy_document.gha_oidc_trust.json
}

data "aws_iam_policy_document" "runner_perms" {
  statement {
    effect = "Allow"
    actions = [
      "ec2:RunInstances",
      "ec2:CreateTags",
    ]
    resources = ["*"]
    condition {
      test     = "StringEquals"
      variable = "ec2:InstanceType"
      values   = [var.instance_type]
    }
  }
  statement {
    effect  = "Allow"
    actions = ["ec2:RunInstances", "ec2:CreateTags"]
    resources = ["arn:aws:ec2:${var.region}:*:network-interface/*",
      "arn:aws:ec2:${var.region}:*:subnet/*",
      "arn:aws:ec2:${var.region}:*:security-group/*",
      "arn:aws:ec2:${var.region}:*:image/*",
      "arn:aws:ec2:${var.region}:*:volume/*",
      "arn:aws:ec2:${var.region}:*:key-pair/*",
    "arn:aws:ec2:${var.region}:*:instance/*"]
  }
  statement {
    effect = "Allow"
    actions = [
      "ec2:DescribeInstances",
      "ec2:DescribeInstanceStatus",
      "ec2:DescribeImages",
      "ec2:DescribeSubnets",
      "ec2:DescribeSecurityGroups",
    ]
    resources = ["*"]
  }
  statement {
    effect    = "Allow"
    actions   = ["ec2:TerminateInstances"]
    resources = ["*"]
    condition {
      test     = "StringEquals"
      variable = "ec2:ResourceTag/smol-agents-e2e"
      values   = ["L2"]
    }
  }
  statement {
    effect    = "Allow"
    actions   = ["iam:PassRole"]
    resources = [aws_iam_role.l2_instance.arn]
  }
  statement {
    effect = "Allow"
    actions = [
      "ssm:SendCommand",
      "ssm:GetCommandInvocation",
      "ssm:DescribeInstanceInformation",
    ]
    resources = ["*"]
  }
  statement {
    effect    = "Allow"
    actions   = ["s3:GetObject", "s3:PutObject"]
    resources = ["${aws_s3_bucket.artifacts.arn}/*"]
  }
  statement {
    effect = "Allow"
    actions = [
      "ecr:GetAuthorizationToken",
      "ecr:BatchCheckLayerAvailability",
      "ecr:GetDownloadUrlForLayer",
      "ecr:BatchGetImage",
      "ecr:PutImage",
      "ecr:InitiateLayerUpload",
      "ecr:UploadLayerPart",
      "ecr:CompleteLayerUpload",
    ]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "runner" {
  name   = "runner-perms"
  role   = aws_iam_role.runner.id
  policy = data.aws_iam_policy_document.runner_perms.json
}

# ---------------------------- L2 instance role ----------------------

data "aws_iam_policy_document" "ec2_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "l2_instance" {
  name               = "smol-agents-e2e-l2"
  assume_role_policy = data.aws_iam_policy_document.ec2_assume.json
}

resource "aws_iam_role_policy_attachment" "ssm_managed" {
  role       = aws_iam_role.l2_instance.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

data "aws_iam_policy_document" "l2_instance_extra" {
  statement {
    effect    = "Allow"
    actions   = ["s3:GetObject", "s3:PutObject"]
    resources = ["${aws_s3_bucket.artifacts.arn}/*"]
  }
  statement {
    effect = "Allow"
    actions = [
      "ecr:GetAuthorizationToken",
      "ecr:BatchCheckLayerAvailability",
      "ecr:GetDownloadUrlForLayer",
      "ecr:BatchGetImage",
    ]
    resources = ["*"]
  }
}

resource "aws_iam_role_policy" "l2_extra" {
  name   = "l2-extra"
  role   = aws_iam_role.l2_instance.id
  policy = data.aws_iam_policy_document.l2_instance_extra.json
}

resource "aws_iam_instance_profile" "l2" {
  name = "smol-agents-e2e-l2"
  role = aws_iam_role.l2_instance.name
}
