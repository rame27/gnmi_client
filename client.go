package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	gnmipb "github.com/openconfig/gnmi/proto/gnmi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

func SetDebugLogger(logger *log.Logger) {
	debugLog = logger
}

type SubscriptionMode int

const (
	Once SubscriptionMode = iota
	Poll
	Stream
)

func (m SubscriptionMode) String() string {
	switch m {
	case Once:
		return "once"
	case Poll:
		return "poll"
	case Stream:
		return "stream"
	default:
		return "unknown"
	}
}

type GNMIConfig struct {
	Connection     ConnectionConfig     `yaml:"connection"`
	TLS            TLSConfig            `yaml:"tls"`
	Authentication AuthenticationConfig `yaml:"authentication"`
	Subscription   SubscriptionConfig   `yaml:"subscription"`
	Stream         StreamConfig         `yaml:"stream"`
	Paths          []PathConfig         `yaml:"paths"`
	Output         OutputConfig         `yaml:"output"`
}

type ConnectionConfig struct {
	Target  string `yaml:"target"`
	Timeout string `yaml:"timeout"`
}

type TLSConfig struct {
	Enabled            bool   `yaml:"enabled"`
	CACertificate      string `yaml:"ca_certificate"`
	ClientCertificate  string `yaml:"client_certificate"`
	ClientKey          string `yaml:"client_key"`
	ServerName         string `yaml:"server_name"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
}

type AuthenticationConfig struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type SubscriptionConfig struct {
	Mode         string `yaml:"mode"`
	PollInterval string `yaml:"poll_interval"`
}

type StreamConfig struct {
	SampleInterval    string `yaml:"sample_interval"`
	SuppressRedundant bool   `yaml:"suppress_redundant"`
	HeartbeatInterval string `yaml:"heartbeat_interval"`
}

type PathConfig struct {
	Path        string `yaml:"path"`
	Description string `yaml:"description"`
	Origin      string `yaml:"origin,omitempty"`
	Target      string `yaml:"target,omitempty"`
}

type OutputConfig struct {
	Format      string `yaml:"format"`
	PrettyPrint bool   `yaml:"pretty_print"`
	LogToFile   bool   `yaml:"log_to_file"`
	LogFile     string `yaml:"log_file"`
}

type GNMIClient struct {
	config     *GNMIConfig
	conn       *grpc.ClientConn
	client     gnmipb.GNMIClient
	mode       SubscriptionMode
	pollTicker *time.Ticker
	stopChan   chan struct{}
	mu         sync.Mutex
}

func NewGNMIClient(config *GNMIConfig) (*GNMIClient, error) {
	client := &GNMIClient{
		config:   config,
		stopChan: make(chan struct{}),
	}

	if err := client.parseMode(); err != nil {
		return nil, err
	}

	return client, nil
}

func (c *GNMIClient) parseMode() error {
	switch strings.ToLower(c.config.Subscription.Mode) {
	case "once":
		c.mode = Once
	case "poll":
		c.mode = Poll
	case "stream":
		c.mode = Stream
	default:
		return fmt.Errorf("invalid subscription mode: %s", c.config.Subscription.Mode)
	}
	return nil
}

func (c *GNMIClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	timeout := 30 * time.Second
	if c.config.Connection.Timeout != "" {
		var err error
		timeout, err = time.ParseDuration(c.config.Connection.Timeout)
		if err != nil {
			return fmt.Errorf("invalid timeout duration: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var opts []grpc.DialOption

	if c.config.TLS.Enabled && !c.isInsecure() {
		tlsConfig, err := c.loadTLSConfig()
		if err != nil {
			return fmt.Errorf("failed to load TLS config: %v", err)
		}
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	conn, err := grpc.DialContext(ctx, c.config.Connection.Target, opts...)
	if err != nil {
		return fmt.Errorf("failed to connect to %s: %v", c.config.Connection.Target, err)
	}

	c.conn = conn
	c.client = gnmipb.NewGNMIClient(conn)

	debugLog.Printf("Connected to gNMI server at %s", c.config.Connection.Target)
	return nil
}

func (c *GNMIClient) isInsecure() bool {
	if c.config.TLS.Enabled {
		return false
	}
	if conn := c.config.Connection; conn.Target != "" {
		return strings.Contains(conn.Target, "localhost") || strings.Contains(conn.Target, "127.0.0.1")
	}
	return true
}

func (c *GNMIClient) loadTLSConfig() (*tls.Config, error) {
	debugLog.Printf("Loading TLS configuration")

	tlsConfig := &tls.Config{
		ServerName:         c.config.TLS.ServerName,
		InsecureSkipVerify: c.config.TLS.InsecureSkipVerify,
	}

	if c.config.TLS.CACertificate != "" {
		debugLog.Printf("Reading CA certificate from: %s", c.config.TLS.CACertificate)
		caCert, err := os.ReadFile(c.config.TLS.CACertificate)
		if err != nil {
			debugLog.Printf("Failed to read CA certificate: %v", err)
			return nil, fmt.Errorf("failed to read CA certificate: %v", err)
		}
		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			debugLog.Printf("Failed to parse CA certificate")
			return nil, fmt.Errorf("failed to parse CA certificate")
		}
		tlsConfig.RootCAs = caCertPool
		debugLog.Printf("CA certificate loaded successfully")
	} else {
		debugLog.Printf("No CA certificate configured, using system roots")
	}

	if c.config.TLS.ClientCertificate != "" && c.config.TLS.ClientKey != "" {
		debugLog.Printf("Loading client certificate from: %s", c.config.TLS.ClientCertificate)
		debugLog.Printf("Loading client key from: %s", c.config.TLS.ClientKey)
		cert, err := tls.LoadX509KeyPair(c.config.TLS.ClientCertificate, c.config.TLS.ClientKey)
		if err != nil {
			debugLog.Printf("Failed to load client certificate/key: %v", err)
			return nil, fmt.Errorf("failed to load client certificate/key: %v", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
		debugLog.Printf("Client certificate loaded successfully")
	}

	debugLog.Printf("TLS config: ServerName=%s, InsecureSkipVerify=%v, RootCAs=%v, Certificates=%d",
		tlsConfig.ServerName, tlsConfig.InsecureSkipVerify, tlsConfig.RootCAs != nil, len(tlsConfig.Certificates))

	return tlsConfig, nil
}

func (c *GNMIClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	close(c.stopChan)

	if c.pollTicker != nil {
		c.pollTicker.Stop()
	}

	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

func (c *GNMIClient) Subscribe(ctx context.Context) error {
	subscriptionList, err := c.buildSubscriptionList()
	if err != nil {
		return fmt.Errorf("failed to build subscription list: %v", err)
	}

	switch c.mode {
	case Once:
		return c.subscribeOnce(ctx, subscriptionList)
	case Poll:
		return c.subscribePoll(ctx, subscriptionList)
	case Stream:
		return c.subscribeStream(ctx, subscriptionList)
	default:
		return fmt.Errorf("unsupported subscription mode: %s", c.mode)
	}
}

func (c *GNMIClient) buildSubscriptionList() (*gnmipb.SubscriptionList, error) {
	subscriptions := make([]*gnmipb.Subscription, 0, len(c.config.Paths))

	for _, pathConfig := range c.config.Paths {
		path, err := c.parsePath(pathConfig.Path, pathConfig.Origin, pathConfig.Target)
		if err != nil {
			debugLog.Printf("Failed to parse path %s: %v", pathConfig.Path, err)
			return nil, fmt.Errorf("failed to parse path %s: %v", pathConfig.Path, err)
		}

		debugLog.Printf("Added subscription path: %s (origin: %s, target: %s)", pathConfig.Path, pathConfig.Origin, pathConfig.Target)

		subscription := &gnmipb.Subscription{
			Path: path,
		}

		if c.mode == Stream {
			interval, err := time.ParseDuration(c.config.Stream.SampleInterval)
			if err != nil {
				interval = 10 * time.Second
			}
			subscription.SampleInterval = uint64(interval.Nanoseconds())
			subscription.SuppressRedundant = c.config.Stream.SuppressRedundant
		}

		subscriptions = append(subscriptions, subscription)
	}

	mode := gnmipb.SubscriptionList_STREAM
	switch c.mode {
	case Once:
		mode = gnmipb.SubscriptionList_ONCE
	case Poll:
		mode = gnmipb.SubscriptionList_POLL
	case Stream:
		mode = gnmipb.SubscriptionList_STREAM
	}

	return &gnmipb.SubscriptionList{
		Subscription: subscriptions,
		Mode:         mode,
	}, nil
}

func (c *GNMIClient) parsePath(pathStr string, origin string, target string) (*gnmipb.Path, error) {
	if !strings.HasPrefix(pathStr, "/") {
		return nil, fmt.Errorf("path must start with /")
	}

	path := &gnmipb.Path{
		Elem:   make([]*gnmipb.PathElem, 0),
		Origin: origin,
		Target: target,
	}

	pathStr = strings.TrimPrefix(pathStr, "/")
	elements := strings.Split(pathStr, "/")

	for _, elem := range elements {
		if elem == "" {
			continue
		}

		pathElem := &gnmipb.PathElem{
			Name: elem,
			Key:  make(map[string]string),
		}

		if idx := strings.Index(elem, "["); idx != -1 {
			name := elem[:idx]
			keys := elem[idx:]
			pathElem.Name = name

			for {
				start := strings.Index(keys, "[")
				if start == -1 {
					break
				}
				end := strings.Index(keys[start:], "]")
				if end == -1 {
					break
				}
				keyStr := keys[start+1 : start+end]
				if eqIdx := strings.Index(keyStr, "="); eqIdx != -1 {
					key := keyStr[:eqIdx]
					value := strings.Trim(keyStr[eqIdx+1:], "\"'")
					pathElem.Key[key] = value
				}
				keys = keys[start+end+1:]
			}
		} else {
			pathElem.Name = elem
		}

		path.Elem = append(path.Elem, pathElem)
	}

	return path, nil
}

func (c *GNMIClient) subscribeOnce(ctx context.Context, subscriptionList *gnmipb.SubscriptionList) error {
	ctx = c.withAuthentication(ctx)

	debugLog.Printf("Executing ONCE subscription with %d paths", len(subscriptionList.Subscription))

	paths := make([]*gnmipb.Path, 0, len(subscriptionList.Subscription))
	for _, sub := range subscriptionList.Subscription {
		paths = append(paths, sub.Path)
	}

	debugLog.Printf("Sending gNMI Get request with %d paths", len(paths))
	for i, p := range paths {
		debugLog.Printf("  Path[%d]: origin=%s, target=%s, elems=%v", i, p.Origin, p.Target, p.Elem)
	}

	debugLog.Printf("Calling gNMI Get RPC...")
	startTime := time.Now()
	response, err := c.client.Get(ctx, &gnmipb.GetRequest{
		Path: paths,
	})
	elapsed := time.Since(startTime)

	if err != nil {
		debugLog.Printf("gNMI Get failed after %v: %v", elapsed, err)
		return fmt.Errorf("gNMI Get failed: %v", err)
	}

	debugLog.Printf("gNMI Get succeeded after %v", elapsed)
	debugLog.Printf("Received response with %d notifications", len(response.Notification))

	for _, notification := range response.Notification {
		if err := c.printNotification(notification); err != nil {
			log.Printf("ERROR: Failed to print notification: %v", err)
		}
	}

	return nil
}

func (c *GNMIClient) subscribePoll(ctx context.Context, subscriptionList *gnmipb.SubscriptionList) error {
	interval, err := time.ParseDuration(c.config.Subscription.PollInterval)
	if err != nil {
		interval = 10 * time.Second
	}

	debugLog.Printf("Starting POLL subscription with interval: %v", interval)

	c.pollTicker = time.NewTicker(interval)
	defer c.pollTicker.Stop()

	ctx = c.withAuthentication(ctx)

	debugLog.Printf("Opening gNMI Subscribe stream for poll mode")
	subscribeClient, err := c.client.Subscribe(ctx)
	if err != nil {
		debugLog.Printf("gNMI Subscribe failed: %v", err)
		return fmt.Errorf("gNMI Subscribe failed: %v", err)
	}

	pollRequest := &gnmipb.SubscribeRequest{
		Request: &gnmipb.SubscribeRequest_Poll{
			Poll: &gnmipb.Poll{},
		},
	}

	for pollCount := 0; ; pollCount++ {
		select {
		case <-c.stopChan:
			debugLog.Printf("Stopping poll subscription")
			return nil
		case <-c.pollTicker.C:
			debugLog.Printf("Sending poll request #%d", pollCount+1)
			if err := subscribeClient.Send(pollRequest); err != nil {
				debugLog.Printf("Failed to send poll request: %v", err)
				return fmt.Errorf("failed to send poll request: %v", err)
			}

			response, err := subscribeClient.Recv()
			if err != nil {
				if err == io.EOF {
					debugLog.Printf("Received EOF, ending poll subscription")
					return nil
				}
				debugLog.Printf("Failed to receive poll response: %v", err)
				return fmt.Errorf("failed to receive poll response: %v", err)
			}

			if update := response.GetUpdate(); update != nil {
				if err := c.printNotification(update); err != nil {
					log.Printf("ERROR: Failed to print notification: %v", err)
				}
			}
		}
	}
}

func (c *GNMIClient) subscribeStream(ctx context.Context, subscriptionList *gnmipb.SubscriptionList) error {
	ctx = c.withAuthentication(ctx)

	debugLog.Printf("Starting STREAM subscription with %d paths", len(subscriptionList.Subscription))

	subscribeClient, err := c.client.Subscribe(ctx)
	if err != nil {
		debugLog.Printf("gNMI Subscribe failed: %v", err)
		return fmt.Errorf("gNMI Subscribe failed: %v", err)
	}

	subscribeRequest := &gnmipb.SubscribeRequest{
		Request: &gnmipb.SubscribeRequest_Subscribe{
			Subscribe: subscriptionList,
		},
	}

	debugLog.Printf("Sending subscribe request for stream mode")
	if err := subscribeClient.Send(subscribeRequest); err != nil {
		debugLog.Printf("Failed to send subscribe request: %v", err)
		return fmt.Errorf("failed to send subscribe request: %v", err)
	}

	responseCount := 0
	for {
		select {
		case <-c.stopChan:
			debugLog.Printf("Stopping stream subscription after %d responses", responseCount)
			return nil
		default:
			response, err := subscribeClient.Recv()
			if err != nil {
				if err == io.EOF {
					debugLog.Printf("Received EOF, ending stream subscription")
					return nil
				}
				debugLog.Printf("Failed to receive stream response: %v", err)
				return fmt.Errorf("failed to receive stream response: %v", err)
			}
			responseCount++
			debugLog.Printf("Received stream response #%d", responseCount)

			if update := response.GetUpdate(); update != nil {
				if err := c.printNotification(update); err != nil {
					log.Printf("ERROR: Failed to print notification: %v", err)
				}
			}

			if response.GetSyncResponse() {
				log.Printf("INFO: Received sync response")
			}
		}
	}
}

func (c *GNMIClient) withAuthentication(ctx context.Context) context.Context {
	if c.config.Authentication.Username != "" && c.config.Authentication.Password != "" {
		md := metadata.Pairs(
			"username", c.config.Authentication.Username,
			"password", c.config.Authentication.Password,
		)
		return metadata.NewOutgoingContext(ctx, md)
	}
	return ctx
}

func (c *GNMIClient) printNotification(notification *gnmipb.Notification) error {
	timestamp := time.Unix(0, notification.Timestamp)
	debugLog.Printf("Processing notification - timestamp: %d (%s), prefix: %v, %d updates",
		notification.Timestamp, timestamp.Format(time.RFC3339), notification.Prefix, len(notification.Update))

	fmt.Printf("\n=== Notification at %s ===\n", timestamp.Format(time.RFC3339))

	for i, update := range notification.Update {
		pathStr := c.pathToString(update.Path)
		value := c.valueToString(update.Val)
		debugLog.Printf("  Update[%d]: path=%s, value_type=%T, value=%s", i, pathStr, update.Val, value)
		fmt.Printf("Path: %s\n", pathStr)
		fmt.Printf("Value: %s\n", value)
		fmt.Println("---")
	}

	return nil
}

func (c *GNMIClient) pathToString(path *gnmipb.Path) string {
	if path == nil {
		return ""
	}

	var parts []string
	for _, elem := range path.Elem {
		part := elem.Name
		if len(elem.Key) > 0 {
			keys := make([]string, 0, len(elem.Key))
			for k, v := range elem.Key {
				keys = append(keys, fmt.Sprintf("%s=%s", k, v))
			}
			part += "[" + strings.Join(keys, ", ") + "]"
		}
		parts = append(parts, part)
	}
	return "/" + strings.Join(parts, "/")
}

func (c *GNMIClient) valueToString(value *gnmipb.TypedValue) string {
	if value == nil {
		return "<nil>"
	}

	switch v := value.Value.(type) {
	case *gnmipb.TypedValue_StringVal:
		return v.StringVal
	case *gnmipb.TypedValue_IntVal:
		return fmt.Sprintf("%d", v.IntVal)
	case *gnmipb.TypedValue_UintVal:
		return fmt.Sprintf("%d", v.UintVal)
	case *gnmipb.TypedValue_BoolVal:
		return fmt.Sprintf("%v", v.BoolVal)
	case *gnmipb.TypedValue_FloatVal:
		return fmt.Sprintf("%f", v.FloatVal)
	case *gnmipb.TypedValue_DoubleVal:
		return fmt.Sprintf("%f", v.DoubleVal)
	case *gnmipb.TypedValue_BytesVal:
		return string(v.BytesVal)
	case *gnmipb.TypedValue_LeaflistVal:
		return fmt.Sprintf("%v", v.LeaflistVal)
	case *gnmipb.TypedValue_ProtoBytes:
		return fmt.Sprintf("<proto bytes: %d bytes>", len(v.ProtoBytes))
	case *gnmipb.TypedValue_AsciiVal:
		return v.AsciiVal
	case *gnmipb.TypedValue_JsonVal:
		return string(v.JsonVal)
	case *gnmipb.TypedValue_JsonIetfVal:
		return string(v.JsonIetfVal)
	default:
		return fmt.Sprintf("<unknown type: %T>", v)
	}
}
