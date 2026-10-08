package web

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/kivali-ai/kivali/internal/agent"
	"github.com/kivali-ai/kivali/internal/owner"
	"github.com/kivali-ai/kivali/internal/seed"
	"github.com/kivali-ai/kivali/internal/store"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// The Org page's API: usage, the organization's name and logo, the
// handbook, project files, skills, network, and backup/restore. Every
// mutation runs through a method that owns the rule (saveHandbook,
// installSkill, …); these
// handlers only translate between JSON or multipart and those methods. List mutations answer
// with the list as it now stands.

// wireAPIOrgRoutes mounts the Org routes on the API mux.
func (s *Server) wireAPIOrgRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/org/usage", s.handleAPIOrgUsage)

	mux.HandleFunc("GET /api/v1/org", s.handleAPIOrg)
	mux.HandleFunc("PUT /api/v1/org", s.handleAPIOrgPut)
	mux.HandleFunc("POST /api/v1/org/logo", s.handleAPIOrgLogoUpload)
	mux.HandleFunc("DELETE /api/v1/org/logo", s.handleAPIOrgLogoDelete)

	mux.HandleFunc("GET /api/v1/org/handbook", s.handleAPIHandbook)
	mux.HandleFunc("PUT /api/v1/org/handbook", s.handleAPIHandbookPut)

	mux.HandleFunc("GET /api/v1/org/files", s.handleAPIFiles)
	mux.HandleFunc("POST /api/v1/org/files", s.handleAPIFilesUpload)
	mux.HandleFunc("GET /api/v1/org/files/{sha}/original", s.handleAPIFileOriginal)
	mux.HandleFunc("DELETE /api/v1/org/files/{sha}", s.handleAPIFileDelete)
	mux.HandleFunc("POST /api/v1/org/files/bulk-delete", s.handleAPIFilesBulkDelete)

	mux.HandleFunc("GET /api/v1/org/skills", s.handleAPISkills)
	mux.HandleFunc("POST /api/v1/org/skills", s.handleAPISkillUpload)
	mux.HandleFunc("POST /api/v1/org/skills/{name}/enable", s.handleAPISkillToggle(true))
	mux.HandleFunc("POST /api/v1/org/skills/{name}/disable", s.handleAPISkillToggle(false))
	mux.HandleFunc("DELETE /api/v1/org/skills/{name}", s.handleAPISkillDelete)

	mux.HandleFunc("GET /api/v1/org/network", s.handleAPINetwork)
	mux.HandleFunc("POST /api/v1/org/network", s.handleAPINetworkAdd)
	mux.HandleFunc("DELETE /api/v1/org/network/{host}", s.handleAPINetworkRemove)

	mux.HandleFunc("POST /api/v1/org/backup", s.handleBackupDownload)
	mux.HandleFunc("POST /api/v1/org/restore", s.handleAPIRestore)
}

// ---- refusals ----

// orgRefusal is an Org action turned down because of the request, not
// the server: an HTTP status, what happened, and who can fix it. The
// API answers it in its error shape (writeRefusalAPI).
type orgRefusal struct {
	Status int
	Msg    string
	// Who is who can fix it; empty means the person asking.
	Who string
}

func (e *orgRefusal) Error() string { return e.Msg }

func refuse(status int, msg string) error { return &orgRefusal{Status: status, Msg: msg} }

func refuseWho(status int, msg, who string) error {
	return &orgRefusal{Status: status, Msg: msg, Who: who}
}

// writeRefusalAPI answers an API request the same way, in the API's
// error shape.
func writeRefusalAPI(w http.ResponseWriter, err error) {
	var refusal *orgRefusal
	if !errors.As(err, &refusal) {
		writeAPIError(w, http.StatusInternalServerError, err.Error(), whoServer)
		return
	}
	who := refusal.Who
	if who == "" {
		who = whoYou
		if refusal.Status >= 500 {
			who = whoServer
		}
	}
	writeAPIError(w, refusal.Status, refusal.Msg, who)
}

// Multipart endpoints read their body with parseAPIMultipart
// (api_setup.go): multipart/form-data only (JSON is a 415), capped
// with http.MaxBytesReader, 32 MB held in memory.

// multipartOverhead is room for the form's boundaries and small fields
// on top of a size cap that applies to the file itself.
const multipartOverhead = 1 << 20

// formFiles is every file uploaded under any of names.
func formFiles(r *http.Request, names ...string) []*multipart.FileHeader {
	return multipartFiles(r, names)
}

// ---- usage ----

func (s *Server) handleAPIOrgUsage(w http.ResponseWriter, _ *http.Request) {
	names := s.agentNames()
	name := func(slug string) string {
		if slug == agent.CEOSlug {
			return "You"
		}
		if n, ok := names[slug]; ok {
			return n
		}
		return slug
	}
	u, err := s.usageRollup(s.clk().Now(), name)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "usage could not be read: "+err.Error(), whoServer)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// agentNames is every agent's display name, active or archived.
func (s *Server) agentNames() map[string]string {
	out := map[string]string{}
	archived, _ := s.Store.ListArchivedAgents()
	for _, a := range archived {
		out[a.Slug] = agentDisplayName(a)
	}
	active, _ := s.Store.ListActiveAgents()
	for _, a := range active {
		out[a.Slug] = agentDisplayName(a)
	}
	return out
}

// ---- organization ----

// orgLogoPath is the logo the API points at: the largest favicon
// derivative, served at /branding/{name}.
const orgLogoPath = "/branding/icon-512.png"

func (s *Server) orgView() (apitypes.Org, error) {
	br, err := s.Store.ReadBranding()
	if err != nil {
		return apitypes.Org{}, err
	}
	o := apitypes.Org{Name: br.CompanyName, HasLogo: br.HasFavicon, Kind: apitypes.TeamKind(br.TeamKind), OwnerName: br.OwnerName}
	if br.HasFavicon {
		u := orgLogoPath
		o.LogoURL = &u
	}
	return o, nil
}

func (s *Server) writeOrg(w http.ResponseWriter) {
	o, err := s.orgView()
	if err != nil {
		writeRefusalAPI(w, err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

func (s *Server) handleAPIOrg(w http.ResponseWriter, _ *http.Request) { s.writeOrg(w) }

func (s *Server) handleAPIOrgPut(w http.ResponseWriter, r *http.Request) {
	var req apitypes.OrgPut
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	// The kind is checked before anything is written, so a refused
	// request changes nothing.
	if req.Kind != nil && !store.ValidTeamKind(string(*req.Kind)) {
		writeRefusalAPI(w, refuse(http.StatusBadRequest, "the kind must be work or personal"))
		return
	}
	if req.OwnerName != nil {
		if _, err := owner.CleanName(*req.OwnerName); err != nil {
			writeRefusalAPI(w, refuse(http.StatusBadRequest, "Use a name of at most 40 characters on one line, without a colon, a backtick or a > ("+err.Error()+")."))
			return
		}
	}
	if err := s.setCompanyName(req.Name); err != nil {
		writeRefusalAPI(w, err)
		return
	}
	if req.Kind != nil {
		if err := s.setTeamKind(string(*req.Kind)); err != nil {
			writeRefusalAPI(w, err)
			return
		}
	}
	if req.OwnerName != nil {
		if err := s.Store.WriteOwnerName(*req.OwnerName); err != nil {
			writeRefusalAPI(w, err)
			return
		}
	}
	s.writeOrg(w)
}

// handleAPIOrgLogoUpload takes the logo as multipart (field "logo";
// "favicon" and "file" also work). The image rules are the favicon
// upload's: a square PNG, at least 64 px a side (512 recommended), at
// most 5 MB.
func (s *Server) handleAPIOrgLogoUpload(w http.ResponseWriter, r *http.Request) {
	if !parseAPIMultipart(w, r, maxFaviconBytes+multipartOverhead, "the logo and its form") {
		return
	}
	defer removeMultipartForm(r)
	files := formFiles(r, "logo", "favicon", "file")
	if len(files) == 0 {
		writeAPIError(w, http.StatusBadRequest, "no logo was uploaded", whoDevelopers)
		return
	}
	f, err := files[0].Open()
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "the logo could not be read", whoYou)
		return
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxFaviconBytes+1))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "the logo could not be read", whoYou)
		return
	}
	if err := s.setLogo(raw); err != nil {
		writeRefusalAPI(w, err)
		return
	}
	s.writeOrg(w)
}

func (s *Server) handleAPIOrgLogoDelete(w http.ResponseWriter, _ *http.Request) {
	if err := s.Store.RemoveFavicon(); err != nil {
		writeRefusalAPI(w, err)
		return
	}
	s.writeOrg(w)
}

// ---- handbook ----

func (s *Server) handbookView() (apitypes.Handbook, error) {
	body, err := s.Store.ReadHandbook()
	draft := errors.Is(err, store.ErrNotFound)
	switch {
	case draft:
		br, _ := s.Store.ReadBranding()
		body = seed.HandbookFor(br.TeamKind)
	case err != nil:
		return apitypes.Handbook{}, err
	}
	c := apitypes.Handbook{Content: body, Sections: splitDocSections(body), Draft: draft}
	if !draft {
		if at, err := s.Store.HandbookUpdatedAt(); err == nil {
			c.UpdatedAt = &at
		}
	}
	return c, nil
}

func (s *Server) writeHandbook(w http.ResponseWriter) {
	c, err := s.handbookView()
	if err != nil {
		writeRefusalAPI(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleAPIHandbook(w http.ResponseWriter, _ *http.Request) {
	s.writeHandbook(w)
}

func (s *Server) handleAPIHandbookPut(w http.ResponseWriter, r *http.Request) {
	var req apitypes.HandbookPut
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	if err := s.saveHandbook(req.Content); err != nil {
		writeRefusalAPI(w, err)
		return
	}
	s.writeHandbook(w)
}

// docHeadingRE is a `##` section heading, as DocDiff's splitSections
// matches it: exactly two hashes, then whitespace.
var docHeadingRE = regexp.MustCompile(`^##\s+(.*)`)

// splitDocSections splits markdown at `##` headings exactly as the
// design system's DocDiff.splitSections does, so the server's sections
// and the client's diff cards agree: text before the first heading is
// "Opening" (dropped when blank), a heading's own line is not in its
// Lines, and an empty section is kept only when it is the first.
// LineCount counts the non-blank lines.
func splitDocSections(md string) []apitypes.DocSection {
	hasText := func(lines []string) bool {
		return slices.ContainsFunc(lines, func(l string) bool { return strings.TrimSpace(l) != "" })
	}
	out := []apitypes.DocSection{}
	cur := apitypes.DocSection{Title: "Opening", Lines: []string{}}
	for _, l := range strings.Split(md, "\n") {
		if m := docHeadingRE.FindStringSubmatch(l); m != nil {
			if hasText(cur.Lines) || (len(out) == 0 && cur.Title != "Opening") {
				out = append(out, cur)
			}
			cur = apitypes.DocSection{Title: strings.TrimSpace(m[1]), Lines: []string{}}
			continue
		}
		cur.Lines = append(cur.Lines, l)
	}
	out = append(out, cur)
	out = slices.DeleteFunc(out, func(s apitypes.DocSection) bool {
		return s.Title == "Opening" && !hasText(s.Lines)
	})
	for i := range out {
		for _, l := range out[i].Lines {
			if strings.TrimSpace(l) != "" {
				out[i].LineCount++
			}
		}
	}
	return out
}

// ---- project files ----

func (s *Server) projectFilesView() (apitypes.ProjectFiles, error) {
	files, err := s.Store.ListProjectFiles()
	if err != nil {
		return apitypes.ProjectFiles{}, err
	}
	out := apitypes.ProjectFiles{Files: make([]apitypes.ProjectFile, 0, len(files))}
	for _, f := range files {
		out.Files = append(out.Files, apitypes.ProjectFile{
			SHA:        f.SHA,
			Name:       f.OriginalName,
			SizeBytes:  f.Size,
			UploadedAt: f.UploadedAt,
			Extracted:  f.CanonicalName != "",
			Kind:       f.Kind,
			Summary:    f.Summary,
			URL:        "/api/v1/org/files/" + f.SHA + "/original",
		})
	}
	return out, nil
}

func (s *Server) writeProjectFiles(w http.ResponseWriter) {
	v, err := s.projectFilesView()
	if err != nil {
		writeRefusalAPI(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleAPIFiles(w http.ResponseWriter, _ *http.Request) { s.writeProjectFiles(w) }

// handleAPIFilesUpload takes one or many files as multipart (field
// "file", or "files" / "files[]"), plus an optional graph "kind" for
// all of them. The whole body is capped at maxUploadBytes, the files
// form's limit.
func (s *Server) handleAPIFilesUpload(w http.ResponseWriter, r *http.Request) {
	if !parseAPIMultipart(w, r, maxUploadBytes, "the files") {
		return
	}
	defer removeMultipartForm(r)
	if err := s.addProjectFiles(r.Context(), formFiles(r, "file", "files", "files[]"), r.FormValue("kind")); err != nil {
		writeRefusalAPI(w, err)
		return
	}
	s.writeProjectFiles(w)
}

// handleAPIFileOriginal downloads a project file as uploaded.
func (s *Server) handleAPIFileOriginal(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if !validFileSHA(sha) {
		writeAPIError(w, http.StatusNotFound, "no such file", whoNoOne)
		return
	}
	pf, err := s.Store.GetProjectFile(sha)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "no such file", whoNoOne)
		return
	}
	rc, err := s.Store.OpenOriginal(sha)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "no such file", whoNoOne)
		return
	}
	defer func() { _ = rc.Close() }()
	ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(pf.OriginalName)))
	w.Header().Set("content-type", cmp.Or(ct, "application/octet-stream"))
	w.Header().Set("content-disposition", mime.FormatMediaType("attachment", map[string]string{"filename": pf.OriginalName}))
	w.Header().Set("x-content-type-options", "nosniff")
	_, _ = io.Copy(w, rc)
}

func (s *Server) handleAPIFileDelete(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	// Only a sha the index holds ever reaches the store's path join.
	if !validFileSHA(sha) {
		writeAPIError(w, http.StatusNotFound, "that file is not in the project files", whoNoOne)
		return
	}
	if _, err := s.Store.GetProjectFile(sha); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "that file is not in the project files", whoNoOne)
			return
		}
		writeRefusalAPI(w, err)
		return
	}
	if err := s.removeProjectFile(sha); err != nil {
		writeRefusalAPI(w, err)
		return
	}
	s.writeProjectFiles(w)
}

func (s *Server) handleAPIFilesBulkDelete(w http.ResponseWriter, r *http.Request) {
	var req apitypes.FilesBulkDelete
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	if len(req.SHAs) == 0 {
		writeAPIError(w, http.StatusBadRequest, "no files selected", whoDevelopers)
		return
	}
	s.finishFilesDelete(w, req.SHAs, "project file bulk delete")
}

func (s *Server) finishFilesDelete(w http.ResponseWriter, shas []string, reason string) {
	failed, err := s.removeProjectFiles(shas, reason)
	if err != nil {
		writeRefusalAPI(w, err)
		return
	}
	if len(failed) > 0 {
		writeAPIError(w, http.StatusInternalServerError, "some files could not be deleted: "+strings.Join(failed, "; "), whoServer)
		return
	}
	s.writeProjectFiles(w)
}

// ---- skills ----

func (s *Server) skillsView() (apitypes.Skills, error) {
	skills, err := s.Store.ListSkills()
	if err != nil {
		return apitypes.Skills{}, err
	}
	out := apitypes.Skills{Skills: make([]apitypes.Skill, 0, len(skills))}
	for _, k := range skills {
		out.Skills = append(out.Skills, apitypes.Skill{
			Name: k.Name, Version: k.Version, Enabled: k.Enabled, Builtin: k.Builtin,
			Description: k.Description, UpdatedAt: k.UpdatedAt,
		})
	}
	return out, nil
}

func (s *Server) writeSkills(w http.ResponseWriter) {
	v, err := s.skillsView()
	if err != nil {
		writeRefusalAPI(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleAPISkills(w http.ResponseWriter, _ *http.Request) { s.writeSkills(w) }

// handleAPISkillUpload adds a skill, or replaces the installed one of
// the same name, from one multipart "file" (.zip, .skill or SKILL.md).
// A version not newer than the installed one is a 409 SkillDowngrade
// (and the form route's X-Kivali-* headers); the same upload with the
// field confirm_downgrade=true, or ?force=1, lands it.
func (s *Server) handleAPISkillUpload(w http.ResponseWriter, r *http.Request) {
	if !parseAPIMultipart(w, r, maxSkillUpload+multipartOverhead, "the skill and its form") {
		return
	}
	defer removeMultipartForm(r)
	var fh *multipart.FileHeader
	if files := formFiles(r, "file"); len(files) > 0 {
		fh = files[0]
	}
	up, err := parseSkillUpload(fh)
	if err != nil {
		writeRefusalAPI(w, err)
		return
	}
	confirm := r.FormValue("confirm_downgrade")
	force := confirm == "true" || confirm == "1" || r.URL.Query().Get("force") == "1"
	if err := s.installSkill(up, force); err != nil {
		var down *skillDowngradeRefusal
		if errors.As(err, &down) {
			w.Header().Set("X-Kivali-Downgrade", "1")
			w.Header().Set("X-Kivali-Skill-Old-Version", down.oldVersion)
			w.Header().Set("X-Kivali-Skill-New-Version", down.newVersion)
			writeJSON(w, http.StatusConflict, apitypes.SkillDowngrade{
				Error:      fmt.Sprintf("%s %s is not newer than the installed %s", down.name, down.newVersion, down.oldVersion),
				Who:        "you, by confirming the replacement",
				OldVersion: down.oldVersion,
				NewVersion: down.newVersion,
			})
			return
		}
		writeRefusalAPI(w, err)
		return
	}
	s.writeSkills(w)
}

func (s *Server) handleAPISkillToggle(enabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := s.setSkillEnabled(r.PathValue("name"), enabled); err != nil {
			writeRefusalAPI(w, err)
			return
		}
		s.writeSkills(w)
	}
}

func (s *Server) handleAPISkillDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.deleteSkill(r.PathValue("name")); err != nil {
		writeRefusalAPI(w, err)
		return
	}
	s.writeSkills(w)
}

// ---- network ----

func (s *Server) networkView() (apitypes.Network, error) {
	al, err := s.Store.ReadEgressAllowlist()
	if err != nil {
		return apitypes.Network{}, err
	}
	out := apitypes.Network{Hosts: make([]apitypes.NetworkHost, 0, len(al.Patterns))}
	for _, p := range al.Patterns {
		out.Hosts = append(out.Hosts, apitypes.NetworkHost{Host: p})
	}
	return out, nil
}

func (s *Server) writeNetwork(w http.ResponseWriter) {
	v, err := s.networkView()
	if err != nil {
		writeRefusalAPI(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleAPINetwork(w http.ResponseWriter, _ *http.Request) { s.writeNetwork(w) }

// validHost is a hostname pattern the egress proxy can use: no
// scheme, path, port separator, whitespace or comment marker. The
// proxy matches the host and every subdomain, and reads "*" and "[a-z]"
// as a glob within one label.
func validHost(h string) bool {
	return h != "" && !strings.HasPrefix(h, "#") && !strings.ContainsAny(h, "/: \t\r\n@?")
}

func (s *Server) handleAPINetworkAdd(w http.ResponseWriter, r *http.Request) {
	var req apitypes.NetworkAdd
	if !decodeJSONOrError(w, r, &req) {
		return
	}
	host := strings.ToLower(strings.TrimSpace(req.Host))
	if !validHost(host) {
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("%q is not a host name; enter it without http:// or a path", req.Host), whoYou)
		return
	}
	al, err := s.Store.ReadEgressAllowlist()
	if err != nil {
		writeRefusalAPI(w, err)
		return
	}
	if err := s.saveEgressPatterns(append(al.Patterns, host)); err != nil {
		writeRefusalAPI(w, err)
		return
	}
	s.writeNetwork(w)
}

func (s *Server) handleAPINetworkRemove(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(strings.TrimSpace(r.PathValue("host")))
	al, err := s.Store.ReadEgressAllowlist()
	if err != nil {
		writeRefusalAPI(w, err)
		return
	}
	i := slices.Index(al.Patterns, host)
	if i < 0 {
		writeAPIError(w, http.StatusNotFound, host+" is not on the network list", whoNoOne)
		return
	}
	if err := s.saveEgressPatterns(slices.Delete(al.Patterns, i, i+1)); err != nil {
		writeRefusalAPI(w, err)
		return
	}
	s.writeNetwork(w)
}

// ---- restore ----

// handleAPIRestore unpacks a backup (multipart field "archive", or
// "file") into a fresh deployment, as the settings form does. The body
// is capped at the restore's 2 GB archive limit, and is streamed once
// onto the data volume (see stageRestoreUpload), never parsed into
// memory or the temp directory.
func (s *Server) handleAPIRestore(w http.ResponseWriter, r *http.Request) {
	// Refuse before reading a multi-GB upload.
	if err := s.checkRestoreAllowed(); err != nil {
		writeRefusalAPI(w, err)
		return
	}
	staged, filename, ok := s.stageRestoreUpload(w, r)
	if !ok {
		return
	}
	defer func() { _ = os.Remove(staged) }()
	if err := s.restoreArchive(r.Context(), staged, filename); err != nil {
		writeRefusalAPI(w, err)
		return
	}
	writeJSON(w, http.StatusOK, apitypes.Empty{})
}
