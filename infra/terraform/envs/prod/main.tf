# prod environment. NOT applied from this repo (see ADR-008); validated with `make tf-validate`.
terraform {
  required_version = ">= 1.9"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.33"
    }
  }
  # backend "s3" {
  #   bucket       = "acme-card-tfstate-prod"
  #   key          = "acme-card/prod.tfstate"
  #   region       = "us-east-1"
  #   encrypt      = true
  #   use_lockfile = true
  # }
}

provider "aws" {
  region = var.region
  default_tags {
    tags = { Project = "acme-card", Environment = "prod", ManagedBy = "terraform" }
  }
}

variable "region" { type = string }
variable "certificate_arn" { type = string }
variable "image_repository" { type = string }
variable "image_tag" { type = string }

module "stack" {
  source               = "../../stack"
  env                  = "prod"
  region               = var.region
  certificate_arn      = var.certificate_arn
  image_repository     = var.image_repository
  image_tag            = var.image_tag
  vpc_cidr             = "10.20.0.0/16"
  azs                  = ["us-east-1a", "us-east-1b", "us-east-1c"]
  single_nat_gateway   = false
  db_instance_class    = "db.r7g.large"
  db_allocated_storage = 200
  db_multi_az          = true
  deletion_protection  = true
  services = {
    gateway  = { cpu = 1024, memory = 2048, desired = 3, port = 8080 }
    accounts = { cpu = 1024, memory = 2048, desired = 3, port = 9001 }
    ledger   = { cpu = 1024, memory = 2048, desired = 3, port = 9002 }
    audit    = { cpu = 512, memory = 1024, desired = 2, port = 0 }
  }
}

output "alb_dns_name" { value = module.stack.alb_dns_name }
