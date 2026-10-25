# Domain event bus: SNS topic → SQS audit queue (+ DLQ), all KMS-encrypted.
variable "name" { type = string }
variable "kms_key_arn" { type = string }

resource "aws_sns_topic" "events" {
  name              = "${var.name}-events"
  kms_master_key_id = var.kms_key_arn
}

resource "aws_sqs_queue" "audit_dlq" {
  name                      = "${var.name}-audit-dlq"
  kms_master_key_id         = var.kms_key_arn
  message_retention_seconds = 1209600
}

resource "aws_sqs_queue" "audit" {
  name                       = "${var.name}-audit"
  kms_master_key_id          = var.kms_key_arn
  visibility_timeout_seconds = 60
  receive_wait_time_seconds  = 20
  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.audit_dlq.arn
    maxReceiveCount     = 5
  })
}

data "aws_iam_policy_document" "audit_queue" {
  statement {
    actions   = ["sqs:SendMessage"]
    resources = [aws_sqs_queue.audit.arn]
    principals {
      type        = "Service"
      identifiers = ["sns.amazonaws.com"]
    }
    condition {
      test     = "ArnEquals"
      variable = "aws:SourceArn"
      values   = [aws_sns_topic.events.arn]
    }
  }
}

resource "aws_sqs_queue_policy" "audit" {
  queue_url = aws_sqs_queue.audit.id
  policy    = data.aws_iam_policy_document.audit_queue.json
}

resource "aws_sns_topic_subscription" "audit" {
  topic_arn            = aws_sns_topic.events.arn
  protocol             = "sqs"
  endpoint             = aws_sqs_queue.audit.arn
  raw_message_delivery = true
}

output "topic_arn" { value = aws_sns_topic.events.arn }
output "audit_queue_url" { value = aws_sqs_queue.audit.url }
output "audit_queue_arn" { value = aws_sqs_queue.audit.arn }
