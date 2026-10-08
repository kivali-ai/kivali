package agentpod

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// kubeClient is a minimal HTTPS client for the in-cluster Kubernetes
// API. It exposes only the verbs the agent-pod provisioner needs —
// create / get / delete on Pods + PVCs in one namespace. No Services
// (agent pods don't run a listener) and no Endpoints.
//
// Credentials come from the Kivali web pod's ServiceAccount mount
// (we run inside the same pod that owns the kube credentials):
//
//   - /var/run/secrets/kubernetes.io/serviceaccount/token   (bearer)
//   - /var/run/secrets/kubernetes.io/serviceaccount/ca.crt  (tls)
//   - KUBERNETES_SERVICE_HOST / KUBERNETES_SERVICE_PORT     (server)
//
// Out-of-cluster use isn't supported — the provisioner is a no-op
// there.
type kubeClient struct {
	server    string
	token     string
	http      *http.Client
	namespace string
}

// newKubeClient reads the in-cluster credentials. Returns an error if
// any mount is missing; callers use that to disable provisioning
// features and fall through to a no-op Manager.
func newKubeClient(namespace string) (*kubeClient, error) {
	const (
		tokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
		caPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	)
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("not running in-cluster: KUBERNETES_SERVICE_HOST/PORT unset")
	}
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, fmt.Errorf("read token: %w", err)
	}
	caBytes, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBytes) {
		return nil, errors.New("invalid CA bundle")
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
	return &kubeClient{
		server:    fmt.Sprintf("https://%s:%s", host, port),
		token:     strings.TrimSpace(string(tokenBytes)),
		http:      &http.Client{Transport: tr, Timeout: 30 * time.Second},
		namespace: namespace,
	}, nil
}

// do issues an HTTP request to the API server with the bearer token
// set. Caller decodes the body (or a *statusError on non-2xx).
func (k *kubeClient) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.server+path, buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", "Bearer "+k.token)
	req.Header.Set("accept", "application/json")
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	return k.http.Do(req)
}

// ErrNotFound means the resource doesn't exist. Other k8s error
// details surface as plain errors with the API server's message.
var ErrNotFound = errors.New("agentpod kube: not found")

// statusError is the decoded form of a non-2xx response body.
type statusError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Reason  string `json:"reason"`
	Code    int    `json:"code"`
}

func (k *kubeClient) checkErr(resp *http.Response) error {
	if resp.StatusCode/100 == 2 {
		return nil
	}
	b, _ := io.ReadAll(resp.Body)
	var se statusError
	_ = json.Unmarshal(b, &se)
	if se.Reason == "NotFound" || resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	msg := se.Message
	if msg == "" {
		msg = strings.TrimSpace(string(b))
	}
	return fmt.Errorf("agentpod kube: %s: %s", resp.Status, msg)
}

// ensureResource creates a resource if absent. Existing resources are
// tolerated (idempotent provisioning). Caller supplies both the GET
// path (to probe) and the POST path (to create) + the object body.
func (k *kubeClient) ensureResource(ctx context.Context, getPath, postPath string, body any) error {
	resp, err := k.do(ctx, http.MethodGet, getPath, nil)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	resp, err = k.do(ctx, http.MethodPost, postPath, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusConflict {
		// Raced with another creator; treat as success.
		return nil
	}
	return k.checkErr(resp)
}

// deleteResource issues a DELETE; NotFound is silently tolerated.
func (k *kubeClient) deleteResource(ctx context.Context, path string) error {
	resp, err := k.do(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return k.checkErr(resp)
}

// getPodSpecHash returns the value of the spec-hash label on the
// named Pod, or ErrNotFound if no such Pod exists. An empty string
// (no error) means the Pod exists but carries no label. Callers treat
// "" as drift to force a recreate.
func (k *kubeClient) getPodSpecHash(ctx context.Context, name string) (string, error) {
	resp, err := k.do(ctx, http.MethodGet, k.podPath(name), nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return "", ErrNotFound
	}
	if err := k.checkErr(resp); err != nil {
		return "", err
	}
	var pod struct {
		Metadata struct {
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pod); err != nil {
		return "", err
	}
	return pod.Metadata.Labels[specHashLabel], nil
}

// waitForPodGone polls until a Pod with the given name returns 404,
// or the deadline elapses. Used between a delete + recreate so the
// recreate doesn't race the still-Terminating pod and fail with
// AlreadyExists. Polls every 500ms.
func (k *kubeClient) waitForPodGone(ctx context.Context, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		resp, err := k.do(ctx, http.MethodGet, k.podPath(name), nil)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("agentpod kube: pod %q still present after %s", name, timeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// getPodReady polls the Pod and returns true once it is in the
// Running phase with all container statuses Ready.
func (k *kubeClient) getPodReady(ctx context.Context, name string) (bool, error) {
	resp, err := k.do(ctx, http.MethodGet, k.podPath(name), nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return false, ErrNotFound
	}
	if err := k.checkErr(resp); err != nil {
		return false, err
	}
	var pod struct {
		Status struct {
			Phase             string `json:"phase"`
			ContainerStatuses []struct {
				Ready bool `json:"ready"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pod); err != nil {
		return false, err
	}
	if pod.Status.Phase != "Running" {
		return false, nil
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if !cs.Ready {
			return false, nil
		}
	}
	return len(pod.Status.ContainerStatuses) > 0, nil
}

// API path helpers — one namespace, two kinds (Pod + PVC). Services
// are intentionally absent: agent pods own no listener, so RBAC needs
// no Service grants.
func (k *kubeClient) podsPath() string {
	return fmt.Sprintf("/api/v1/namespaces/%s/pods", k.namespace)
}
func (k *kubeClient) podPath(name string) string {
	return fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", k.namespace, name)
}
func (k *kubeClient) pvcsPath() string {
	return fmt.Sprintf("/api/v1/namespaces/%s/persistentvolumeclaims", k.namespace)
}
func (k *kubeClient) pvcPath(name string) string {
	return fmt.Sprintf("/api/v1/namespaces/%s/persistentvolumeclaims/%s", k.namespace, name)
}
