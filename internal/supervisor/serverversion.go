package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/kivali-ai/kivali/internal/supervisor/guestapi"
)

// serverCommand reports a server deployment whose kivali container runs
// a program of its own (a command) in place of the one its image holds.
// The chart sets no command, so Helm's upgrade leaves one in place: the
// new release's image would come up running the old program, and every
// check that reads the chart label and the image would pass.
func serverCommand(ctx context.Context, g Guest) error {
	out, err := kubectl(ctx, g, "-n", Namespace, "get", "deployment", "kivali", "-o", "json")
	if err != nil {
		return err
	}
	var d deployment
	if err := json.Unmarshal(out, &d); err != nil {
		return fmt.Errorf("deployment: %w", err)
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == "kivali" && len(c.Command) > 0 {
			return fmt.Errorf("the server runs %s in place of the Kivali in its image, a change made to its deployment outside the chart; "+
				"an update keeps that change, so the new release would run the same program. "+
				"Remove the kivali container's command, and anything mounted for it, from the deployment "+
				"(`kubectl -n %s edit deployment kivali`), then update again; nothing was changed",
				strings.Join(c.Command, " "), Namespace)
		}
	}
	return nil
}

// bootLine matches the line the server logs as it starts, naming the
// version stamped into its binary (main.bootLine).
var bootLine = regexp.MustCompile(`kivali boot: version=(\S+)`)

// ServerBootVersion is the version the server's log names on its boot
// line, without a leading v; ok is false when the log has none.
func ServerBootVersion(log []byte) (version string, ok bool) {
	m := bootLine.FindSubmatch(log)
	if m == nil {
		return "", false
	}
	return strings.TrimPrefix(string(m[1]), "v"), true
}

// bootLogBytes is how much of the server's log, from its start, is read
// for the boot line.
const bootLogBytes = 1 << 20

// serverVersion checks that the server running image says it is
// Kivali want: the image tag and the chart label name a release, the
// binary that serves is stamped with one. A development build on
// either side is not compared (its binary says 0.0.0-dev+g<commit>).
// A log that cannot be read, or that names no version, is logged and
// not failed: only a server that names another version is.
func serverVersion(ctx context.Context, g Guest, want, image string, logf Logf) error {
	want = strings.TrimPrefix(want, "v")
	pod, err := serverPod(ctx, g, image)
	if err != nil {
		logf("the server's version: %v", err)
		return nil
	}
	out, err := kubectl(ctx, g, "-n", Namespace, "logs", pod, "-c", "kivali", fmt.Sprintf("--limit-bytes=%d", bootLogBytes))
	if err != nil {
		logf("the server's version: %v", err)
		return nil
	}
	got, ok := ServerBootVersion(out)
	if !ok {
		logf("the server's version: pod %s logged no boot line", pod)
		return nil
	}
	if isDevVersion(got) || isDevVersion(want) {
		logf("the server is Kivali %s (a development build; not compared with %s)", got, want)
		return nil
	}
	if got != want {
		return fmt.Errorf("the server runs image %s but says it is Kivali %s, not %s: "+
			"something other than the release's own program is serving", image, got, want)
	}
	logf("the server says it is Kivali %s", got)
	return nil
}

// serverPod is the server pod the rollout made: running image, and not
// on its way out.
func serverPod(ctx context.Context, g Guest, image string) (string, error) {
	out, err := kubectl(ctx, g, "-n", Namespace, "get", "pods", "-l", "app=kivali", "-o", "json")
	if err != nil {
		return "", err
	}
	var pl podList
	if err := json.Unmarshal(out, &pl); err != nil {
		return "", fmt.Errorf("pods: %w", err)
	}
	for _, p := range pl.Items {
		if p.Metadata.DeletionTimestamp != "" || p.Status.Phase != "Running" {
			continue
		}
		for _, c := range p.Spec.Containers {
			if c.Name == "kivali" && guestapi.NormalizeRef(c.Image) == guestapi.NormalizeRef(image) {
				return p.Metadata.Name, nil
			}
		}
	}
	return "", fmt.Errorf("no running server pod on %s", image)
}
