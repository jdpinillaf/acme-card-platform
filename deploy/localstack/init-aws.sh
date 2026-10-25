#!/usr/bin/env bash
# Runs inside LocalStack on ready (/etc/localstack/init/ready.d). Creates topic, queue, subscription, bucket, KMS key.
set -euo pipefail
REGION=${AWS_REGION:-us-east-1}
awslocal sns create-topic --name acme-card-events --region "$REGION" >/dev/null
awslocal sqs create-queue --queue-name acme-card-audit --region "$REGION" >/dev/null
awslocal sqs create-queue --queue-name acme-card-audit-dlq --region "$REGION" >/dev/null
QUEUE_ARN=$(awslocal sqs get-queue-attributes --queue-url "http://localhost:4566/000000000000/acme-card-audit" --attribute-names QueueArn --query 'Attributes.QueueArn' --output text)
awslocal sns subscribe --topic-arn "arn:aws:sns:$REGION:000000000000:acme-card-events" --protocol sqs --notification-endpoint "$QUEUE_ARN" --attributes RawMessageDelivery=true >/dev/null
awslocal s3 mb s3://acme-card-statements-dev >/dev/null 2>&1 || true
# Audit checkpoints, reports and archives: write-once (Object Lock) bucket (ADR-021).
awslocal s3api create-bucket --bucket acme-card-audit-dev --object-lock-enabled-for-bucket >/dev/null 2>&1 || true
awslocal kms create-key --description "acme-card dev key" >/dev/null 2>&1 || true
echo "localstack: acme-card resources ready"
