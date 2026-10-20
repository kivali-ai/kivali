package supervisor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kivali-ai/kivali/internal/supervisor/guestapi"
)

// Guest paths, relative to the data directory (vm/README.md, "Data disk
// layout").
const (
	ManifestRel     = "k3s/server/manifests/kivali.yaml"
	StaticChartsRel = "k3s/server/static/charts"
)

// PodCIDR is k3s's default pod network, excluded from the server's
// ingress rule and refused by the egress proxy.
const PodCIDR = "10.42.0.0/16"

// ServiceCIDR is k3s's default Service network, refused by the egress
// proxy.
const ServiceCIDR = "10.43.0.0/16"

// Image repository names the install looks for among the baked images.
const (
	repoKivali  = "kivali"
	repoEgress  = "kivali-egress-proxy"
	repoShell   = "kivali-agent-shell"
	repoBrowser = "kivali-browser"
	repoBusybox = "mirrored-library-busybox"
)

// InstallOptions are the values of a first install.
type InstallOptions struct {
	// Owner is the Google account that owns the org (OWNER_EMAILS).
	Owner string `json:"owner,omitempty"`
	// Chart is a chart tarball on the host; empty uses the chart baked
	// into the VM image.
	Chart string `json:"chart,omitempty"`
	// PublicClientID sets oauth.publicClientID (OAUTH_PUBLIC_CLIENT_ID).
	PublicClientID string `json:"public_client_id,omitempty"`
	// Env is extra server environment, NAME=VALUE (extraEnv). A name
	// the chart already sets is refused by the API server (duplicate
	// env entries); use Set for those.
	Env []string `json:"env,omitempty"`
	// Set are chart value overrides, path.to.key=value, applied last;
	// "true" and "false" become booleans, everything else a string.
	Set []string `json:"set,omitempty"`
}

// applySets applies --set overrides to values.
func applySets(v map[string]any, sets []string) error {
	for _, s := range sets {
		k, val, ok := strings.Cut(s, "=")
		if !ok || k == "" || strings.HasPrefix(k, ".") || strings.HasSuffix(k, ".") || strings.Contains(k, "..") {
			return fmt.Errorf("set %q: want path.to.key=value", s)
		}
		var x any = val
		switch val {
		case "true":
			x = true
		case "false":
			x = false
		}
		setValue(v, strings.Split(k, "."), x)
	}
	return nil
}

// helmChart is k3s's HelmChart object.
type helmChart struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		Chart           string `yaml:"chart"`
		TargetNamespace string `yaml:"targetNamespace"`
		CreateNamespace bool   `yaml:"createNamespace"`
		ValuesContent   string `yaml:"valuesContent"`
	} `yaml:"spec"`
}

// chartURL is the Helm controller's static-charts URL of a chart file.
func chartURL(file string) string {
	return "https://%{KUBERNETES_API}%/static/charts/" + file
}

func chartFileName(m guestapi.ChartMeta) string {
	return m.Name + "-" + m.Version + ".tgz"
}

// RenderManifest renders the HelmChart for chart file with values.
func RenderManifest(file string, values map[string]any) ([]byte, error) {
	v, err := yaml.Marshal(values)
	if err != nil {
		return nil, err
	}
	var hc helmChart
	hc.APIVersion = "helm.cattle.io/v1"
	hc.Kind = "HelmChart"
	hc.Metadata.Name = "kivali"
	hc.Metadata.Namespace = "kube-system"
	hc.Spec.Chart = chartURL(file)
	hc.Spec.TargetNamespace = Namespace
	hc.Spec.CreateNamespace = true
	hc.Spec.ValuesContent = string(v)
	b, err := yaml.Marshal(&hc)
	if err != nil {
		return nil, err
	}
	return append([]byte("# Written by kivali-supervisor. Holds the org's session key; owner-only.\n"), b...), nil
}

// ParseManifest reads a HelmChart written by RenderManifest.
func ParseManifest(b []byte) (chartFile string, values map[string]any, err error) {
	var hc helmChart
	if err := yaml.Unmarshal(b, &hc); err != nil {
		return "", nil, fmt.Errorf("manifest: %w", err)
	}
	if hc.Kind != "HelmChart" {
		return "", nil, fmt.Errorf("manifest: kind %q, want HelmChart", hc.Kind)
	}
	values = map[string]any{}
	if err := yaml.Unmarshal([]byte(hc.Spec.ValuesContent), &values); err != nil {
		return "", nil, fmt.Errorf("manifest: valuesContent: %w", err)
	}
	return path.Base(hc.Spec.Chart), values, nil
}

// setValue sets a nested key, creating maps on the way.
func setValue(m map[string]any, keys []string, v any) {
	for _, k := range keys[:len(keys)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	m[keys[len(keys)-1]] = v
}

// Images are the image references an install uses.
type Images struct {
	Kivali  string // repo:tag of the server (and agent) image
	Egress  string
	Busybox string // k3s's baked busybox helper, the init container
}

// pickImages finds Kivali's images among refs, all at one tag: the
// server, the egress proxy, the agent shell and, with browser, the
// browser image. browser follows the chart being installed
// (ChartMeta.Browser), since a release from before browser pods ships
// three images. With busybox it also finds k3s's busybox helper.
func pickImages(refs []string, browser, busybox bool) (Images, error) {
	byRepo := map[string][]string{} // repo name -> full refs
	for _, r := range refs {
		repo, t := guestapi.SplitRef(r)
		if t == "" {
			continue
		}
		name := guestapi.RepoName(repo)
		byRepo[name] = append(byRepo[name], r)
	}
	var im Images
	ks := byRepo[repoKivali]
	switch len(ks) {
	case 0:
		return im, fmt.Errorf("no %s image among %v", repoKivali, refs)
	case 1:
		im.Kivali = ks[0]
	default:
		sort.Strings(ks)
		return im, fmt.Errorf("several %s images (%v); name the tag", repoKivali, ks)
	}
	krepo, ktag := guestapi.SplitRef(im.Kivali)
	prefix := strings.TrimSuffix(krepo, repoKivali)
	wants := []string{repoEgress, repoShell}
	if browser {
		wants = append(wants, repoBrowser)
	}
	for _, want := range wants {
		ref := prefix + want + ":" + ktag
		found := false
		for _, r := range byRepo[want] {
			if r == ref {
				found = true
			}
		}
		if !found {
			return im, fmt.Errorf("image %s is missing (the %d Kivali images must share one tag)", ref, len(wants)+1)
		}
		if want == repoEgress {
			im.Egress = ref
		}
	}
	if !busybox {
		return im, nil
	}
	bb := byRepo[repoBusybox]
	if len(bb) == 0 {
		return im, fmt.Errorf("k3s's busybox helper (%s) is not baked into the VM image", repoBusybox)
	}
	sort.Strings(bb)
	im.Busybox = bb[len(bb)-1]
	return im, nil
}

func newSessionKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// InstallValues are the chart values of a first install (docs/developers/supervisor.md).
func InstallValues(o InstallOptions, im Images, sessionKey string) (map[string]any, error) {
	krepo, ktag := guestapi.SplitRef(im.Kivali)
	erepo, etag := guestapi.SplitRef(im.Egress)
	secrets := map[string]any{
		"create":      true,
		"ownerEmails": o.Owner,
		"sessionKey":  sessionKey,
	}
	v := map[string]any{
		"kivaliEnv": "dev",
		"devMode":   false,
		"initImage": im.Busybox,
		"image":     map[string]any{"repository": guestapi.ShortRepo(krepo), "tag": ktag, "pullPolicy": "IfNotPresent"},
		"egress":    map[string]any{"image": map[string]any{"repository": guestapi.ShortRepo(erepo), "tag": etag}},
		"secrets":   secrets,
		"networkPolicy": map[string]any{
			"enabled":     true,
			"podCIDR":     PodCIDR,
			"serviceCIDR": ServiceCIDR,
		},
	}
	if o.PublicClientID != "" {
		v["oauth"] = map[string]any{"publicClientID": o.PublicClientID}
	}
	if len(o.Env) > 0 {
		var env []any
		for _, e := range o.Env {
			name, val, ok := strings.Cut(e, "=")
			if !ok || name == "" {
				return nil, fmt.Errorf("env %q: want NAME=VALUE", e)
			}
			env = append(env, map[string]any{"name": name, "value": val})
		}
		v["extraEnv"] = env
	}
	return v, nil
}

// kubectl runs `k3s kubectl` in the guest and returns stdout.
func kubectl(ctx context.Context, g Guest, args ...string) ([]byte, error) {
	var out, errb bytes.Buffer
	argv := append([]string{"k3s", "kubectl"}, args...)
	code, err := g.Exec(ctx, guestapi.ExecSpec{Argv: argv}, nil, &out, &errb)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return out.Bytes(), fmt.Errorf("kubectl %s: exit %d: %s", strings.Join(args, " "), code, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// readGuestFile reads a file in the guest; ok is false if it does not
// exist.
func readGuestFile(ctx context.Context, g Guest, p string) ([]byte, bool, error) {
	var out, errb bytes.Buffer
	code, err := g.Exec(ctx, guestapi.ExecSpec{Argv: []string{"sh", "-c", `[ -e "$1" ] || exit 3; cat "$1"`, "sh", p}}, nil, &out, &errb)
	if err != nil {
		return nil, false, err
	}
	switch code {
	case 0:
		return out.Bytes(), true, nil
	case 3:
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("read %s: exit %d: %s", p, code, strings.TrimSpace(errb.String()))
}

// installedLocked reports whether the HelmChart manifest exists on the
// data disk, and records its chart version in local.json if that has
// none (an install interrupted before it finished waiting).
func (s *Supervisor) installedLocked(ctx context.Context) (bool, error) {
	g, err := s.running()
	if err != nil {
		return false, err
	}
	b, ok, err := readGuestFile(ctx, g, guestapi.DataDir+"/"+ManifestRel)
	if err != nil || !ok {
		return ok, err
	}
	if s.State().Kivali == "" {
		if file, _, err := ParseManifest(b); err == nil {
			v := strings.TrimSuffix(strings.TrimPrefix(file, "kivali-"), ".tgz")
			if err := s.update(func(st *State) { st.Kivali = v }); err != nil {
				return true, err
			}
		}
	}
	return true, nil
}

// placeChart puts the chart into the static charts directory: from the
// host if o.Chart is set, else the one baked into the VM image.
func (s *Supervisor) placeChart(ctx context.Context, g Guest, st guestapi.Status, hostChart string, logf Logf) (guestapi.ChartMeta, error) {
	if hostChart != "" {
		meta, err := guestapi.ChartInfoFile(hostChart)
		if err != nil {
			return meta, err
		}
		f, err := os.Open(hostChart)
		if err != nil {
			return meta, err
		}
		defer func() { _ = f.Close() }()
		if err := g.WriteFile(ctx, StaticChartsRel+"/"+chartFileName(meta), 0o644, f); err != nil {
			return meta, fmt.Errorf("copy chart: %w", err)
		}
		logf("chart %s %s copied from %s", meta.Name, meta.Version, hostChart)
		return meta, nil
	}
	var baked []guestapi.BakedChart
	for _, c := range st.BakedCharts {
		if c.Name == "kivali" {
			baked = append(baked, c)
		}
	}
	if len(baked) != 1 {
		return guestapi.ChartMeta{}, fmt.Errorf("the VM image bakes %d kivali charts; pass --chart <tgz>", len(baked))
	}
	c := baked[0]
	meta := guestapi.ChartMeta{Name: c.Name, Version: c.Version, AppVersion: c.AppVersion}
	dst := guestapi.DataDir + "/" + StaticChartsRel + "/" + chartFileName(meta)
	code, err := g.Exec(ctx, guestapi.ExecSpec{Argv: []string{"cp", c.Path, dst}}, nil, nil, nil)
	if err != nil {
		return meta, err
	}
	if code != 0 {
		return meta, fmt.Errorf("cp %s %s: exit %d", c.Path, dst, code)
	}
	logf("chart %s %s placed from the VM image (%s)", meta.Name, meta.Version, c.Path)
	return meta, nil
}

func (s *Supervisor) installLocked(ctx context.Context, o InstallOptions, logf Logf) error {
	if o.Owner == "" {
		return errors.New("not installed yet: --owner <google account> is required on the first up")
	}
	if !strings.Contains(o.Owner, "@") {
		return fmt.Errorf("--owner %q is not an email address", o.Owner)
	}
	g, err := s.running()
	if err != nil {
		return err
	}
	st, err := g.Status(ctx)
	if err != nil {
		return err
	}
	im, err := pickImages(st.BakedImages, true, true)
	if err != nil {
		return err
	}
	logf("images: %s, %s; init container %s", im.Kivali, im.Egress, im.Busybox)
	meta, err := s.placeChart(ctx, g, st, o.Chart, logf)
	if err != nil {
		return err
	}
	key, err := newSessionKey()
	if err != nil {
		return err
	}
	vals, err := InstallValues(o, im, key)
	if err != nil {
		return err
	}
	if err := applySets(vals, o.Set); err != nil {
		return err
	}
	manifest, err := RenderManifest(chartFileName(meta), vals)
	if err != nil {
		return err
	}
	if err := g.WriteFile(ctx, ManifestRel, 0o600, bytes.NewReader(manifest)); err != nil {
		return fmt.Errorf("write HelmChart: %w", err)
	}
	logf("HelmChart kube-system/kivali written (chart %s, owner %s)", chartFileName(meta), o.Owner)
	if err := s.waitServing(ctx, meta.Version, im.Kivali, logf); err != nil {
		return err
	}
	if err := s.update(func(st *State) { st.Kivali = meta.Version }); err != nil {
		return err
	}
	logf("Kivali %s installed: %s/", meta.Version, s.baseURL())
	return nil
}

// Install installs Kivali into the running VM.
func (s *Supervisor) Install(ctx context.Context, o InstallOptions, logf Logf) error {
	ctx, end, err := s.begin(ctx, "install", true)
	if err != nil {
		return err
	}
	defer end()
	setStage(ctx, StageSettingUp)
	logf = s.tee(ctx, logf)
	installed, err := s.installedLocked(ctx)
	if err != nil {
		return err
	}
	if installed {
		return fmt.Errorf("already installed (Kivali %s); use upgrade", s.State().Kivali)
	}
	return s.installLocked(ctx, o, logf)
}

// deployment is the part of `kubectl get deploy -o json` the waits read.
type deployment struct {
	Metadata struct {
		Generation int64             `json:"generation"`
		Labels     map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Replicas *int32 `json:"replicas"`
		Template struct {
			Spec struct {
				Containers []struct {
					Name  string `json:"name"`
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration int64 `json:"observedGeneration"`
		Replicas           int32 `json:"replicas"`
		UpdatedReplicas    int32 `json:"updatedReplicas"`
		ReadyReplicas      int32 `json:"readyReplicas"`
		AvailableReplicas  int32 `json:"availableReplicas"`
	} `json:"status"`
}

type podList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			ContainerStatuses []struct {
				Name         string `json:"name"`
				Image        string `json:"image"`
				RestartCount int    `json:"restartCount"`
				State        struct {
					Waiting *struct {
						Reason  string `json:"reason"`
						Message string `json:"message"`
					} `json:"waiting"`
				} `json:"state"`
			} `json:"containerStatuses"`
			InitContainerStatuses []struct {
				Name  string `json:"name"`
				State struct {
					Waiting *struct {
						Reason  string `json:"reason"`
						Message string `json:"message"`
					} `json:"waiting"`
				} `json:"state"`
			} `json:"initContainerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

// fatalWaiting are container waiting reasons that do not resolve on
// their own in an airgapped VM.
var fatalWaiting = map[string]bool{
	"ErrImagePull":      true,
	"ImagePullBackOff":  true,
	"ErrImageNeverPull": true,
	"InvalidImageName":  true,
}

// deploymentState checks the server deployment once. chartVersion and
// image, when set, must match (the helm.sh/chart label and the kivali
// container's image).
func deploymentState(ctx context.Context, g Guest, chartVersion, image string) error {
	out, err := kubectl(ctx, g, "-n", Namespace, "get", "deployment", "kivali", "-o", "json")
	if err != nil {
		if strings.Contains(err.Error(), "NotFound") {
			if jerr := helmJobFailed(ctx, g); jerr != nil {
				return permanent(jerr)
			}
			return errors.New("deployment kivali does not exist yet")
		}
		return err
	}
	var d deployment
	if err := json.Unmarshal(out, &d); err != nil {
		return fmt.Errorf("deployment: %w", err)
	}
	if chartVersion != "" {
		if got := d.Metadata.Labels["helm.sh/chart"]; got != "kivali-"+chartVersion {
			if jerr := helmJobFailed(ctx, g); jerr != nil {
				return permanent(jerr)
			}
			return fmt.Errorf("deployment is at chart %q, waiting for kivali-%s", got, chartVersion)
		}
	}
	if image != "" {
		var got string
		for _, c := range d.Spec.Template.Spec.Containers {
			if c.Name == "kivali" {
				got = c.Image
			}
		}
		if guestapi.NormalizeRef(got) != guestapi.NormalizeRef(image) {
			err := fmt.Errorf("deployment runs image %q, expected %s", got, image)
			if chartVersion != "" {
				// Helm applied the chart (the label matches) and it
				// renders another image: waiting will not change that.
				return permanent(fmt.Errorf("chart kivali-%s: %w", chartVersion, err))
			}
			return err
		}
	}
	// A pod that cannot get its image will not get it later.
	var waiting []string
	if pods, err := kubectl(ctx, g, "-n", Namespace, "get", "pods", "-l", "app=kivali", "-o", "json"); err == nil {
		var pl podList
		if json.Unmarshal(pods, &pl) == nil {
			for _, p := range pl.Items {
				for _, c := range p.Status.ContainerStatuses {
					if w := c.State.Waiting; w != nil && fatalWaiting[w.Reason] {
						return permanent(fmt.Errorf("pod %s container %s: %s: %s", p.Metadata.Name, c.Name, w.Reason, w.Message))
					} else if w != nil {
						waiting = append(waiting, fmt.Sprintf("%s/%s %s (restarts %d)", p.Metadata.Name, c.Name, w.Reason, c.RestartCount))
					}
				}
				for _, c := range p.Status.InitContainerStatuses {
					if w := c.State.Waiting; w != nil && fatalWaiting[w.Reason] {
						return permanent(fmt.Errorf("pod %s init container %s: %s: %s", p.Metadata.Name, c.Name, w.Reason, w.Message))
					}
				}
			}
		}
	}
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	st := d.Status
	if st.ObservedGeneration < d.Metadata.Generation || st.UpdatedReplicas != want || st.Replicas != want || st.AvailableReplicas != want || st.ReadyReplicas != want {
		msg := fmt.Sprintf("rollout in progress (updated %d, ready %d, available %d of %d)", st.UpdatedReplicas, st.ReadyReplicas, st.AvailableReplicas, want)
		if len(waiting) > 0 {
			msg += "; waiting: " + strings.Join(waiting, ", ")
		}
		return errors.New(msg)
	}
	return nil
}

// helmJobFailures is how many failed runs of the Helm controller's job
// count as a failed install: it retries without end, and a chart or
// values error fails the same way every time.
const helmJobFailures = 3

// helmJob is the Helm controller's install job of the kivali HelmChart,
// and helmJobPods selects its pods.
const (
	helmJob     = "helm-install-kivali"
	helmJobPods = "batch.kubernetes.io/job-name=" + helmJob
)

// terminatedState is a container's terminated state.
type terminatedState struct {
	ExitCode int    `json:"exitCode"`
	Reason   string `json:"reason"`
	Message  string `json:"message"`
}

// waitingState is a container's waiting state.
type waitingState struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// containerStatus is the part of a pod's container status the waits
// read.
type containerStatus struct {
	Name         string `json:"name"`
	RestartCount int    `json:"restartCount"`
	State        struct {
		Waiting    *waitingState    `json:"waiting"`
		Terminated *terminatedState `json:"terminated"`
	} `json:"state"`
	LastState struct {
		Terminated *terminatedState `json:"terminated"`
	} `json:"lastState"`
}

// failedRuns counts the container's failed runs: restartCount (the runs
// before the current one), plus the last run while it is a failure the
// container has not restarted from (terminated, or waiting to restart
// in CrashLoopBackOff). previous is whether the latest failed run is
// the container's previous instance rather than its current one, and
// last is that run's terminated state (nil when none is known).
func (c containerStatus) failedRuns() (n int, previous bool, last *terminatedState) {
	n, previous, last = c.RestartCount, true, c.LastState.Terminated
	switch now := c.State.Terminated; {
	case now != nil && now.ExitCode != 0:
		n++
		previous, last = false, now
	case c.State.Waiting != nil && last != nil && last.ExitCode != 0:
		n++
	}
	return n, previous, last
}

// helmPodList is the part of the install job's pods helmJobFailed reads.
type helmPodList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Phase             string            `json:"phase"`
			ContainerStatuses []containerStatus `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

// helmRun is the install job's container that has failed the most
// times.
type helmRun struct {
	pod, container string
	failures       int
	// previous is whether the latest failed run is the container's
	// previous instance rather than its current one.
	previous bool
	// message is that run's termination message.
	message string
}

// failedHelmRuns finds the container of the install job's unfinished
// pods that has failed the most times. The Helm controller's job runs
// its pod with restartPolicy OnFailure, so a failed helm run restarts
// the container in place and the job's .status.failed stays 0: the
// failures are the container's (containerStatus.failedRuns).
func failedHelmRuns(pl helmPodList) helmRun {
	var worst helmRun
	for _, p := range pl.Items {
		if p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed" {
			// An earlier install's pod, or one the job's .status.failed
			// already counts.
			continue
		}
		for _, c := range p.Status.ContainerStatuses {
			n, previous, last := c.failedRuns()
			if n <= worst.failures {
				continue
			}
			worst = helmRun{pod: p.Metadata.Name, container: c.Name, failures: n, previous: previous}
			if last != nil {
				worst.message = last.Message
			}
		}
	}
	return worst
}

// helmError is helm's own error in an install job's log: from the last
// line starting "Error:" to the end, without the shell's trace lines.
func helmError(logs []byte) string {
	var lines []string
	capture := false
	for _, l := range strings.Split(string(logs), "\n") {
		if strings.HasPrefix(l, "Error:") {
			capture = true
			lines = lines[:0]
		}
		if capture && !strings.HasPrefix(l, "+") && strings.TrimSpace(l) != "" {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	return strings.Join(lines, " ")
}

// helmJobFailed reports the Helm controller's install job as failed,
// with helm's own error, once its runs have failed helmJobFailures
// times: the job's failed pods plus the failed runs of its unfinished
// pod.
func helmJobFailed(ctx context.Context, g Guest) error {
	out, err := kubectl(ctx, g, "-n", "kube-system", "get", "job", helmJob, "-o", "jsonpath={.status.failed}")
	if err != nil {
		return nil
	}
	var failed int
	if s := strings.TrimSpace(string(out)); s != "" {
		if _, err := fmt.Sscan(s, &failed); err != nil {
			return nil
		}
	}
	var run helmRun
	if out, err := kubectl(ctx, g, "-n", "kube-system", "get", "pods", "-l", helmJobPods, "-o", "json"); err == nil {
		var pl helmPodList
		if json.Unmarshal(out, &pl) == nil {
			run = failedHelmRuns(pl)
		}
	}
	failed += run.failures
	if failed < helmJobFailures {
		return nil
	}
	// The failed run's own log: the current instance may be a fresh
	// run that has not failed yet.
	logArgs := []string{"-n", "kube-system", "logs", "job/" + helmJob, "--tail=40"}
	if run.pod != "" {
		logArgs = []string{"-n", "kube-system", "logs", run.pod, "-c", run.container, "--tail=40"}
		if run.previous {
			logArgs = append(logArgs, "--previous")
		}
	}
	var reason string
	if logs, err := kubectl(ctx, g, logArgs...); err == nil {
		reason = helmError(logs)
	}
	if reason == "" {
		reason = strings.TrimSpace(run.message)
	}
	if reason == "" {
		reason = "see `k3s kubectl -n kube-system logs job/" + helmJob + "`"
	}
	return fmt.Errorf("the Helm controller failed to install the chart %d times: %s", failed, reason)
}

// waitServing waits for the deployment (at chartVersion and image, when
// given) to be rolled out and for /readyz through the forward.
func (s *Supervisor) waitServing(ctx context.Context, chartVersion, image string, logf Logf) error {
	g, err := s.running()
	if err != nil {
		return err
	}
	start := s.o.Clock.Now()
	what := "the server deployment to roll out"
	if chartVersion != "" {
		what = "the server deployment to roll out at chart " + chartVersion
	}
	logf("waiting for %s", what)
	if err := poll(ctx, s.o.Clock, deployTimeout, pollInterval, what, func(ctx context.Context) error {
		return deploymentState(ctx, g, chartVersion, image)
	}); err != nil {
		return err
	}
	logf("deployment rolled out after %s; waiting for /readyz", s.o.Clock.Now().Sub(start).Round(time.Second))
	url := s.baseURL() + "/readyz"
	if err := poll(ctx, s.o.Clock, 2*time.Minute, time.Second, url, func(ctx context.Context) error {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(c, http.MethodGet, url, nil)
		if err != nil {
			return permanent(err)
		}
		resp, err := s.o.HTTP.Do(req)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("/readyz: %s", resp.Status)
		}
		return nil
	}); err != nil {
		return err
	}
	logf("%s answers 200", url)
	return nil
}
