variable "aws_region" {
  description = "AWS region"
  type        = string
  default     = "ap-south-1"
}

variable "project_name" {
  description = "Project name prefix"
  type        = string
  default     = "optifye"
}

variable "vpc_cidr" {
  description = "VPC CIDR block"
  type        = string
  default     = "10.0.0.0/16"
}

variable "ssh_key_name" {
  description = "EC2 SSH key pair name"
  type        = string
  default     = "optifye-key"
}

variable "ssh_allowed_cidr" {
  description = "CIDR block allowed to SSH into RTSP instance (restrict to your public IP/CIDR)"
  type        = string
  default     = "0.0.0.0/0"
}
