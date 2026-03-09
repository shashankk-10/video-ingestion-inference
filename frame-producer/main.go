package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/IBM/sarama"
)

type StreamConfig struct {
	RTSPUrl string
	Topic   string
	ID      string // e.g. "stream-0", "stream-1"
}

type Config struct {
	Streams      []StreamConfig
	KafkaBrokers []string
	TopicPrefix  string
	FPS          int
	JPEGQuality  int
	Width        int
	Height       int
}

func loadConfig() Config {
	parseEnvInt := func(key string, fallback int) int {
		if val, err := strconv.Atoi(os.Getenv(key)); err == nil {
			return val
		}
		return fallback
	}

	brokers := splitCSV(getEnv("KAFKA_BROKERS", "localhost:9092"))
	topicPrefix := getEnv("KAFKA_TOPIC_PREFIX", "video-stream")

	// RTSP_URLS: comma-separated list of RTSP stream URLs.
	// Falls back to single RTSP_URL for backward compatibility.
	rawURLs := getEnv("RTSP_URLS", "")
	var rtspURLs []string
	if rawURLs != "" {
		rtspURLs = splitCSV(rawURLs)
	} else {
		rtspURLs = []string{getEnv("RTSP_URL", "rtsp://localhost:8554/stream")}
	}

	streams := make([]StreamConfig, len(rtspURLs))
	for i, url := range rtspURLs {
		streams[i] = StreamConfig{
			RTSPUrl: url,
			Topic:   fmt.Sprintf("%s-%d", topicPrefix, i+1),
			ID:      fmt.Sprintf("stream-%d", i),
		}
	}

	return Config{
		Streams:      streams,
		KafkaBrokers: brokers,
		TopicPrefix:  topicPrefix,
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

func splitCSV(s string) []string {
	var parts []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	return parts
}

// startFFmpeg launches an FFmpeg process that reads from an RTSP URL and outputs MJPEG frames to stdout.
func startFFmpeg(ctx context.Context, cfg Config, rtspURL string) (*exec.Cmd, io.ReadCloser, error) {
	qScale := 31 - (cfg.JPEGQuality * 30 / 100)
	if qScale < 2 {
		qScale = 2
	}

	args := []string{
		"-rtsp_transport", "tcp",
		"-analyzeduration", "5000000",
		"-probesize", "10000000",
		"-i", rtspURL,
		"-vf", fmt.Sprintf("fps=%d,scale=%d:%d", cfg.FPS, cfg.Width, cfg.Height),
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"-q:v", strconv.Itoa(qScale),
		"-an", "-",
	}

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stderr = os.Stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}

	return cmd, stdout, nil
}

// readJPEGFrame scans for SOI (0xFF 0xD8) and EOI (0xFF 0xD9) markers to extract a single JPEG frame.
func readJPEGFrame(br *bufio.Reader) ([]byte, error) {
	var buf bytes.Buffer

	for {
		b, err := br.ReadByte()
		if err != nil {
			return nil, err
		}
		if b == 0xFF {
			next, err := br.Peek(1)
			if err == nil && next[0] == 0xD8 {
				br.ReadByte()
				buf.Write([]byte{0xFF, 0xD8})
				break
			}
		}
	}

	for {
		b, err := br.ReadByte()
		if err != nil {
			return nil, err
		}
		buf.WriteByte(b)

		if b == 0xFF {
			next, err := br.Peek(1)
			if err == nil && next[0] == 0xD9 {
				br.ReadByte()
				buf.WriteByte(0xD9)
				return buf.Bytes(), nil
			}
		}

		if buf.Len() > 5*1024*1024 {
			return nil, fmt.Errorf("frame exceeded 5MB safety limit")
		}
	}
}

// runStream handles a single RTSP stream: reconnects on failure and publishes frames to Kafka.
func runStream(ctx context.Context, cfg Config, sc StreamConfig, producer sarama.AsyncProducer) {
	var frameCount uint64
	log.Printf("[%s] Starting | topic=%s url=%s", sc.ID, sc.Topic, sc.RTSPUrl)

	for {
		select {
		case <-ctx.Done():
			log.Printf("[%s] Shutdown. Total frames: %d", sc.ID, frameCount)
			return
		default:
		}

		log.Printf("[%s] Connecting to RTSP: %s", sc.ID, sc.RTSPUrl)
		cmd, stdout, err := startFFmpeg(ctx, cfg, sc.RTSPUrl)
		if err != nil {
			log.Printf("[%s] FFmpeg start failed: %v", sc.ID, err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}

		reader := bufio.NewReaderSize(stdout, 64*1024)

		for {
			jpegData, err := readJPEGFrame(reader)
			if err != nil {
				if err != io.EOF {
					log.Printf("[%s] Read error: %v", sc.ID, err)
				}
				break
			}

			ts := time.Now()
			idxBytes := make([]byte, 8)
			binary.BigEndian.PutUint64(idxBytes, frameCount)

			msg := &sarama.ProducerMessage{
				Topic: sc.Topic,
				Key:   sarama.ByteEncoder(idxBytes),
				Value: sarama.ByteEncoder(jpegData),
				Headers: []sarama.RecordHeader{
					{Key: []byte("frame_index"), Value: idxBytes},
					{Key: []byte("timestamp"), Value: []byte(ts.Format(time.RFC3339Nano))},
					{Key: []byte("stream_id"), Value: []byte(sc.ID)},
				},
				Timestamp: ts,
			}

			select {
			case producer.Input() <- msg:
				frameCount++
			default:
				log.Printf("[%s] Kafka buffer full, dropping frame %d", sc.ID, frameCount)
			}

			if frameCount%100 == 0 {
				log.Printf("[%s] Processed %d frames", sc.ID, frameCount)
			}

			select {
			case <-ctx.Done():
				cmd.Process.Kill()
				log.Printf("[%s] Shutdown. Total frames: %d", sc.ID, frameCount)
				return
			default:
			}
		}

		cmd.Wait()
		log.Printf("[%s] FFmpeg exited, retrying in 2s...", sc.ID)

		select {
		case <-ctx.Done():
			log.Printf("[%s] Shutdown. Total frames: %d", sc.ID, frameCount)
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func main() {
	cfg := loadConfig()
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.Lshortfile)
	log.Printf("Starting frame-producer | %d stream(s)", len(cfg.Streams))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// ── Kafka Async Producer (shared across all streams; thread-safe) ──
	kCfg := sarama.NewConfig()
	kCfg.Net.TLS.Enable = true
	kCfg.Net.TLS.Config = &tls.Config{}
	kCfg.Producer.RequiredAcks = sarama.WaitForLocal
	kCfg.Producer.Compression = sarama.CompressionSnappy
	kCfg.Producer.Return.Errors = true
	kCfg.Producer.MaxMessageBytes = 2 * 1024 * 1024

	producer, err := sarama.NewAsyncProducer(cfg.KafkaBrokers, kCfg)
	if err != nil {
		log.Fatalf("Kafka error: %v", err)
	}
	defer producer.AsyncClose()

	// Drain Kafka errors
	var kafkaErrors uint64
	go func() {
		for err := range producer.Errors() {
			atomic.AddUint64(&kafkaErrors, 1)
			log.Printf("Kafka Drop: %v", err.Err)
		}
	}()

	// ── Launch one goroutine per stream ──
	var wg sync.WaitGroup
	for _, sc := range cfg.Streams {
		wg.Add(1)
		go func(sc StreamConfig) {
			defer wg.Done()
			runStream(ctx, cfg, sc, producer)
		}(sc)
	}

	wg.Wait()
	log.Printf("All streams stopped. Kafka errors: %d", atomic.LoadUint64(&kafkaErrors))
}
