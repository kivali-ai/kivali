package web

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kivali-ai/kivali/internal/seed"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// The sign-in screen's data and the four-step setup, as JSON. Setup's
// writes run through setup.go and onboarding.go.

// wireAPISetupRoutes mounts setup on the API mux. GET /api/v1/login is not here: it answers without a session, so
// it sits on the public mux (web.go).
func (s *Server) wireAPISetupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/setup", s.handleAPISetup)
	mux.HandleFunc("POST /api/v1/setup/org", s.handleAPISetupOrg)
	mux.HandleFunc("POST /api/v1/setup/org/logo", s.handleAPISetupLogo)
	mux.HandleFunc("POST /api/v1/setup/files", s.handleAPISetupFiles)
	mux.HandleFunc("DELETE /api/v1/setup/files/{sha}", s.handleAPISetupFileDelete)
	mux.HandleFunc("POST /api/v1/setup/seed-cos", s.handleAPISetupSeedCoS)
	mux.HandleFunc("GET /api/v1/setup/progress", s.handleAPISetupProgress)
}

// ---- GET /api/v1/login (public) ----

// handleAPILogin serves GET /api/v1/login without a session: the org's
// name and logo flag, whether sign-in is configured, and whether the
// server runs in dev mode. Nothing else, since anyone can read it.
func (s *Server) handleAPILogin(w http.ResponseWriter, _ *http.Request) {
	br, _ := s.Store.ReadBranding()
	w.Header().Set("cache-control", "no-store")
	writeJSON(w, http.StatusOK, apitypes.Login{
		Org:       apitypes.MeOrg{Name: br.CompanyName, HasLogo: br.HasFavicon},
		AuthReady: s.OAuth.Ready(),
		DevMode:   s.DevUser != "",
	})
}

// ---- GET /api/v1/setup ----

func (s *Server) handleAPISetup(w http.ResponseWriter, _ *http.Request) {
	s.writeSetupState(w, http.StatusOK)
}

// writeSetupState answers with setup as it stands; every setup write
// answers with it too.
func (s *Server) writeSetupState(w http.ResponseWriter, status int) {
	st, err := s.setupState()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "could not read setup: "+err.Error(), whoServer)
		return
	}
	writeJSON(w, status, st)
}

func (s *Server) setupState() (apitypes.Setup, error) {
	br, _ := s.Store.ReadBranding()
	files, err := s.Store.ListProjectFiles()
	if err != nil {
		return apitypes.Setup{}, err
	}
	handbook, err := s.Store.ReadHandbook()
	if errors.Is(err, store.ErrNotFound) {
		handbook, err = seed.HandbookFor(br.TeamKind), nil
	}
	if err != nil {
		return apitypes.Setup{}, err
	}
	hired := s.cosExists()
	st := apitypes.Setup{
		Needed: !hired,
		Org:    apitypes.MeOrg{Name: br.CompanyName, HasLogo: br.HasFavicon, OwnerName: br.OwnerName},
		Kind:   apitypes.TeamKind(br.TeamKind),
		Files:  make([]apitypes.SetupFile, 0, len(files)),
		CoS: apitypes.SetupCoS{
			Exists:            hired,
			DefaultRoleMD:     seed.ChiefOfStaffRoleFor(br.TeamKind),
			DefaultHandbookMD: handbook,
		},
		Credential:       s.setupCredential(context.Background()),
		RestoreAvailable: s.IsFreshForRestore(),
	}
	for _, f := range files {
		st.Files = append(st.Files, apitypes.SetupFile{
			SHA:       f.SHA,
			Name:      f.OriginalName,
			SizeBytes: f.Size,
			Extracted: f.CanonicalName != "",
		})
	}
	st.Step = setupStep(hired, s.seedTracker().active(), br.CompanyName != "", len(files) > 0)
	return st, nil
}

// setupStep is where setup resumes. Welcome, org and files are all
// optional, so "incomplete" is read from what is set: nothing yet is
// welcome, files without a name is org, a name without files is files.
// A hire that is running or failed holds setup on its last step.
func setupStep(hired, hiring, named, hasFiles bool) apitypes.SetupStep {
	switch {
	case hired:
		return apitypes.SetupStepDone
	case hiring:
		return apitypes.SetupStepCoS
	case !named && !hasFiles:
		return apitypes.SetupStepWelcome
	case !named:
		return apitypes.SetupStepOrg
	case !hasFiles:
		return apitypes.SetupStepFiles
	default:
		return apitypes.SetupStepCoS
	}
}

// setupCredential says whether a model is connected and how calls are
// paid for, and when no model is connected, who can fix it, in words:
// the provider's Credentials.Status and Guidance. Ready is whether a
// driver is wired (s.Claude is nil only in tests); whether its
// credential works is only known on the first call.
func (s *Server) setupCredential(ctx context.Context) apitypes.SetupCredential {
	creds := s.Provider.Credentials()
	st, err := creds.Status(ctx)
	if err != nil {
		log.Printf("setup: credential status: %v", err)
	}
	c := apitypes.SetupCredential{
		Ready:    s.Claude != nil,
		Provider: s.Provider.Name(),
		Present:  st.Present,
		Who:      st.Who,
		Billing:  st.Billing,
	}
	if !c.Ready {
		c.Guidance = notConnectedGuidance(creds.Guidance())
	}
	return c
}

// notConnectedGuidance is the one sentence setup shows while no model
// is connected: that, then the provider's own sentence on who can fix
// it, joined into one.
func notConnectedGuidance(who string) string {
	who = strings.TrimSpace(who)
	if who == "" {
		return "No model is connected yet."
	}
	r, size := utf8.DecodeRuneInString(who)
	return "No model is connected yet; " + string(unicode.ToLower(r)) + who[size:]
}

// ---- POST /api/v1/setup/org, /org/logo ----

func (s *Server) handleAPISetupOrg(w http.ResponseWriter, r *http.Request) {
	var req apitypes.SetupOrgRequest
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	if err := s.setCompanyName(req.Name); err != nil {
		var refusal *orgRefusal
		if errors.As(err, &refusal) {
			writeAPIError(w, http.StatusBadRequest,
				fmt.Sprintf("the org name is over %d characters", maxCompanyNameRunes), whoYou)
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "could not save the org name: "+err.Error(), whoServer)
		return
	}
	s.writeSetupState(w, http.StatusOK)
}

// setupLogoFields are the multipart fields a logo may arrive in.
var setupLogoFields = []string{"logo", "file", "favicon"}

func (s *Server) handleAPISetupLogo(w http.ResponseWriter, r *http.Request) {
	if !parseAPIMultipart(w, r, maxFaviconBytes+1<<10, "the logo") {
		return
	}
	defer removeMultipartForm(r)
	fhs := multipartFiles(r, setupLogoFields)
	if len(fhs) == 0 {
		writeAPIError(w, http.StatusBadRequest, "no image was attached", whoDevelopers)
		return
	}
	f, err := fhs[0].Open()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "could not read the upload: "+err.Error(), whoServer)
		return
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxFaviconBytes))
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "could not read the upload: "+err.Error(), whoServer)
		return
	}
	if err := s.setLogo(raw); err != nil {
		var refusal *orgRefusal
		if errors.As(err, &refusal) {
			writeAPIError(w, http.StatusBadRequest, "that image cannot be used as a logo: "+refusal.Msg, whoYou)
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "could not save the logo: "+err.Error(), whoServer)
		return
	}
	s.writeSetupState(w, http.StatusOK)
}

// ---- POST /api/v1/setup/files, DELETE /api/v1/setup/files/{sha} ----

// setupFileFields are the multipart fields project files may arrive in:
// "files[]" is what the web app sends; the others match the form and
// any FormData spelling.
var setupFileFields = []string{"files[]", "files", "file"}

func (s *Server) handleAPISetupFiles(w http.ResponseWriter, r *http.Request) {
	if !parseAPIMultipart(w, r, maxUploadBytes, "the files") {
		return
	}
	defer removeMultipartForm(r)
	fhs := multipartFiles(r, setupFileFields)
	if len(fhs) == 0 {
		writeAPIError(w, http.StatusBadRequest, "no files were attached", whoDevelopers)
		return
	}
	if err := s.addSetupFiles(r.Context(), fhs); err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), whoServer)
		return
	}
	s.writeSetupState(w, http.StatusOK)
}

func (s *Server) handleAPISetupFileDelete(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	// Look the file up first: an unknown sha is a 404, and only a sha
	// the index holds ever reaches the store's path join.
	if _, err := s.Store.GetProjectFile(sha); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "that file is not in the project files", whoNoOne)
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "could not read the project files: "+err.Error(), whoServer)
		return
	}
	if err := s.removeProjectFile(sha); err != nil {
		writeAPIError(w, http.StatusInternalServerError, "could not remove the file: "+err.Error(), whoServer)
		return
	}
	s.writeSetupState(w, http.StatusOK)
}

// setupFormMemory is how much of a setup upload is held in memory;
// larger files spill to temporary files the server removes when the
// request ends.
const setupFormMemory = 32 << 20

// parseAPIMultipart parses a multipart body of at most maxBytes, or
// writes the refusal: 415 for anything but multipart/form-data, 413
// over the cap. what names the upload in the 413's sentence.
func parseAPIMultipart(w http.ResponseWriter, r *http.Request, maxBytes int64, what string) bool {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "multipart/form-data" {
		writeAPIError(w, http.StatusUnsupportedMediaType,
			"send the upload as multipart/form-data, not "+cmp.Or(mt, "an untyped body"), whoDevelopers)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	if err := r.ParseMultipartForm(setupFormMemory); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeAPIError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("%s are over %d MB", what, maxBytes>>20), whoYou)
			return false
		}
		writeAPIError(w, http.StatusBadRequest, "the request body is not a form", whoDevelopers)
		return false
	}
	return true
}

// removeMultipartForm deletes the temporary files a parsed multipart
// form spilled to disk (anything past setupFormMemory). net/http removes
// them only for the *Request it passed to the handler chain
// (server.go's finishRequest reads w.req), and every middleware that
// adds to the context (the session, DevBypass) hands the handler a
// shallow copy whose MultipartForm the server never sees. So a handler
// that parses a multipart body defers this itself.
func removeMultipartForm(r *http.Request) {
	if r.MultipartForm != nil {
		_ = r.MultipartForm.RemoveAll()
	}
}

// multipartFiles is every file under any of fields, in field order.
func multipartFiles(r *http.Request, fields []string) []*multipart.FileHeader {
	var out []*multipart.FileHeader
	if r.MultipartForm == nil {
		return out
	}
	for _, f := range fields {
		out = append(out, r.MultipartForm.File[f]...)
	}
	return out
}

// ---- POST /api/v1/setup/seed-cos, GET /api/v1/setup/progress ----

// handleAPISetupSeedCoS starts hiring the Chief of Staff in the
// background and answers 202; GET /api/v1/setup/progress follows it.
// One hire at a time: a second request while one runs, or after the
// Chief of Staff exists, is a 409.
func (s *Server) handleAPISetupSeedCoS(w http.ResponseWriter, r *http.Request) {
	var req apitypes.SeedCoSRequest
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	if s.Claude == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "no model is connected yet", whoServer)
		return
	}
	plan, ok := s.cosSeedFromRequest(w, req)
	if !ok {
		return
	}
	tr := s.seedTracker()
	run, err := tr.begin(s.clk().Now(), plan.stageLabels(), s.cosExists)
	switch {
	case errors.Is(err, errCoSHired):
		writeAPIError(w, http.StatusConflict, "your Chief of Staff is already hired", whoNoOne)
		return
	case err != nil:
		writeAPIError(w, http.StatusConflict, "your Chief of Staff is already being hired", "no one; it is still running")
		return
	}
	// Detached from the request: the hire outlives this response.
	go func() {
		if err := s.runSeed(context.Background(), plan, tr, run); err != nil {
			log.Printf("setup: hire chief of staff: %v", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, apitypes.Empty{})
}

// runSeed runs a hire begun on tr and always ends the run, so a panic
// in the hire cannot leave the tracker "running" and refuse every later
// hire with a 409. A panic is reported as a failed hire, at the stage
// it reached, and returned as an error rather than re-raised.
//
// The hire runs under a deadline of s.SeedTimeout on the server's
// clock: a model call that hangs is cancelled, and the hire ends as
// failed at the stage it reached, with the deadline as its cause.
func (s *Server) runSeed(ctx context.Context, plan cosSeed, tr *seedTracker, run *seedRun) (err error) {
	stage := seedStageRead
	limit := s.seedTimeout()
	ctx, cancel := context.WithCancelCause(ctx)
	timer := s.clk().NewTimer(limit)
	go func() {
		select {
		case <-timer.C():
			cancel(&seedTimeoutError{After: limit})
		case <-ctx.Done():
		}
	}()
	defer func() {
		if p := recover(); p != nil {
			err = &seedFailure{Stage: stage, Status: http.StatusInternalServerError,
				Msg: "hiring the chief of staff panicked", Err: fmt.Errorf("%v", p)}
		}
		var (
			f       *seedFailure
			timeout *seedTimeoutError
		)
		if errors.As(err, &f) && errors.As(context.Cause(ctx), &timeout) {
			f.Err = errors.Join(timeout, f.Err)
		}
		timer.Stop()
		cancel(nil)
		tr.finish(run, s.clk().Now(), err)
	}()
	return s.seedCoS(ctx, plan, func(st int) {
		stage = st
		tr.setStage(run, st)
	})
}

// defaultSeedTimeout is how long a hire may run when s.SeedTimeout is
// unset: well past a slow seed call, short enough that a hung one does
// not hold setup for good.
const defaultSeedTimeout = 10 * time.Minute

func (s *Server) seedTimeout() time.Duration {
	if s.SeedTimeout > 0 {
		return s.SeedTimeout
	}
	return defaultSeedTimeout
}

// seedTimeoutError is the cause a hire is cancelled with when it runs
// past its deadline.
type seedTimeoutError struct{ After time.Duration }

func (e *seedTimeoutError) Error() string {
	return "the hire ran past its " + e.After.String() + " deadline"
}

// words is the deadline in words: "10 minutes", "1 minute", "45 seconds".
func (e *seedTimeoutError) words() string {
	n, unit := int64(e.After/time.Second), "second"
	if e.After >= time.Minute && e.After%time.Minute == 0 {
		n, unit = int64(e.After/time.Minute), "minute"
	}
	if n != 1 {
		unit += "s"
	}
	return fmt.Sprintf("%d %s", n, unit)
}

// cosSeedFromRequest builds the hire the API asked for, or writes the
// refusal. The handbook is the edited one when sent, else the
// stored one, else the built-in default for the team's kind; either of
// the last two cases that is not already stored is written when the
// hire lands. With handbook_from_files, the default's placeholder
// company section is what the Chief of Staff's first message asks it to
// replace. A personal team's Chief of Staff always gets the personal
// first message, which has it ask the CEO a few questions and then
// draft the "About you" section.
func (s *Server) cosSeedFromRequest(w http.ResponseWriter, req apitypes.SeedCoSRequest) (cosSeed, bool) {
	handbook, err := s.Store.ReadHandbook()
	stored := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeAPIError(w, http.StatusInternalServerError, "could not read the handbook: "+err.Error(), whoServer)
		return cosSeed{}, false
	}
	kind := s.teamKind()
	personal := kind == store.TeamKindPersonal
	plan := cosSeed{
		Handbook:      handbook,
		WriteHandbook: !stored,
		Role:          seed.ChiefOfStaffRoleFor(kind),
		Instruction:   defaultSeedInstruction,
	}
	if personal {
		plan.Instruction = personalSeedInstruction
	}
	if !stored {
		plan.Handbook = seed.HandbookFor(kind)
	}
	if req.HandbookMD != nil {
		if strings.TrimSpace(*req.HandbookMD) == "" {
			writeAPIError(w, http.StatusBadRequest, "the handbook is empty", whoYou)
			return cosSeed{}, false
		}
		plan.Handbook, plan.WriteHandbook = *req.HandbookMD, true
	}
	if req.RoleMD != nil {
		role := strings.TrimSpace(*req.RoleMD)
		if role == "" {
			writeAPIError(w, http.StatusBadRequest, "the Chief of Staff's role is empty", whoYou)
			return cosSeed{}, false
		}
		plan.Role = role
	}
	switch {
	case personal:
		// A personal team's Chief of Staff always starts by getting to
		// know its person, with project files or without.
		plan.FirstMessage = strings.TrimSpace(seed.PersonalFirstMessage)
		plan.FirstMessageLabel = personalFirstMessageLabel
	case req.HandbookFromFiles:
		plan.FirstMessage = strings.TrimSpace(seed.HandbookFromFilesMessage)
	}
	return plan, true
}

func (s *Server) handleAPISetupProgress(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.seedTracker().progress(s.clk().Now(), s.cosExists()))
}

// seedRun is one Chief of Staff hire, form or API. Its fields after
// labels and started are guarded by the tracker's mutex.
type seedRun struct {
	labels  []string
	started time.Time

	stage    int
	ended    bool
	finished time.Time
	err      error

	// done closes when the run ends.
	done chan struct{}
}

// seedTracker holds the latest hire so progress can be read and a
// second hire refused while one runs. In memory: after a restart the
// Chief of Staff either exists (done) or does not (idle).
type seedTracker struct {
	mu  sync.Mutex
	run *seedRun
}

// seedTracker is the Server's tracker (Server.seedTrack); its zero
// value is idle, so no init step is needed.
func (s *Server) seedTracker() *seedTracker {
	return &s.seedTrack
}

var (
	errSeedRunning = errors.New("a chief of staff hire is already running")
	errCoSHired    = errors.New("the chief of staff is already hired")
)

// begin starts a run, unless one is running or hired says the Chief of
// Staff exists. hired is asked under the tracker's lock: every hire
// goes through begin and creates the agent only while its run is
// active, so no second hire can slip in between the check and the
// start.
func (t *seedTracker) begin(now time.Time, labels []string, hired func() bool) (*seedRun, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.run != nil && !t.run.ended {
		return nil, errSeedRunning
	}
	if hired() {
		return nil, errCoSHired
	}
	t.run = &seedRun{labels: labels, started: now, done: make(chan struct{})}
	return t.run, nil
}

func (t *seedTracker) setStage(r *seedRun, stage int) {
	t.mu.Lock()
	r.stage = stage
	t.mu.Unlock()
}

func (t *seedTracker) finish(r *seedRun, now time.Time, err error) {
	t.mu.Lock()
	r.ended, r.finished, r.err = true, now, err
	var f *seedFailure
	if errors.As(err, &f) {
		r.stage = f.Stage
	}
	t.mu.Unlock()
	close(r.done)
}

// current is the latest run, or nil. Test helper.
func (t *seedTracker) current() *seedRun {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.run
}

// active reports whether the latest run is running or failed: setup
// stays on its last step for either.
func (t *seedTracker) active() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.run != nil && (!t.run.ended || t.run.err != nil)
}

func (t *seedTracker) progress(now time.Time, hired bool) apitypes.SetupProgress {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.run
	if r == nil {
		p := apitypes.SetupProgress{State: apitypes.SetupProgressStateIdle, Stages: []apitypes.SetupStage{}}
		if hired {
			p.State = apitypes.SetupProgressStateDone
		}
		return p
	}
	end := now
	if r.ended {
		end = r.finished
	}
	p := apitypes.SetupProgress{
		Stages:   make([]apitypes.SetupStage, len(r.labels)),
		ElapsedS: int(end.Sub(r.started) / time.Second),
	}
	switch {
	case !r.ended:
		p.State = apitypes.SetupProgressStateRunning
	case r.err == nil:
		p.State = apitypes.SetupProgressStateDone
	default:
		p.State = apitypes.SetupProgressStateFailed
		p.Error, p.Who = seedFailureWords(r.err)
	}
	for i, label := range r.labels {
		st := apitypes.SetupStageStateQueued
		switch {
		case p.State == apitypes.SetupProgressStateDone || i < r.stage:
			st = apitypes.SetupStageStateDone
		case i == r.stage && p.State == apitypes.SetupProgressStateFailed:
			st = apitypes.SetupStageStateFailed
		case i == r.stage:
			st = apitypes.SetupStageStateRunning
		}
		p.Stages[i] = apitypes.SetupStage{Label: label, State: st}
	}
	return p
}

// seedFailureWords says what stopped a hire and who can fix it, for
// the hiring panel. The underlying error goes to the log, not the page.
func seedFailureWords(err error) (what, who string) {
	var f *seedFailure
	if !errors.As(err, &f) {
		return "hiring your Chief of Staff failed", whoServer
	}
	var timeout *seedTimeoutError
	if errors.As(err, &timeout) {
		retry := "you, by trying again, or whoever runs this Kivali server if it keeps happening"
		if f.Stage == seedStageBrief {
			return "your Chief of Staff could not write its first briefing because the model did not answer within " +
				timeout.words(), retry
		}
		return "hiring your Chief of Staff stopped because it did not finish within " + timeout.words(), retry
	}
	switch f.Stage {
	case seedStageRead:
		return "could not read the project files", whoServer
	case seedStageBrief:
		return "your Chief of Staff could not write its first briefing because the model call failed",
			"you, by trying again, or whoever runs this Kivali server if it keeps failing"
	case seedStageFirstMessage:
		return "your Chief of Staff is hired, but its first message could not be sent", "you, by messaging it from its chat"
	default:
		return "could not save your Chief of Staff", whoServer
	}
}
