#!/usr/bin/env bash
# sweep.sh — manual escape hatch for stranded L2 EC2 instances.
# Terminates every instance tagged knative-agents-e2e=L2 in us-east-2
# under the stigen profile. Use only when the sweeper Lambda + budget
# alarm both failed.
#
# Satisfies R-E2E-VRF-3.
set -euo pipefail

PROFILE=${AWS_PROFILE:-stigen}
REGION=${AWS_REGION:-us-east-2}

if [[ "$REGION" != "us-east-2" ]]; then
  echo "REGION must be us-east-2 (got $REGION)" >&2
  exit 1
fi

ids=$(aws --profile "$PROFILE" --region "$REGION" ec2 describe-instances \
  --filters \
    'Name=tag:knative-agents-e2e,Values=L2,L1,infra' \
    'Name=instance-state-name,Values=pending,running,stopping,stopped' \
  --query 'Reservations[].Instances[].InstanceId' --output text)

if [[ -z "$ids" ]]; then
  echo "no stranded instances"
  exit 0
fi

echo "terminating: $ids"
aws --profile "$PROFILE" --region "$REGION" ec2 terminate-instances \
  --instance-ids $ids
