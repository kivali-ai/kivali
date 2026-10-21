// Package secrets keeps the values an integration's connection holds
// secret: a pasted token, a key. They live outside the data directory,
// so no backup, log or API answer carries them: in a Kubernetes Secret
// per connection when the server runs in a cluster (Kube), in memory
// otherwise (Memory). The store keeps only which fields are set and
// the Secret's name.
package secrets

import (
	"context"
	"errors"
	"maps"
	"strings"
	"sync"

	"github.com/kivali-ai/kivali/internal/kube"
)

// ErrNotFound means there is no secret of that name.
var ErrNotFound = errors.New("secrets: not found")

// Store holds named sets of secret values.
type Store interface {
	// Put replaces the named secret's values with data.
	Put(ctx context.Context, name string, data map[string]string) error
	// Get returns the named secret's values, or ErrNotFound.
	Get(ctx context.Context, name string) (map[string]string, error)
	// Delete removes the named secret; one that does not exist is no
	// error.
	Delete(ctx context.Context, name string) error
}

// connectionPrefix starts the name of every connection's Secret.
const connectionPrefix = "kivali-connection-"

// ConnectionSecretName is the Secret that holds one connection's
// secret fields: kivali-connection-<integration>-<id>. Both parts are
// lowercase letters, digits and hyphens (an id has no hyphen), so the
// name is a valid Kubernetes object name.
func ConnectionSecretName(integration, id string) string {
	return connectionPrefix + integration + "-" + id
}

// connectionIntegration is the integration a connection Secret's name
// names, or "" for a name ConnectionSecretName did not make.
func connectionIntegration(name string) string {
	rest, ok := strings.CutPrefix(name, connectionPrefix)
	if !ok {
		return ""
	}
	i := strings.LastIndexByte(rest, '-')
	if i <= 0 {
		return ""
	}
	return rest[:i]
}

// Memory is a Store in this process's memory, for tests and for a
// server outside a cluster (make run, DEV_MODE): the values are gone
// when it stops.
type Memory struct {
	mu   sync.Mutex
	data map[string]map[string]string
}

// NewMemory returns an empty Memory.
func NewMemory() *Memory { return &Memory{data: map[string]map[string]string{}} }

// Put implements Store.
func (m *Memory) Put(_ context.Context, name string, data map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[name] = maps.Clone(data)
	return nil
}

// Get implements Store.
func (m *Memory) Get(_ context.Context, name string) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.data[name]
	if !ok {
		return nil, ErrNotFound
	}
	return maps.Clone(d), nil
}

// Delete implements Store.
func (m *Memory) Delete(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, name)
	return nil
}

// Names is every secret's name, for tests.
func (m *Memory) Names() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.data))
	for k := range m.data {
		out = append(out, k)
	}
	return out
}

// Kube is a Store of Opaque Secrets in the server's namespace,
// labelled app=kivali and, for a connection's,
// kivali.ai/integration=<name>.
type Kube struct {
	client *kube.Client
}

// NewKube returns a Kube writing through client.
func NewKube(client *kube.Client) *Kube { return &Kube{client: client} }

// IntegrationLabel is the label naming the integration a connection's
// Secret belongs to.
const IntegrationLabel = "kivali.ai/integration"

// Put implements Store.
func (k *Kube) Put(ctx context.Context, name string, data map[string]string) error {
	labels := map[string]string{"app": "kivali"}
	if it := connectionIntegration(name); it != "" {
		labels[IntegrationLabel] = it
	}
	raw := make(map[string][]byte, len(data))
	for key, v := range data {
		raw[key] = []byte(v)
	}
	return k.client.PutSecret(ctx, name, labels, raw)
}

// Get implements Store.
func (k *Kube) Get(ctx context.Context, name string) (map[string]string, error) {
	raw, err := k.client.GetSecret(ctx, name)
	if errors.Is(err, kube.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(raw))
	for key, v := range raw {
		out[key] = string(v)
	}
	return out, nil
}

// Delete implements Store.
func (k *Kube) Delete(ctx context.Context, name string) error {
	return k.client.DeleteSecret(ctx, name)
}
