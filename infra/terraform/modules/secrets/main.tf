# Application secrets (JWT HS256 key, bank webhook HMAC key) in Secrets Manager, KMS-encrypted.
# Values are generated here and never appear in tfvars or source control.
variable "name" { type = string }
variable "kms_key_arn" { type = string }

resource "random_password" "jwt" {
  length  = 64
  special = false
}

resource "random_password" "bank_webhook" {
  length  = 64
  special = false
}

resource "aws_secretsmanager_secret" "jwt" {
  name       = "${var.name}/jwt-hs256"
  kms_key_id = var.kms_key_arn
}

resource "aws_secretsmanager_secret_version" "jwt" {
  secret_id     = aws_secretsmanager_secret.jwt.id
  secret_string = random_password.jwt.result
}

resource "aws_secretsmanager_secret" "bank_webhook" {
  name       = "${var.name}/bank-webhook-hmac"
  kms_key_id = var.kms_key_arn
}

resource "aws_secretsmanager_secret_version" "bank_webhook" {
  secret_id     = aws_secretsmanager_secret.bank_webhook.id
  secret_string = random_password.bank_webhook.result
}

output "jwt_secret_arn" { value = aws_secretsmanager_secret.jwt.arn }
output "bank_webhook_secret_arn" { value = aws_secretsmanager_secret.bank_webhook.arn }
