package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/IBM/sarama"
)

type Config struct {
	RTSPUrl      string
	KafkaBrokers []string
	KafkaTopic   string
	FPS          int
	JPEGQuality  int
	Width        int
	Height       int
}

func loadConfig() Config {
	// Helper to parse ints with a fallback
	parseEnvInt := func(key string, fallback int) int {
		if val, err := strconv.Atoi(os.Getenv(key)); err == nil {
			return val
		}
		return fallback
	}

	return Config{
		RTSPUrl:      getEnv("RTSP_URL", "rtsp://localhost:8554/stream"),
		KafkaBrokers: splitBrokers(getEnv("KAFKA_BROKERS", "localhost:9092")),
		KafkaTopic:   getEnv("KAFKA_TOPIC", "video-stream-1"),
		FPS:          parseEnvInt("TARGET_FPS", 5),
		JPEGQuality:  parseEnvInt("JPEG_QUALITY", 80),
		Width:        parseEnvInt("FRAME_WIDTH", 640),
		Height:       parseEnvInt("FRAME_HEIGHT", 480),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func splitBrokers(s string) []string {
	var brokers []string
	for _, b := range strings.Split(s, ",") {
		if b = strings.TrimSpace(b); b != "" {
			brokers = append(brokers, b)
		}
	}
	return brokers
}

// startFFmpeg uses exec.CommandContext to ensure the process dies when the context is cancelled.
func startFFmpeg(ctx context.Context, cfg Config) (*exec.Cmd, io.ReadCloser, error) {
	// FFmpeg quality: 2 (best) to 31 (worst)
	qScale := 31 - (cfg.JPEGQuality * 30 / 100)
	if qScale < 2 {
		qScale = 2
	}

	args := []string{
		"-rtsp_transport", "tcp",
		"-analyzeduration", "5000000",
		"-probesize", "10000000",
		"-i", cfg.RTSPUrl,
		"-vf", fmt.Sprintf("fps=%d,scale=%d:%d", cfg.FPS, cfg.Width, cfg.Height),
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"-q:v", strconv.Itoa(qScale),
		"-an", "-",
	}

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stderr = os.Stderr // Useful for debugging FFmpeg connection issues

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}

	return cmd, stdout, nil
}

// readJPEGFrame uses bufio.Reader to minimize syscalls and scans for markers.
func readJPEGFrame(br *bufio.Reader) ([]byte, error) {
	var buf bytes.Buffer

	// 1. Find Start of Image (SOI): 0xFF 0xD8
	for {
		b, err := br.ReadByte()
		if err != nil {
			return nil, err
		}
		if b == 0xFF {
			next, err := br.Peek(1)
			if err == nil && next[0] == 0xD8 {
				br.ReadByte() // consume 0xD8
				buf.Write([]byte{0xFF, 0xD8})
				break
			}
		}
	}

	// 2. Read until End of Image (EOI): 0xFF 0xD9
	for {
		b, err := br.ReadByte()
		if err != nil {
			return nil, err
		}
		buf.WriteByte(b)

		if b == 0xFF {
			next, err := br.Peek(1)
			if err == nil && next[0] == 0xD9 {
				br.ReadByte() // consume 0xD9
				buf.WriteByte(0xD9)
				return buf.Bytes(), nil
			}
		}

		// Safety cap: 5MB
		if buf.Len() > 5*1024*1024 {
			return nil, fmt.Errorf("frame exceeded safety limit")
		}
	}
}

func main() {
	cfg := loadConfig()
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.Lshortfile)
	log.Printf("Starting frame-producer | topic=%s fps=%d", cfg.KafkaTopic, cfg.FPS)

	// ── Graceful Shutdown Setup ──
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ── Kafka Async Producer ──
	kCfg := sarama.NewConfig()
	kCfg.Producer.RequiredAcks = sarama.WaitForLocal
	kCfg.Producer.Compression = sarama.CompressionSnappy
	kCfg.Producer.Return.Errors = true
	// MaxMessageBytes should be larger than your max expected JPEG size
	kCfg.Producer.MaxMessageBytes = 1024 * 1024 * 2

	producer, err := sarama.NewAsyncProducer(cfg.KafkaBrokers, kCfg)
	if err != nil {
		log.Fatalf("Kafka error: %v", err)
	}
	defer producer.AsyncClose()

	// Handle Kafka errors in the background
	go func() {
		for err := range producer.Errors() {
			log.Printf("Kafka Drop: %v", err.Err)
		}
	}()

	var frameCount uint64

	// ── Main Stream Loop ──
loop:
	for {
		log.Printf("Connecting to RTSP: %s", cfg.RTSPUrl)
		cmd, stdout, err := startFFmpeg(ctx, cfg)
		if err != nil {
			log.Printf("FFmpeg start failed: %v", err)
			select {
			case <-ctx.Done():
				break loop
			case <-time.After(5 * time.Second):
				continue
			}
		}

		reader := bufio.NewReaderSize(stdout, 64*1024) // 64KB buffer

		for {
			jpegData, err := readJPEGFrame(reader)
			if err != nil {
				if err != io.EOF {
					log.Printf("Read error: %v", err)
				}
				break // Restart FFmpeg
			}

			// Metadata
			ts := time.Now()
			idxBytes := make([]byte, 8)
			binary.BigEndian.PutUint64(idxBytes, frameCount)

			msg := &sarama.ProducerMessage{
				Topic: cfg.KafkaTopic,
				Key:   sarama.ByteEncoder(idxBytes),
				Value: sarama.ByteEncoder(jpegData),
				Headers: []sarama.RecordHeader{
					{Key: []byte("frame_index"), Value: idxBytes},
					{Key: []byte("timestamp"), Value: []byte(ts.Format(time.RFC3339Nano))},
				},
				Timestamp: ts,
			}

			// Avoid producer backpressure stalling frame extraction.
			select {
			case producer.Input() <- msg:
				frameCount++
			default:
				log.Printf("Kafka producer buffer full, dropping frame index=%d", frameCount)
			}

			if frameCount%100 == 0 {
				log.Printf("Processed %d frames", frameCount)
			}

			// Check if we should shut down
			select {
			case <-ctx.Done():
				cmd.Process.Kill()
				break loop
			default:
			}
		}

		cmd.Wait()
		log.Println("FFmpeg exited, retrying...")
		time.Sleep(2 * time.Second)
	}

	log.Printf("Shutdown complete. Total frames: %d", frameCount)
}
