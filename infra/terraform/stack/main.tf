# Full acme-card stack. Instantiated by envs/dev and envs/prod with different sizing.
terraform {
  required_version = ">= 1.9"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.33"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}

variable "env" { type = string }
variable "region" { type = string }
variable "vpc_cidr" { type = string }
variable "azs" { type = list(string) }
variable "single_nat_gateway" { type = bool }
variable "db_instance_class" { type = string }
variable "db_allocated_storage" { type = number }
variable "db_multi_az" { type = bool }
variable "deletion_protection" { type = bool }
variable "certificate_arn" { type = string }
variable "image_repository" { type = string }
variable "image_tag" { type = string }
variable "services" {
  type = map(object({
    cpu     = number
    memory  = number
    desired = number
    port    = number
  }))
}

locals {
  name = "acme-card-${var.env}"
}

module "kms" {
  source = "../modules/kms"
  name   = local.name
}

module "network" {
  source             = "../modules/network"
  name               = local.name
  cidr               = var.vpc_cidr
  azs                = var.azs
  single_nat_gateway = var.single_nat_gateway
}

module "secrets" {
  source      = "../modules/secrets"
  name        = local.name
  kms_key_arn = module.kms.secrets_key_arn
}

module "messaging" {
  source      = "../modules/messaging"
  name        = local.name
  kms_key_arn = module.kms.data_key_arn
}

module "statements_bucket" {
  source      = "../modules/s3"
  bucket_name = "${local.name}-statements"
  kms_key_arn = module.kms.data_key_arn
}

module "audit_archive_bucket" {
  source      = "../modules/s3"
  bucket_name = "${local.name}-audit-archive"
  kms_key_arn = module.kms.data_key_arn
  object_lock = true # WORM: archived audit exports cannot be altered
}

module "ecs" {
  source                     = "../modules/ecs"
  name                       = local.name
  region                     = var.region
  vpc_id                     = module.network.vpc_id
  vpc_cidr                   = var.vpc_cidr
  public_subnet_ids          = module.network.public_subnet_ids
  private_subnet_ids         = module.network.private_subnet_ids
  certificate_arn            = var.certificate_arn
  image_repository           = var.image_repository
  image_tag                  = var.image_tag
  secrets_kms_key_arn        = module.kms.secrets_key_arn
  data_kms_key_arn           = module.kms.data_key_arn
  jwt_secret_arn             = module.secrets.jwt_secret_arn
  bank_webhook_secret_arn    = module.secrets.bank_webhook_secret_arn
  network_webhook_secret_arn = module.secrets.network_webhook_secret_arn
  db_master_secret_arn       = module.rds.master_secret_arn
  db_endpoint                = module.rds.endpoint
  topic_arn                  = module.messaging.topic_arn
  audit_queue_url            = module.messaging.audit_queue_url
  audit_queue_arn            = module.messaging.audit_queue_arn
  services                   = var.services
}

module "rds" {
  source                     = "../modules/rds"
  name                       = local.name
  vpc_id                     = module.network.vpc_id
  subnet_ids                 = module.network.private_subnet_ids
  allowed_security_group_ids = [module.ecs.tasks_security_group_id]
  kms_key_arn                = module.kms.data_key_arn
  secrets_kms_key_arn        = module.kms.secrets_key_arn
  instance_class             = var.db_instance_class
  allocated_storage          = var.db_allocated_storage
  multi_az                   = var.db_multi_az
  deletion_protection        = var.deletion_protection
}

output "alb_dns_name" { value = module.ecs.alb_dns_name }
output "db_endpoint" { value = module.rds.endpoint }
output "events_topic_arn" { value = module.messaging.topic_arn }
