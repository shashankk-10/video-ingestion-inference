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

# Ensure Docker is running
sudo systemctl start docker 2>/dev/null || true

# Check video exists
if [ ! -f "/home/ec2-user/video.mp4" ] && [ ! -f "$VIDEO_PATH" ]; then
    echo "ERROR: No video found. Upload video.mp4 to /home/ec2-user/ first"
    exit 1
fi

# Move video to media dir if needed
mkdir -p /home/ec2-user/media
if [ -f "/home/ec2-user/video.mp4" ] && [ ! -f "$VIDEO_PATH" ]; then
    mv /home/ec2-user/video.mp4 "$VIDEO_PATH"
fi

# Create mediamtx config for looping the video
cat > /home/ec2-user/mediamtx.yml << 'EOF'
paths:
  stream:
    source: publisher
    runOnReady:
    runOnReadyRestart: no
EOF

echo "Starting RTSP server with looping video..."

# Stop existing container if running
docker rm -f rtsp-server 2>/dev/null || true

# Run ffmpeg to loop video → RTSP, and mediamtx as the RTSP server
docker run -d \
    --name rtsp-server \
    --restart unless-stopped \
    -p 8554:8554 \
    -v /home/ec2-user/media:/media \
    bluenviron/mediamtx:latest

# Wait for server to start
sleep 3

# Use ffmpeg to push the looping video to the RTSP server
# Install ffmpeg if not present
sudo yum install -y ffmpeg 2>/dev/null || sudo amazon-linux-extras install -y ffmpeg 2>/dev/null || {
    echo "Installing ffmpeg via docker..."
    docker run -d \
        --name ffmpeg-streamer \
        --restart unless-stopped \
        --network host \
        -v /home/ec2-user/media:/media \
        linuxserver/ffmpeg \
        -re -stream_loop -1 -i /media/video.mp4 \
        -c copy -f rtsp rtsp://localhost:8554/stream
    echo "RTSP stream active at rtsp://$(hostname -I | awk '{print $1}'):8554/stream"
    exit 0
}

# If ffmpeg is available natively, run it in background
nohup bash -c 'while true; do ffmpeg -re -i /home/ec2-user/media/video.mp4 -c copy -f rtsp rtsp://localhost:8554/stream 2>/dev/null; sleep 1; done' &>/dev/null &

echo "RTSP stream active at rtsp://$(hostname -I | awk '{print $1}'):8554/stream"
echo "Test with: ffprobe rtsp://$(hostname -I | awk '{print $1}'):8554/stream"
