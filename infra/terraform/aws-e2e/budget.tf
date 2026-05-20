# Budget alarm + nuke Lambda.
#
# At 80% of monthly_budget_usd → SNS notify (informational).
# At 100% → SNS → nuke Lambda terminates ALL smol-agents-e2e=*
# tagged instances. Implements R-E2E-COST-4 + R-E2E-CLEAN-3.

resource "aws_sns_topic" "budget_alerts" {
  name = "smol-agents-e2e-budget-alerts"
}

resource "aws_budgets_budget" "e2e" {
  name              = "smol-agents-e2e"
  budget_type       = "COST"
  limit_amount      = tostring(var.monthly_budget_usd)
  limit_unit        = "USD"
  time_unit         = "MONTHLY"
  time_period_start = "2026-01-01_00:00"

  cost_filter {
    name   = "TagKeyValue"
    values = ["user:smol-agents-e2e$L2", "user:smol-agents-e2e$infra"]
  }

  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 50
    threshold_type             = "PERCENTAGE"
    notification_type          = "FORECASTED"
    subscriber_sns_topic_arns  = [aws_sns_topic.budget_alerts.arn]
    subscriber_email_addresses = []
  }

  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 80
    threshold_type             = "PERCENTAGE"
    notification_type          = "ACTUAL"
    subscriber_sns_topic_arns  = [aws_sns_topic.budget_alerts.arn]
    subscriber_email_addresses = []
  }

  notification {
    comparison_operator        = "GREATER_THAN"
    threshold                  = 100
    threshold_type             = "PERCENTAGE"
    notification_type          = "ACTUAL"
    subscriber_sns_topic_arns  = [aws_sns_topic.budget_alerts.arn]
    subscriber_email_addresses = []
  }
}

# Nuke Lambda — wired to the SNS topic. When 100% threshold fires,
# this terminates every L2 instance regardless of age.

data "archive_file" "nuke" {
  type        = "zip"
  source_file = "${path.module}/budget/bootstrap"
  output_path = "${path.module}/budget/nuke.zip"
}

resource "aws_lambda_function" "nuke" {
  function_name    = "smol-agents-e2e-nuke"
  role             = aws_iam_role.nuke.arn
  handler          = "bootstrap"
  runtime          = "provided.al2023"
  architectures    = ["arm64"]
  timeout          = 60
  memory_size      = 128
  filename         = data.archive_file.nuke.output_path
  source_code_hash = data.archive_file.nuke.output_base64sha256
}

resource "aws_iam_role" "nuke" {
  name               = "smol-agents-e2e-nuke"
  assume_role_policy = data.aws_iam_policy_document.sweeper_assume.json
}

resource "aws_iam_role_policy_attachment" "nuke_basic" {
  role       = aws_iam_role.nuke.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
}

data "aws_iam_policy_document" "nuke_perms" {
  statement {
    effect    = "Allow"
    actions   = ["ec2:DescribeInstances"]
    resources = ["*"]
  }
  statement {
    effect    = "Allow"
    actions   = ["ec2:TerminateInstances"]
    resources = ["*"]
    # Nuke is broader than sweeper — when budget triggers, kill
    # anything tagged smol-agents-e2e (any value).
    condition {
      test     = "StringLike"
      variable = "ec2:ResourceTag/smol-agents-e2e"
      values   = ["*"]
    }
  }
}

resource "aws_iam_role_policy" "nuke" {
  name   = "perms"
  role   = aws_iam_role.nuke.id
  policy = data.aws_iam_policy_document.nuke_perms.json
}

resource "aws_sns_topic_subscription" "nuke" {
  topic_arn = aws_sns_topic.budget_alerts.arn
  protocol  = "lambda"
  endpoint  = aws_lambda_function.nuke.arn
}

resource "aws_lambda_permission" "nuke" {
  statement_id  = "AllowSNS"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.nuke.function_name
  principal     = "sns.amazonaws.com"
  source_arn    = aws_sns_topic.budget_alerts.arn
}
