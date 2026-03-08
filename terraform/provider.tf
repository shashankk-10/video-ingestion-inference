terraform {
  required_version = ">= 1.5.0"

  # Remote backend — create the S3 bucket and DynamoDB table manually before first init:
  #   aws s3api create-bucket --bucket optifye-tf-state --region ap-south-1 \
  #     --create-bucket-configuration LocationConstraint=ap-south-1
  #   aws dynamodb create-table --table-name optifye-tf-lock \
  #     --attribute-definitions AttributeName=LockID,AttributeType=S \
  #     --key-schema AttributeName=LockID,KeyType=HASH \
  #     --billing-mode PAY_PER_REQUEST --region ap-south-1
  backend "s3" {
    bucket         = "optifye-tf-state"
    key            = "infra/terraform.tfstate"
    region         = "ap-south-1"
    dynamodb_table = "optifye-tf-lock"
    encrypt        = true
  }

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "~> 4.0"
    }
  }
}

provider "aws" {
  region = var.aws_region

  default_tags {
    tags = {
      Project   = var.project_name
      ManagedBy = "terraform"
    }
  }
}

data "aws_availability_zones" "available" {
  state = "available"
}
