package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

var (
	configFile = flag.String("config", "config.yaml", "Path to configuration file")
	mode       = flag.String("mode", "", "Subscription mode: once, poll, stream (overrides config)")
	target     = flag.String("target", "", "Target address (overrides config)")
	timeout    = flag.Duration("timeout", 30*time.Second, "Connection timeout")
	debug      = flag.Bool("d", false, "Enable debug logging")
)

var debugLog *log.Logger

func main() {
	flag.Parse()

	if *debug {
		debugLog = log.New(os.Stdout, "[DEBUG] ", log.LstdFlags|log.Lmicroseconds)
	} else {
		debugLog = log.New(io.Discard, "", 0)
	}

	config, err := loadConfig(*configFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	if *mode != "" {
		config.Subscription.Mode = *mode
	}

	if *target != "" {
		config.Connection.Target = *target
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client, err := NewGNMIClient(config)
	if err != nil {
		debugLog.Printf("Client creation error: %v", err)
		fmt.Fprintf(os.Stderr, "Failed to create gNMI client: %v\n", err)
		os.Exit(1)
	}

	SetDebugLogger(debugLog)
	debugLog.Printf("Created gNMI client with mode: %s", config.Subscription.Mode)
	debugLog.Printf("Target: %s", config.Connection.Target)
	debugLog.Printf("TLS enabled: %v", config.TLS.Enabled)

	if err := client.Connect(ctx); err != nil {
		debugLog.Printf("Connection error: %v", err)
		fmt.Fprintf(os.Stderr, "Failed to connect: %v\n", err)
		os.Exit(1)
	}
	defer client.Close()

	debugLog.Printf("Successfully connected to gNMI server")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	done := make(chan error, 1)
	go func() {
		debugLog.Printf("Starting subscription...")
		err := client.Subscribe(ctx)
		debugLog.Printf("Subscription finished with error: %v", err)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			debugLog.Printf("Subscription failed: %v", err)
			fmt.Fprintf(os.Stderr, "Subscription error: %v\n", err)
			os.Exit(1)
		}
		debugLog.Printf("Subscription completed successfully")
	case sig := <-sigChan:
		debugLog.Printf("Received signal %v, initiating shutdown", sig)
		fmt.Printf("\nReceived signal %v, shutting down...\n", sig)
		cancel()
		time.Sleep(1 * time.Second)
	}

	fmt.Println("gNMI client stopped")
}

func loadConfig(filename string) (*GNMIConfig, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	var config GNMIConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %v", err)
	}

	if config.Connection.Target == "" {
		config.Connection.Target = "localhost:50051"
	}

	if config.Subscription.Mode == "" {
		config.Subscription.Mode = "stream"
	}

	if config.Stream.SampleInterval == "" {
		config.Stream.SampleInterval = "10s"
	}

	if config.Subscription.PollInterval == "" {
		config.Subscription.PollInterval = "10s"
	}

	return &config, nil
}
