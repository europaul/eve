package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

var (
	uploadSockVectorSource = "/run/devUpload_source.sock"
	keepSockVectorSource   = "/run/devKeep_source.sock"
	uploadSockVectorSink   = "/run/devUpload_sink.sock"
	keepSockVectorSink     = "/run/devKeep_sink.sock"

	vectorConfigPath       = "/persist/vector/config/vector.yaml"
	vectorConfigBackupPath = "/persist/vector/config/vector.yaml.bak"
)

type bufferedSockWriter struct {
	path      string
	buffer    chan []byte
	reconnect time.Duration
}

func NewBufferedSockWriter(path string, bufSize int, reconnect time.Duration) *bufferedSockWriter {
	sw := &bufferedSockWriter{
		path:      path,
		buffer:    make(chan []byte, bufSize),
		reconnect: reconnect,
	}
	go sw.run()
	return sw
}

func (sw *bufferedSockWriter) run() {
	for {
		conn, err := net.Dial("unix", sw.path)
		if err != nil {
			log.Errorf("socket connect failed: %v, retrying...", err)
			time.Sleep(sw.reconnect)
			continue
		}

		for msg := range sw.buffer {
			_, err := conn.Write(msg)
			if err != nil {
				log.Errorf("socket write failed: %v, reconnecting...", err)
				conn.Close()
				break // reconnect
			}
		}
	}
}

func (sw *bufferedSockWriter) Write(p []byte) (int, error) {
	// Don't block forever, drop if buffer full
	select {
	case sw.buffer <- slices.Clone(p): // copy buffer
		return len(p), nil
	default:
		return 0, fmt.Errorf("buffer full, dropping log")
	}
}

// listenOnSocketAndWriteToChan - goroutine to listen on unix sockets for incoming log entries
func listenOnSocketAndWriteToChan(sockPath string, sendToChan chan<- string) {
	// Create unix socket
	os.Remove(sockPath) // Remove any existing socket
	unixAddr, err := net.ResolveUnixAddr("unix", sockPath)
	if err != nil {
		log.Fatalf("createIncomingSockListener: ResolveUnixAddr failed: %v", err)
	}
	unixListener, err := net.ListenUnix("unix", unixAddr)
	if err != nil {
		log.Fatalf("createIncomingSockListener: ListenUnix failed: %v", err)
	}
	defer unixListener.Close()
	defer os.Remove(sockPath) // Ensure the socket is removed when done

	// Set permissions on socket
	if err := os.Chmod(sockPath, 0666); err != nil {
		log.Errorf("createIncomingSockListener: chmod socket failed: %v", err)
	}

	// Handle socket connections
	for {
		conn, err := unixListener.Accept()
		if err != nil {
			log.Errorf("createIncomingSockListener: upload accept failed: %v", err)
			continue
		}
		go handleIncomingConnection(conn, sendToChan)
	}
}

// handleIncomingConnection processes incoming log entries from unix socket connections
func handleIncomingConnection(conn net.Conn, sendToChan chan<- string) {
	defer conn.Close()

	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		logline := scanner.Text()
		if logline == "" {
			continue
		}

		// add newline character to the end of the logline
		if !strings.HasSuffix(logline, "\n") {
			logline += "\n"
		}

		sendToChan <- logline
	}

	if err := scanner.Err(); err != nil {
		log.Errorf("handleIncomingConnection: scanner error for socket: %v", err)
	}
}

type transformConfig struct {
	Type      string
	Inputs    []string
	Condition string
}

func writeTransformConfig(condition string) error {
	config := transformConfig{
		Type:      "filter",
		Inputs:    []string{"dev_upload_paser"},
		Condition: condition,
	}

	configBytes, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal transform config: %w", err)
	}

	// Create the directory if it doesn't exist
	if err := os.MkdirAll("/persist/vector/config/transforms", 0755); err != nil {
		return fmt.Errorf("failed to create transforms directory: %w", err)
	}

	configFile, err := os.OpenFile("/persist/vector/config/transforms/dev_upload_filter.json", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to open transform config file: %w", err)
	}
	defer configFile.Close()

	_, err = configFile.Write(configBytes)
	if err != nil {
		return fmt.Errorf("failed to write transform config: %w", err)
	}

	log.Noticef("Transform config written successfully: %s", string(configBytes))
	return nil
}

func writeVectorConfig(text []byte) error {
	// Create the directory if it doesn't exist
	if err := os.MkdirAll(filepath.Dir(vectorConfigPath), 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	configFile, err := os.OpenFile(vectorConfigPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to open config file: %w", err)
	}
	defer configFile.Close()

	_, err = configFile.Write(text)
	if err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}

	log.Noticef("Vector configuration written successfully")
	return nil
}

func handleVectorConfig(config string) error {
	// vector.config parameter is in base64 encoded format
	decodedConfig, err := base64.StdEncoding.DecodeString(config)
	if err != nil {
		return fmt.Errorf("failed to decode vector config: %w", err)
	}
	// backup the old vector config
	if err := os.Rename(vectorConfigPath, vectorConfigBackupPath); err != nil {
		return fmt.Errorf("failed to backup vector config: %w", err)
	}
	log.Functionf("renamed old vector config to %s", vectorConfigBackupPath)
	// write the decoded config to vector config file
	if err := writeVectorConfig(decodedConfig); err != nil {
		return fmt.Errorf("failed to write vector config: %w", err)
	}
	log.Functionf("wrote vector config to %s", vectorConfigPath)

	return nil
}
