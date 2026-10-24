package web

import (
	"cmp"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/assignments"
	"github.com/kivali-ai/kivali/internal/messaging"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// Home's JSON: the screen's data (GET /api/v1/home), its history
// (GET /api/v1/home/history), and the actions its rows take, each a
// shared method (releaseOne, releaseMany, bounceOne, respondAsCEO,
// closeAssignmentAsCEO).

// wireAPIHomeRoutes registers Home's routes on the API mux.
func (s *Server) wireAPIHomeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/home", s.handleAPIHome)
	mux.HandleFunc("GET /api/v1/home/history", s.handleAPIHomeHistory)
	mux.HandleFunc("POST /api/v1/queue/release", s.handleAPIQueueRelease)
	mux.HandleFunc("POST /api/v1/queue/release-all", s.handleAPIQueueReleaseAll)
	mux.HandleFunc("POST /api/v1/queue/bounce", s.handleAPIQueueBounce)
	mux.HandleFunc("POST /api/v1/needs/approve", s.handleAPINeedsResponse(responseKindApproval, boolPtr(true)))
	mux.HandleFunc("POST /api/v1/needs/deny", s.handleAPINeedsResponse(responseKindApproval, boolPtr(false)))
	mux.HandleFunc("POST /api/v1/needs/ack", s.handleAPINeedsResponse(responseKindAck, nil))
	mux.HandleFunc("GET /api/v1/agents/{slug}/subagents/{id}/files/{name}", s.handleAPIWebTaskFile)
	mux.HandleFunc("POST /api/v1/assignments/{id}/close", s.handleAPIAssignmentClose)
}

// Who can fix what, for the error bodies below.
const (
	whoYou    = "you"
	whoNoOne  = "no one; reload to see what is waiting"
	whoServer = "whoever runs this Kivali server"
)

// ---- GET /api/v1/home ----

func (s *Server) handleAPIHome(w http.ResponseWriter, _ *http.Request) {
	snap := s.orgSnapshotValue()
	ceoView, err := s.buildCEOInbox(1)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the requests waiting on you could not be read", whoServer)
		return
	}
	q, err := s.Store.ReadMessageQueue()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the queue could not be read", whoServer)
		return
	}
	v := s.newHomeView()
	pending := s.buildPendingList(q)

	needs := make([]apitypes.NeedItem, 0, len(ceoView.Needs))
	for _, it := range ceoView.Needs {
		needs = append(needs, v.needItem(it))
	}
	for _, a := range snap.Agents {
		if item, ok := s.needsHelpItem(a); ok {
			needs = append(needs, item)
		}
	}
	if s.SubagentService != nil {
		for _, j := range s.SubagentService.waitingWebJobs() {
			needs = append(needs, s.webSignInItem(j))
		}
		for _, j := range s.SubagentService.awaitingApprovalJobs() {
			needs = append(needs, s.webUploadItem(j))
		}
		for _, n := range s.SubagentService.webBudgetNotices() {
			needs = append(needs, s.webBudgetItem(n))
		}
	}
	needs = append(needs, s.integrationSignInItems()...)
	queue := make([]apitypes.QueueItem, 0, len(pending))
	for _, p := range pending {
		queue = append(queue, v.queueItem(p))
	}
	threads, err := s.homeHistoryThreads(q, ceoView)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the message history could not be read", whoServer)
		return
	}
	writeJSON(w, http.StatusOK, apitypes.Home{
		Goals:        snap.Goals,
		Readouts:     snap.Readouts,
		Needs:        needs,
		Queue:        queue,
		AutoRelease:  snap.Inbox.AutoRelease,
		HistoryTotal: len(threads),
	})
}

// homeView resolves what rows name: agents, attachments, assignments.
// One per request, so each lookup reads the store once.
type homeView struct {
	s              *Server
	names          map[string]string
	sizes          map[string]int64
	assignmentSet  *assignments.Set
	assignmentRead bool
}

func (s *Server) newHomeView() *homeView {
	v := &homeView{s: s, names: map[string]string{}, sizes: map[string]int64{}}
	v.addNames()
	return v
}

// addNames records every active agent's display name. Called again
// after an action that adds an agent (a hire); names already known,
// such as an agent just archived, are kept.
func (v *homeView) addNames() {
	actives, _ := v.s.Store.ListActiveAgents()
	for _, a := range actives {
		v.names[a.Slug] = agentDisplayName(a)
	}
}

// person names an agent the way the app does: its display name, "You"
// for the CEO, and its slug when it is no longer on the team.
func (v *homeView) person(slug string) apitypes.PersonRef {
	return personNamed(v.names, slug)
}

// personNamed is slug for the wire with its display name from names:
// "You" for the CEO, the slug itself for an agent names does not hold.
func personNamed(names map[string]string, slug string) apitypes.PersonRef {
	if slug == agent.CEOSlug {
		return apitypes.PersonRef{Slug: slug, Name: "You"}
	}
	if n, ok := names[slug]; ok {
		return apitypes.PersonRef{Slug: slug, Name: n}
	}
	return apitypes.PersonRef{Slug: slug, Name: slug}
}

func (v *homeView) people(slugs []string) []apitypes.PersonRef {
	out := make([]apitypes.PersonRef, 0, len(slugs))
	for _, s := range slugs {
		out = append(out, v.person(s))
	}
	return out
}

func (v *homeView) attachments(atts []store.MessageAttachment) []apitypes.AttachmentRef {
	out := make([]apitypes.AttachmentRef, 0, len(atts))
	for _, a := range atts {
		size, ok := v.sizes[a.SHA]
		if !ok {
			if att, err := v.s.Store.GetAttachment(a.SHA); err == nil {
				size = att.Size
			}
			v.sizes[a.SHA] = size
		}
		out = append(out, apitypes.AttachmentRef{Name: a.Name, SizeBytes: size, URL: "/attachments/" + a.SHA})
	}
	return out
}

// assignment is the assignment id names, with its title when the tracker
// still has it.
func (v *homeView) assignment(id int) *apitypes.AssignmentRef {
	if !v.assignmentRead {
		v.assignmentRead = true
		v.assignmentSet = v.s.assignmentSetCached(v.s.assignmentsVersion.Load())
	}
	ref := &apitypes.AssignmentRef{ID: id}
	if v.assignmentSet != nil {
		if iss, ok := v.assignmentSet.Get(id); ok {
			ref.Title = iss.Title
		}
	}
	return ref
}

// proposalKind is which change an approval request proposes, if any.
func proposalKind(m store.Message) (apitypes.ProposalKind, bool) {
	switch {
	case m.Hire != nil:
		return apitypes.ProposalKindHire, true
	case m.RoleUpdate != nil:
		return apitypes.ProposalKindRoleUpdate, true
	case m.HandbookUpdate != nil:
		return apitypes.ProposalKindHandbookUpdate, true
	case m.Offboard != nil:
		return apitypes.ProposalKindOffboard, true
	case m.Reorg != nil:
		return apitypes.ProposalKindReorg, true
	case m.SoftwareInstall != nil:
		return apitypes.ProposalKindSoftwareInstall, true
	case m.IntegrationInstall != nil:
		return apitypes.ProposalKindIntegrationInstall, true
	}
	return "", false
}

// proposalReviewPath is the web app's page for reviewing a proposal.
func proposalReviewPath(requestPath string) string {
	return "/proposals/" + requestPath
}

// needItem is one CEO-inbox request as a Needs-you row.
func (v *homeView) needItem(it CEOInboxItem) apitypes.NeedItem {
	raw := "/" + it.RequestPath
	n := apitypes.NeedItem{
		ID:          it.RequestPath,
		Title:       it.Request.Title,
		From:        v.person(it.Request.From),
		At:          it.Request.Date,
		BodyMD:      strings.TrimRight(it.Request.Body, "\n"),
		Attachments: v.attachments(it.Request.Attachments),
		Packages:    []apitypes.SoftwarePackage{},
		RawURL:      &raw,
	}
	switch it.RequestKind {
	case "approval":
		n.Kind = apitypes.NeedKindApproval
		pk, ok := proposalKind(it.Request)
		switch {
		case ok && pk == apitypes.ProposalKindSoftwareInstall:
			// Short enough to answer in place: the packages and the
			// agent's reason are the whole of it.
			n.ProposalKind = &pk
			n.Packages = v.s.softwarePackages(it.Request.SoftwareInstall.Packages)
		case ok && pk == apitypes.ProposalKindIntegrationInstall:
			// Reviewed on the version's install screen, which says
			// what it reaches, holds and does.
			ii := it.Request.IntegrationInstall
			review := integrationVersionPath(ii.Name, ii.Version)
			n.Kind = apitypes.NeedKindProposal
			n.ProposalKind = &pk
			n.ReviewPath = &review
		case ok:
			review := proposalReviewPath(it.RequestPath)
			n.Kind = apitypes.NeedKindProposal
			n.ProposalKind = &pk
			n.ReviewPath = &review
		}
	case "notification":
		n.Kind = apitypes.NeedKindNotification
	case "assignment":
		n.Kind = apitypes.NeedKindAssignment
		switch {
		case it.Assignment != nil:
			n.Assignment = &apitypes.AssignmentRef{ID: it.Assignment.ID, Title: it.Assignment.Title}
		case it.Request.Assignment != nil:
			n.Assignment = v.assignment(it.Request.Assignment.ID)
		}
		if n.Assignment != nil {
			n.ID = fmt.Sprintf("assignment:%d", n.Assignment.ID)
			if n.Title == "" {
				n.Title = n.Assignment.Title
			}
		}
	}
	return n
}

// webSignInItem is the Needs-you row for a web task waiting for a
// person in its browser window. Every word is core's: nothing the task
// said reaches the person here, and the page the window shows is named
// in fields of its own, as the browser reported it, for the app to show
// as text.
func (s *Server) webSignInItem(j subagentJob) apitypes.NeedItem {
	name := j.Parent
	if n := s.agentNames()[j.Parent]; n != "" {
		name = n
	}
	title, body := webSignInWords(name, j.Site, j.WaitReason)
	pageURL, pageTitle := s.SubagentService.waitingPage(j)
	return apitypes.NeedItem{
		ID:          webSignInNeedPrefix + j.Parent + "/" + j.ID,
		Kind:        apitypes.NeedKindWebSignIn,
		Title:       title,
		From:        apitypes.PersonRef{Slug: j.Parent, Name: name},
		At:          j.WaitSince,
		BodyMD:      body,
		Attachments: []apitypes.AttachmentRef{},
		Packages:    []apitypes.SoftwarePackage{},
		Subagent: &apitypes.SubagentRef{
			Parent: j.Parent, ID: j.ID, Description: j.Description, Site: j.Site,
			Reason:  apitypes.WebSignInReason(j.WaitReason),
			PageURL: pageURL, PageTitle: pageTitle,
		},
	}
}

// webSignInWords are a web_sign_in row's title and body: what the page
// asks of the person, by reason, and what to do about it.
func webSignInWords(name, site, reason string) (title, body string) {
	const then = " then choose Continue the task."
	switch apitypes.WebSignInReason(reason) {
	case apitypes.WebSignInReasonMFA:
		return name + " needs your second factor for " + site, "The page asks for a second factor. Open the browser, enter it," + then
	case apitypes.WebSignInReasonCaptcha:
		return name + " needs you to solve a CAPTCHA on " + site, "The page shows a CAPTCHA. Open the browser, solve it," + then
	case apitypes.WebSignInReasonConsent:
		return name + " needs your consent on " + site, "The page asks for a consent only a person can give. Open the browser, answer it," + then
	default:
		return name + " needs you to sign in to " + site, "The page asks for a sign-in. Open the browser, sign in," + then
	}
}

// webUploadItem is the Needs-you row for a web task awaiting approval
// of its uploads: what it would upload, to which site, where it starts
// and the prompt its agent gave it. It has no body: the title and the
// task's details say it all.
func (s *Server) webUploadItem(j subagentJob) apitypes.NeedItem {
	name := j.Parent
	if n := s.agentNames()[j.Parent]; n != "" {
		name = n
	}
	files := append([]string{}, j.Files...)
	// Each file as it was at dispatch, the bytes an upload sends, as a
	// download like any file on a request.
	atts := make([]apitypes.AttachmentRef, 0, len(j.ApprovedFiles))
	for _, f := range j.ApprovedFiles {
		atts = append(atts, apitypes.AttachmentRef{Name: path.Base(f.Path), SizeBytes: f.Size, URL: webTaskFileURL(j.Parent, j.ID, path.Base(f.Path))})
	}
	return apitypes.NeedItem{
		ID:          webUploadNeedPrefix + j.Parent + "/" + j.ID,
		Kind:        apitypes.NeedKindWebUpload,
		Title:       fmt.Sprintf("Upload %d file%s to %s", len(files), pluralS(len(files)), j.LockSite),
		From:        apitypes.PersonRef{Slug: j.Parent, Name: name},
		At:          j.WaitSince,
		Attachments: atts,
		Packages:    []apitypes.SoftwarePackage{},
		WebUpload: &apitypes.WebUploadRef{
			Parent: j.Parent, ID: j.ID, Description: j.Description, Site: j.LockSite,
			StartURL: j.StartURL, Files: files, Prompt: j.Prompt,
		},
	}
}

// webTaskFileURL is where a person downloads one of a web task's files
// to check it before approving the upload.
func webTaskFileURL(parent, id, name string) string {
	return "/api/v1/agents/" + url.PathEscape(parent) + "/subagents/" + url.PathEscape(id) + "/files/" + url.PathEscape(name)
}

// handleAPIWebTaskFile serves one file a web task may upload as a
// download: the bytes approved at dispatch, read again from the
// agent's /files/ and checked against their digest. A file changed
// since is refused, as the task's upload of it would be.
func (s *Server) handleAPIWebTaskFile(w http.ResponseWriter, r *http.Request) {
	slug, id, name := r.PathValue("slug"), r.PathValue("id"), r.PathValue("name")
	var job subagentJob
	found := false
	if s.SubagentService != nil {
		job, found = s.SubagentService.lookupJob(id)
	}
	if !found || job.Parent != slug {
		writeAPIError(w, http.StatusNotFound, "That task is no longer on file", whoNoOne)
		return
	}
	file, ok := webApprovedFileNamed(job, name)
	if !ok {
		writeAPIError(w, http.StatusNotFound, "That file is not one the task may upload", whoNoOne)
		return
	}
	body, err := s.SubagentService.readApprovedFile(slug, file)
	switch {
	case errors.Is(err, errWebFileChanged):
		writeAPIError(w, http.StatusConflict, "This file has changed since the task was dispatched, so the task will not upload it", "the agent, which can dispatch the task again")
		return
	case err != nil:
		writeAPIError(w, http.StatusNotFound, "That file cannot be read", whoNoOne)
		return
	}
	// Always a download, never rendered: the bytes are an agent's.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `attachment; filename="`+sanitizeDownloadName(name)+`"`)
	_, _ = w.Write(body)
}

// maxWebDenyNote bounds the note a person sends with an answer to a
// web_upload row; the agent reads it.
const maxWebDenyNote = 2 << 10

// webBudgetPhrases say each limit, for a web_budget row.
var webBudgetPhrases = map[webBudgetLimit]string{
	webBudgetLimitAction: "One action may send " + byteSize(webBudgetPerAction) + ".",
	webBudgetLimitTask:   "One task may send " + byteSize(webBudgetPerTask) + ".",
	webBudgetLimitDay:    "An agent's web tasks may send " + byteSize(webBudgetPerDay) + " a day.",
}

// webBudgetItem is the Needs-you row for a web task refused an action
// over the web budget. Every word is core's.
func (s *Server) webBudgetItem(n webBudgetNotice) apitypes.NeedItem {
	name := n.Parent
	if an := s.agentNames()[n.Parent]; an != "" {
		name = an
	}
	title := fmt.Sprintf("%s's web task tried to send %s out", name, byteSize(n.Bytes))
	if n.Site != "" {
		title = fmt.Sprintf("%s's web task tried to send %s to %s", name, byteSize(n.Bytes), n.Site)
	}
	return apitypes.NeedItem{
		ID:          webBudgetNeedPrefix + n.Parent + "/" + n.ID,
		Kind:        apitypes.NeedKindWebBudget,
		Title:       title,
		From:        apitypes.PersonRef{Slug: n.Parent, Name: name},
		At:          n.At,
		BodyMD:      webBudgetPhrases[n.Limit] + " That is far more than a search or a form needs, so a page may have led the task to send out data it should not. Kivali refused the action and told the task, which carries on without it. Open the task to see what it was doing.",
		Attachments: []apitypes.AttachmentRef{},
		Packages:    []apitypes.SoftwarePackage{},
		WebBudget: &apitypes.WebBudgetRef{
			Parent: n.Parent, ID: n.ID, Description: n.Description, Site: n.Site,
			Bytes: n.Bytes, Limit: apitypes.WebBudgetLimit(n.Limit),
		},
	}
}

// The ID prefixes of a web task's Needs-you rows. A web_upload or
// web_budget row takes an answer through POST /api/v1/needs/*.
const (
	webSignInNeedPrefix = "web_sign_in:"
	webUploadNeedPrefix = "web_upload:"
	webBudgetNeedPrefix = "web_budget:"
)

// integrationSignInNeedPrefix is the ID prefix of a connection's
// Needs-you row for a new sign-in. The row takes no answer: it leaves
// once the owner signs in again or deletes the connection.
const integrationSignInNeedPrefix = "integration_sign_in:"

// integrationSignInFrom is who an integration_sign_in row is from.
var integrationSignInFrom = apitypes.PersonRef{Slug: store.BuiltinIntegrationPublisher, Name: "Kivali"}

// integrationSignInItems are the Needs-you rows of the connections
// whose sign-in the vendor refused to renew, read from the
// connections as they stand: one per connection whose record says why
// it needs a new sign-in, of a switched-on integration whose version
// in use signs in, oldest refusal first. Every word is core's but the
// refusal, which is the vendor's, shown as the body.
func (s *Server) integrationSignInItems() []apitypes.NeedItem {
	its, err := s.Store.ListIntegrations()
	if err != nil {
		return nil
	}
	var out []apitypes.NeedItem
	for _, it := range its {
		// Switched off, nothing calls it; a version in use that is
		// connected with a token has no sign-in to renew.
		if !it.Enabled {
			continue
		}
		if _, m, err := s.Store.CurrentIntegrationVersion(it.Name); err != nil || !m.Kivali.Connection.OAuth() {
			continue
		}
		conns, err := s.Store.ListIntegrationConnections(it.Name)
		if err != nil {
			continue
		}
		display := cmp.Or(it.DisplayName, it.Name)
		for _, c := range conns {
			if c.SignIn == nil || c.SignIn.Error == "" {
				continue
			}
			page := "/org/integrations/" + url.PathEscape(it.Name)
			out = append(out, apitypes.NeedItem{
				ID:          integrationSignInNeedPrefix + it.Name + "/" + c.ID,
				Kind:        apitypes.NeedKindIntegrationSignIn,
				Title:       display + " needs you to sign in again",
				From:        integrationSignInFrom,
				At:          c.SignIn.ErrorAt,
				BodyMD:      sentence(errors.New(c.SignIn.Error)) + " Agents cannot use the connection until you sign in again.",
				Attachments: []apitypes.AttachmentRef{},
				Packages:    []apitypes.SoftwarePackage{},
				ReviewPath:  &page,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// answerWebNeed answers a web task's Needs-you row when path names one:
// approve or deny a web_upload, acknowledge a web_budget. handled is
// false for any other path, which is a message's.
func (s *Server) answerWebNeed(kind responseKind, approved *bool, path, message string) (handled bool, out apitypes.NeedActionResponse, err error) {
	var rest string
	var upload bool
	switch {
	case strings.HasPrefix(path, webUploadNeedPrefix):
		rest, upload = strings.TrimPrefix(path, webUploadNeedPrefix), true
	case strings.HasPrefix(path, webBudgetNeedPrefix):
		rest = strings.TrimPrefix(path, webBudgetNeedPrefix)
	default:
		return false, out, nil
	}
	parent, id, ok := strings.Cut(rest, "/")
	if !ok || s.SubagentService == nil {
		return true, out, errWebNeedGone
	}
	if !upload {
		if kind != responseKindAck {
			return true, out, &ceoRefusal{Status: http.StatusBadRequest, Msg: "web_budget takes ack",
				Words: "That is a notice, so it can only be acknowledged", Who: whoDevelopers}
		}
		if !s.SubagentService.ackWebBudgetNotice(parent, id) {
			return true, out, errWebNeedGone
		}
		return true, out, nil
	}
	if kind != responseKindApproval || approved == nil {
		return true, out, &ceoRefusal{Status: http.StatusBadRequest, Msg: "web_upload takes approve or deny",
			Words: "That request is an approval, so it can only be approved or denied", Who: whoDevelopers}
	}
	note := strings.TrimSpace(message)
	if len(note) > maxWebDenyNote {
		return true, out, &ceoRefusal{Status: http.StatusBadRequest, Msg: "web_upload note over 2 KB",
			Words: "The note is over 2 KB; shorten it", Who: whoYou}
	}
	if err := s.SubagentService.answerWebUpload(parent, id, *approved, note); err != nil {
		return true, out, err
	}
	if *approved {
		return true, apitypes.NeedActionResponse{Result: "Upload approved. The task starts now", Link: "/agents/" + parent + "/subagents/" + id}, nil
	}
	return true, apitypes.NeedActionResponse{Result: "Upload denied. The task ends without uploading anything"}, nil
}

// needsHelpItem is the Needs-you row for an agent that has stopped:
// its last turn ended on an error, it keeps restarting without getting
// anything done (quarantined), or its runtime is not connected. The
// row opens the agent's chat, where it is answered.
func (s *Server) needsHelpItem(a apitypes.SnapshotAgent) (apitypes.NeedItem, bool) {
	var title, body string
	switch a.State {
	case apitypes.AgentStateNeedsHelp:
		title = a.Name + " stopped on an error"
		body = "Its last turn ended on an error, and it waits for you before it tries again. Open the chat to see what happened and send it on."
	case apitypes.AgentStateQuarantined:
		title = a.Name + " keeps restarting"
		body = "It restarted several times in a row without getting anything done, so it will not wake again until you message it."
	case apitypes.AgentStateDisconnected:
		title = a.Name + " is not connected"
		body = "Its runtime is not connected, so nothing reaches it. It reconnects on its own after a restart; if it stays like this, whoever runs this Kivali server can look at it."
	default:
		return apitypes.NeedItem{}, false
	}
	// The row's time is the agent's last chat entry: when it stopped.
	// An errored turn's own text is the most useful body there is.
	var at time.Time
	hist, _ := s.Store.ReadChatHistory(a.Slug)
	if len(hist) > 0 {
		at = hist[len(hist)-1].TS
	}
	if a.State == apitypes.AgentStateNeedsHelp {
		for i := len(hist) - 1; i >= 0; i-- {
			if hist[i].Kind == store.KindTurnError {
				if c := strings.TrimSpace(hist[i].Content); c != "" {
					body = c
				}
				break
			}
		}
	}
	if at.IsZero() {
		if ag, err := s.Store.GetAgent(a.Slug); err == nil {
			at = ag.CreatedAt
		}
	}
	return apitypes.NeedItem{
		ID:          "agent:" + a.Slug,
		Kind:        apitypes.NeedKindNeedsHelp,
		Title:       title,
		From:        apitypes.PersonRef{Slug: a.Slug, Name: a.Name},
		At:          at,
		BodyMD:      body,
		Attachments: []apitypes.AttachmentRef{},
		Packages:    []apitypes.SoftwarePackage{},
		Agent:       &apitypes.AgentRef{Slug: a.Slug, Name: a.Name, State: a.State},
	}, true
}

// queueItem is one queued message as a Queue row.
func (v *homeView) queueItem(p pendingDelivery) apitypes.QueueItem {
	iv := p.View
	title := iv.Title
	if title == "" {
		title = iv.Label
	}
	item := apitypes.QueueItem{
		Path:        p.Path,
		Kind:        apitypes.QueueKindNotice,
		Title:       title,
		From:        v.person(iv.From),
		To:          v.people(p.Recipients),
		BodyMD:      iv.Body,
		Attachments: v.attachments(iv.Attachments),
		QueuedAt:    p.Message.Date,
		Held:        p.Due.IsZero(),
		RawURL:      "/" + p.Path,
	}
	if p.Message.Type == store.MsgAssignmentEvent {
		item.Kind = apitypes.QueueKindAssignment
	}
	if iv.Assignment != nil {
		item.Assignment = v.assignment(iv.Assignment.ID)
	}
	if !p.Due.IsZero() {
		due := p.Due.UTC()
		item.ReleasesAt = &due
	}
	return item
}

// ---- GET /api/v1/home/history ----

// historyDefaultLimit and historyMaxLimit bound one history page.
const (
	historyDefaultLimit = inboxDefaultHistoryThreads
	historyMaxLimit     = 100
)

// homeHistoryThreads is every history thread, newest activity first:
// every message on disk grouped by thread, leaving out what is still
// queued or still waiting on the CEO (the Queue and Needs you show
// those), exactly as the inbox's history section does. Only the
// messages' heads are read (home_cache.go); loadHistoryThread reads a
// thread's messages for the page that shows it.
func (s *Server) homeHistoryThreads(q store.MessageQueue, ceoView CEOInboxView) ([]historyThreadRef, error) {
	exclude := map[string]bool{}
	for _, p := range pendingPathsOf(q) {
		exclude[p] = true
	}
	for _, it := range ceoView.Needs {
		exclude[it.RequestPath] = true
	}
	heads, err := s.messageHeads()
	if err != nil {
		return nil, err
	}
	return groupHistoryThreads(heads, exclude), nil
}

// historyCursor is a thread's place in the newest-first order, as the
// opaque next_before string: the thread's last activity in Unix
// nanoseconds, a colon, its root path. Parsing splits at the first
// colon, so a root path holding one still round-trips.
func historyCursor(t historyThreadRef) string {
	return strconv.FormatInt(t.LastActivity.UnixNano(), 10) + ":" + t.RootPath
}

func parseHistoryCursor(c string) (time.Time, string, bool) {
	ns, path, ok := strings.Cut(c, ":")
	if !ok {
		return time.Time{}, "", false
	}
	n, err := strconv.ParseInt(ns, 10, 64)
	if err != nil {
		return time.Time{}, "", false
	}
	return time.Unix(0, n), path, true
}

func (s *Server) handleAPIHomeHistory(w http.ResponseWriter, r *http.Request) {
	limit := historyDefaultLimit
	if l := r.URL.Query().Get("limit"); l != "" {
		n, err := atoiPositive(l)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "limit must be a positive whole number", whoDevelopers)
			return
		}
		limit = min(n, historyMaxLimit)
	}
	var (
		beforeAt   time.Time
		beforePath string
		hasBefore  bool
	)
	if c := r.URL.Query().Get("before"); c != "" {
		var ok bool
		if beforeAt, beforePath, ok = parseHistoryCursor(c); !ok {
			writeAPIError(w, http.StatusBadRequest, "the history cursor is not valid", whoDevelopers)
			return
		}
		hasBefore = true
	}
	ceoView, err := s.buildCEOInbox(1)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the requests waiting on you could not be read", whoServer)
		return
	}
	q, err := s.Store.ReadMessageQueue()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the queue could not be read", whoServer)
		return
	}
	threads, err := s.homeHistoryThreads(q, ceoView)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "the message history could not be read", whoServer)
		return
	}
	// groupHistoryThreads orders by last activity, newest first, then
	// root path: the cursor's order.
	start := 0
	if hasBefore {
		start = sort.Search(len(threads), func(i int) bool {
			t := threads[i]
			if !t.LastActivity.Equal(beforeAt) {
				return t.LastActivity.Before(beforeAt)
			}
			return t.RootPath > beforePath
		})
	}
	end := min(start+limit, len(threads))
	v := s.newHomeView()
	out := apitypes.HistoryResponse{
		Threads: make([]apitypes.HistoryThread, 0, end-start),
		Total:   len(threads),
	}
	for _, ref := range threads[start:end] {
		t, err := s.loadHistoryThread(ref)
		if errors.Is(err, store.ErrNotFound) {
			continue // its root moved away since the listing
		}
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "the message history could not be read", whoServer)
			return
		}
		out.Threads = append(out.Threads, v.historyThread(t))
	}
	if end < len(threads) && end > start {
		next := historyCursor(threads[end-1])
		out.NextBefore = &next
	}
	writeJSON(w, http.StatusOK, out)
}

func (v *homeView) historyThread(t historyThread) apitypes.HistoryThread {
	out := apitypes.HistoryThread{
		Path:         t.RootPath,
		LastActivity: t.LastActivity,
		Request:      v.historyMessage(t.Root, t.RootPath, t.RootView),
		Replies:      make([]apitypes.HistoryMessage, 0, len(t.Replies)),
	}
	for _, r := range t.Replies {
		m := v.historyMessage(r.Message, r.Path, r.View)
		if m.Decision != nil {
			out.Decision = m.Decision
		}
		out.Replies = append(out.Replies, m)
	}
	return out
}

func (v *homeView) historyMessage(m store.Message, path string, iv agent.InboxView) apitypes.HistoryMessage {
	out := apitypes.HistoryMessage{
		Path:        path,
		Type:        string(m.Type),
		Label:       iv.Label,
		Title:       iv.Title,
		From:        v.person(m.From),
		To:          v.people(m.To),
		At:          m.Date,
		BodyMD:      iv.Body,
		Attachments: v.attachments(iv.Attachments),
		RawURL:      "/" + path,
	}
	if d := decisionLabel(m); d != "" {
		out.Decision = &d
	}
	if iv.Assignment != nil {
		out.Assignment = v.assignment(iv.Assignment.ID)
	}
	return out
}

// ---- POST /api/v1/queue/* ----

func (s *Server) handleAPIQueueRelease(w http.ResponseWriter, r *http.Request) {
	var req apitypes.QueueReleaseRequest
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	if err := s.releaseOne(r.Context(), req.Path, req.Note); err != nil {
		writeQueueError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apitypes.QueueReleaseResponse{Released: 1})
}

func (s *Server) handleAPIQueueReleaseAll(w http.ResponseWriter, r *http.Request) {
	var req apitypes.QueueReleaseAllRequest
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	// The form endpoint drops a bad path and, when none is left,
	// releases everything. Here a bad path is refused instead: a
	// client that named what to release must never release more.
	for _, p := range req.Paths {
		if _, ok := cleanMessagePath(p); !ok {
			writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("%q is not a message path", p), whoDevelopers)
			return
		}
	}
	for p := range req.Notes {
		if _, ok := cleanMessagePath(p); !ok {
			writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("%q is not a message path", p), whoDevelopers)
			return
		}
	}
	// Absent paths release everything; an empty list names nothing, so
	// nothing goes (releaseMany would read it as everything).
	if req.Paths != nil && len(req.Paths) == 0 {
		writeJSON(w, http.StatusOK, apitypes.QueueReleaseResponse{Released: 0})
		return
	}
	paths, notes := cleanReleaseAll(req.Paths, req.Notes)
	n, err := s.releaseMany(r.Context(), paths, notes)
	if err != nil {
		writeQueueError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apitypes.QueueReleaseResponse{Released: n})
}

func (s *Server) handleAPIQueueBounce(w http.ResponseWriter, r *http.Request) {
	var req apitypes.QueueBounceRequest
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	if err := s.bounceOne(r.Context(), req.Path, req.Comment); err != nil {
		writeQueueError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apitypes.Empty{})
}

// writeQueueError answers a refused queue action.
func writeQueueError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errBadMessagePath):
		writeAPIError(w, http.StatusBadRequest, "that is not a message path", whoDevelopers)
	case errors.Is(err, errBounceNeedsComment):
		writeAPIError(w, http.StatusBadRequest, "a bounce needs a comment saying why", whoYou)
	case errors.Is(err, errBounceAssignmentEvent):
		writeAPIError(w, http.StatusConflict, "an assignment update cannot be bounced; change the assignment instead", whoYou)
	case errors.Is(err, messaging.ErrNotQueued), errors.Is(err, store.ErrNotFound):
		// Not found: a bounced message has left the ledger.
		writeAPIError(w, http.StatusNotFound, "that message is no longer queued", whoNoOne)
	case errors.Is(err, errNoEngine):
		writeAPIError(w, http.StatusServiceUnavailable, "no model is connected yet, so nothing can be delivered", whoServer)
	default:
		writeAPIError(w, http.StatusInternalServerError, "the message could not be delivered: "+err.Error(), whoServer)
	}
}

// ---- POST /api/v1/needs/* ----

// needsAttachmentFields are the multipart fields that carry files:
// "attachments[]" is what the web app sends; the others are accepted
// so any FormData spelling works.
var needsAttachmentFields = []string{"attachments[]", "attachments", "attachment"}

// needsFormMemory is how much of a needs answer's multipart body is
// held in memory; larger files spill to temporary files, which the
// server removes when the request ends.
const needsFormMemory = 32 << 20

// handleAPINeedsResponse answers a Needs-you request: approve, deny or
// acknowledge. Multipart (path, message, attachments[]), since files
// can go with the answer; a urlencoded body works when there are none.
// The whole body is capped at maxUploadBytes, the form endpoints'
// upload size.
func (s *Server) handleAPINeedsResponse(kind responseKind, approved *bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mt != "multipart/form-data" && mt != "application/x-www-form-urlencoded" {
			writeAPIError(w, http.StatusUnsupportedMediaType,
				"send the answer as multipart/form-data (path, message, attachments[]), not "+cmp.Or(mt, "an untyped body"), whoDevelopers)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
		if err := r.ParseMultipartForm(needsFormMemory); err != nil {
			if errors.Is(err, http.ErrNotMultipart) {
				err = r.ParseForm()
			}
			var tooBig *http.MaxBytesError
			switch {
			case errors.As(err, &tooBig):
				writeAPIError(w, http.StatusRequestEntityTooLarge,
					fmt.Sprintf("the answer and its attachments are over %d MB", maxUploadBytes>>20), whoYou)
				return
			case err != nil:
				writeAPIError(w, http.StatusBadRequest, "the request body is not a form", whoDevelopers)
				return
			}
		}
		var files []*multipart.FileHeader
		if r.MultipartForm != nil {
			for _, f := range needsAttachmentFields {
				files = append(files, r.MultipartForm.File[f]...)
			}
		}
		if handled, out, err := s.answerWebNeed(kind, approved, r.FormValue("path"), r.FormValue("message")); handled {
			if err != nil {
				writeNeedsError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, out)
			return
		}
		v := s.newHomeView()
		done, err := s.respondAsCEO(r.Context(), kind, approved, r.FormValue("path"), r.FormValue("message"), files)
		if err != nil {
			writeNeedsError(w, err)
			return
		}
		v.addNames()
		writeJSON(w, http.StatusOK, v.proposalOutcome(done))
	}
}

// writeNeedsError answers a refused Needs-you action in the refusal's
// API words (ceoRefusal.Words), never the form's prefixed text. A
// refusal without words is worded from its status.
func writeNeedsError(w http.ResponseWriter, err error) {
	var refusal *ceoRefusal
	if !errors.As(err, &refusal) {
		writeAPIError(w, http.StatusInternalServerError, "Your answer could not be recorded", whoServer)
		return
	}
	if refusal.Status == http.StatusNotFound {
		writeAPIError(w, http.StatusNotFound, "That request is no longer on file", whoNoOne)
		return
	}
	if refusal.Words != "" {
		writeAPIError(w, refusal.Status, refusal.Words, cmp.Or(refusal.Who, whoServer))
		return
	}
	words := sentenceCase(plainCause(refusal))
	switch refusal.Status {
	case http.StatusBadRequest:
		writeAPIError(w, http.StatusBadRequest, words, whoDevelopers)
	case http.StatusConflict:
		writeAPIError(w, http.StatusConflict, words, whoYou)
	default:
		writeAPIError(w, refusal.Status, words, whoServer)
	}
}

// proposalOutcome says, in words, what answering a proposal changed,
// with a link to where it shows. Empty for a request that is not a
// proposal.
func (v *homeView) proposalOutcome(done ceoResponse) apitypes.NeedActionResponse {
	req := done.Request
	approved := done.Response.Approved != nil && *done.Response.Approved
	name := func(slug string) string { return v.person(slug).Name }
	switch {
	case req.Hire != nil:
		role := req.Hire.Role
		if role == "" {
			role = req.Hire.Slug
		}
		if !approved {
			return apitypes.NeedActionResponse{Result: role + " is not hired"}
		}
		manager := name(req.Hire.ReportsTo)
		if req.Hire.ReportsTo == "" || req.Hire.ReportsTo == agent.CEOSlug {
			manager = "you"
		}
		return apitypes.NeedActionResponse{
			Result: fmt.Sprintf("%s is hired and reports to %s", role, manager),
			Link:   "/agents/" + req.Hire.Slug,
		}
	case req.RoleUpdate != nil:
		who := name(req.RoleUpdate.Slug)
		if !approved {
			return apitypes.NeedActionResponse{Result: who + " keeps its current role"}
		}
		return apitypes.NeedActionResponse{
			Result: who + " has its new role",
			Link:   "/agents/" + req.RoleUpdate.Slug,
		}
	case req.HandbookUpdate != nil:
		if !approved {
			return apitypes.NeedActionResponse{Result: "The handbook stays as it was"}
		}
		return apitypes.NeedActionResponse{Result: "The handbook is updated", Link: "/org"}
	case req.Offboard != nil:
		who := name(req.Offboard.Slug)
		if !approved {
			return apitypes.NeedActionResponse{Result: who + " stays on the team"}
		}
		return apitypes.NeedActionResponse{
			Result: who + " is offboarded and its files are in the archive",
			Link:   "/team",
		}
	case req.SoftwareInstall != nil:
		who := name(req.SoftwareInstall.Slug)
		if !approved {
			return apitypes.NeedActionResponse{Result: "Nothing is installed for " + who}
		}
		result := who + " is restarting to install the software"
		if si := done.Response.SoftwareInstall; si != nil {
			switch si.Outcome {
			case store.SoftwareAlreadyInstalled:
				result = who + " already has this software"
			case store.SoftwareSavedNotApplied:
				result = "The software is saved for " + who + " and installs the next time it starts"
			case store.SoftwareRestartWhenIdle:
				result = who + " restarts to install the software once it has finished what it is doing"
			}
		}
		return apitypes.NeedActionResponse{
			Result: result,
			Link:   "/agents/" + req.SoftwareInstall.Slug,
		}
	case req.Reorg != nil:
		if !approved {
			return apitypes.NeedActionResponse{Result: "The reporting lines stay as they were"}
		}
		return apitypes.NeedActionResponse{Result: reorgOutcome(done.Response.Reorg), Link: "/team"}
	case req.IntegrationInstall != nil:
		ii := req.IntegrationInstall
		what := v.s.integrationWords(ii.Name, ii.Version)
		if !approved {
			return apitypes.NeedActionResponse{Result: what + " is rejected"}
		}
		return apitypes.NeedActionResponse{Result: what + " is approved", Link: "/org/integrations/" + url.PathEscape(ii.Name)}
	}
	return apitypes.NeedActionResponse{}
}

// reorgOutcome counts an applied reorg's moves in words.
func reorgOutcome(r *store.Reorg) string {
	if r == nil {
		return "The reporting lines are updated"
	}
	applied, failed := len(r.Applied), len(r.Failed)
	lines := func(n int) string {
		if n == 1 {
			return "1 reporting line"
		}
		return strconv.Itoa(n) + " reporting lines"
	}
	if failed == 0 {
		return lines(applied) + " changed"
	}
	return fmt.Sprintf("%s changed and %d could not be", lines(applied), failed)
}

// ---- POST /api/v1/assignments/{id}/close ----

// handleAPIAssignmentClose hands an assignment back: Needs you's "Hand
// back", closing it as done or dropped with the outcome its asker
// reads first.
func (s *Server) handleAPIAssignmentClose(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 1 {
		writeAPIError(w, http.StatusNotFound, "there is no such assignment", whoNoOne)
		return
	}
	var req apitypes.AssignmentCloseRequest
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	if req.Resolution != apitypes.AssignmentResolutionDone && req.Resolution != apitypes.AssignmentResolutionDropped {
		writeAPIError(w, http.StatusBadRequest, "resolution must be done or dropped", whoDevelopers)
		return
	}
	if strings.TrimSpace(req.Outcome) == "" {
		writeAPIError(w, http.StatusBadRequest, "say what was done, or why it is dropped", whoYou)
		return
	}
	if s.tracker() == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "no model is connected yet, so assignments cannot change", whoServer)
		return
	}
	if _, err := s.Store.ReadAssignment(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, fmt.Sprintf("there is no assignment #%d", id), whoNoOne)
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "the assignment could not be read", whoServer)
		return
	}
	if err := s.closeAssignmentAsCEO(r.Context(), id, string(req.Resolution), req.Outcome); err != nil {
		if assignments.IsRefusal(err) {
			writeAPIError(w, http.StatusConflict, err.Error(), whoYou)
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, fmt.Sprintf("there is no assignment #%d", id), whoNoOne)
			return
		}
		writeAPIError(w, http.StatusInternalServerError, "the assignment could not be closed", whoServer)
		return
	}
	s.NotifyOrgState()
	writeJSON(w, http.StatusOK, apitypes.Empty{})
}
