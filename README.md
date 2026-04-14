# gNMI Client for SONiC

A Go-based gNMI client implementation for SONiC switches with support for TLS, multiple subscription modes, and configurable paths based on YANG models.

## Features

- **TLS Support**: Full TLS configuration with CA certificates, client certificates, and server name verification
- **Multiple Subscription Modes**:
  - `once`: Single GET request to retrieve current state
  - `poll`: Periodic polling at configurable intervals
  - `stream`: Continuous streaming subscription with sample intervals
- **Set Operations**: Configure SONiC switch settings via gNMI SetRequest
  - `update`: Set or modify configuration values
  - `replace`: Replace existing configuration values
  - `delete`: Remove configuration paths
- **YANG Model Based**: Pre-configured paths based on SONiC YANG models from `src/sonic-mgmt-common/models/yang`
- **Configurable**: YAML-based configuration file for easy customization
- **Counter Support**: Sample counter paths for interface statistics, ACL counters, etc.

## Building

The client is designed to build independently of the root tree:

```bash
cd src/gnmi_client
go mod tidy
go build -o gnmi_client .
```

## Usage

### Basic Usage

```bash
./gnmi_client -config config.yaml
```

### Command Line Options

```bash
./gnmi_client -h
```

Available options:
- `-config`: Path to configuration file (default: `config.yaml`)
- `-mode`: Subscription mode override (`once`, `poll`, `stream`)
- `-target`: Target address override (e.g., `localhost:50051`)
- `-timeout`: Connection timeout (default: `30s`)
- `-d`: Enable debug logging (verbose output with timestamps)
- `-set`: Execute set operation instead of subscription

### Examples

#### Once Mode (Single GET)
```bash
./gnmi_client -config config.yaml -mode once
```

#### Poll Mode (Periodic)
```bash
./gnmi_client -config config.yaml -mode poll
```

#### Stream Mode (Continuous)
```bash
./gnmi_client -config config.yaml -mode stream
```

#### Connect to Remote Target
```bash
./gnmi_client -config config.yaml -target 192.168.1.1:50051
```

#### Enable Debug Logging
```bash
./gnmi_client -config config.yaml -d -mode stream
```

#### Debug with Insecure TLS (Testing)
```bash
# Option 1: Use test config without TLS
./gnmi_client -config config-test.yaml -d -mode once

# Option 2: Edit config.yaml to set insecure_skip_verify: true
./gnmi_client -config config.yaml -d -mode once

# Option 3: Disable TLS entirely
./gnmi_client -config config.yaml -d -mode once
# (Set tls.enabled: false in config.yaml)
```

#### Set Operations (Configuration Changes)
```bash
# Run a Set operation using set_config.yaml
./gnmi_client -config set_config.yaml -d -set
```

#### Running on SONiC Switch
```bash
# Copy client to switch
scp gnmi_client admin@sonic-switch:/home/admin/

# Copy certificates if needed (for production)
scp -r /etc/sonic/telemetry/ admin@sonic-switch:/tmp/telemetry/

# Run on switch with proper certificates
./gnmi_client -config config.yaml -d -mode once
```

#### Query Specific Path with Origin
```bash
# Edit config.yaml to add paths with origin and target
./gnmi_client -config config.yaml -d -mode once
```

## Configuration

The configuration file (`config.yaml`) is organized into sections:

### Connection Settings
```yaml
connection:
  target: "localhost:50051"
  timeout: 30s
```

### TLS Configuration
```yaml
tls:
  enabled: true
  ca_certificate: "/etc/sonic/telemetry/ca.crt"
  client_certificate: "/etc/sonic/telemetry/client.crt"
  client_key: "/etc/sonic/telemetry/client.key"
  server_name: "sonic-switch"
  insecure_skip_verify: false
```

**Note:** Certificate files must exist on the system where the client runs. On SONiC switches, certificates are typically at `/etc/sonic/telemetry/`. For local testing without certificates, use `config-test.yaml` or set `enabled: false`.

### Subscription Settings
```yaml
subscription:
  mode: "stream"  # Options: once, poll, stream
  poll_interval: 10s

stream:
  sample_interval: 10s  # Minimum 1s
  suppress_redundant: false
  heartbeat_interval: 0s
```

### Paths

Paths are based on YANG models from `src/sonic-mgmt-common/models/yang`:

#### Path with Explicit Origin
```yaml
paths:
  - path: "/interfaces/interface[name=Ethernet0]/state/counters/in-octets"
    origin: "openconfig"
    description: "Input octets counter with explicit origin"
```

#### Path with Origin and Target
```yaml
paths:
  - path: "/components/component[name=Chassis]/state"
    origin: "openconfig"
    target: "PLATFORM"
    description: "Chassis state with origin and target"
```

#### SONiC Native Path
```yaml
paths:
  - path: "/acl/ACL_TABLE/ACL_TABLE_LIST[name=ACL_INGRESS]"
    origin: "sonic"
    target: "CONFIG_DB"
    description: "SONiC ACL table from CONFIG_DB"
```

#### Sample Counters (OpenConfig Interfaces)
```yaml
paths:
  - path: "/interfaces/interface[name=Ethernet0]/state/counters/in-octets"
    origin: "openconfig"
    description: "Input octets counter for Ethernet0"
  - path: "/interfaces/interface[name=Ethernet0]/state/counters/in-pkts"
    origin: "openconfig"
    description: "Input packets counter for Ethernet0"
  - path: "/interfaces/interface[name=Ethernet0]/state/counters/in-errors"
    origin: "openconfig"
    description: "Input errors counter for Ethernet0"
```

#### OpenConfig Platform
```yaml
paths:
  - path: "/openconfig-platform:components/component[name=Chassis]/state"
    description: "Chassis component state"
  - path: "/openconfig-platform:components/component[name=Fan]/state"
    description: "Fan component state"
```

#### OpenConfig System
```yaml
paths:
  - path: "/openconfig-system:system/state/hostname"
    description: "System hostname"
  - path: "/openconfig-system:system/state/current-datetime"
    description: "System current datetime"
```

#### OpenConfig ACL (Sample Counters)
```yaml
paths:
  - path: "/openconfig-acl:acl/acl-sets/acl-set[name=ACL_INGRESS,type=IP]/acl-entries/acl-entry[sequence=1]/state/matched-pkts"
    description: "ACL matched packets counter"
```

#### OpenConfig LLDP
```yaml
paths:
  - path: "/openconfig-lldp:lldp/interfaces/interface[name=Ethernet0]/neighbors/neighbor/state/system-name"
    description: "LLDP neighbor system name"
```

#### OpenConfig Sampling (sFlow)
```yaml
paths:
  - path: "/openconfig-sampling-sflow:sampling/sflow/interfaces/interface[name=Ethernet0]/state/sampling-rate"
    description: "sFlow sampling rate"
```

## YANG Models Reference

The client includes paths based on the following YANG models from SONiC:

### OpenConfig Models
- `openconfig-interfaces.yang` - Interface configuration and state
- `openconfig-if-ethernet.yang` - Ethernet interface specifics
- `openconfig-if-ip.yang` - IP configuration on interfaces
- `openconfig-platform.yang` - Platform components (chassis, linecards, fans, PSUs)
- `openconfig-system.yang` - System-level configuration and state
- `openconfig-acl.yang` - Access control lists and counters
- `openconfig-lldp.yang` - LLDP neighbor information
- `openconfig-sampling-sflow.yang` - sFlow sampling configuration

### SONiC Models
- `sonic-acl.yang` - SONiC ACL configuration
- `sonic-interface.yang` - SONiC interface configuration
- `sonic-port.yang` - SONiC port configuration

## Output Format

The client outputs notifications in a human-readable format:

```
=== Notification at 2024-01-15T10:30:45Z ===
Path: /openconfig-interfaces:interfaces/interface[name=Ethernet0]/state/counters/in-octets
Value: 1234567890
---
Path: /openconfig-interfaces:interfaces/interface[name=Ethernet0]/state/counters/in-pkts
Value: 987654
---
```

## Authentication

The client supports basic authentication via username/password:

```yaml
authentication:
  username: "admin"
  password: "password"
```

Credentials are sent via gRPC metadata headers.

## Set Operations

The client supports gNMI Set operations for configuring SONiC switches. Use the `-set` flag
with a config file that has a `set:` section.

```bash
./gnmi_client -config set_config.yaml -d -set
```

### SONiC Translib Path/Value Rules (Critical)

The SONiC gNMI server uses `translib` / `request_binder` which has specific requirements:

1. **Path must end at the parent container**, not the leaf:
   - Correct: `/interfaces/interface[name=Ethernet12]/config`
   - Wrong: `/interfaces/interface[name=Ethernet12]/config/mtu`

2. **Value must be a JSON object** that wraps the container name:
   - Correct: `'{"config": {"mtu": 9100}}'`
   - Wrong: `'{"mtu": 9100}'` → error "unexpected field mtu in interface container"
   - Wrong: `9100` → error "got type float64, expect map[string]interface{}"

3. **Value encoding**: the client always uses `JsonIetfVal` (IETF JSON), which is what SONiC translib requires.

**Why the extra wrapping?** The translib `request_binder` strips the last path element and treats the
value as the body of that element. So if the path ends at `config`, the JSON body must contain
`{"config": {...}}` for the binding to succeed.

### Set Configuration File

The `set_config.yaml` file shows a working example. The general structure:

```yaml
set:
  updates:
    - path: "/interfaces/interface[name=Ethernet12]/config"
      value: '{"config": {"mtu": 9100}}'
      origin: "openconfig"
      description: "Set MTU to 9100 for Ethernet12"

  replaces: []

  deletes:
    # Path only, no value:
    # - path: "/interfaces/interface[name=Ethernet12]/config"
    #   origin: "openconfig"
```

### Scalar value shorthand

As an alternative to writing raw JSON, you can point the path directly at the leaf and supply
a plain scalar value. The client will automatically split off the leaf, build the correct JSON
envelope, and send the path to the parent container:

```yaml
set:
  updates:
    - path: "/interfaces/interface[name=Ethernet12]/config/mtu"
      value: "9100"         # plain integer — client wraps to {"mtu": 9100}
      origin: "openconfig"
```

This produces exactly the same wire format as the explicit JSON form above.

### Working Examples

#### Set MTU
```yaml
set:
  updates:
    - path: "/interfaces/interface[name=Ethernet12]/config"
      value: '{"config": {"mtu": 9100}}'
      origin: "openconfig"
```

#### Enable/disable an interface
```yaml
set:
  updates:
    - path: "/interfaces/interface[name=Ethernet12]/config"
      value: '{"config": {"enabled": true}}'
      origin: "openconfig"
```

#### Set interface description
```yaml
set:
  updates:
    - path: "/interfaces/interface[name=Ethernet12]/config"
      value: '{"config": {"description": "Uplink to server"}}'
      origin: "openconfig"
```

### Switch-side prerequisites

For Set operations to work on a SONiC switch the following must be configured on the switch:

1. **Enable translib write** — the telemetry daemon must be started with `-gnmi_translib_write`:
   ```bash
   docker exec gnmi sonic-db-cli CONFIG_DB hset 'GNMI|gnmi' 'gnmi_translib_write' 'true'
   # Then restart the telemetry process:
   docker exec gnmi supervisorctl restart gnmi-native
   ```

2. **Client certificate role** — the role for the client cert CN in CONFIG_DB must be `gnmi_readwrite`:
   ```bash
   docker exec gnmi sonic-db-cli CONFIG_DB hset 'GNMI_CLIENT_CERT|admin' 'role@' 'gnmi_readwrite'
   ```
   The role must start with the gNMI service name (`gnmi`) followed by `_readwrite`; `readwrite`
   alone is not sufficient (checked in `clientCertAuth.go`).

## Error Handling

The client handles common error scenarios:
- Connection failures with timeout
- TLS certificate validation errors
- Invalid path errors
- Stream interruption

All errors are logged to stderr with descriptive messages.

## Dependencies

- `github.com/openconfig/gnmi` - gNMI protobuf definitions
- `google.golang.org/grpc` - gRPC implementation
- `google.golang.org/grpc/security/advancedtls` - Advanced TLS support
- `gopkg.in/yaml.v3` - YAML parsing
- `golang.org/x/crypto` - Cryptographic utilities

## License

MIT
