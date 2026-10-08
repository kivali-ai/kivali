package web

import (
	"net/http"
	"strings"
	"unicode"

	"github.com/kivali-ai/kivali/internal/auth"
	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// handleAPIMe serves GET /api/v1/me: the signed-in person and the org.
func (s *Server) handleAPIMe(w http.ResponseWriter, r *http.Request) {
	email := auth.UserFromContext(r.Context())
	name, initials := nameFromEmail(email)
	br, _ := s.Store.ReadBranding()
	writeJSON(w, http.StatusOK, apitypes.Me{
		User:    apitypes.MeUser{Email: email, Name: name, Initials: initials},
		Org:     apitypes.MeOrg{Name: br.CompanyName, HasLogo: br.HasFavicon, OwnerName: br.OwnerName},
		Version: s.VersionName,
		DevMode: s.DevUser != "",
	})
}

// handleAPISnapshot serves GET /api/v1/snapshot: the same bytes the
// next /org/stream `snapshot` event would carry, for first paint.
func (s *Server) handleAPISnapshot(w http.ResponseWriter, _ *http.Request) {
	writeJSONBytes(w, http.StatusOK, s.buildOrgSnapshot())
}

// nameFromEmail derives a display name and initials from an email's
// local part, since the session carries nothing else: "jane.doe" is
// "Jane Doe" / "JD", "maya2645" is "Maya" / "M". A "+tag" is dropped,
// words split on dots, dashes and underscores, and digits trimmed from
// each word's ends. A local part with no letters is used as it is.
func nameFromEmail(email string) (name, initials string) {
	local, _, _ := strings.Cut(email, "@")
	local, _, _ = strings.Cut(local, "+")
	var words []string
	for _, w := range strings.FieldsFunc(local, func(r rune) bool { return r == '.' || r == '-' || r == '_' }) {
		w = strings.TrimFunc(w, unicode.IsDigit)
		if w == "" {
			continue
		}
		rs := []rune(strings.ToLower(w))
		rs[0] = unicode.ToUpper(rs[0])
		words = append(words, string(rs))
	}
	if len(words) == 0 {
		if local == "" {
			return "", ""
		}
		return local, strings.ToUpper(string([]rune(local)[:1]))
	}
	initials = string([]rune(words[0])[:1])
	if len(words) > 1 {
		initials += string([]rune(words[len(words)-1])[:1])
	}
	return strings.Join(words, " "), initials
}
