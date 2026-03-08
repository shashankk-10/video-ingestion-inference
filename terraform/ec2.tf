# ─────────────────────────────────────────────
# Amazon Linux 2023 AMI
# ─────────────────────────────────────────────
data "aws_ami" "amazon_linux" {
  most_recent = true
  owners      = ["amazon"]

  filter {
    name   = "name"
    values = ["al2023-ami-*-x86_64"]
  }

  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
}

# ─────────────────────────────────────────────
# SSH Key Pair (generate locally, import public key)
# ─────────────────────────────────────────────
resource "tls_private_key" "ssh" {
  algorithm = "RSA"
  rsa_bits  = 4096
}

resource "aws_key_pair" "main" {
  key_name   = var.ssh_key_name
  public_key = tls_private_key.ssh.public_key_openssh
}

resource "local_file" "ssh_private_key" {
  content         = tls_private_key.ssh.private_key_pem
  filename        = "${path.module}/optifye-key.pem"
  file_permission = "0400"
}

# ─────────────────────────────────────────────
# EC2 Instance — RTSP Server
# ─────────────────────────────────────────────
resource "aws_instance" "rtsp" {
  ami                    = data.aws_ami.amazon_linux.id
  instance_type          = "t3.micro"
  subnet_id              = aws_subnet.public[0].id
  vpc_security_group_ids = [aws_security_group.rtsp.id]
  key_name               = aws_key_pair.main.key_name

  user_data = <<-EOF
    #!/bin/bash
    set -e
    yum update -y
    yum install -y docker
    systemctl start docker
    systemctl enable docker
    usermod -aG docker ec2-user

    # Download a sample traffic video for object detection
    mkdir -p /home/ec2-user/media
    cd /home/ec2-user/media
    # Short pedestrian/traffic clip (public domain)
    curl -L -o video.mp4 "https://www.pexels.com/download/video/855564/?fps=25.0&h=1080&w=1920"

    # If download fails, create a fallback plan
    if [ ! -s video.mp4 ]; then
      echo "Download failed — upload a video manually to /home/ec2-user/media/video.mp4"
    fi

    # Pull RTSP server image
    docker pull bluenviron/mediamtx:latest
  EOF

  root_block_device {
    volume_size = 20
    volume_type = "gp3"
  }

  tags = { Name = "${var.project_name}-rtsp-server" }
}

# ─────────────────────────────────────────────
# ECR Repositories for our Docker images
# ─────────────────────────────────────────────
resource "aws_ecr_repository" "inference" {
  name                 = "${var.project_name}/inference-service"
  image_tag_mutability = "MUTABLE"
  force_delete         = true
}

resource "aws_ecr_repository" "consumer" {
  name                 = "${var.project_name}/kafka-consumer"
  image_tag_mutability = "MUTABLE"
  force_delete         = true
}

resource "aws_ecr_repository" "producer" {
  name                 = "${var.project_name}/frame-producer"
  image_tag_mutability = "MUTABLE"
  force_delete         = true
}
