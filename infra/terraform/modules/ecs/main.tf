# ECS Fargate cluster. Public ALB terminates TLS 1.3 only and forwards to the gateway.
# Internal services talk gRPC over ECS Service Connect (private DNS: <svc>.acme.internal).
variable "name" { type = string }
variable "region" { type = string }
variable "vpc_id" { type = string }
variable "public_subnet_ids" { type = list(string) }
variable "private_subnet_ids" { type = list(string) }
variable "certificate_arn" { type = string }
variable "image_repository" { type = string }
variable "image_tag" { type = string }
variable "secrets_kms_key_arn" { type = string }
variable "data_kms_key_arn" { type = string }
variable "jwt_secret_arn" { type = string }
variable "bank_webhook_secret_arn" { type = string }
variable "db_master_secret_arn" { type = string }
variable "db_endpoint" { type = string }
variable "topic_arn" { type = string }
variable "audit_queue_url" { type = string }
variable "audit_queue_arn" { type = string }
variable "services" {
  description = "Per-service sizing: { name = { cpu, memory, desired, port } }"
  type = map(object({
    cpu     = number
    memory  = number
    desired = number
    port    = number
  }))
}

locals {
  grpc_services = { for k, v in var.services : k => v if k != "gateway" && k != "audit" }
}

resource "aws_ecs_cluster" "this" {
  name = var.name
  setting {
    name  = "containerInsights"
    value = "enabled"
  }
  service_connect_defaults {
    namespace = aws_service_discovery_http_namespace.this.arn
  }
}

resource "aws_service_discovery_http_namespace" "this" {
  name = "acme.internal"
}

resource "aws_cloudwatch_log_group" "svc" {
  for_each          = var.services
  name              = "/ecs/${var.name}/${each.key}"
  retention_in_days = 30
  kms_key_id        = var.data_kms_key_arn
}

# ---------- security groups ----------
resource "aws_security_group" "alb" {
  name   = "${var.name}-alb"
  vpc_id = var.vpc_id
  ingress {
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
  ingress {
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_security_group" "tasks" {
  name   = "${var.name}-tasks"
  vpc_id = var.vpc_id
  ingress {
    from_port       = 8080
    to_port         = 8080
    protocol        = "tcp"
    security_groups = [aws_security_group.alb.id]
  }
  ingress {
    from_port = 9000
    to_port   = 9100
    protocol  = "tcp"
    self      = true
  }
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

# ---------- ALB (TLS 1.3 only) ----------
resource "aws_lb" "this" {
  name                       = var.name
  load_balancer_type         = "application"
  subnets                    = var.public_subnet_ids
  security_groups            = [aws_security_group.alb.id]
  drop_invalid_header_fields = true
  enable_deletion_protection = true
}

resource "aws_lb_target_group" "gateway" {
  name        = "${var.name}-gw"
  port        = 8080
  protocol    = "HTTP"
  target_type = "ip"
  vpc_id      = var.vpc_id
  health_check {
    path    = "/readyz"
    matcher = "200"
  }
  deregistration_delay = 15
}

resource "aws_lb_listener" "https" {
  load_balancer_arn = aws_lb.this.arn
  port              = 443
  protocol          = "HTTPS"
  ssl_policy        = "ELBSecurityPolicy-TLS13-1-3-2021-06"
  certificate_arn   = var.certificate_arn
  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.gateway.arn
  }
}

resource "aws_lb_listener" "http_redirect" {
  load_balancer_arn = aws_lb.this.arn
  port              = 80
  protocol          = "HTTP"
  default_action {
    type = "redirect"
    redirect {
      port        = "443"
      protocol    = "HTTPS"
      status_code = "HTTP_301"
    }
  }
}

# ---------- IAM ----------
data "aws_iam_policy_document" "assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

resource "aws_iam_role" "execution" {
  name               = "${var.name}-exec"
  assume_role_policy = data.aws_iam_policy_document.assume.json
}

resource "aws_iam_role_policy_attachment" "execution" {
  role       = aws_iam_role.execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

data "aws_iam_policy_document" "execution_secrets" {
  statement {
    actions   = ["secretsmanager:GetSecretValue"]
    resources = [var.jwt_secret_arn, var.bank_webhook_secret_arn, var.db_master_secret_arn]
  }
  statement {
    actions   = ["kms:Decrypt"]
    resources = [var.secrets_kms_key_arn]
  }
}

resource "aws_iam_role_policy" "execution_secrets" {
  role   = aws_iam_role.execution.id
  policy = data.aws_iam_policy_document.execution_secrets.json
}

resource "aws_iam_role" "task" {
  name               = "${var.name}-task"
  assume_role_policy = data.aws_iam_policy_document.assume.json
}

data "aws_iam_policy_document" "task" {
  statement {
    sid       = "PublishEvents"
    actions   = ["sns:Publish"]
    resources = [var.topic_arn]
  }
  statement {
    sid       = "ConsumeAudit"
    actions   = ["sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:GetQueueAttributes"]
    resources = [var.audit_queue_arn]
  }
  statement {
    sid       = "UseDataKey"
    actions   = ["kms:GenerateDataKey", "kms:Decrypt"]
    resources = [var.data_kms_key_arn]
  }
}

resource "aws_iam_role_policy" "task" {
  role   = aws_iam_role.task.id
  policy = data.aws_iam_policy_document.task.json
}

# ---------- task definitions & services ----------
resource "aws_ecs_task_definition" "svc" {
  for_each                 = var.services
  family                   = "${var.name}-${each.key}"
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = each.value.cpu
  memory                   = each.value.memory
  execution_role_arn       = aws_iam_role.execution.arn
  task_role_arn            = aws_iam_role.task.arn
  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "ARM64"
  }
  container_definitions = jsonencode([{
    name         = each.key
    image        = "${var.image_repository}/${each.key}:${var.image_tag}"
    essential    = true
    portMappings = each.value.port > 0 ? [{ name = each.key, containerPort = each.value.port, protocol = "tcp" }] : []
    environment = [
      { name = "APP_ENV", value = var.name },
      { name = "AWS_REGION", value = var.region },
      { name = "SNS_TOPIC_ARN", value = var.topic_arn },
      { name = "AUDIT_SQS_QUEUE_URL", value = var.audit_queue_url },
      { name = "GRPC_ADDR", value = ":${each.value.port}" },
      { name = "GATEWAY_HTTP_ADDR", value = ":8080" },
      { name = "ACCOUNTS_GRPC_ADDR", value = "accounts.acme.internal:9001" },
      { name = "LEDGER_GRPC_ADDR", value = "ledger.acme.internal:9002" },
      { name = "DB_HOST", value = var.db_endpoint },
    ]
    secrets = concat(
      [{ name = "DB_CREDENTIALS", valueFrom = var.db_master_secret_arn }],
      each.key == "gateway" ? [
        { name = "JWT_HS256_SECRET", valueFrom = var.jwt_secret_arn },
        { name = "BANK_WEBHOOK_SECRET", valueFrom = var.bank_webhook_secret_arn },
      ] : []
    )
    readonlyRootFilesystem = true
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.svc[each.key].name
        awslogs-region        = var.region
        awslogs-stream-prefix = each.key
      }
    }
  }])
}

resource "aws_ecs_service" "svc" {
  for_each        = var.services
  name            = each.key
  cluster         = aws_ecs_cluster.this.id
  task_definition = aws_ecs_task_definition.svc[each.key].arn
  desired_count   = each.value.desired
  launch_type     = "FARGATE"

  network_configuration {
    subnets         = var.private_subnet_ids
    security_groups = [aws_security_group.tasks.id]
  }

  dynamic "load_balancer" {
    for_each = each.key == "gateway" ? [1] : []
    content {
      target_group_arn = aws_lb_target_group.gateway.arn
      container_name   = "gateway"
      container_port   = 8080
    }
  }

  service_connect_configuration {
    enabled = true
    dynamic "service" {
      for_each = contains(keys(local.grpc_services), each.key) ? [1] : []
      content {
        port_name = each.key
        client_alias {
          port     = each.value.port
          dns_name = "${each.key}.acme.internal"
        }
      }
    }
  }

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }
}

resource "aws_appautoscaling_target" "gateway" {
  service_namespace  = "ecs"
  resource_id        = "service/${aws_ecs_cluster.this.name}/${aws_ecs_service.svc["gateway"].name}"
  scalable_dimension = "ecs:service:DesiredCount"
  min_capacity       = var.services["gateway"].desired
  max_capacity       = var.services["gateway"].desired * 4
}

resource "aws_appautoscaling_policy" "gateway_cpu" {
  name               = "${var.name}-gateway-cpu"
  service_namespace  = aws_appautoscaling_target.gateway.service_namespace
  resource_id        = aws_appautoscaling_target.gateway.resource_id
  scalable_dimension = aws_appautoscaling_target.gateway.scalable_dimension
  policy_type        = "TargetTrackingScaling"
  target_tracking_scaling_policy_configuration {
    target_value = 60
    predefined_metric_specification {
      predefined_metric_type = "ECSServiceAverageCPUUtilization"
    }
  }
}

output "alb_dns_name" { value = aws_lb.this.dns_name }
output "tasks_security_group_id" { value = aws_security_group.tasks.id }
