#!/bin/bash
set -euo pipefail

# ──────────────────────────────────────────────
# deploy.sh — Build, push, and deploy the full pipeline
# ──────────────────────────────────────────────
# Usage:
#   1. Run: terraform output -json > tf-outputs.json
#   2. Run: ./deploy.sh
# ──────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REGION="ap-south-1"

# ── Parse terraform outputs ──
if [ ! -f "$SCRIPT_DIR/terraform/tf-outputs.json" ]; then
    echo "ERROR: terraform/tf-outputs.json not found. Run 'cd terraform && terraform output -json > tf-outputs.json'"
    exit 1
fi

TF="$SCRIPT_DIR/terraform/tf-outputs.json"
ACCOUNT_ID=$(jq -r '.account_id.value' "$TF")
ECR_INFERENCE=$(jq -r '.ecr_inference_url.value' "$TF")
ECR_CONSUMER=$(jq -r '.ecr_consumer_url.value' "$TF")
ECR_PRODUCER=$(jq -r '.ecr_producer_url.value' "$TF")
MSK_BROKERS=$(jq -r '.msk_bootstrap_brokers.value' "$TF")
S3_BUCKET=$(jq -r '.s3_bucket_name.value' "$TF")
RTSP_PRIVATE_IP=$(jq -r '.rtsp_instance_private_ip.value' "$TF")
EKS_CLUSTER=$(jq -r '.eks_cluster_name.value' "$TF")

echo "═══════════════════════════════════════════"
echo "  Optifye Pipeline Deployment"
echo "═══════════════════════════════════════════"
echo "Account:     $ACCOUNT_ID"
echo "MSK Brokers: $MSK_BROKERS"
echo "S3 Bucket:   $S3_BUCKET"
echo "RTSP IP:     $RTSP_PRIVATE_IP"
echo "EKS Cluster: $EKS_CLUSTER"
echo "═══════════════════════════════════════════"

# ── Step 1: ECR Login ──
echo -e "\n[1/6] Logging into ECR..."
aws ecr get-login-password --region $REGION | docker login --username AWS --password-stdin "$ACCOUNT_ID.dkr.ecr.$REGION.amazonaws.com"

# ── Step 2: Build and Push Inference Service ──
echo -e "\n[2/6] Building inference-service..."
docker build -t "$ECR_INFERENCE:latest" "$SCRIPT_DIR/inference-service/"
docker push "$ECR_INFERENCE:latest"
echo "  ✓ Pushed $ECR_INFERENCE:latest"

# ── Step 3: Build and Push Kafka Consumer ──
echo -e "\n[3/6] Building kafka-consumer..."
docker build -t "$ECR_CONSUMER:latest" "$SCRIPT_DIR/kafka-consumer/"
docker push "$ECR_CONSUMER:latest"
echo "  ✓ Pushed $ECR_CONSUMER:latest"

# ── Step 4: Build and Push Frame Producer ──
echo -e "\n[4/6] Building frame-producer..."
docker build -t "$ECR_PRODUCER:latest" "$SCRIPT_DIR/frame-producer/"
docker push "$ECR_PRODUCER:latest"
echo "  ✓ Pushed $ECR_PRODUCER:latest"

# ── Step 5: Generate K8s manifests from templates ──
echo -e "\n[5/6] Generating K8s manifests from templates..."
K8S_SRC="$SCRIPT_DIR/k8s"
K8S_DIR="$SCRIPT_DIR/k8s-generated"
rm -rf "$K8S_DIR"
cp -r "$K8S_SRC" "$K8S_DIR"

# Cross-platform sed in-place (works on both GNU and BSD/macOS)
_sed_inplace() {
    if sed --version >/dev/null 2>&1; then
        sed -i "$@"      # GNU sed
    else
        sed -i '' "$@"   # BSD/macOS sed
    fi
}

# Patch configmap
_sed_inplace "s|<MSK_BOOTSTRAP_BROKERS>|$MSK_BROKERS|g" "$K8S_DIR/configmap.yaml"
_sed_inplace "s|<RTSP_PRIVATE_IP>|$RTSP_PRIVATE_IP|g" "$K8S_DIR/configmap.yaml"
_sed_inplace "s|<S3_BUCKET_NAME>|$S3_BUCKET|g" "$K8S_DIR/configmap.yaml"

# Patch image references
_sed_inplace "s|<ACCOUNT_ID>|$ACCOUNT_ID|g" "$K8S_DIR/inference-deployment.yaml"
_sed_inplace "s|<ACCOUNT_ID>|$ACCOUNT_ID|g" "$K8S_DIR/consumer-deployment.yaml"
_sed_inplace "s|<ACCOUNT_ID>|$ACCOUNT_ID|g" "$K8S_DIR/producer-deployment.yaml"

# Patch KEDA
_sed_inplace "s|<MSK_BOOTSTRAP_BROKERS>|$MSK_BROKERS|g" "$K8S_DIR/keda-scaledobject.yaml"

echo "  ✓ Manifests generated in k8s-generated/"

# ── Step 6: Deploy to EKS ──
echo -e "\n[6/6] Deploying to EKS..."
aws eks update-kubeconfig --name "$EKS_CLUSTER" --region $REGION

# Topic auto-creation is enabled in MSK config (auto.create.topics.enable=true)
kubectl apply -f "$K8S_DIR/configmap.yaml"
echo "  ✓ ConfigMap applied"

kubectl apply -f "$K8S_DIR/inference-deployment.yaml"
echo "  ✓ Inference service deployed"

echo "  Waiting for inference pods to be ready..."
kubectl rollout status deployment/inference-service --timeout=180s

kubectl apply -f "$K8S_DIR/consumer-deployment.yaml"
echo "  ✓ Kafka consumer deployed"

kubectl apply -f "$K8S_DIR/producer-deployment.yaml"
echo "  ✓ Frame producer deployed"

# Apply KEDA autoscalers (requires KEDA to be installed on the cluster)
if kubectl api-resources | grep -q scaledobjects; then
    kubectl apply -f "$K8S_DIR/keda-scaledobject.yaml"
    echo "  ✓ KEDA autoscalers applied"
else
    echo "  ⚠ KEDA not installed — skipping autoscaler manifests"
    echo "    Install with: helm install keda kedacore/keda --namespace keda --create-namespace"
fi

echo ""
echo "═══════════════════════════════════════════"
echo "  Deployment Complete!"
echo "═══════════════════════════════════════════"
echo ""
echo "Monitor with:"
echo "  kubectl get pods -w"
echo "  kubectl logs -f deployment/frame-producer"
echo "  kubectl logs -f deployment/kafka-consumer"
echo "  kubectl logs -f deployment/inference-service"
echo ""
echo "Check S3 for annotated frames:"
echo "  aws s3 ls s3://$S3_BUCKET/annotated/ --recursive"