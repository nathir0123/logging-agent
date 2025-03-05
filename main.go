package main

import (
	"bufio"
	"encoding/json"
	
	"log"
	"os"
	"os/exec"
	"regexp"
	"time"

	"github.com/confluentinc/confluent-kafka-go/kafka"

	"github.com/fsnotify/fsnotify"
	"github.com/gocql/gocql"
	"github.com/hpcloud/tail"
	"github.com/joho/godotenv"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var db *gorm.DB
var cassandraSession *gocql.Session
var producer *kafka.Producer
var consumer *kafka.Consumer

// LogEntry represents a detected log entry
type LogEntry struct {
	ID        gocql.UUID `json:"id"`
	Timestamp time.Time  `json:"timestamp"`
	LogLevel  string     `json:"log_level"`
	Message   string     `json:"message"`
	Source    string     `json:"source"`
}

func main() {
	// Load .env file
	err := godotenv.Load()
	if err != nil {
		log.Println("Warning: .env file not found. Using system environment variables.")
	}

	// Get configuration
	dsn := os.Getenv("DATABASE_URL")
	kafkaBroker := os.Getenv("KAFKA_BROKER")
	cassandraHost := os.Getenv("CASSANDRA_HOST")

	if dsn == "" || kafkaBroker == "" || cassandraHost == "" {
		log.Fatal("Missing required environment variables! Check DATABASE_URL, KAFKA_BROKER, and CASSANDRA_HOST.")
	}


	// Initialize PostgreSQL
	db, err = gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatalf("Failed to connect to PostgreSQL: %v", err)
	}
	db.AutoMigrate(&LogEntry{})

	// Initialize Cassandra
	cluster := gocql.NewCluster(cassandraHost)
	cluster.Keyspace = os.Getenv("CASSANDRA_KEYSPACE")
	cluster.Consistency = gocql.Quorum
	cassandraSession, err = cluster.CreateSession()
	if err != nil {
		log.Fatalf("Failed to connect to Cassandra: %v", err)
	}
	defer cassandraSession.Close()

	// Initialize Kafka producer
	producer, err = kafka.NewProducer(&kafka.ConfigMap{"bootstrap.servers": kafkaBroker})
	if err != nil {
		log.Fatalf("Failed to create Kafka producer: %v", err)
	}
	defer producer.Close()

	// Initialize Kafka consumer
	consumer, err = kafka.NewConsumer(&kafka.ConfigMap{
		"bootstrap.servers": kafkaBroker,
		"group.id":          "log-consumer-group",
		"auto.offset.reset": "earliest",
	})
	if err != nil {
		log.Fatalf("Failed to create Kafka consumer: %v", err)
	}
	defer consumer.Close()

	// Subscribe to Kafka topic
	consumer.Subscribe("logs", nil)

	// Start Kafka consumer to process logs from Kafka
	go consumeLogsFromKafka()

	// Start monitoring log directories
	logDirs := []string{"/var/log", "/tmp", "/var/lib/docker/containers"}
	for _, dir := range logDirs {
		go watchLogDirectory(dir)
	}

	// Start capturing system logs
	go monitorSystemdLogs()
	go monitorSysdig()

	// Keep the program running
	select {}
}

// Watch directory for new log files
func watchLogDirectory(dir string) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalf("Error initializing file watcher: %v", err)
	}
	defer watcher.Close()

	err = watcher.Add(dir)
	if err != nil {
		log.Fatalf("Error adding directory to watcher: %v", err)
	}

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&fsnotify.Create == fsnotify.Create {
				go monitorLogFile(event.Name)
			}
		}
	}
}

// Monitor a log file for new entries
func monitorLogFile(filename string) {
	t, err := tail.TailFile(filename, tail.Config{Follow: true, ReOpen: true})
	if err != nil {
		log.Printf("Error opening log file %s: %v", filename, err)
		return
	}

	for line := range t.Lines {
		processLogLine(line.Text, filename)
	}
}

// Capture system logs using journalctl
func monitorSystemdLogs() {
	cmd := exec.Command("journalctl", "-f", "-o", "cat")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatalf("Failed to start journalctl: %v", err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start command: %v", err)
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		processLogLine(scanner.Text(), "systemd")
	}
}

// Monitor system-wide activity using sysdig
func monitorSysdig() {
	cmd := exec.Command("sudo", "sysdig", "-c", "echo_fds")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatalf("Failed to start sysdig: %v", err)
	}
	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start command: %v", err)
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		processLogLine(scanner.Text(), "sysdig")
	}
}

// Process log lines, filter them, and send them to Kafka, PostgreSQL, and Cassandra
func processLogLine(logLine string, source string) {
	logLevel := detectLogLevel(logLine)
	timestamp, extractedMessage := extractTimestamp(logLine)

	// Create log entry
	logEntry := LogEntry{
		ID:        gocql.TimeUUID(),
		Timestamp: timestamp,
		LogLevel:  logLevel,
		Message:   extractedMessage,
		Source:    source,
	}

	// Convert log to JSON and send to Kafka
	logJSON, _ := json.Marshal(logEntry)
	topic := "logs"
	producer.Produce(&kafka.Message{
		TopicPartition: kafka.TopicPartition{Topic: &topic, Partition: kafka.PartitionAny},
		Value:          logJSON,
	}, nil)

	// Ensure messages are flushed
	producer.Flush(500)
}

// Consume logs from Kafka
func consumeLogsFromKafka() {
	for {
		msg, err := consumer.ReadMessage(-1)
		if err == nil {
			var logEntry LogEntry
			json.Unmarshal(msg.Value, &logEntry)

			// Store in PostgreSQL
			db.Create(&logEntry)

			// Store in Cassandra
			err := cassandraSession.Query(
				`INSERT INTO logs (id, timestamp, log_level, message, source) VALUES (?, ?, ?, ?, ?)`,
			).Bind(logEntry.ID, logEntry.Timestamp, logEntry.LogLevel, logEntry.Message, logEntry.Source).Exec()

			if err != nil {
				log.Printf("Failed to insert into Cassandra: %v", err)
			}

		} else {
			log.Printf("Consumer error: %v\n", err)
		}
	}
}

// Extract timestamp from logs
func extractTimestamp(logLine string) (time.Time, string) {
	timestamp := time.Now()
	return timestamp, logLine
}

// Detect log severity using regex
func detectLogLevel(logLine string) string {
	levelPatterns := map[string]string{
		"CRITICAL":    `(?i)(panic|fatal)`,
		"SECURITY":    `(?i)(unauthorized|attack|security breach)`,
		"PERFORMANCE": `(?i)(timeout|slow response)`,
		"SYSTEM":      `(?i)(disk full|cpu overload|memory leak)`,
		"ERROR":       `(?i)(error|failed|exception)`,
		"WARNING":     `(?i)(warning|deprecated)`,
		"INFO":        `(?i)(info|started|initialized)`,
	}

	for level, pattern := range levelPatterns {
		matched, _ := regexp.MatchString(pattern, logLine)
		if matched {
			return level
		}
	}
	return "UNKNOWN"
}
