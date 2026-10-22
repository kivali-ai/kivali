// Package integrationpod runs an integration: one pod per approved,
// switched-on integration, which core provisions (Provisioner), and
// the shim inside it (Shim), which starts the integration's server and
// carries JSON-RPC between that server and core.
//
// The pod dials core over HTTP on a Unix socket, integration.sock in
// the UDSSubdir subdirectory of the UDS directory: the only part of
// that directory the pod mounts, read-only, so it never reaches the
// agent routes on core.sock or the browser pods' socket. Two routes,
// both carrying TokenHeader, the per-pod token core put in the pod's
// environment:
//
//   - GET /v1/integration/{name}/events: one long-lived SSE stream of
//     core's messages (EventStart, EventRPC, EventRestart,
//     EventShutdown; comment lines as heartbeats). The handshake also
//     carries VersionHeader, the version the pod runs, which core
//     refuses unless it is the approved one, and ProxyHashHeader, the
//     hash of the egress proxy secret the shim drew.
//   - POST /v1/integration/{name}/rpc: one JSON-RPC message from the
//     pod: a response to a request core sent, or a request or
//     notification of the pod's (MethodCredential, MethodLog). Core
//     answers a request with its JSON-RPC response and anything else
//     with 204.
//
// docs/developers/integrations.md is the reference.
package integrationpod

import (
	"encoding/json"
	"strings"
)

const (
	// AppLabel is the app label on every integration pod; List finds
	// them by it and the chart's network policy selects it.
	AppLabel = "kivali-integrationpod"

	// UDSSubdir is the subdirectory of the UDS directory that holds
	// SocketName, core's socket for integration pods.
	UDSSubdir = "integration"
	// SocketName serves the integration routes and nothing else.
	SocketName = "integration.sock"

	// The pod's environment, which configures the shim.
	EnvName    = "KIVALI_INTEGRATION_NAME"
	EnvVersion = "KIVALI_INTEGRATION_VERSION"
	EnvToken   = "KIVALI_INTEGRATION_TOKEN"
	EnvCoreUDS = "KIVALI_INTEGRATION_CORE_UDS"
	EnvBundle  = "KIVALI_INTEGRATION_BUNDLE"
	// EnvCommand, when set, replaces the manifest's mcp_config.command
	// with a path. A test hook: the pod spec never sets it.
	EnvCommand = "KIVALI_INTEGRATION_COMMAND"

	// DefaultBundle is where the pod mounts the version's bundle.
	DefaultBundle = "/bundle"

	// TokenHeader carries the per-pod token on every request.
	TokenHeader = "X-Kivali-Integration-Token"
	// VersionHeader names the version the pod runs, on the events
	// handshake.
	VersionHeader = "X-Kivali-Integration-Version"
	// ProxyHashHeader carries egresscred.Hash of the proxy secret the
	// shim drew, on the events handshake; absent without a proxy.
	ProxyHashHeader = "X-Kivali-Integration-Proxy-Hash"
)

// podPrefix starts every integration pod's name.
const podPrefix = "integrationpod-"

// PodName is the integration's pod: integrationpod-<name>. An
// integration name is at most 32 lowercase letters, digits and
// hyphens (integration.ValidName), so the result is a valid pod name.
func PodName(name string) string { return podPrefix + name }

// nameFromPod is the integration a pod name names, or "".
func nameFromPod(pod string) string {
	name, ok := strings.CutPrefix(pod, podPrefix)
	if !ok {
		return ""
	}
	return name
}

// ProxyUser is the user the integration's server presents to the
// egress proxy, beside its secret: integration-<name>. The proxy's
// agents file lists it under scoped.
func ProxyUser(name string) string { return "integration-" + name }

// EventType is the kind of one message on the events stream.
type EventType string

const (
	// EventStart: fetch the connection's values and start the server.
	// Core sends it on every connect; a shim whose server is running
	// starts it again only if the values have changed.
	EventStart EventType = "start"
	// EventRPC: Data is one JSON-RPC request or notification for the
	// server.
	EventRPC EventType = "rpc"
	// EventRestart: the connection changed; stop the server, fetch the
	// values again and start it.
	EventRestart EventType = "restart"
	// EventShutdown: stop the server and exit.
	EventShutdown EventType = "shutdown"
)

// Event is one SSE frame: its event type and its one-line JSON data.
type Event struct {
	Type EventType
	Data []byte
}

// JSON-RPC methods the pod sends core.
const (
	// MethodCredential asks for the connection's values. The result
	// is a Credential; with no connection the error is
	// CodeNoConnection.
	MethodCredential = "kivali/credential"
	// MethodLog is one line for the integration's log: LogParams.
	MethodLog = "kivali/log"
)

// JSON-RPC error codes.
const (
	// CodeNoConnection: the integration has no connection.
	CodeNoConnection = -32001
	// CodeMethodNotFound is JSON-RPC's own.
	CodeMethodNotFound = -32601
	// CodeInvalidRequest is JSON-RPC's own.
	CodeInvalidRequest = -32600
)

// ProtocolVersion is the MCP protocol version the shim's initialize
// names.
const ProtocolVersion = "2025-06-18"

// Credential is MethodCredential's result.
type Credential struct {
	// Env is the manifest's mcp_config.env with the connection's
	// values put in (integration.ServerEnv).
	Env map[string]string `json:"env"`
	// Fields is every connection field's value by user_config key.
	Fields map[string]string `json:"fields"`
}

// LogParams is MethodLog's params.
type LogParams struct {
	// Level is info, warning, error or debug (detail, such as apt-get's
	// output while the integration's software installs), or stderr for a
	// line the server wrote to its error output.
	Level   string `json:"level"`
	Message string `json:"message"`
}

// Message is one JSON-RPC 2.0 message: a request (Method and ID), a
// notification (Method, no ID) or a response (ID and Result or Error).
type Message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

// IsRequest reports whether m is a request: a method and an id.
func (m *Message) IsRequest() bool { return m.Method != "" && len(m.ID) > 0 }

// IsNotification reports whether m is a notification: a method and no
// id.
func (m *Message) IsNotification() bool { return m.Method != "" && len(m.ID) == 0 }

// IsResponse reports whether m is a response: an id and no method.
func (m *Message) IsResponse() bool { return m.Method == "" && len(m.ID) > 0 }

// Response is the response to the request with id: result, or err
// when it is not nil.
func Response(id json.RawMessage, result any, err *RPCError) ([]byte, error) {
	out := Message{JSONRPC: "2.0", ID: id}
	if err != nil {
		out.Error = err
	} else {
		b, merr := json.Marshal(result)
		if merr != nil {
			return nil, merr
		}
		out.Result = b
	}
	return json.Marshal(out)
}
