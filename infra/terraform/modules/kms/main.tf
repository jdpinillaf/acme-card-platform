# Customer-managed KMS keys: one for data at rest (RDS, S3, SNS/SQS), one for secrets.
variable "name" { type = string }
variable "deletion_window_days" {
  type    = number
  default = 30
}

data "aws_caller_identity" "current" {}
data "aws_region" "current" {}

locals {
  account_root = "arn:aws:iam::${data.aws_caller_identity.current.account_id}:root"
}

# Data key: account admins via IAM, plus the AWS services that encrypt with it on our behalf.
data "aws_iam_policy_document" "data" {
  statement {
    sid       = "AccountAdmin"
    actions   = ["kms:*"]
    resources = ["*"]
    principals {
      type        = "AWS"
      identifiers = [local.account_root]
    }
  }
  statement {
    sid       = "CloudWatchLogs"
    actions   = ["kms:Encrypt*", "kms:Decrypt*", "kms:ReEncrypt*", "kms:GenerateDataKey*", "kms:Describe*"]
    resources = ["*"]
    principals {
      type        = "Service"
      identifiers = ["logs.${data.aws_region.current.region}.amazonaws.com"]
    }
    condition {
      test     = "ArnLike"
      variable = "kms:EncryptionContext:aws:logs:arn"
      values   = ["arn:aws:logs:${data.aws_region.current.region}:${data.aws_caller_identity.current.account_id}:log-group:*"]
    }
  }
  statement {
    sid       = "SNSToEncryptedSQS"
    actions   = ["kms:GenerateDataKey*", "kms:Decrypt"]
    resources = ["*"]
    principals {
      type        = "Service"
      identifiers = ["sns.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }
}

resource "aws_kms_key" "data" {
  description             = "${var.name} data at rest"
  enable_key_rotation     = true
  deletion_window_in_days = var.deletion_window_days
  policy                  = data.aws_iam_policy_document.data.json
}

resource "aws_kms_alias" "data" {
  name          = "alias/${var.name}-data"
  target_key_id = aws_kms_key.data.key_id
}

resource "aws_kms_key" "secrets" {
  description             = "${var.name} application secrets"
  enable_key_rotation     = true
  deletion_window_in_days = var.deletion_window_days
}

resource "aws_kms_alias" "secrets" {
  name          = "alias/${var.name}-secrets"
  target_key_id = aws_kms_key.secrets.key_id
}

output "data_key_arn" { value = aws_kms_key.data.arn }
output "secrets_key_arn" { value = aws_kms_key.secrets.arn }
