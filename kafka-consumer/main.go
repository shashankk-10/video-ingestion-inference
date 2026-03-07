package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/IBM/sarama"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// ── Globals for Performance ──

var bufferPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

// ── Configuration ──

type Config struct {
	KafkaBrokers    []string
	KafkaTopic      string
	ConsumerGroup   string
	InferenceURL    string
	S3Bucket        string
	S3Prefix        string
	BatchSize       int
	BatchTimeoutSec int
	AWSRegion       string
}

func loadConfig() Config {
	parseEnvInt := func(key string, fallback int) int {
		if val, err := strconv.Atoi(os.Getenv(key)); err == nil {
			return val
		}
		return fallback
	}

	return Config{
		KafkaBrokers:    strings.Split(getEnv("KAFKA_BROKERS", "localhost:9092"), ","),
		KafkaTopic:      getEnv("KAFKA_TOPIC", "video-stream-1"),
		ConsumerGroup:   getEnv("CONSUMER_GROUP", "inference-consumer-group"),
		InferenceURL:    getEnv("INFERENCE_URL", "http://inference-service:8080/predict"),
		S3Bucket:        getEnv("S3_BUCKET", "optifye-annotated-frames"),
		S3Prefix:        getEnv("S3_PREFIX", "annotated"),
		BatchSize:       parseEnvInt("BATCH_SIZE", 25),
		BatchTimeoutSec: parseEnvInt("BATCH_TIMEOUT_SEC", 10),
		AWSRegion:       getEnv("AWS_REGION", "ap-south-1"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ── Inference API Types ──

type BBox struct {
	X1 float64 `json:"x1"`
	Y1 float64 `json:"y1"`
	X2 float64 `json:"x2"`
	Y2 float64 `json:"y2"`
}

type Detection struct {
	BBox       BBox    `json:"bbox"`
	Confidence float64 `json:"confidence"`
	ClassID    int     `json:"class_id"`
	ClassName  string  `json:"class_name"`
}

type InferenceResponse struct {
	BatchSize       int           `json:"batch_size"`
	Detections      [][]Detection `json:"detections"`
	InferenceTimeMs float64       `json:"inference_time_ms"`
}

// ── Consumer Group Handler ──

type ConsumerHandler struct {
	cfg        Config
	s3Client   *s3.Client
	httpClient *http.Client
}

func (h *ConsumerHandler) Setup(_ sarama.ConsumerGroupSession) error   { return nil }
func (h *ConsumerHandler) Cleanup(_ sarama.ConsumerGroupSession) error { return nil }

func (h *ConsumerHandler) ConsumeClaim(session sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	batch := make([][]byte, 0, h.cfg.BatchSize)
	batchMsgs := make([]*sarama.ConsumerMessage, 0, h.cfg.BatchSize)
	batchTimeout := time.NewTimer(time.Duration(h.cfg.BatchTimeoutSec) * time.Second)
	defer batchTimeout.Stop()

	var batchCount uint64

	processAndReset := func() {
		if len(batch) > 0 {
			// Pass session context to ensure uploads cancel on shutdown
			h.processBatch(session.Context(), batch, batchCount)
			for _, m := range batchMsgs {
				session.MarkMessage(m, "")
			}
			batchCount++
			batch = batch[:0] // Reuse slice capacity
			batchMsgs = batchMsgs[:0]
		}
	}

	for {
		select {
		case msg, ok := <-claim.Messages():
			if !ok {
				processAndReset()
				return nil
			}

			batch = append(batch, msg.Value)
			batchMsgs = append(batchMsgs, msg)

			if len(batch) == 1 {
				if !batchTimeout.Stop() {
					select {
					case <-batchTimeout.C:
					default:
					}
				}
				batchTimeout.Reset(time.Duration(h.cfg.BatchTimeoutSec) * time.Second)
			}

			if len(batch) >= h.cfg.BatchSize {
				processAndReset()
			}

		case <-batchTimeout.C:
			processAndReset()

		case <-session.Context().Done():
			return nil
		}
	}
}

// ── Core Pipeline ──

func (h *ConsumerHandler) processBatch(ctx context.Context, frames [][]byte, batchID uint64) {
	start := time.Now()

	resp, err := h.callInference(ctx, frames)
	if err != nil {
		log.Printf("[Batch %d] Inference failed: %v", batchID, err)
		return
	}

	if len(resp.Detections) > 0 && len(frames) > 0 {
		annotated, err := annotateFrame(frames[0], resp.Detections[0])
		if err != nil {
			log.Printf("[Batch %d] Annotation failed: %v", batchID, err)
			return
		}

		key := fmt.Sprintf("%s/batch_%06d_%s.jpg",
			h.cfg.S3Prefix, batchID, time.Now().Format("20060102_150405"))

		if err := h.uploadToS3(ctx, annotated, key); err != nil {
			log.Printf("[Batch %d] S3 upload failed: %v", batchID, err)
			return
		}

		log.Printf("[Batch %d] Success: s3://%s/%s (Inf: %.1fms, Total: %v)",
			batchID, h.cfg.S3Bucket, key, resp.InferenceTimeMs, time.Since(start))
	}
}

func (h *ConsumerHandler) callInference(ctx context.Context, frames [][]byte) (*InferenceResponse, error) {
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufferPool.Put(buf)

	writer := multipart.NewWriter(buf)
	for i, frame := range frames {
		part, err := writer.CreateFormFile(fmt.Sprintf("frame_%d", i), "f.jpg")
		if err != nil {
			return nil, err
		}
		part.Write(frame)
	}
	writer.Close()

	req, err := http.NewRequestWithContext(ctx, "POST", h.cfg.InferenceURL, buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server error: %d", resp.StatusCode)
	}

	var inferResp InferenceResponse
	return &inferResp, json.NewDecoder(resp.Body).Decode(&inferResp)
}

// ── Optimized Annotation ──

func annotateFrame(jpegData []byte, detections []Detection) ([]byte, error) {
	img, err := jpeg.Decode(bytes.NewReader(jpegData))
	if err != nil {
		return nil, err
	}

	b := img.Bounds()
	rgba := image.NewRGBA(b)
	draw.Draw(rgba, b, img, b.Min, draw.Src)

	for _, det := range detections {
		x1, y1 := int(det.BBox.X1), int(det.BBox.Y1)
		x2, y2 := int(det.BBox.X2), int(det.BBox.Y2)
		col := getBBoxColor(det.ClassID)

		// Faster box drawing: 4 draw calls instead of pixel loop
		thickness := 3
		draw.Draw(rgba, image.Rect(x1, y1, x2, y1+thickness), &image.Uniform{col}, image.Point{}, draw.Src) // Top
		draw.Draw(rgba, image.Rect(x1, y2-thickness, x2, y2), &image.Uniform{col}, image.Point{}, draw.Src) // Bottom
		draw.Draw(rgba, image.Rect(x1, y1, x1+thickness, y2), &image.Uniform{col}, image.Point{}, draw.Src) // Left
		draw.Draw(rgba, image.Rect(x2-thickness, y1, x2, y2), &image.Uniform{col}, image.Point{}, draw.Src) // Right

		// Label
		label := fmt.Sprintf("%s %.0f%%", det.ClassName, det.Confidence*100)
		labelH, labelW := 15, len(label)*7+4

		textY := maxInt(y1, labelH)
		labelRect := image.Rect(x1, textY-labelH, minInt(x1+labelW, b.Max.X), textY)
		draw.Draw(rgba, labelRect, &image.Uniform{col}, image.Point{}, draw.Src)
		drawLabel(rgba, label, x1+2, textY-3, color.White)
	}

	out := bufferPool.Get().(*bytes.Buffer)
	out.Reset()
	defer bufferPool.Put(out)

	if err := jpeg.Encode(out, rgba, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	// Return a copy because we put the buffer back in the pool
	return append([]byte(nil), out.Bytes()...), nil
}

func drawLabel(img *image.RGBA, text string, x, y int, c color.Color) {
	d := &font.Drawer{
		Dst: img, Src: image.NewUniform(c),
		Face: basicfont.Face7x13,
		Dot:  fixed.Point26_6{X: fixed.I(x), Y: fixed.I(y)},
	}
	d.DrawString(text)
}

func getBBoxColor(id int) color.RGBA {
	palette := []color.RGBA{
		{0, 255, 0, 255}, {255, 0, 0, 255}, {0, 120, 255, 255},
		{255, 255, 0, 255}, {255, 0, 255, 255}, {0, 255, 255, 255},
	}
	return palette[id%len(palette)]
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (h *ConsumerHandler) uploadToS3(ctx context.Context, data []byte, key string) error {
	contentType := "image/jpeg"
	_, err := h.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &h.cfg.S3Bucket, Key: &key,
		Body: bytes.NewReader(data), ContentType: &contentType,
	})
	return err
}

func main() {
	cfg := loadConfig()
	awsCfg, _ := config.LoadDefaultConfig(context.TODO(), config.WithRegion(cfg.AWSRegion))

	kafkaCfg := sarama.NewConfig()
	kafkaCfg.Consumer.Offsets.Initial = sarama.OffsetOldest
	kafkaCfg.Consumer.Fetch.Max = 15 * 1024 * 1024 // Increased for high-res frames

	group, err := sarama.NewConsumerGroup(cfg.KafkaBrokers, cfg.ConsumerGroup, kafkaCfg)
	if err != nil {
		log.Fatalf("Kafka error: %v", err)
	}

	handler := &ConsumerHandler{
		cfg:      cfg,
		s3Client: s3.NewFromConfig(awsCfg),
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:    50, // Increased for concurrent processing
				IdleConnTimeout: 90 * time.Second,
			},
		},
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Println("Optifye Consumer Online.")
	for {
		if err := group.Consume(ctx, []string{cfg.KafkaTopic}, handler); err != nil {
			log.Printf("Consumer Error: %v", err)
		}
		if ctx.Err() != nil {
			return
		}
	}
}
