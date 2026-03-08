# ─────────────────────────────────────────────
# S3 Bucket — Annotated frame outputs
# ─────────────────────────────────────────────
resource "aws_s3_bucket" "frames" {
  bucket        = "${var.project_name}-annotated-frames-${data.aws_caller_identity.current.account_id}"
  force_destroy = true

  tags = { Name = "${var.project_name}-annotated-frames" }
}

resource "aws_s3_bucket_versioning" "frames" {
  bucket = aws_s3_bucket.frames.id
  versioning_configuration {
    status = "Enabled"
  }
}

data "aws_caller_identity" "current" {}
