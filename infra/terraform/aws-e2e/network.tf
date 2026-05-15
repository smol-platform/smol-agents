# Network layer: either creates a fresh VPC+IGW+subnet (greenfield)
# or reuses an existing VPC+subnet supplied via var.vpc_id +
# var.subnet_id. Reuse is the escape hatch when the account's
# IGW-per-region quota (default 5) is saturated.

locals {
  byo_vpc = var.vpc_id != ""
}

# --- Greenfield path: create our own networking ----------------------

data "aws_availability_zones" "available" {
  count = local.byo_vpc ? 0 : 1
  state = "available"
  filter {
    name   = "region-name"
    values = [var.region]
  }
}

resource "aws_vpc" "main" {
  count                = local.byo_vpc ? 0 : 1
  cidr_block           = "10.99.0.0/16"
  enable_dns_hostnames = true
  enable_dns_support   = true
  tags = {
    Name = "knative-agents-e2e"
  }
}

resource "aws_internet_gateway" "main" {
  count  = local.byo_vpc ? 0 : 1
  vpc_id = aws_vpc.main[0].id
  tags = {
    Name = "knative-agents-e2e"
  }
}

resource "aws_subnet" "public" {
  count                   = local.byo_vpc ? 0 : 1
  vpc_id                  = aws_vpc.main[0].id
  cidr_block              = "10.99.1.0/24"
  availability_zone       = data.aws_availability_zones.available[0].names[0]
  map_public_ip_on_launch = true
  tags = {
    Name = "knative-agents-e2e-public-a"
  }
}

resource "aws_route_table" "public" {
  count  = local.byo_vpc ? 0 : 1
  vpc_id = aws_vpc.main[0].id
  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main[0].id
  }
  tags = {
    Name = "knative-agents-e2e-public"
  }
}

resource "aws_route_table_association" "public" {
  count          = local.byo_vpc ? 0 : 1
  subnet_id      = aws_subnet.public[0].id
  route_table_id = aws_route_table.public[0].id
}

# --- Resolved IDs ----------------------------------------------------
# Other resources (security group, IAM, the L2 driver via outputs)
# reference these locals and never the raw resource attributes, so
# the BYO/greenfield split stays contained to this file.

locals {
  vpc_id    = local.byo_vpc ? var.vpc_id : aws_vpc.main[0].id
  subnet_id = local.byo_vpc ? var.subnet_id : aws_subnet.public[0].id
}

resource "aws_security_group" "l2" {
  name        = "knative-agents-e2e-l2"
  description = "L2 e2e: full egress, intra-SG full ingress, NodePort 30080/30443 from caller IP."
  vpc_id      = local.vpc_id

  egress {
    description = "all egress"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  tags = {
    Name = "knative-agents-e2e-l2"
  }
}

# Intra-SG: every instance in this SG can talk to every other on
# any port. Replaces the default-VPC SG behaviour for instances we
# launch with knative-agents-e2e-l2 attached.
resource "aws_security_group_rule" "l2_intra" {
  type              = "ingress"
  description       = "intra-SG: all traffic"
  from_port         = 0
  to_port           = 0
  protocol          = "-1"
  security_group_id = aws_security_group.l2.id
  self              = true
}

# NodePort exposure for driver-to-cluster scenarios (fake-llm at
# 30080, fake-gateway at 30443 — phases A and B of the SKIPPED-
# scenarios plan). Only created when test_runner_ingress_cidr is
# set; defaults to closed.
resource "aws_security_group_rule" "l2_nodeport_fake_llm" {
  count             = var.test_runner_ingress_cidr == "" ? 0 : 1
  type              = "ingress"
  description       = "test-runner ingress to fake-llm NodePort"
  from_port         = 30080
  to_port           = 30080
  protocol          = "tcp"
  cidr_blocks       = [var.test_runner_ingress_cidr]
  security_group_id = aws_security_group.l2.id
}

resource "aws_security_group_rule" "l2_nodeport_fake_gateway_http" {
  count             = var.test_runner_ingress_cidr == "" ? 0 : 1
  type              = "ingress"
  description       = "test-runner ingress to fake-gateway HTTP NodePort"
  from_port         = 30081
  to_port           = 30081
  protocol          = "tcp"
  cidr_blocks       = [var.test_runner_ingress_cidr]
  security_group_id = aws_security_group.l2.id
}

resource "aws_security_group_rule" "l2_nodeport_fake_gateway_tcp" {
  count             = var.test_runner_ingress_cidr == "" ? 0 : 1
  type              = "ingress"
  description       = "test-runner ingress to fake-gateway TCP NodePort"
  from_port         = 30443
  to_port           = 30443
  protocol          = "tcp"
  cidr_blocks       = [var.test_runner_ingress_cidr]
  security_group_id = aws_security_group.l2.id
}
