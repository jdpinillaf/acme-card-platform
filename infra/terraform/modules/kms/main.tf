# Customer-managed KMS keys: one for data at rest (RDS, S3, SNS/SQS), one for secrets.
variable "name" { type = string }
variable "deletion_window_days" {
  type    = number
  default = 30
}

resource "aws_kms_key" "data" {
  description             = "${var.name} data at rest"
  enable_key_rotation     = true
  deletion_window_in_days = var.deletion_window_days
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
