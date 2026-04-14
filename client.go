package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
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
	Set            *SetConfig           `yaml:"set,omitempty"`
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

// PathStreamMode controls how a single path is reported in a STREAM subscription.
// Valid values: "target_defined" (default), "on_change", "sample".
// Ignored for ONCE and POLL subscription modes.
type PathStreamMode int

const (
	PathStreamTargetDefined PathStreamMode = iota
	PathStreamOnChange
	PathStreamSample
)

type PathConfig struct {
	Path        string `yaml:"path"`
	Description string `yaml:"description"`
	Origin      string `yaml:"origin,omitempty"`
	Target      string `yaml:"target,omitempty"`
	// StreamMode controls how updates are reported for this path in stream mode.
	// Options: "target_defined" (default), "on_change", "sample".
	// When set to "sample", SampleInterval overrides the global stream.sample_interval.
	StreamMode string `yaml:"stream_mode,omitempty"`
	// SampleInterval overrides the global stream.sample_interval for this path.
	// Only meaningful when stream_mode is "sample". Example: "30s", "60s".
	SampleInterval string `yaml:"sample_interval,omitempty"`
}

type OutputConfig struct {
	Format      string `yaml:"format"`
	PrettyPrint bool   `yaml:"pretty_print"`
	LogToFile   bool   `yaml:"log_to_file"`
	LogFile     string `yaml:"log_file"`
}

type SetConfig struct {
	Operation string       `yaml:"operation"`
	Updates   []SetUpdate  `yaml:"updates,omitempty"`
	Deletes   []SetDelete  `yaml:"deletes,omitempty"`
	Replaces  []SetReplace `yaml:"replaces,omitempty"`
}

type SetUpdate struct {
	Path   string `yaml:"path"`
	Value  string `yaml:"value"`
	Origin string `yaml:"origin,omitempty"`
	Target string `yaml:"target,omitempty"`
}

type SetDelete struct {
	Path   string `yaml:"path"`
	Origin string `yaml:"origin,omitempty"`
	Target string `yaml:"target,omitempty"`
}

type SetReplace struct {
	Path   string `yaml:"path"`
	Value  string `yaml:"value"`
	Origin string `yaml:"origin,omitempty"`
	Target string `yaml:"target,omitempty"`
}

type GNMIClient struct {
	config     *GNMIConfig
	conn       *grpc.ClientConn
	client     gnmipb.GNMIClient
	mode       SubscriptionMode
	pollTicker *time.Ticker
	stopChan   chan struct{}
	mu         sync.Mutex

	// verbose controls display style.
	// true  → debug mode: full notification banners (existing behaviour)
	// false → compact mode: one line per changed value, suppress duplicates
	verbose bool

	// lastValues tracks the most-recently displayed value for each full path
	// string so that duplicate values are suppressed in compact mode.
	lastValues map[string]string
}

func NewGNMIClient(config *GNMIConfig, verbose bool) (*GNMIClient, error) {
	client := &GNMIClient{
		config:     config,
		stopChan:   make(chan struct{}),
		verbose:    verbose,
		lastValues: make(map[string]string),
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

// parsePathStreamMode converts a string stream_mode value to the gNMI
// SubscriptionMode enum. Defaults to TARGET_DEFINED for empty/unknown values.
func parsePathStreamMode(s string) gnmipb.SubscriptionMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on_change":
		return gnmipb.SubscriptionMode_ON_CHANGE
	case "sample":
		return gnmipb.SubscriptionMode_SAMPLE
	default:
		// "target_defined", "", or anything unrecognised
		return gnmipb.SubscriptionMode_TARGET_DEFINED
	}
}

func (c *GNMIClient) buildSubscriptionList() (*gnmipb.SubscriptionList, error) {
	// Parse the global sample interval once; used as fallback for per-path overrides.
	globalInterval, err := time.ParseDuration(c.config.Stream.SampleInterval)
	if err != nil {
		globalInterval = 30 * time.Second
	}

	subscriptions := make([]*gnmipb.Subscription, 0, len(c.config.Paths))

	for _, pathConfig := range c.config.Paths {
		path, err := c.parsePath(pathConfig.Path, pathConfig.Origin, pathConfig.Target)
		if err != nil {
			debugLog.Printf("Failed to parse path %s: %v", pathConfig.Path, err)
			return nil, fmt.Errorf("failed to parse path %s: %v", pathConfig.Path, err)
		}

		subscription := &gnmipb.Subscription{
			Path: path,
		}

		if c.mode == Stream {
			streamMode := parsePathStreamMode(pathConfig.StreamMode)
			subscription.Mode = streamMode

			switch streamMode {
			case gnmipb.SubscriptionMode_SAMPLE:
				// Use per-path sample_interval if set, otherwise fall back to global.
				interval := globalInterval
				if pathConfig.SampleInterval != "" {
					if d, err := time.ParseDuration(pathConfig.SampleInterval); err == nil {
						interval = d
					} else {
						debugLog.Printf("Invalid sample_interval %q for path %s, using global %v: %v",
							pathConfig.SampleInterval, pathConfig.Path, globalInterval, err)
					}
				}
				subscription.SampleInterval = uint64(interval.Nanoseconds())
				subscription.SuppressRedundant = c.config.Stream.SuppressRedundant
				debugLog.Printf("Added path: %s  stream_mode=sample  interval=%v  suppress_redundant=%v",
					pathConfig.Path, interval, c.config.Stream.SuppressRedundant)

			case gnmipb.SubscriptionMode_ON_CHANGE:
				// HeartbeatInterval keeps the subscription alive even when no changes occur.
				if c.config.Stream.HeartbeatInterval != "" {
					if d, err := time.ParseDuration(c.config.Stream.HeartbeatInterval); err == nil && d > 0 {
						subscription.HeartbeatInterval = uint64(d.Nanoseconds())
					}
				}
				debugLog.Printf("Added path: %s  stream_mode=on_change  heartbeat=%s",
					pathConfig.Path, c.config.Stream.HeartbeatInterval)

			default: // TARGET_DEFINED
				debugLog.Printf("Added path: %s  stream_mode=target_defined", pathConfig.Path)
			}
		} else {
			debugLog.Printf("Added path: %s (origin: %s, target: %s)", pathConfig.Path, pathConfig.Origin, pathConfig.Target)
		}

		subscriptions = append(subscriptions, subscription)
	}

	listMode := gnmipb.SubscriptionList_STREAM
	switch c.mode {
	case Once:
		listMode = gnmipb.SubscriptionList_ONCE
	case Poll:
		listMode = gnmipb.SubscriptionList_POLL
	case Stream:
		listMode = gnmipb.SubscriptionList_STREAM
	}

	return &gnmipb.SubscriptionList{
		Subscription: subscriptions,
		Mode:         listMode,
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
	debugLog.Printf("Context has metadata: %v", ctx.Value("gRPC-Key"))

	startTime := time.Now()

	// Create a GetRequest directly to see more details
	req := &gnmipb.GetRequest{
		Path:     paths,
		Type:     gnmipb.GetRequest_ALL,
		Encoding: gnmipb.Encoding_JSON,
	}
	debugLog.Printf("GetRequest: type=%v, encoding=%v, path_count=%d", req.Type, req.Encoding, len(req.Path))

	response, err := c.client.Get(ctx, req)
	elapsed := time.Since(startTime)

	if err != nil {
		debugLog.Printf("gNMI Get failed after %v: %v", elapsed, err)

		// Check if it's a context cancellation or deadline
		select {
		case <-ctx.Done():
			debugLog.Printf("Context done: %v", ctx.Err())
		default:
		}

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

	ctx = c.withAuthentication(ctx)

	debugLog.Printf("Opening gNMI Subscribe stream for poll mode")
	subscribeClient, err := c.client.Subscribe(ctx)
	if err != nil {
		debugLog.Printf("gNMI Subscribe failed: %v", err)
		return fmt.Errorf("gNMI Subscribe failed: %v", err)
	}

	// gNMI poll protocol: first send a SubscriptionList with mode=POLL,
	// then send Poll{} messages to trigger each data collection.
	subscriptionList.Mode = gnmipb.SubscriptionList_POLL
	initRequest := &gnmipb.SubscribeRequest{
		Request: &gnmipb.SubscribeRequest_Subscribe{
			Subscribe: subscriptionList,
		},
	}
	debugLog.Printf("Sending initial SubscriptionList for poll mode")
	if err := subscribeClient.Send(initRequest); err != nil {
		debugLog.Printf("Failed to send initial subscription list: %v", err)
		return fmt.Errorf("failed to send initial subscription list: %v", err)
	}

	pollRequest := &gnmipb.SubscribeRequest{
		Request: &gnmipb.SubscribeRequest_Poll{
			Poll: &gnmipb.Poll{},
		},
	}

	c.pollTicker = time.NewTicker(interval)
	defer c.pollTicker.Stop()

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

			// Drain all responses until sync_response
			for {
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

				if response.GetSyncResponse() {
					debugLog.Printf("Poll #%d sync_response received", pollCount+1)
					break
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
		debugLog.Printf("Sending authentication: username=%s, password len=%d", c.config.Authentication.Username, len(c.config.Authentication.Password))
		md := metadata.Pairs(
			"username", c.config.Authentication.Username,
			"password", c.config.Authentication.Password,
		)
		return metadata.NewOutgoingContext(ctx, md)
	}
	debugLog.Printf("No authentication configured")
	return ctx
}

// printNotification is the entry point called from every subscription path.
// It delegates to verbose (debug) or compact display based on c.verbose.
func (c *GNMIClient) printNotification(notification *gnmipb.Notification) error {
	timestamp := time.Unix(0, notification.Timestamp)
	debugLog.Printf("Processing notification - timestamp: %d (%s), prefix: %v, %d updates",
		notification.Timestamp, timestamp.Format(time.RFC3339), notification.Prefix, len(notification.Update))

	if c.verbose {
		c.printVerbose(notification, timestamp)
	} else {
		c.printCompact(notification, timestamp)
	}

	return nil
}

// printVerbose reproduces the original banner-style output used in debug mode.
func (c *GNMIClient) printVerbose(notification *gnmipb.Notification, timestamp time.Time) {
	fmt.Printf("\n=== Notification at %s ===\n", timestamp.Format(time.RFC3339))
	for i, update := range notification.Update {
		pathStr := c.pathToString(update.Path)
		value := c.valueToString(update.Val)
		debugLog.Printf("  Update[%d]: path=%s, value_type=%T, value=%s", i, pathStr, update.Val, value)
		fmt.Printf("Path: %s\n", pathStr)
		fmt.Printf("Value: %s\n", value)
		fmt.Println("---")
	}
}

// printCompact emits one line per update, but only when the value has changed
// since the last time it was displayed.
//
// Output format (tab-separated columns, fixed-width timestamp):
//
//	2026-04-13T17:25:31+05:30  in-octets                961870
//	2026-04-13T17:25:31+05:30  mtu                      9100
//
// The path prefix (from the notification's prefix field) is combined with each
// update's path to form a fully-qualified label. Only the leaf element name is
// shown as the label; the full path is used as the de-duplication key.
func (c *GNMIClient) printCompact(notification *gnmipb.Notification, timestamp time.Time) {
	// Build the prefix string once for all updates in this notification.
	prefixStr := c.pathToString(notification.Prefix)

	ts := timestamp.Format(time.RFC3339)

	for _, update := range notification.Update {
		updatePathStr := c.pathToString(update.Path)
		value := unwrapJSON(c.valueToString(update.Val))

		// Full path = prefix path + update path (de-duplication key).
		fullPath := prefixStr + updatePathStr

		// Suppress if value is identical to last displayed value for this path.
		if prev, seen := c.lastValues[fullPath]; seen && prev == value {
			debugLog.Printf("  Suppressed unchanged value for %s = %s", fullPath, value)
			continue
		}
		c.lastValues[fullPath] = value

		// Label: leaf element name (last path element), falling back to full path.
		label := leafLabel(update.Path, updatePathStr)

		fmt.Printf("%-35s  %-40s  %s\n", ts, label, value)
	}
}

// leafLabel extracts a human-readable label for a path.
// It uses the last PathElem name if available, otherwise the raw path string.
func leafLabel(path *gnmipb.Path, fallback string) string {
	if path != nil && len(path.Elem) > 0 {
		last := path.Elem[len(path.Elem)-1]
		label := last.Name
		// Append key values when the last element is a list node (e.g. interface[name=Eth12])
		if len(last.Key) > 0 {
			for k, v := range last.Key {
				label += "[" + k + "=" + v + "]"
			}
		}
		return label
	}
	if fallback != "" {
		return fallback
	}
	return "<unknown>"
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

// unwrapJSON takes a raw JSON string returned by valueToString and, if it is a
// single-key object (e.g. {"openconfig-interfaces:in-octets":"1073758"}),
// returns just the bare scalar value ("1073758").  Multi-key objects and
// non-object values are returned unchanged.
// This is used in compact (non-debug) mode so the output shows clean values
// instead of wrapped JSON objects.
func unwrapJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return raw
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil || len(m) != 1 {
		return raw
	}
	for _, v := range m {
		switch val := v.(type) {
		case string:
			return val
		case float64:
			// Avoid scientific notation for large integers.
			if val == float64(int64(val)) {
				return fmt.Sprintf("%d", int64(val))
			}
			return fmt.Sprintf("%g", val)
		case bool:
			return fmt.Sprintf("%v", val)
		default:
			// For nested objects/arrays, re-marshal compactly.
			b, err := json.Marshal(v)
			if err != nil {
				return raw
			}
			return string(b)
		}
	}
	return raw
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

func (c *GNMIClient) Set(ctx context.Context) error {
	if c.config.Set == nil {
		return fmt.Errorf("no set configuration provided")
	}

	ctx = c.withAuthentication(ctx)

	req := &gnmipb.SetRequest{}

	for _, update := range c.config.Set.Updates {
		path, val, err := c.buildPathAndValue(update.Path, update.Origin, update.Target, update.Value)
		if err != nil {
			return fmt.Errorf("failed to build update for path %s: %v", update.Path, err)
		}
		req.Update = append(req.Update, &gnmipb.Update{Path: path, Val: val})
		debugLog.Printf("Added update: path=%s, value=%s", update.Path, update.Value)
	}

	for _, replace := range c.config.Set.Replaces {
		path, val, err := c.buildPathAndValue(replace.Path, replace.Origin, replace.Target, replace.Value)
		if err != nil {
			return fmt.Errorf("failed to build replace for path %s: %v", replace.Path, err)
		}
		req.Replace = append(req.Replace, &gnmipb.Update{Path: path, Val: val})
		debugLog.Printf("Added replace: path=%s, value=%s", replace.Path, replace.Value)
	}

	for _, del := range c.config.Set.Deletes {
		path, err := c.parsePath(del.Path, del.Origin, del.Target)
		if err != nil {
			return fmt.Errorf("failed to parse delete path %s: %v", del.Path, err)
		}
		req.Delete = append(req.Delete, path)
		debugLog.Printf("Added delete: path=%s", del.Path)
	}

	if len(req.Update) == 0 && len(req.Replace) == 0 && len(req.Delete) == 0 {
		return fmt.Errorf("no updates, replaces, or deletes specified in set configuration")
	}

	debugLog.Printf("Sending SetRequest with %d updates, %d replaces, %d deletes",
		len(req.Update), len(req.Replace), len(req.Delete))

	response, err := c.client.Set(ctx, req)
	if err != nil {
		debugLog.Printf("SetRequest failed: %v", err)
		return fmt.Errorf("SetRequest failed: %v", err)
	}

	debugLog.Printf("SetRequest succeeded")
	c.printSetResponse(response)

	return nil
}

// buildPathAndValue splits the path into a parent path + leaf name and wraps
// the scalar value in a {"<leaf>": <value>} JSON envelope — the format expected
// by the SONiC translib request_binder for OpenConfig leaf nodes.
// If the value string is already a JSON object/array it is used as-is (no wrapping).
func (c *GNMIClient) buildPathAndValue(pathStr, origin, target, valueStr string) (*gnmipb.Path, *gnmipb.TypedValue, error) {
	// If the value is already a JSON object/array, use the full path unchanged.
	trimmed := strings.TrimSpace(valueStr)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		path, err := c.parsePath(pathStr, origin, target)
		if err != nil {
			return nil, nil, err
		}
		val := &gnmipb.TypedValue{
			Value: &gnmipb.TypedValue_JsonIetfVal{JsonIetfVal: []byte(trimmed)},
		}
		return path, val, nil
	}

	// Split path into parent + leaf.
	// e.g. "/interfaces/interface[name=Eth12]/config/mtu"
	//   -> parent = "/interfaces/interface[name=Eth12]/config"
	//   -> leaf   = "mtu"
	slashIdx := strings.LastIndex(pathStr, "/")
	if slashIdx <= 0 {
		// No parent — send full path with bare JSON value.
		path, err := c.parsePath(pathStr, origin, target)
		if err != nil {
			return nil, nil, err
		}
		jsonVal, err := c.scalarToJSON(valueStr)
		if err != nil {
			return nil, nil, err
		}
		val := &gnmipb.TypedValue{
			Value: &gnmipb.TypedValue_JsonIetfVal{JsonIetfVal: []byte(jsonVal)},
		}
		return path, val, nil
	}

	parentPath := pathStr[:slashIdx]
	leaf := pathStr[slashIdx+1:]

	// Strip any YANG module prefix from the leaf name (e.g. "oc-intf:mtu" -> "mtu")
	if colonIdx := strings.LastIndex(leaf, ":"); colonIdx >= 0 {
		leaf = leaf[colonIdx+1:]
	}

	path, err := c.parsePath(parentPath, origin, target)
	if err != nil {
		return nil, nil, err
	}

	// Build JSON envelope: {"<leaf>": <value>}
	jsonVal, err := c.scalarToJSON(valueStr)
	if err != nil {
		return nil, nil, err
	}
	jsonBody := fmt.Sprintf(`{"%s": %s}`, leaf, jsonVal)
	debugLog.Printf("buildPathAndValue: parent=%s, leaf=%s, body=%s", parentPath, leaf, jsonBody)

	val := &gnmipb.TypedValue{
		Value: &gnmipb.TypedValue_JsonIetfVal{JsonIetfVal: []byte(jsonBody)},
	}
	return path, val, nil
}

// scalarToJSON converts a plain string value to its JSON representation.
// Numbers and booleans are returned as-is; strings are JSON-quoted.
func (c *GNMIClient) scalarToJSON(valueStr string) (string, error) {
	lower := strings.ToLower(valueStr)
	if lower == "true" || lower == "false" {
		return lower, nil
	}
	// Try integer
	if _, err := strconv.ParseInt(valueStr, 10, 64); err == nil {
		return valueStr, nil
	}
	if _, err := strconv.ParseUint(valueStr, 10, 64); err == nil {
		return valueStr, nil
	}
	// Try float
	if _, err := strconv.ParseFloat(valueStr, 64); err == nil {
		return valueStr, nil
	}
	// String — JSON-quote it
	b, err := json.Marshal(valueStr)
	if err != nil {
		return "", fmt.Errorf("failed to marshal string: %v", err)
	}
	return string(b), nil
}

func (c *GNMIClient) printSetResponse(response *gnmipb.SetResponse) {
	fmt.Println("=== Set Response ===")
	if response.Prefix != nil {
		fmt.Printf("Prefix: %s\n", c.pathToString(response.Prefix))
	}
	if len(response.Response) > 0 {
		fmt.Printf("Responses: %d\n", len(response.Response))
		for i, resp := range response.Response {
			fmt.Printf("  Response[%d]: path=%s\n", i, c.pathToString(resp.Path))
		}
	}
	if response.Timestamp != 0 {
		timestamp := time.Unix(0, response.Timestamp)
		fmt.Printf("Timestamp: %s\n", timestamp.Format(time.RFC3339))
	}
	fmt.Println("====================")
}
