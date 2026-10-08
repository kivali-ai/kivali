package web

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/agentpod"
	"github.com/kivali-ai/kivali/internal/backup"
	"github.com/kivali-ai/kivali/internal/files"
	"github.com/kivali-ai/kivali/internal/store"
)

// What a backup holds, and the manifest a restore checks it against,
// are internal/backup's. These handlers are the one way an org is
// backed up and restored: the app's pages call them, and so does
// kivali-supervisor (signed in through the desktop handoff).

// restoreFreeReserve is the room a restore leaves on the data volume:
// an upload is staged, and an archive unpacked, only while this much
// would still be free. There is no fixed size cap: the volume is the
// limit, and an archive's manifest (checked against every member's
// content before anything is written) bounds what it unpacks to, so a
// zip bomb cannot write past what it declares.
const restoreFreeReserve = 256 << 20

// restoreRoom is how many bytes a restore may still write on the data
// volume, or -1 when the free space cannot be read (no limit).
func (s *Server) restoreRoom() int64 {
	free := diskFree(s.Store.Root())
	if free < 0 {
		return -1
	}
	return max(free-restoreFreeReserve, 0)
}

// handleBackupDownload streams a zip of DataDir, its manifest last, as
// a file download, with no temp file on the server. The status line is
// long gone by the time a file could fail to read, so a failure aborts
// the connection instead: the browser reports a failed download rather
// than saving a zip that is quietly missing a file.
func (s *Server) handleBackupDownload(w http.ResponseWriter, r *http.Request) {
	var orgName string
	if br, err := s.Store.ReadBranding(); err != nil {
		log.Printf("backup: branding: %v (the file name leaves the org's name out)", err)
	} else {
		orgName = br.CompanyName
	}
	filename := backupFilename(orgName, time.Now())
	w.Header().Set("content-type", "application/zip")
	w.Header().Set("content-disposition", "attachment; filename="+filename)
	// The archive carries graph/index.json; make it the current index.
	if err := s.Store.Graph().Flush(); err != nil {
		log.Printf("backup: %s: graph index: %v (the archive carries the last one written)", filename, err)
	}
	// Send the headers now: the browser starts its download (and shows
	// its progress) at the attachment header, not at the first 4 KiB.
	_ = http.NewResponseController(w).Flush()
	m, err := backup.WriteZip(s.Store.Root(), w, s.VersionName)
	if err != nil {
		log.Printf("backup: %s aborted: %v", filename, err)
		panic(http.ErrAbortHandler)
	}
	var total int64
	for _, f := range m.Files {
		total += f.Size
	}
	log.Printf("backup: wrote %d files, %d bytes to %s", len(m.Files), total, filename)
}

// backupFilenameNameMax caps the org's part of a backup's file name.
const backupFilenameNameMax = 40

// backupFilename names a backup after its org and the moment it was
// taken: kivali-backup-acme-corp-20261006T192333Z.zip. The name keeps
// only ASCII letters and digits, lowercased, with a dash for every run
// of anything else, so it is safe in a Content-Disposition header and
// on any filesystem. A name with nothing left is left out.
func backupFilename(orgName string, now time.Time) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(orgName) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
			continue
		}
		dash = true
	}
	name := b.String()
	if len(name) > backupFilenameNameMax {
		name = strings.TrimRight(name[:backupFilenameNameMax], "-")
	}
	stamp := now.UTC().Format("20060102T150405Z")
	if name == "" {
		return "kivali-backup-" + stamp + ".zip"
	}
	return "kivali-backup-" + name + "-" + stamp + ".zip"
}

// shouldExcludeFromBackup is a thin alias for the shared
// files.ShouldExcludeFromBackup so this package's call sites and tests
// keep reading naturally.
func shouldExcludeFromBackup(rel string) bool {
	return files.ShouldExcludeFromBackup(rel)
}

// checkRestoreAllowed refuses a restore unless the deployment is fresh
// (IsFreshForRestore) and no chat turn is in flight. Even on a fresh
// deployment, CoS's first chat could fire concurrently with the
// restore (Send on the wizard's CoS chat in one tab while uploading the
// archive in another); an active hub is an in-flight turn that would
// append to chat.jsonl mid-unpack, then get clobbered by the backup's
// chat.jsonl.
func (s *Server) checkRestoreAllowed() error {
	if !s.IsFreshForRestore() {
		return refuseWho(http.StatusConflict,
			"restore is only available on a fresh deployment — chief-of-staff's chat.jsonl must be empty and no other agents may exist",
			"no one; restore into a new deployment instead")
	}
	s.streamMu.Lock()
	hubsActive := 0
	for _, hub := range s.chatHubs {
		if !hub.isCompleted() {
			hubsActive++
		}
	}
	s.streamMu.Unlock()
	if hubsActive > 0 {
		return refuseWho(http.StatusConflict,
			"restore refused: a chat turn is in flight — wait for it to complete and retry",
			"you, once the turn finishes")
	}
	return nil
}

// stageRestoreUpload streams the backup in a multipart upload (field
// "archive", or "file") into a temp file on the data volume, and
// answers the request itself when it cannot. A zip needs random
// access, so the upload has to land somewhere whole; the data volume
// is sized for the org the archive unpacks into, where the server's
// temp directory is a small emptyDir (1 GiB in the chart) that a
// parsed form would spill a large upload into, and a second copy of it
// after that. The .tmp- prefix keeps it out of a backup taken
// meanwhile. The caller removes the file.
func (s *Server) stageRestoreUpload(w http.ResponseWriter, r *http.Request) (path, filename string, ok bool) {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "multipart/form-data" {
		writeAPIError(w, http.StatusUnsupportedMediaType,
			"send the upload as multipart/form-data, not "+cmp.Or(mt, "an untyped body"), whoDevelopers)
		return "", "", false
	}
	// The staged copy and what it unpacks to share the data volume; the
	// upload may take what is free beyond the reserve, and the restore
	// checks the unpacked size against what is left after it.
	room := s.restoreRoom()
	if room >= 0 {
		r.Body = http.MaxBytesReader(w, r.Body, room+multipartOverhead)
	}
	tooBig := func() {
		writeAPIError(w, http.StatusInsufficientStorage,
			fmt.Sprintf("the backup does not fit: the data volume has %d bytes to spare for it", room), "you: give the data volume more space")
	}
	mr, err := r.MultipartReader()
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "the request body is not a form", whoDevelopers)
		return "", "", false
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			writeAPIError(w, http.StatusBadRequest, "no backup was uploaded", whoDevelopers)
			return "", "", false
		}
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			tooBig()
			return "", "", false
		}
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "the request body is not a form", whoDevelopers)
			return "", "", false
		}
		if part.FileName() == "" || (part.FormName() != "archive" && part.FormName() != "file") {
			continue
		}
		f, err := os.CreateTemp(s.Store.Root(), ".tmp-restore-*")
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "the backup could not be staged: "+err.Error(), whoDevelopers)
			return "", "", false
		}
		var src io.Reader = part
		if room >= 0 {
			src = io.LimitReader(part, room+1)
		}
		n, err := io.Copy(f, src)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		switch {
		case errors.As(err, &maxErr) || (err == nil && room >= 0 && n > room):
			tooBig()
		case err != nil:
			writeAPIError(w, http.StatusBadRequest, "the backup could not be read: "+err.Error(), whoYou)
		default:
			return f.Name(), part.FileName(), true
		}
		_ = os.Remove(f.Name())
		return "", "", false
	}
}

// restoreArchive unpacks a backup zip staged at path into DataDir and
// then does what a boot does with someone else's data. Refused (see
// checkRestoreAllowed) unless the deployment is fresh; an archive that
// is not a zip, does not match its manifest, or has an entry escaping
// DataDir is refused before anything is written.
func (s *Server) restoreArchive(ctx context.Context, path, filename string) error {
	if err := s.checkRestoreAllowed(); err != nil {
		return err
	}
	// A zip only: it is checked whole before anything is written. A
	// tarball can only be checked as it is written, and a failure part
	// way would leave this deployment half-restored and no longer fresh.
	var magic [4]byte
	if f, err := os.Open(path); err == nil {
		_, _ = io.ReadFull(f, magic[:])
		_ = f.Close()
	}
	if string(magic[:]) != "PK\x03\x04" {
		return refuse(http.StatusBadRequest, "this is not a Kivali backup: a backup is the .zip the app downloads")
	}
	var n int64
	if fi, err := os.Stat(path); err == nil {
		n = fi.Size()
	}
	// What the volume can take beside the staged upload; -1 (unknown)
	// sets no limit, and none left refuses any archive with a file.
	var opt backup.Options
	switch room := s.restoreRoom(); {
	case room > 0:
		opt.MaxTotalBytes = room
	case room == 0:
		opt.MaxTotalBytes = 1
	}
	src, closeSrc, err := backup.OpenArchive(path, opt)
	if errors.Is(err, backup.ErrNoRoom) {
		return refuseWho(http.StatusInsufficientStorage, err.Error(), "you: give the data volume more space")
	}
	if err != nil {
		return asRefusal(err)
	}
	defer closeSrc()
	// Nothing of this deployment's graph may be written over the
	// archive's once it is unpacked; Reload below reads the archive's.
	if err := s.Store.Graph().Flush(); err != nil {
		log.Printf("restore: graph index: %v", err)
	}
	// A fresh deployment may already have published files (a seeded
	// chief-of-staff's, say). Left in place they would stay published
	// beside the archive's.
	if err := files.EmptyPublished(s.Store.Root()); err != nil {
		log.Printf("restore: empty the published trees: %v", err)
		return fmt.Errorf("empty the published trees before restoring: %w", err)
	}
	// The archive's handbook replaces whatever setup wrote here: an
	// archive without one restores a deployment without one.
	if err := s.Store.RemoveHandbook(); err != nil {
		return fmt.Errorf("remove the handbook before restoring: %w", err)
	}
	// What this deployment's setup chose (team kind, what the owner is
	// called), for an archive that does not carry it; see below.
	prior, err := s.Store.ReadBranding()
	if err != nil {
		log.Printf("restore: read branding: %v", err)
	}
	res, err := backup.Restore(s.Store.Root(), src, opt)
	if err != nil {
		log.Printf("restore: %s failed after writing %d files: %v", filename, res.Files, err)
		return asRefusal(err)
	}
	log.Printf("restore: unpacked %d files and %d directories from %s (%d bytes); every file matches the manifest", res.Files, res.Dirs, filename, n)
	// Read what was unpacked with the store's own readers, as an
	// operator would before trusting it: what this binary cannot read
	// is named in the log. The data is already written, so it is
	// reported, not refused.
	if inv, err := backup.Audit(s.Store.Root()); err != nil {
		log.Printf("restore: audit: %v", err)
	} else {
		for _, p := range inv.Problems {
			log.Printf("restore: audit: %s", p)
		}
		log.Printf("restore: audit: read %d files, %d agents; %d problems", inv.Files, inv.ActiveAgents, len(inv.Problems))
	}

	// The archive's branding.yaml replaced this deployment's. One that
	// carries no team kind or chosen owner name keeps what this
	// deployment's setup chose, which boot would not put back once it
	// was set in the app.
	if br, err := s.Store.ReadBranding(); err != nil {
		log.Printf("restore: read branding: %v", err)
	} else {
		if br.TeamKind == "" && prior.TeamKind != "" {
			if err := s.Store.WriteTeamKind(prior.TeamKind); err != nil {
				log.Printf("restore: keep the team kind: %v", err)
			}
		}
		if !br.OwnerNameSet && prior.OwnerNameSet {
			if err := s.Store.WriteOwnerName(prior.OwnerName); err != nil {
				log.Printf("restore: keep the owner's name: %v", err)
			}
		}
	}
	// The archive carries data/skills/ as the binary that wrote it
	// installed them, which may be another version's built-in skills.
	// Boot writes this binary's back; so does a restore, before
	// the sync below mirrors the skills into each agent's /files/.
	if err := s.Store.InstallBuiltinSkills(); err != nil {
		log.Printf("restore: install built-in skills: %v", err)
	}
	// The restored allowed_egress.yaml is on disk; the egress proxy
	// reads its own copy, which boot writes and nothing else would
	// until the next edit of the allowlist.
	if err := s.Store.SyncEgressAllowlistToProxy(); err != nil {
		log.Printf("restore: egress allowlist: %v", err)
	}

	// Regenerate per-agent /files/ symlinks. Backup skips
	// non-regular files (symlinks, devices); without this pass, the
	// restored agents would have empty past-chats/ and attachments/
	// views under /files/ until something else triggered a sync.
	if agents, err := s.Store.ListActiveAgents(); err == nil {
		for _, a := range agents {
			if err := s.Store.SyncAgentFilesystem(a.Slug); err != nil {
				log.Printf("restore: files sync %s: %v", a.Slug, err)
			}
			// Belt-and-suspenders: tell each agent pod that its
			// cached role.md / agent_memory.md may be stale. The
			// ETag-validated path-cache will catch the change on its
			// own (sha changes when bytes change), so this is purely
			// an "evict immediately" hint. The CoS pod is the most
			// common live cache holder during a restore — it pre-
			// existed the upload and may still be running.
			if s.AgentpodHub != nil {
				_ = s.PublishAgentpodEvent(a.Slug, agentpod.EventCacheInvalidate, agentpod.CacheInvalidateEvent{
					Paths: []string{
						"agent/" + a.Slug + "/role",
						"agent/" + a.Slug + "/memory",
					},
				})
			}
		}
	}

	// A restore is a boot from someone else's data, so it ends the way
	// main.go's boot does. The graph maintainer reloads the restored
	// index and version log and scans the restored files: a backup is
	// not atomic, so the index it carried can be a moment older or
	// newer than the artifacts it describes, and the scan reconciles
	// by content hash.
	// The tracker routes any wake the archive carried in
	// assignments/pending/; without this they would wait for the next
	// process restart, and nothing else on the restore path reads that
	// directory. And the archive's agents were pinned against the
	// catalog of whatever binary wrote it: a pin on a model this
	// binary has retired moves along its lineage here, exactly as it
	// would at boot, so a restored agent never runs on a model the
	// picker does not offer.
	if err := s.Store.Graph().Reload(); err != nil {
		log.Printf("restore: graph reload: %v", err)
	}
	// The restored files carry the modification times they were backed
	// up with; nothing derived from the old ones may stand for them.
	s.forgetSnapshotCache()
	if err := s.ReconcileAssignments(ctx); err != nil {
		log.Printf("restore: assignments reconcile: %v", err)
	}
	moved, err := s.Store.UpgradeModelPins(s.Provider.Current)
	for _, m := range moved {
		log.Printf("restore: model pin: %s %s → %s", m.Slug, m.From, m.To)
	}
	if err != nil {
		log.Printf("restore: upgrade model pins: %v", err)
	}
	// The archive replaced the assignment files without going through
	// WriteAssignment, so the store's post-write hook never fired; the
	// snapshot's goals and the web app's work views would describe the
	// replaced data until the next assignment moved.
	// onAssignmentWrite is that hook: bump assignments_version and
	// republish the snapshot. usage.jsonl was replaced the
	// same way, so the spend readouts' cache goes too.
	s.bumpUsageVersion()
	s.onAssignmentWrite()
	// message_queue.json was replaced the same way: the auto-release
	// scheduler learns of queued messages and their deadlines only
	// from this hook or a boot.
	s.onMessageQueueWrite()

	// A backup taken mid-turn carries that agent's active-turn marker.
	// Boot turns one into a disruption row and resumes the agent once
	// its pod is up; a restore does the same, and brings up the pods
	// boot would have: a restored agent has none until something
	// provisions it, and a turn for an agent without a pod is dropped.
	recovered := s.RecoverInterruptedTurns()
	s.restoreBG.Add(1)
	go func() {
		defer s.restoreBG.Done()
		if s.AgentPod != nil {
			pctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			s.BulkProvisionAgentPods(pctx)
		}
		s.SpawnRecovered(recovered)
	}()
	return nil
}

// safeRestoreTarget resolves an archive member's name under root,
// refusing any that would escape it. See backup.SafeTarget.
func safeRestoreTarget(root, entryName string) (string, error) {
	return backup.SafeTarget(root, entryName)
}

// asRefusal passes a refusal through and makes anything else one: a
// restore fails on what the archive holds, which the uploader can see.
func asRefusal(err error) error {
	var r *orgRefusal
	if errors.As(err, &r) {
		return err
	}
	return refuse(http.StatusBadRequest, err.Error())
}

// IsFreshForRestore reports whether a restore would overwrite only
// seed state. The rule: active agents are limited to {ceo,
// chief-of-staff} and chief-of-staff's chat.jsonl has no entries.
// This is the bar both GET /api/v1/setup (restore_available) and the
// restore handler check against — the app offers restore only when
// true, the handler 409s when false, so a stale upload can't clobber
// an active deployment.
//
// Archived agents and archived chat generations are ignored — they're
// records a restore would layer under, not over.
func (s *Server) IsFreshForRestore() bool {
	if s.Store == nil {
		return false
	}
	actives, err := s.Store.ListActiveAgents()
	if err != nil {
		return false
	}
	for _, a := range actives {
		if a.Slug == agent.CEOSlug || a.Slug == "chief-of-staff" {
			continue
		}
		return false
	}
	hist, err := s.Store.ReadChatHistory("chief-of-staff")
	if err == nil && len(hist) > 0 {
		return false
	}
	// ErrNotFound (no chat file yet) reads as fresh. Any other error
	// we treat conservatively as not-fresh so we don't overwrite
	// potentially-populated state on a read hiccup.
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return false
	}
	return true
}
