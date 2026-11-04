package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/kivali-ai/kivali/internal/supervisor/guestapi"
)

// Kubelet's default hard eviction thresholds for disk, in percent of
// the filesystem kept available. containerd's images (imagefs) and
// kubelet's pod directory (nodefs) are both on the guest's data disk
// (vm/rootfs/usr/libexec/kivali/boot), so the higher of the two is the
// floor: below it the node is tainted node.kubernetes.io/disk-pressure,
// its pods are evicted and none is scheduled, and kubelet's image
// garbage collection deletes every image no running container uses
// that is not pinned.
const (
	imagefsAvailablePct = 15
	nodefsAvailablePct  = 10
)

// guestHeadroom is the room above the floor an upgrade leaves on the
// data disk once the new release's images are in: what the release
// writes as it starts (its containers' layers, its logs).
const guestHeadroom = 1 << 30

// importFactor estimates what importing an image bundle adds to the
// data disk, as a multiple of the bundle's download size: containerd
// keeps the layers as the bundle holds them and unpacks each beside
// them, and layers the installed release shares cost nothing. A release
// that shares fewer takes more; the check after the import measures
// what it took, and undoes an import that left too little.
const importFactor = 2

// pinnedLabel is the containerd label kubelet's image garbage collection
// skips an image for (the CRI image's pinned field).
const pinnedLabel, pinnedValue = "io.cri-containerd.pinned", "pinned"

// guestDisk is the data disk's filesystem as df reports it.
type guestDisk struct{ size, avail uint64 }

// floor is the free space below which kubelet evicts the team's pods.
func (d guestDisk) floor() uint64 {
	return d.size / 100 * max(imagefsAvailablePct, nodefsAvailablePct)
}

// guestDiskUsage reads the data disk's size and available bytes in the
// guest (df -P -k, which busybox and coreutils print alike).
func guestDiskUsage(ctx context.Context, g Guest) (guestDisk, error) {
	var out, errb bytes.Buffer
	code, err := g.Exec(ctx, guestapi.ExecSpec{Argv: []string{"df", "-P", "-k", guestapi.DataDir}}, nil, &out, &errb)
	if err != nil {
		return guestDisk{}, err
	}
	if code != 0 {
		return guestDisk{}, fmt.Errorf("df: exit %d: %s", code, strings.TrimSpace(errb.String()))
	}
	return parseDF(out.String())
}

// parseDF reads the size and available columns (1024-byte blocks) of
// df -P -k's one filesystem line.
func parseDF(out string) (guestDisk, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	f := strings.Fields(lines[len(lines)-1])
	if len(lines) < 2 || len(f) < 6 {
		return guestDisk{}, fmt.Errorf("df: unexpected output %q", out)
	}
	size, err1 := strconv.ParseUint(f[1], 10, 64)
	avail, err2 := strconv.ParseUint(f[3], 10, 64)
	if err := errors.Join(err1, err2); err != nil {
		return guestDisk{}, fmt.Errorf("df: %w", err)
	}
	return guestDisk{size: size << 10, avail: avail << 10}, nil
}

// guestRoom refuses an upgrade the team's data disk has no room for:
// cost (what the new images take) on top of the floor and
// guestHeadroom. The preflight asks with the import's estimate before
// anything is downloaded; the upgrade asks again with nothing once the
// images are in. A disk df cannot read is not refused.
func (s *Supervisor) guestRoom(ctx context.Context, g Guest, cost uint64, outcome string, logf Logf) error {
	d, err := guestDiskUsage(ctx, g)
	if err != nil {
		logf("the data disk's free space: %v", err)
		return nil
	}
	need := d.floor() + guestHeadroom + cost
	logf("the data disk has %s free of %s; the upgrade needs %s", gb(d.avail), gb(d.size), gb(need))
	if d.avail >= need {
		return nil
	}
	what := "the room Kubernetes keeps free"
	if cost > 0 {
		what = fmt.Sprintf("%s for the new release's images, plus %s", gb(cost), what)
	}
	return fmt.Errorf("updating this team needs %s free on its data disk (%s), and only %s is free. "+
		"Free up %s of the team's files, then update it again; %s",
		gb(need), what, gb(d.avail), gb(need-d.avail), outcome)
}

// diskFullError is a start that failed because the team's data disk is
// too full for Kubernetes to run its pods.
type diskFullError struct {
	// Why is what showed it: the node's disk-pressure taint, an evicted
	// server pod, or free space below the floor.
	Why string
	// disk is the data disk's usage, when df could read it.
	disk guestDisk
}

func (e *diskFullError) Error() string {
	msg := "the team's data disk is too full to run on: " + e.Why
	if d := e.disk; d.size > 0 {
		msg += fmt.Sprintf(" (%s free of %s; Kubernetes stops the team below %s free)", gb(d.avail), gb(d.size), gb(d.floor()))
		if want := d.floor() + guestHeadroom; d.avail < want {
			msg += fmt.Sprintf(". It runs again once %s more of the disk is free", gb(want-d.avail))
		}
	}
	return msg
}

// withDisk adds the data disk's usage to a diskFullError that has none.
func withDisk(ctx context.Context, g Guest, err error) error {
	var full *diskFullError
	if errors.As(err, &full) && full.disk.size == 0 {
		if d, derr := guestDiskUsage(ctx, g); derr == nil {
			full.disk = d
		}
	}
	return err
}

// evictedServer is the eviction of a server pod among pl, if kubelet
// evicted one for disk: status.reason Evicted, with "ephemeral-storage"
// (what imagefs and nodefs pressure evict for) in its message. A pod
// evicted for memory is not the disk's.
func evictedServer(pl podList) *diskFullError {
	for _, p := range pl.Items {
		if p.Status.Reason == "Evicted" && strings.Contains(p.Status.Message, "ephemeral-storage") {
			why := "Kubernetes evicted the server"
			if m := strings.TrimSpace(p.Status.Message); m != "" {
				why += " (" + m + ")"
			}
			return &diskFullError{Why: why}
		}
	}
	return nil
}

// serverEvicted reports a server pod kubelet evicted; nil when none was,
// or the pods could not be read.
func serverEvicted(ctx context.Context, g Guest) error {
	out, err := kubectl(ctx, g, "-n", Namespace, "get", "pods", "-l", "app=kivali", "-o", "json")
	if err != nil {
		return nil
	}
	var pl podList
	if json.Unmarshal(out, &pl) != nil {
		return nil
	}
	if e := evictedServer(pl); e != nil {
		return e
	}
	return nil
}

// diskPressure reports a data disk too full for the team to run: the
// node tainted for disk pressure, or free space below the floor kubelet
// evicts at (which a node that has only just started may not have acted
// on yet). An evicted server pod is deploymentState's and
// serverEvicted's to report. nil when neither shows, or neither could
// be read.
func diskPressure(ctx context.Context, g Guest) error {
	d, derr := guestDiskUsage(ctx, g)
	if derr != nil {
		d = guestDisk{}
	}
	if out, err := kubectl(ctx, g, "get", "node", "kivali", "-o", "json"); err == nil {
		var n struct {
			Spec struct {
				Taints []struct {
					Key string `json:"key"`
				} `json:"taints"`
			} `json:"spec"`
		}
		if json.Unmarshal(out, &n) == nil {
			for _, t := range n.Spec.Taints {
				if t.Key == "node.kubernetes.io/disk-pressure" {
					return &diskFullError{Why: "Kubernetes reports the VM under disk pressure", disk: d}
				}
			}
		}
	}
	if derr == nil && d.avail < d.floor() {
		return &diskFullError{Why: "its free space is below what Kubernetes keeps free", disk: d}
	}
	return nil
}

// dropFailedServerPods deletes the server pods that ended (an earlier
// start's evicted pod among them), so a wait sees only this start's.
func dropFailedServerPods(ctx context.Context, g Guest, logf Logf) {
	if _, err := kubectl(ctx, g, "-n", Namespace, "delete", "pod", "-l", "app=kivali", "--field-selector=status.phase=Failed", "--ignore-not-found"); err != nil {
		logf("deleting ended server pods: %v", err)
	}
}

// kivaliRepos are the repositories of a release's images.
var kivaliRepos = map[string]bool{repoKivali: true, repoEgress: true, repoShell: true, repoBrowser: true}

// containerdImages is the set of image references containerd lists.
func containerdImages(ctx context.Context, g Guest) (map[string]bool, error) {
	out, err := kubectlLikeCtr(ctx, g)
	if err != nil {
		return nil, err
	}
	refs := map[string]bool{}
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			refs[l] = true
		}
	}
	return refs, nil
}

// installedRefs are the images of the release the HelmChart manifest
// names that containerd lists: the server and egress proxy images the
// values name, and the agent shell and browser images at the server's
// tag. Nil when the manifest cannot be read.
func installedRefs(ctx context.Context, g Guest, have map[string]bool) (server string, refs []string) {
	cur, err := readManifest(ctx, g)
	if err != nil {
		return "", nil
	}
	_, vals, err := ParseManifest(cur)
	if err != nil {
		return "", nil
	}
	ref := func(keys ...string) string {
		m, _ := vals[keys[0]].(map[string]any)
		for _, k := range keys[1:] {
			m, _ = m[k].(map[string]any)
		}
		repo, _ := m["repository"].(string)
		tag, _ := m["tag"].(string)
		if repo == "" || tag == "" {
			return ""
		}
		return guestapi.NormalizeRef(repo + ":" + tag)
	}
	server = ref("image")
	if server == "" {
		return "", nil
	}
	krepo, ktag := guestapi.SplitRef(server)
	prefix := strings.TrimSuffix(krepo, repoKivali)
	for _, r := range []string{server, ref("egress", "image"), prefix + repoShell + ":" + ktag, prefix + repoBrowser + ":" + ktag} {
		if r != "" && have[r] {
			refs = append(refs, r)
		}
	}
	return server, refs
}

// pinImages pins keep in containerd, so kubelet's image garbage
// collection never deletes them, and unpins every other Kivali image
// there. A release's images exist only in the VM (nothing can pull
// them again), so the release the team runs, and the one an upgrade
// rolls back to, keep theirs while the disk is under pressure; the
// images of releases the team no longer needs are left to the garbage
// collection. Only images whose label differs are labelled. A failure
// is logged: pinning protects, and nothing waits on it.
func pinImages(ctx context.Context, g Guest, keep []string, logf Logf) {
	if len(keep) == 0 {
		// The release's images are unknown (a manifest that cannot be
		// read): unpinning the rest could leave the team's own unpinned.
		return
	}
	pins, err := containerdPins(ctx, g)
	if err != nil {
		logf("pinning the release's images: %v", err)
		return
	}
	want := map[string]bool{}
	for _, r := range keep {
		want[guestapi.NormalizeRef(r)] = true
	}
	var pinned, changed []string
	for r, is := range pins {
		repo, tag := guestapi.SplitRef(r)
		if tag == "" || !kivaliRepos[guestapi.RepoName(repo)] {
			continue
		}
		if want[r] {
			pinned = append(pinned, r)
		}
		if is == want[r] {
			continue
		}
		v := ""
		if want[r] {
			v = pinnedValue
		}
		changed = append(changed, r)
		var errb bytes.Buffer
		code, err := g.Exec(ctx, guestapi.ExecSpec{Argv: []string{"k3s", "ctr", "-n", "k8s.io", "images", "label", r, pinnedLabel + "=" + v}}, nil, nil, &errb)
		if err == nil && code != 0 {
			err = fmt.Errorf("exit %d: %s", code, strings.TrimSpace(errb.String()))
		}
		if err != nil {
			logf("labelling %s: %v", r, err)
		}
	}
	if len(changed) > 0 {
		logf("pinned %d images: %s", len(pinned), strings.Join(slices.Sorted(slices.Values(pinned)), ", "))
	}
}

// containerdPins lists containerd's images with whether each is pinned:
// `ctr images ls` prints a reference first and its labels last
// (comma-separated, "-" for none); the size between them has a space.
func containerdPins(ctx context.Context, g Guest) (map[string]bool, error) {
	var out, errb bytes.Buffer
	code, err := g.Exec(ctx, guestapi.ExecSpec{Argv: []string{"k3s", "ctr", "-n", "k8s.io", "images", "ls"}}, nil, &out, &errb)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("ctr images ls: exit %d: %s", code, strings.TrimSpace(errb.String()))
	}
	return parsePins(out.String()), nil
}

// parsePins reads `ctr images ls`'s table (containerdPins).
func parsePins(out string) map[string]bool {
	pins := map[string]bool{}
	for i, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if i == 0 || len(f) < 2 {
			continue
		}
		pins[f[0]] = slices.Contains(strings.Split(f[len(f)-1], ","), pinnedLabel+"="+pinnedValue)
	}
	return pins
}

// added are the refs the upgrade's import brought: those containerd did
// not list before it. With that listing unknown (nil), the refs at
// another tag than installed (the installed release's server image),
// since an image at the installed release's tag is the team's own; none
// when the installed release is unknown too.
func added(refs []string, before map[string]bool, installed string) []string {
	_, itag := guestapi.SplitRef(installed)
	var out []string
	for _, r := range refs {
		switch _, tag := guestapi.SplitRef(r); {
		case before != nil && !before[guestapi.NormalizeRef(r)]:
			out = append(out, r)
		case before == nil && itag != "" && tag != itag:
			out = append(out, r)
		}
	}
	return out
}

// removeImages deletes refs from containerd, with the content only they
// used.
func removeImages(ctx context.Context, g Guest, refs []string, logf Logf) {
	if len(refs) == 0 {
		return
	}
	var errb bytes.Buffer
	argv := append([]string{"k3s", "ctr", "-n", "k8s.io", "images", "rm", "--sync"}, refs...)
	code, err := g.Exec(ctx, guestapi.ExecSpec{Argv: argv}, nil, nil, &errb)
	if err == nil && code != 0 {
		err = fmt.Errorf("exit %d: %s", code, strings.TrimSpace(errb.String()))
	}
	if err != nil {
		logf("removing %s: %v", strings.Join(refs, ", "), err)
		return
	}
	logf("removed %s", strings.Join(refs, ", "))
}
