# ─────────────────────────────────────────────
# Outputs — you'll need these for the next steps
# ─────────────────────────────────────────────

output "vpc_id" {
  value = aws_vpc.main.id
}

output "rtsp_instance_public_ip" {
  value = aws_instance.rtsp.public_ip
}

output "rtsp_instance_private_ip" {
  value = aws_instance.rtsp.private_ip
}

output "msk_bootstrap_brokers" {
  value = aws_msk_cluster.main.bootstrap_brokers_tls
}

output "eks_cluster_name" {
  value = aws_eks_cluster.main.name
}

output "eks_cluster_endpoint" {
  value = aws_eks_cluster.main.endpoint
}

output "s3_bucket_name" {
  value = aws_s3_bucket.frames.id
}

output "ecr_inference_url" {
  value = aws_ecr_repository.inference.repository_url
}

output "ecr_consumer_url" {
  value = aws_ecr_repository.consumer.repository_url
}

output "ecr_producer_url" {
  value = aws_ecr_repository.producer.repository_url
}

output "ssh_key_path" {
  value = local_file.ssh_private_key.filename
}

output "aws_region" {
  value = var.aws_region
}

output "account_id" {
  value = data.aws_caller_identity.current.account_id
}
