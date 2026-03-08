import logging
import os
import time

import cv2
import numpy as np
import torch
from flask import Flask, jsonify, request
from ultralytics import YOLO

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
)
logger = logging.getLogger("inference-service")

app = Flask(__name__)
MAX_BATCH_SIZE = int(os.getenv("MAX_BATCH_SIZE", "64"))

device = "cuda" if torch.cuda.is_available() else "cpu"
logger.info(f"Loading YOLOv5n model on device: {device}")

model = YOLO("yolov5nu.pt")
model.to(device)

@app.route("/health", methods=["GET"])
def health():
    return jsonify({
        "status": "healthy",
        "model": "yolov5nu",
        "device": device,
        "cuda_available": torch.cuda.is_available()
    })

@app.route("/predict", methods=["POST"])
def predict():
    """
    Accepts multipart form data with multiple JPEG frames.
    Supports optional query params: 'conf' (0-1) and 'iou' (0-1).
    """
    start_time = time.time()

    try:
        conf_thres = float(request.args.get("conf", 0.25))
        iou_thres = float(request.args.get("iou", 0.45))
    except ValueError:
        return jsonify({"error": "Invalid conf/iou values. Expected numeric values between 0 and 1."}), 400

    if not (0 <= conf_thres <= 1) or not (0 <= iou_thres <= 1):
        return jsonify({"error": "Invalid conf/iou values. Both must be between 0 and 1."}), 400

    frame_keys = sorted(
        [k for k in request.files.keys() if k.startswith("frame_")],
        key=lambda x: int(x.split("_")[1]) if "_" in x else 0,
    )

    if not frame_keys:
        return jsonify({"error": "No frames provided. Use fields 'frame_0', 'frame_1', etc."}), 400

    if len(frame_keys) > MAX_BATCH_SIZE:
        return jsonify({
            "error": f"Batch too large. Max supported frames per request: {MAX_BATCH_SIZE}",
            "received": len(frame_keys),
        }), 413

    frames = []
    valid_keys = []

    for key in frame_keys:
        file = request.files[key]
        try:
            file_bytes = file.read()
            nparr = np.frombuffer(file_bytes, np.uint8)
            img = cv2.imdecode(nparr, cv2.IMREAD_COLOR)

            if img is not None:
                frames.append(img)
                valid_keys.append(key)
            else:
                logger.warning(f"Failed to decode image from key: {key}")
        except Exception as e:
            logger.error(f"Error processing {key}: {e}")

    if not frames:
        return jsonify({"error": "Could not decode any valid frames"}), 400

    results = model(
        frames,
        conf=conf_thres,
        iou=iou_thres,
        device=device,
        verbose=False
    )

    output = []
    for i, result in enumerate(results):
        boxes = result.boxes.cpu().numpy()

        detections = []
        for j in range(len(boxes)):
            xyxy = boxes.xyxy[j]
            detections.append({
                "bbox": {
                    "x1": round(float(xyxy[0]), 1),
                    "y1": round(float(xyxy[1]), 1),
                    "x2": round(float(xyxy[2]), 1),
                    "y2": round(float(xyxy[3]), 1),
                },
                "confidence": round(float(boxes.conf[j]), 4),
                "class_id": int(boxes.cls[j]),
                "class_name": model.names[int(boxes.cls[j])],
            })

        output.append({
            "key": valid_keys[i],
            "detections": detections,
            "count": len(detections)
        })

    total_time = (time.time() - start_time) * 1000
    logger.info(f"Batch processed: {len(frames)} frames in {total_time:.2f}ms on {device}")

    return jsonify({
        "inference_time_ms": round(total_time, 2),
        "device": device,
        "batch_size": len(frames),
        "results": output
    })

if __name__ == "__main__":
    app.run(host="0.0.0.0", port=8080, debug=False)
