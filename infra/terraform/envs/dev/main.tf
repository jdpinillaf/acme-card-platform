# dev environment. NOT applied from this repo (see ADR-008); validated with `make tf-validate`.
terraform {
  required_version = ">= 1.9"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.33"
    }
  }
  # backend "s3" {
  #   bucket       = "acme-card-tfstate-dev"
  #   key          = "acme-card/dev.tfstate"
  #   region       = "us-east-1"
  #   encrypt      = true
  #   use_lockfile = true
  # }
}

provider "aws" {
  region = var.region
  default_tags {
    tags = { Project = "acme-card", Environment = "dev", ManagedBy = "terraform" }
  }
}

variable "region" { type = string }
variable "certificate_arn" { type = string }
variable "image_repository" { type = string }
variable "image_tag" { type = string }

module "stack" {
  source               = "../../stack"
  env                  = "dev"
  region               = var.region
  certificate_arn      = var.certificate_arn
  image_repository     = var.image_repository
  image_tag            = var.image_tag
  vpc_cidr             = "10.10.0.0/16"
  azs                  = ["us-east-1a", "us-east-1b"]
  single_nat_gateway   = true
  db_instance_class    = "db.t4g.medium"
  db_allocated_storage = 50
  db_multi_az          = true # Multi-AZ in every env, per requirement
  deletion_protection  = false
  services = {
    gateway  = { cpu = 512, memory = 1024, desired = 2, port = 8080 }
    accounts = { cpu = 512, memory = 1024, desired = 2, port = 9001 }
    ledger   = { cpu = 512, memory = 1024, desired = 2, port = 9002 }
    audit    = { cpu = 256, memory = 512, desired = 1, port = 0 }
  }
}

output "alb_dns_name" { value = module.stack.alb_dns_name }
