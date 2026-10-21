// Package kube is the minimal in-cluster Kubernetes client the pod
// provisioners share (internal/agentpod, internal/browserpod): create,
// get, list and delete on Pods and PVCs in one namespace, plus the
// spec-hash label a provisioner stamps on each pod it creates so it can
// tell a pod built from an older manifest. It also gets, writes and
// deletes the Secrets that hold integration connections
// (internal/secrets).
package kube

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Client is a minimal HTTPS client for the in-cluster Kubernetes API.
// It exposes only the verbs Kivali needs: create / get / list / delete
// on Pods and PVCs in one namespace, and get / create / update /
// delete on Secrets. No Services (neither agent pods nor browser pods
// run a listener) and no Endpoints.
//
// Credentials come from the Kivali web pod's ServiceAccount mount
// (the client runs inside the pod that owns the kube credentials):
//
//   - /var/run/secrets/kubernetes.io/serviceaccount/token   (bearer)
//   - /var/run/secrets/kubernetes.io/serviceaccount/ca.crt  (tls)
//   - KUBERNETES_SERVICE_HOST / KUBERNETES_SERVICE_PORT     (server)
//
// Out-of-cluster use isn't supported: New fails there and the
// provisioners run as no-ops.
type Client struct {
	server    string
	token     string
	http      *http.Client
	namespace string
}

// New reads the in-cluster credentials. Returns an error if any mount
// is missing; callers use that to disable provisioning and run as a
// no-op.
func New(namespace string) (*Client, error) {
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
	return &Client{
		server:    fmt.Sprintf("https://%s:%s", host, port),
		token:     strings.TrimSpace(string(tokenBytes)),
		http:      &http.Client{Transport: tr, Timeout: 30 * time.Second},
		namespace: namespace,
	}, nil
}

// NewForServer is a Client for the API server at server (a base URL)
// reached through httpClient, with no bearer token. Tests point it at
// an httptest server standing in for the API.
func NewForServer(server string, httpClient *http.Client, namespace string) *Client {
	return &Client{server: server, http: httpClient, namespace: namespace}
}

// do issues an HTTP request to the API server with the bearer token
// set. Caller decodes the body (or checkErr's error on non-2xx).
func (k *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
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
	if k.token != "" {
		req.Header.Set("authorization", "Bearer "+k.token)
	}
	req.Header.Set("accept", "application/json")
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	return k.http.Do(req)
}

// ErrNotFound means the resource doesn't exist. Other k8s error
// details surface as plain errors with the API server's message.
var ErrNotFound = errors.New("kube: not found")

// statusError is the decoded form of a non-2xx response body.
type statusError struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Reason  string `json:"reason"`
	Code    int    `json:"code"`
}

func (k *Client) checkErr(resp *http.Response) error {
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
	return fmt.Errorf("kube: %s: %s", resp.Status, msg)
}

// EnsureResource creates a resource if absent. Existing resources are
// tolerated (idempotent provisioning). Caller supplies both the GET
// path (to probe) and the POST path (to create) + the object body.
func (k *Client) EnsureResource(ctx context.Context, getPath, postPath string, body any) error {
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

// DeleteResource issues a DELETE; NotFound is silently tolerated.
func (k *Client) DeleteResource(ctx context.Context, path string) error {
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

// Pod is the part of a Pod object the provisioners read.
type Pod struct {
	Name   string
	Labels map[string]string
	// Phase is the pod's status.phase: Pending, Running, Succeeded,
	// Failed or Unknown.
	Phase string
	// Unschedulable is the scheduler's message while the pod's
	// PodScheduled condition is False for reason Unschedulable ("0/1
	// nodes are available: 1 Insufficient memory. ..."), empty
	// otherwise.
	Unschedulable string
}

// SpecHash is the pod's spec-hash label, "" when it carries none.
func (p Pod) SpecHash() string { return p.Labels[SpecHashLabel] }

// Finished reports whether the pod has terminated for good: phase
// Succeeded or Failed.
func (p Pod) Finished() bool { return p.Phase == "Succeeded" || p.Phase == "Failed" }

// podObject is the wire form Pod decodes from.
type podObject struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Status struct {
		Phase             string `json:"phase"`
		ContainerStatuses []struct {
			Ready bool `json:"ready"`
		} `json:"containerStatuses"`
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

func (o podObject) pod() Pod {
	p := Pod{Name: o.Metadata.Name, Labels: o.Metadata.Labels, Phase: o.Status.Phase}
	for _, c := range o.Status.Conditions {
		if c.Type == "PodScheduled" && c.Status == "False" && c.Reason == "Unschedulable" {
			p.Unschedulable = c.Message
			if p.Unschedulable == "" {
				p.Unschedulable = c.Reason
			}
		}
	}
	return p
}

// getPodObject fetches the named Pod, or ErrNotFound.
func (k *Client) getPodObject(ctx context.Context, name string) (podObject, error) {
	var obj podObject
	resp, err := k.do(ctx, http.MethodGet, k.PodPath(name), nil)
	if err != nil {
		return obj, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return obj, ErrNotFound
	}
	if err := k.checkErr(resp); err != nil {
		return obj, err
	}
	err = json.NewDecoder(resp.Body).Decode(&obj)
	return obj, err
}

// GetPod returns the named Pod, or ErrNotFound if no such Pod exists.
func (k *Client) GetPod(ctx context.Context, name string) (Pod, error) {
	obj, err := k.getPodObject(ctx, name)
	if err != nil {
		return Pod{}, err
	}
	return obj.pod(), nil
}

// GetPodSpecHash returns the value of the spec-hash label on the
// named Pod, or ErrNotFound if no such Pod exists. An empty string
// (no error) means the Pod exists but carries no label. Callers treat
// "" as drift to force a recreate.
func (k *Client) GetPodSpecHash(ctx context.Context, name string) (string, error) {
	pod, err := k.GetPod(ctx, name)
	if err != nil {
		return "", err
	}
	return pod.SpecHash(), nil
}

// ListPods returns the namespace's Pods matching labelSelector (the
// API's selector syntax, e.g. "app=kivali-browserpod"); an empty
// selector lists every Pod.
func (k *Client) ListPods(ctx context.Context, labelSelector string) ([]Pod, error) {
	path := k.PodsPath()
	if labelSelector != "" {
		path += "?labelSelector=" + url.QueryEscape(labelSelector)
	}
	resp, err := k.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := k.checkErr(resp); err != nil {
		return nil, err
	}
	var list struct {
		Items []podObject `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	pods := make([]Pod, 0, len(list.Items))
	for _, o := range list.Items {
		pods = append(pods, o.pod())
	}
	return pods, nil
}

// WaitForPodGone polls until a Pod with the given name returns 404,
// or the deadline elapses. Used between a delete + recreate so the
// recreate doesn't race the still-Terminating pod and fail with
// AlreadyExists. Polls every 500ms.
func (k *Client) WaitForPodGone(ctx context.Context, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		resp, err := k.do(ctx, http.MethodGet, k.PodPath(name), nil)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("kube: pod %q still present after %s", name, timeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// GetPodReady returns true once the named Pod is in the Running phase
// with all container statuses Ready, or ErrNotFound.
func (k *Client) GetPodReady(ctx context.Context, name string) (bool, error) {
	pod, err := k.getPodObject(ctx, name)
	if err != nil {
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

// PodsPath is the API path of the namespace's Pod collection. The
// client handles three kinds, Pods, PVCs and Secrets; Services are
// absent because no provisioned pod owns a listener, so RBAC needs no
// Service grants.
func (k *Client) PodsPath() string {
	return fmt.Sprintf("/api/v1/namespaces/%s/pods", k.namespace)
}

// PodPath is the API path of the named Pod.
func (k *Client) PodPath(name string) string {
	return fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", k.namespace, name)
}

// PVCsPath is the API path of the namespace's PVC collection.
func (k *Client) PVCsPath() string {
	return fmt.Sprintf("/api/v1/namespaces/%s/persistentvolumeclaims", k.namespace)
}

// PVCPath is the API path of the named PVC.
func (k *Client) PVCPath(name string) string {
	return fmt.Sprintf("/api/v1/namespaces/%s/persistentvolumeclaims/%s", k.namespace, name)
}

// SecretsPath is the API path of the namespace's Secret collection.
func (k *Client) SecretsPath() string {
	return fmt.Sprintf("/api/v1/namespaces/%s/secrets", k.namespace)
}

// SecretPath is the API path of the named Secret.
func (k *Client) SecretPath(name string) string {
	return fmt.Sprintf("/api/v1/namespaces/%s/secrets/%s", k.namespace, url.PathEscape(name))
}

// secretObject is the wire form of an Opaque Secret. The API carries
// data base64-encoded, which encoding/json does for []byte.
type secretObject struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace,omitempty"`
		Labels    map[string]string `json:"labels,omitempty"`
	} `json:"metadata"`
	Type string            `json:"type,omitempty"`
	Data map[string][]byte `json:"data"`
}

// GetSecret returns the named Secret's data, or ErrNotFound.
func (k *Client) GetSecret(ctx context.Context, name string) (map[string][]byte, error) {
	resp, err := k.do(ctx, http.MethodGet, k.SecretPath(name), nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := k.checkErr(resp); err != nil {
		return nil, err
	}
	var obj secretObject
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
		return nil, err
	}
	if obj.Data == nil {
		obj.Data = map[string][]byte{}
	}
	return obj.Data, nil
}

// PutSecret writes the named Opaque Secret with labels and data,
// replacing what it held: an update, or a create when there is none.
func (k *Client) PutSecret(ctx context.Context, name string, labels map[string]string, data map[string][]byte) error {
	var obj secretObject
	obj.APIVersion, obj.Kind, obj.Type = "v1", "Secret", "Opaque"
	obj.Metadata.Name, obj.Metadata.Namespace, obj.Metadata.Labels = name, k.namespace, labels
	obj.Data = data
	resp, err := k.do(ctx, http.MethodPut, k.SecretPath(name), obj)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		return k.checkErr(resp)
	}
	created, err := k.do(ctx, http.MethodPost, k.SecretsPath(), obj)
	if err != nil {
		return err
	}
	defer func() { _ = created.Body.Close() }()
	return k.checkErr(created)
}

// DeleteSecret deletes the named Secret; one that does not exist is no
// error.
func (k *Client) DeleteSecret(ctx context.Context, name string) error {
	return k.DeleteResource(ctx, k.SecretPath(name))
}

// SpecHashLabel keys the per-pod label that records the hash of the
// manifest the pod was created from. The kivali- prefix keeps it from
// colliding with other labels; it has no domain prefix because no
// domain is shared by every install.
const SpecHashLabel = "kivali-spec-hash"

// SpecHash returns a stable 12-hex-char hash of the manifest content.
// The hash MUST be computed on a manifest that does not already carry
// the spec-hash label (chicken-and-egg), so callers only ever invoke
// this on a freshly built manifest before stamping the label.
//
// json.Marshal of map[string]any emits keys in sorted order so the
// hash is deterministic across builds and Go versions; the manifest
// builders are pure (no randomness, no time), so the same input
// produces the same hash.
func SpecHash(manifest map[string]any) string {
	b, err := json.Marshal(manifest)
	if err != nil {
		// Should never happen: a manifest is a plain map of basic
		// types. If it does, return empty so drift detection re-runs
		// (recreate-once semantics) rather than panicking the Kivali
		// boot path.
		return ""
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:6])
}

// SiblingImage returns the image named name released alongside image:
// same registry path and tag, last path segment swapped. An image
// without a tag is treated as ":latest", docker's default; an empty
// image returns empty.
//
//	SiblingImage("ghcr.io/kivali-ai/kivali:v0.8.3", "kivali-browser")
//	  → "ghcr.io/kivali-ai/kivali-browser:v0.8.3"
func SiblingImage(image, name string) string {
	if image == "" {
		return ""
	}
	// Split off the tag (last colon AFTER any registry-port colon).
	// "registry:5000/name:v1" → base="registry:5000/name", tag="v1".
	// strings.LastIndex gets the right colon because the path
	// separator "/" sits between the port colon and any tag colon.
	tag := "latest"
	base := image
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		base = image[:i]
		tag = image[i+1:]
	}
	if i := strings.LastIndex(base, "/"); i >= 0 {
		return base[:i+1] + name + ":" + tag
	}
	return name + ":" + tag
}
