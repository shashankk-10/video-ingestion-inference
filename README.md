# Optifye Pipeline

End-to-end real-time video analytics pipeline on AWS:

`RTSP stream -> Kafka (MSK) -> Inference service -> Annotated frames -> S3`

## Architecture

1. `frame-producer` (Go + FFmpeg)
- Reads RTSP stream, extracts JPEG frames, publishes to Kafka topic `video-stream-1`.

2. `inference-service` (Python/Flask + Ultralytics YOLO)
- Accepts frame batches over HTTP and returns detections.

3. `kafka-consumer` (Go)
- Consumes Kafka frames in batches, calls inference API, draws bounding boxes, uploads annotated images to S3.

4. Autoscaling (KEDA)
- Scales `kafka-consumer` and `inference-service` based on Kafka lag.

5. Infra (Terraform)
- VPC, subnets, NAT/IGW, EKS, MSK, EC2 RTSP host, S3 bucket, ECR repos, IAM roles/policies.

## Tech Stack

- AWS: EKS, MSK, ECR, S3, EC2, IAM, VPC
- Runtime: Kubernetes + KEDA
- Languages: Go, Python, Bash, Terraform
- CV/ML: Ultralytics YOLO (`yolov5nu`)
- Streaming: RTSP, FFmpeg, Kafka (Sarama)

## Design Choices

- Kafka decouples producer and inference/consumer for resilience and scaling.
- Batch inference reduces per-frame request overhead.
- KEDA lag-based scaling matches compute to stream load.
- Generated manifests (`k8s-generated/`) are derived from templates (`k8s/`) and Terraform outputs.
- Deployment script includes preflight checks (tools, Docker daemon, AWS auth) for fast failure.

## Deployment Approach

1. Provision infra with Terraform in `terraform/`.
2. Export outputs:
```bash
cd terraform
terraform output -json > tf-outputs.json
```
3. Deploy from project root:
```bash
cd ..
./deploy.sh
```

## CI/CD

GitHub Actions workflow at `.github/workflows/deploy.yaml`:
- Builds and pushes all 3 images to ECR (parallel matrix)
- Generates manifests from Terraform outputs
- Deploys to EKS and verifies rollout

Required repository secrets (choose one auth mode):
- OIDC mode: `AWS_ROLE_ARN`
- Access key fallback: `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`

## Key Operational Checks

- Pods: `kubectl get pods -A`
- Scaling: `kubectl get scaledobjects -A`
- Logs: `kubectl logs -f deployment/inference-service`
- S3 output: `aws s3 ls s3://<bucket>/annotated/ --recursive`

## Notes

- `k8s/` contains templates with placeholders.
- `k8s-generated/` contains rendered manifests used for deployment.
- If you migrate to TLS MSK endpoints, refresh `terraform/tf-outputs.json` before deploy.
