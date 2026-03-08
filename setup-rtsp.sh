#!/bin/bash
set -euo pipefail

# ──────────────────────────────────────────────
# setup-rtsp.sh — Run on the RTSP EC2 instance
# ──────────────────────────────────────────────
# Usage:
#   scp -i optifye-key.pem setup-rtsp.sh video.mp4 ec2-user@<RTSP_PUBLIC_IP>:/home/ec2-user/
#   ssh -i optifye-key.pem ec2-user@<RTSP_PUBLIC_IP> 'bash /home/ec2-user/setup-rtsp.sh'
# ──────────────────────────────────────────────

VIDEO_PATH="/home/ec2-user/media/video.mp4"
MEDIA_DIR="/home/ec2-user/media"
MEDIAMTX_CFG="/home/ec2-user/mediamtx.yml"

get_local_ip() {
    hostname -I 2>/dev/null | awk '{print $1}' || true
}

# Ensure Docker exists and is running
if ! command -v docker >/dev/null 2>&1; then
    echo "Docker not found. Installing..."
    sudo yum install -y docker
fi
sudo systemctl enable --now docker

# Check video exists
if [ ! -f "/home/ec2-user/video.mp4" ] && [ ! -f "$VIDEO_PATH" ]; then
    echo "ERROR: No video found. Upload video.mp4 to /home/ec2-user/ first"
    exit 1
fi

# Move video to media dir if needed
mkdir -p "$MEDIA_DIR"
if [ -f "/home/ec2-user/video.mp4" ] && [ ! -f "$VIDEO_PATH" ]; then
    mv /home/ec2-user/video.mp4 "$VIDEO_PATH"
fi

# Create mediamtx config for looping the video
cat > "$MEDIAMTX_CFG" << 'EOF'
paths:
  stream:
    source: publisher
EOF

echo "Starting RTSP server with looping video..."

# Stop existing containers if running
docker rm -f rtsp-server 2>/dev/null || true
docker rm -f ffmpeg-streamer 2>/dev/null || true

# Run ffmpeg to loop video → RTSP, and mediamtx as the RTSP server
docker run -d \
    --name rtsp-server \
    --restart unless-stopped \
    -p 8554:8554 \
    -v "$MEDIA_DIR:/media" \
    -v "$MEDIAMTX_CFG:/mediamtx.yml" \
    bluenviron/mediamtx:latest

# Wait for server to start
sleep 3

# Use ffmpeg to push the looping video to the RTSP server
# Install ffmpeg if not present
sudo yum install -y ffmpeg 2>/dev/null || {
    echo "Installing ffmpeg via docker..."
    docker run -d \
        --name ffmpeg-streamer \
        --restart unless-stopped \
        --network host \
        -v "$MEDIA_DIR:/media" \
        linuxserver/ffmpeg:latest \
        -re -stream_loop -1 -i /media/video.mp4 \
        -c copy -f rtsp rtsp://127.0.0.1:8554/stream
    LOCAL_IP=$(get_local_ip)
    echo "RTSP stream active at rtsp://${LOCAL_IP}:8554/stream"
    exit 0
}

# If ffmpeg is available natively, run it in background
pkill -f "ffmpeg -re -stream_loop -1 -i $VIDEO_PATH -c copy -f rtsp rtsp://127.0.0.1:8554/stream" 2>/dev/null || true
nohup ffmpeg -re -stream_loop -1 -i "$VIDEO_PATH" -c copy -f rtsp rtsp://127.0.0.1:8554/stream >/home/ec2-user/ffmpeg-stream.log 2>&1 &

LOCAL_IP=$(get_local_ip)
echo "RTSP stream active at rtsp://${LOCAL_IP}:8554/stream"
echo "Test with: ffprobe rtsp://${LOCAL_IP}:8554/stream"
