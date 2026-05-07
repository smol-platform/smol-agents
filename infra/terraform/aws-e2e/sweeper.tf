# Sweeper Lambda + EventBridge schedule.
#
# Every 30 min the Lambda lists EC2 instances tagged
# `knative-agents-e2e=L2` whose LaunchTime > 1h ago and terminates
# them. Belt #3 in the cleanup defense (after Go t.Cleanup + Spot
# auto-reclaim). Implements R-E2E-CLEAN-2.

data "archive_file" "sweeper" {
  type        = "zip"
  source_file = "${path.module}/sweeper/bootstrap"
  output_path = "${path.module}/sweeper/sweeper.zip"
}

resource "aws_lambda_function" "sweeper" {
  function_name    = "knative-agents-e2e-sweeper"
  role             = aws_iam_role.sweeper.arn
  handler          = "bootstrap"
  runtime          = "provided.al2023"
  architectures    = ["arm64"]
  timeout          = 60
  memory_size      = 128
  filename         = data.archive_file.sweeper.output_path
  source_code_hash = data.archive_file.sweeper.output_base64sha256
  environment {
    variables = {
      MAX_AGE_SECONDS = "3600" # 1h
    }
  }
}

data "aws_iam_policy_document" "sweeper_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "sweeper" {
  name               = "knative-agents-e2e-sweeper"
  assume_role_policy = data.aws_iam_policy_document.sweeper_assume.json
}

resource "aws_iam_role_policy_attachment" "sweeper_basic" {
  role       = aws_iam_role.sweeper.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
}

data "aws_iam_policy_document" "sweeper_perms" {
  statement {
    effect    = "Allow"
    actions   = ["ec2:DescribeInstances"]
    resources = ["*"]
  }
  statement {
    effect    = "Allow"
    actions   = ["ec2:TerminateInstances"]
    resources = ["*"]
    condition {
      test     = "StringEquals"
      variable = "ec2:ResourceTag/knative-agents-e2e"
      values   = ["L2"]
    }
  }
}

resource "aws_iam_role_policy" "sweeper" {
  name   = "perms"
  role   = aws_iam_role.sweeper.id
  policy = data.aws_iam_policy_document.sweeper_perms.json
}

resource "aws_cloudwatch_event_rule" "sweeper" {
  name                = "knative-agents-e2e-sweeper"
  description         = "Fires every 30 min to terminate stranded L2 instances."
  schedule_expression = "rate(30 minutes)"
}

resource "aws_cloudwatch_event_target" "sweeper" {
  rule      = aws_cloudwatch_event_rule.sweeper.name
  target_id = "sweeper"
  arn       = aws_lambda_function.sweeper.arn
}

resource "aws_lambda_permission" "sweeper" {
  statement_id  = "AllowEventBridge"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.sweeper.function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.sweeper.arn
}
