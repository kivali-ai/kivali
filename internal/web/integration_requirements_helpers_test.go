package web

import (
	"net/http"
	"slices"
	"testing"

	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// unmetIDs is the ids of page's requirements that are not met, in the
// page's order.
func unmetIDs(page apitypes.IntegrationPage) []string {
	out := []string{}
	for _, r := range page.Requirements {
		if !r.Met {
			out = append(out, r.ID)
		}
	}
	return out
}

// requirementIDs is the ids of page's requirements, in order.
func requirementIDs(page apitypes.IntegrationPage) []string {
	out := []string{}
	for _, r := range page.Requirements {
		out = append(out, r.ID)
	}
	return out
}

// requirementOf is page's requirement of that id; ok is false with none.
func requirementOf(page apitypes.IntegrationPage, id string) (apitypes.IntegrationRequirement, bool) {
	i := slices.IndexFunc(page.Requirements, func(r apitypes.IntegrationRequirement) bool { return r.ID == id })
	if i < 0 {
		return apitypes.IntegrationRequirement{}, false
	}
	return page.Requirements[i], true
}

// requirementNeeds is Home's integration_requirement rows, by id.
func requirementNeeds(t *testing.T, srv *Server) map[string]apitypes.NeedItem {
	t.Helper()
	home := decodeAPI[apitypes.Home](t, homeDo(t, srv, http.MethodGet, "/api/v1/home", "", nil, nil))
	out := map[string]apitypes.NeedItem{}
	for _, item := range home.Needs {
		if item.Kind == apitypes.NeedKindIntegrationRequirement {
			out[item.ID] = item
		}
	}
	return out
}

// installedAndMet reports whether page is installed with every
// requirement met.
func installedAndMet(page apitypes.IntegrationPage) bool {
	return page.Summary.State == apitypes.IntegrationStateInstalled && len(unmetIDs(page)) == 0 && page.Summary.Needs == 0
}
