package supervisor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/kivali-ai/kivali/internal/supervisor/guestapi"
)

// UpgradeOptions are the flags of `upgrade`.
type UpgradeOptions struct {
	// Feed is release.json's URL or path; local.json's feed if empty.
	Feed string `json:"feed,omitempty"`
	// Force upgrades even to the same or an older version (development
	// and the rollback rehearsal).
	Force bool `json:"force,omitempty"`
}

// minFreeBytes is the host free space the upgrade insists on beyond
// the downloads: the snapshot itself may cost nothing until blocks
// diverge (an APFS clone on macOS), which the new release's first
// writes make happen.
const minFreeBytes = 2 << 30

// staged is everything step 2 produced.
type staged struct {
	rel    Release
	chart  string // host path of the verified chart
	meta   guestapi.ChartMeta
	images Images
	dir    string // downloads/<version>, deleted when the upgrade ends
}

// Upgrade runs the journaled upgrade of docs/developers/desktop-app.md, "Upgrade
// the org". Nothing touches the org until the release's chart and images
// are fetched, verified and imported; from the journal on, a failure
// rolls the data disk back to the snapshot.
func (s *Supervisor) Upgrade(ctx context.Context, o UpgradeOptions, logf Logf) error {
	ctx, end, err := s.begin(ctx, "upgrade", false)
	if err != nil {
		return err
	}
	defer end()
	setStage(ctx, StageDownloading)
	logf = s.tee(ctx, logf)
	if j := s.State().Upgrade; j != nil {
		return fmt.Errorf("an upgrade journal from %s to %s is open at step %s; run `up` to recover it first", j.From, j.To, j.Step)
	}
	g, err := s.running()
	if err != nil {
		return err
	}
	from := s.State().Kivali
	if from == "" {
		return errors.New("not installed; nothing to upgrade")
	}

	// 1. Preflight.
	feed := o.Feed
	if feed == "" {
		feed = s.State().Feed
	}
	rel, err := s.fetchRelease(ctx, feed)
	if err != nil {
		return fmt.Errorf("fetch release.json: %w", err)
	}
	res := s.evaluate(rel, from)
	switch res.Status {
	case CheckUpgrade:
	case CheckUpToDate:
		if !o.Force {
			return errors.New(res.Message)
		}
		logf("%s; upgrading anyway (--force)", res.Message)
	default:
		return errors.New(res.Message)
	}
	gs, err := g.Status(ctx)
	if err != nil {
		return fmt.Errorf("preflight: guest agent: %w", err)
	}
	if gs.State != guestapi.StateReady || !gs.NodeReady {
		return fmt.Errorf("preflight: the VM is not healthy (state %s, node ready %v)", gs.State, gs.NodeReady)
	}
	if err := deploymentState(ctx, g, "", ""); err != nil {
		return fmt.Errorf("preflight: the org is not healthy: %w", err)
	}
	if free, err := s.o.Host.FreeBytes(s.dataDir()); err == nil && free < minFreeBytes {
		return fmt.Errorf("preflight: %d MiB free in %s; the upgrade needs at least %d MiB", free>>20, s.dataDir(), minFreeBytes>>20)
	}

	// 2. Fetch, verify and import. The org keeps running.
	sg, err := s.stage(ctx, g, feed, rel, logf)
	if err != nil {
		return err
	}

	// 3. Journal.
	setStage(ctx, StageSnapshot)
	snap := s.snapshotPath()
	if err := s.update(func(st *State) {
		st.Upgrade = &Journal{From: from, To: sg.meta.Version, Step: StepPrepared, Snapshot: snap, Started: s.o.Clock.Now().UTC()}
	}); err != nil {
		return err
	}
	logf("journal: upgrading %s -> %s (snapshot %s)", from, sg.meta.Version, snap)

	// 4. Quiesce.
	if err := s.step(StepQuiescing); err != nil {
		return err
	}
	if _, err := kubectl(ctx, g, "-n", Namespace, "delete", "pod", "-l", "app=kivali-agentpod", "--ignore-not-found", "--wait", "--timeout=120s"); err != nil {
		logf("deleting agent pods: %v (the clean shutdown deletes them too)", err)
	} else {
		logf("agent pods deleted")
	}
	running, _ := s.current()
	if err := s.stopLocked(ctx, logf); err != nil {
		// A hard stop leaves a disk that e2fsck will fix; the
		// snapshot is then of that disk, which is still the org.
		logf("stop: %v", err)
	}
	if running != nil {
		select {
		case <-running.Done():
		default:
			// A copy of a disk the guest is still writing is no
			// snapshot: a rollback to it would be a torn org.
			_ = s.update(func(st *State) { st.Upgrade = nil })
			s.dropDownloads(sg, logf)
			return fmt.Errorf("upgrade aborted, still on %s: the VM did not stop, so the data disk was not snapshotted", from)
		}
	}

	// 5. Snapshot.
	if err := s.step(StepSnapshotting); err != nil {
		return err
	}
	if err := removeIfExists(snap); err != nil {
		return err
	}
	err = s.o.Host.Snapshot(s.dataPath(), snap)
	if err == nil {
		// The snapshot must be durable before the journal says it is.
		err = s.o.Host.SyncDir(s.dataDir())
	}
	if err != nil {
		_ = removeIfExists(snap)
		s.dropDownloads(sg, logf)
		logf("snapshot failed: %v; starting the org as it was", err)
		_ = s.update(func(st *State) { st.Upgrade = nil })
		if berr := s.bootAndServe(ctx, "", logf); berr != nil {
			return fmt.Errorf("snapshot: %w; restart: %v", err, berr)
		}
		return fmt.Errorf("upgrade aborted, still on %s: snapshot: %w", from, err)
	}
	if err := s.o.Host.ExcludeFromBackup(snap); err != nil {
		logf("exclude %s from backups: %v", snap, err)
	}
	if err := s.update(func(st *State) { st.Upgrade.SnapshotComplete = true; st.Upgrade.Step = StepSnapshotted }); err != nil {
		return err
	}
	logf("data disk snapshotted to %s", snap)

	// 6 and 7. Start, apply, wait.
	setStage(ctx, StageInstalling)
	if err := s.step(StepApplying); err != nil {
		return err
	}
	if err := s.apply(ctx, sg, logf); err != nil {
		logf("upgrade failed: %v", err)
		_ = s.update(func(st *State) { st.Upgrade.Error = err.Error() })
		restored, rerr := s.rollbackLocked(ctx, logf)
		s.dropDownloads(sg, logf)
		switch {
		case rerr != nil && restored:
			return fmt.Errorf("upgrade failed (%v); the data disk was restored to %s, but starting it failed: %w", err, from, rerr)
		case rerr != nil:
			return fmt.Errorf("upgrade failed (%v) and the rollback failed: %w", err, rerr)
		}
		return fmt.Errorf("upgrade failed, you're still on %s (anything written since the upgrade started was discarded): %w", from, err)
	}

	// 8. Commit, then clean up.
	if err := s.update(func(st *State) { st.Upgrade.Step = StepCommitted; st.Kivali = sg.meta.Version }); err != nil {
		// The journal still says applying, so the next start rolls back
		// to the snapshot: an org left running now would take writes that
		// rollback silently discards. Stop it, so the loss is only what
		// the upgrade itself did, and say so.
		if serr := s.stopLocked(context.WithoutCancel(ctx), logf); serr != nil {
			logf("stop: %v", serr)
		}
		return fmt.Errorf("the upgrade to %s could not be recorded (%w); the org was stopped, and the next start rolls back to %s", sg.meta.Version, err, from)
	}
	if err := removeIfExists(snap); err != nil {
		logf("delete snapshot: %v", err)
	}
	s.dropDownloads(sg, logf)
	if err := s.update(func(st *State) { st.Upgrade = nil }); err != nil {
		return err
	}
	logf("Kivali %s installed", sg.meta.Version)
	return nil
}

// dropDownloads deletes a release's downloads once the upgrade ended
// either way; a retry downloads again.
func (s *Supervisor) dropDownloads(sg staged, logf Logf) {
	if sg.dir == "" {
		return
	}
	if err := os.RemoveAll(sg.dir); err != nil {
		logf("delete %s: %v", sg.dir, err)
	}
}

// step records the journal's step.
func (s *Supervisor) step(name string) error {
	return s.update(func(st *State) { st.Upgrade.Step = name })
}

// stage is step 2: download and verify the chart and the image bundle,
// import the images and check containerd has them.
func (s *Supervisor) stage(ctx context.Context, g Guest, feed string, rel Release, logf Logf) (staged, error) {
	sg := staged{rel: rel}
	img, ok := rel.Images[s.platform()]
	if !ok {
		return sg, fmt.Errorf("release %s has no images for %s", rel.Version, s.platform())
	}
	downloads := filepath.Join(s.o.ConfigDir, "downloads")
	dir := filepath.Join(downloads, strings.TrimPrefix(rel.Version, "v"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return sg, err
	}
	// Release assets are no secret, but nothing under the config
	// directory belongs in a backup.
	if err := s.o.Host.ExcludeFromBackup(downloads); err != nil {
		logf("exclude %s from backups: %v", downloads, err)
	}
	chart, err := s.download(ctx, feed, rel.Chart, dir)
	if err != nil {
		return sg, fmt.Errorf("chart: %w", err)
	}
	meta, err := guestapi.ChartInfoFile(chart)
	if err != nil {
		return sg, err
	}
	if meta.Name != "kivali" {
		return sg, fmt.Errorf("chart %s is %q, not kivali", chart, meta.Name)
	}
	if want := strings.TrimPrefix(rel.Version, "v"); strings.TrimPrefix(meta.Version, "v") != want {
		return sg, fmt.Errorf("release.json says version %s but its chart's Chart.yaml says %s; refusing the release", want, meta.Version)
	}
	logf("chart %s %s verified", meta.Name, meta.Version)
	bundle, err := s.download(ctx, feed, img, dir)
	if err != nil {
		return sg, fmt.Errorf("images: %w", err)
	}
	refs, err := guestapi.ImageRefsFile(bundle)
	if err != nil {
		return sg, err
	}
	im, err := pickImages(refs, false)
	if err != nil {
		return sg, fmt.Errorf("image bundle: %w", err)
	}
	logf("image bundle verified: %s", strings.Join(refs, ", "))
	if err := s.importFile(ctx, g, bundle, logf); err != nil {
		return sg, err
	}
	out, err := kubectlLikeCtr(ctx, g)
	if err != nil {
		return sg, err
	}
	for _, r := range refs {
		if !bytes.Contains(out, []byte(r+"\n")) {
			return sg, fmt.Errorf("image %s is not in containerd after the import", r)
		}
	}
	sg.chart, sg.meta, sg.images, sg.dir = chart, meta, im, dir
	return sg, nil
}

// kubectlLikeCtr lists containerd's images in the CRI namespace.
func kubectlLikeCtr(ctx context.Context, g Guest) ([]byte, error) {
	var out, errb bytes.Buffer
	code, err := g.Exec(ctx, guestapi.ExecSpec{Argv: []string{"k3s", "ctr", "-n", "k8s.io", "images", "ls", "-q"}}, nil, &out, &errb)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("ctr images ls: exit %d: %s", code, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// importFile streams an image archive (decompressed on the host) into
// the guest.
func (s *Supervisor) importFile(ctx context.Context, g Guest, p string, logf Logf) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	r, closeFn, err := guestapi.Decompress(f, p)
	if err != nil {
		return err
	}
	defer closeFn()
	logf("importing %s into k3s", filepath.Base(p))
	res, err := g.ImportImages(ctx, r)
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("import: exit %d: %s", res.ExitCode, strings.TrimSpace(res.Output))
	}
	logf("import finished")
	return nil
}

// apply is steps 6 and 7: boot (the old release comes up first), place
// the new chart, rewrite the manifest and wait for the new release.
func (s *Supervisor) apply(ctx context.Context, sg staged, logf Logf) error {
	if err := s.bootLocked(ctx, logf); err != nil {
		return err
	}
	if err := s.ensureForwardLocked(s.State().port(), logf); err != nil {
		return err
	}
	g, err := s.running()
	if err != nil {
		return err
	}
	if _, err := readManifest(ctx, g); err != nil {
		return err
	}
	gs, err := g.Status(ctx)
	if err != nil {
		return err
	}
	if _, err := s.placeChart(ctx, g, gs, sg.chart, logf); err != nil {
		return err
	}
	krepo, ktag := guestapi.SplitRef(sg.images.Kivali)
	erepo, etag := guestapi.SplitRef(sg.images.Egress)
	if _, err := rewriteManifest(ctx, g, chartFileName(sg.meta), func(vals map[string]any) {
		setValue(vals, []string{"image", "repository"}, guestapi.ShortRepo(krepo))
		setValue(vals, []string{"image", "tag"}, ktag)
		setValue(vals, []string{"egress", "image", "repository"}, guestapi.ShortRepo(erepo))
		setValue(vals, []string{"egress", "image", "tag"}, etag)
	}); err != nil {
		return err
	}
	logf("HelmChart now points at %s, images %s", chartFileName(sg.meta), ktag)
	setStage(ctx, StageStarting)
	return s.waitServing(ctx, sg.meta.Version, sg.images.Kivali, logf)
}

// readManifest reads the HelmChart manifest from the data disk.
func readManifest(ctx context.Context, g Guest) ([]byte, error) {
	cur, ok, err := readGuestFile(ctx, g, guestapi.DataDir+"/"+ManifestRel)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("the HelmChart manifest is missing from the data disk")
	}
	return cur, nil
}

// rewriteManifest rewrites the HelmChart manifest on the data disk with
// edit applied to its values, pointing at chartFile ("" keeps the
// chart), and reports whether the values changed; the Helm controller
// applies the new manifest. The values (the owner, the session key) go
// from the org's disk through memory and back, never anywhere else.
func rewriteManifest(ctx context.Context, g Guest, chartFile string, edit func(vals map[string]any)) (bool, error) {
	cur, err := readManifest(ctx, g)
	if err != nil {
		return false, err
	}
	file, vals, err := ParseManifest(cur)
	if err != nil {
		return false, err
	}
	_, before, err := ParseManifest(cur)
	if err != nil {
		return false, err
	}
	edit(vals)
	if chartFile == "" {
		chartFile = file
	}
	manifest, err := RenderManifest(chartFile, vals)
	if err != nil {
		return false, err
	}
	if err := g.WriteFile(ctx, ManifestRel, 0o600, bytes.NewReader(manifest)); err != nil {
		return false, err
	}
	return !reflect.DeepEqual(before, vals), nil
}

// rollbackLocked puts the snapshot back and starts the org on it.
// It reports whether the data disk was restored, so a failure to start
// the restored disk is not reported as a failed rollback.
func (s *Supervisor) rollbackLocked(ctx context.Context, logf Logf) (bool, error) {
	setStage(ctx, StageRollingBack)
	logf("rolling back to %s", s.State().Upgrade.From)
	// Stopped first: a failure to record the step below must not leave
	// the failed release running on a disk the journal will roll back.
	if m, _ := s.current(); m != nil {
		if err := s.stopLocked(ctx, logf); err != nil {
			logf("stop: %v", err)
		}
	}
	if err := s.step(StepRollingBack); err != nil {
		return false, err
	}
	j := *s.State().Upgrade
	if err := s.restoreSnapshot(&j, logf); err != nil {
		return false, err
	}
	return true, s.bootAndServe(ctx, j.From, logf)
}

// restoreSnapshot swaps the snapshot over the data disk (replacing the
// failed disk) and closes the journal. A complete snapshot that is
// missing is taken as already swapped back only at step rolling-back,
// where a crash between the rename and the journal update leaves
// exactly that; at any other step the data disk may be the half-applied
// new release, so it refuses and keeps the journal.
func (s *Supervisor) restoreSnapshot(j *Journal, logf Logf) error {
	switch {
	case exists(j.Snapshot):
		if j.Step != StepRollingBack {
			if err := s.step(StepRollingBack); err != nil {
				return err
			}
		}
		if err := s.o.Host.Rename(j.Snapshot, s.dataPath()); err != nil {
			return fmt.Errorf("restore snapshot: %w", err)
		}
		logf("data disk restored from the snapshot")
	case j.Step == StepRollingBack:
		logf("the snapshot was already swapped back before the interruption")
	default:
		return fmt.Errorf("cannot recover the upgrade %s -> %s: the journal (step %s) records a complete snapshot at %s, but that file is missing, "+
			"so %s may hold the half-applied %s. Nothing was booted or changed and the journal is kept. Either "+
			"(a) put the snapshot file back at %s and run `kivali-supervisor up` again, which rolls back to %s; or "+
			"(b) to boot %s as it is, edit %s so that \"upgrade\" is null, then run `kivali-supervisor up` again. "+
			"serve does not need to be stopped for either (up re-reads local.json), but run no other command in between",
			j.From, j.To, j.Step, j.Snapshot, s.dataPath(), j.To,
			j.Snapshot, j.From, s.dataPath(), filepath.Join(s.o.ConfigDir, "local.json"))
	}
	return s.update(func(st *State) { st.Upgrade = nil; st.Kivali = j.From })
}

// bootAndServe boots, forwards and waits for the org (at chartVersion
// when given).
func (s *Supervisor) bootAndServe(ctx context.Context, chartVersion string, logf Logf) error {
	if err := s.bootLocked(ctx, logf); err != nil {
		return err
	}
	if err := s.ensureForwardLocked(s.State().port(), logf); err != nil {
		return err
	}
	return s.waitServing(ctx, chartVersion, "", logf)
}

// recoverLocked finishes or undoes an upgrade a crash interrupted. The
// VM is not running.
func (s *Supervisor) recoverLocked(logf Logf) error {
	j := s.State().Upgrade
	if j == nil {
		return nil
	}
	logf("found an upgrade journal %s -> %s at step %s", j.From, j.To, j.Step)
	defer func() {
		if s.State().Upgrade == nil {
			_ = os.RemoveAll(filepath.Join(s.o.ConfigDir, "downloads", j.To))
		}
	}()
	switch {
	case j.Step == StepCommitted:
		if err := removeIfExists(j.Snapshot); err != nil {
			return err
		}
		logf("the upgrade had committed; snapshot deleted")
		return s.update(func(st *State) { st.Upgrade = nil; st.Kivali = j.To })
	case j.SnapshotComplete:
		logf("rolling back to %s", j.From)
		return s.restoreSnapshot(j, logf)
	default:
		// Crashed before the snapshot completed: the data disk was
		// never touched. Delete any partial snapshot.
		if err := removeIfExists(j.Snapshot); err != nil {
			return err
		}
		logf("the upgrade stopped before the snapshot completed; the org is unchanged on %s", j.From)
		return s.update(func(st *State) { st.Upgrade = nil })
	}
}

func removeIfExists(p string) error {
	if p == "" {
		return nil
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
