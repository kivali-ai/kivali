package githubintegration

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/kivali-ai/kivali/internal/builtinintegrations"
)

// The requirements' ids.
const (
	reqKey          = "key"
	reqInstalled    = "installed"
	reqRepositories = "repositories"
	reqPermissions  = "permissions"
)

// Requirements is GitHub's health, in order: the app's key works (GET
// /app with its JWT); the app is installed (GET
// /app/installations/{id}), and not suspended; the repositories the
// installation reaches; and its permissions cover what this version
// needs. A row that cannot be checked because one before it is not met
// is left out. A failure that names none of these (GitHub not
// answering, its own error) is an error, which core shows as one row.
// Nothing is cached beyond the call.
func (g *GitHub) Requirements(ctx context.Context, conn builtinintegrations.Connection) ([]builtinintegrations.Requirement, error) {
	key := builtinintegrations.Requirement{ID: reqKey, Title: "The app's key works"}
	c, err := readConnection(conn)
	if err != nil {
		key.Words, key.Fix = "Kivali does not hold all of the app's values. Set GitHub up again.", builtinintegrations.FlowSetup
		return []builtinintegrations.Requirement{key}, nil
	}
	app, err := g.appOf(ctx, c)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
			key.Words, key.Fix = "GitHub refused the app's key (HTTP 401): "+sentence(ae.Message)+". Set GitHub up again.", builtinintegrations.FlowSetup
			return []builtinintegrations.Requirement{key}, nil
		}
		return nil, checkError(err)
	}
	key.Met = true
	key.Words = "GitHub accepts the key of the app " + cmp.Or(app.Name, "Kivali made") + ". Uninstalling GitHub here leaves the app on GitHub, for you to delete in its settings at " + app.settingsURL() + "."
	rows := []builtinintegrations.Requirement{key}

	installed := builtinintegrations.Requirement{ID: reqInstalled, Title: "Installed on GitHub"}
	inst, err := g.installationOf(ctx, c)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			installed.Words, installed.Fix = "The app is no longer installed on GitHub.", FlowInstall
			return append(rows, installed), nil
		}
		return nil, checkError(err)
	}
	who := inst.Account.Login
	switch {
	case inst.suspended():
		installed.Words, installed.Fix = "The app's installation on "+who+" is suspended on GitHub.", FlowSettings
	default:
		installed.Met, installed.Words = true, "Installed on "+who+"."
	}
	rows = append(rows, installed)

	if !inst.suspended() {
		repos := builtinintegrations.Requirement{ID: reqRepositories, Title: "Repositories it reaches", Met: true}
		if inst.RepositorySelection == "all" {
			repos.Words = "Installed on " + who + ", reaching all repositories."
		} else {
			n, err := g.repositoryCount(ctx, c)
			if err != nil {
				return nil, checkError(err)
			}
			switch n {
			case 0:
				repos.Met, repos.Fix = false, FlowSettings
				repos.Words = "Installed on " + who + ", reaching no repositories. Choose them on GitHub."
			case 1:
				repos.Words = "Installed on " + who + ", reaching 1 repository."
			default:
				repos.Words = "Installed on " + who + ", reaching " + strconv.Itoa(n) + " repositories."
			}
		}
		rows = append(rows, repos)
	}

	current := builtinintegrations.Requirement{ID: reqPermissions, Title: "Permissions current", Met: true,
		Words: "The installation has every permission this version needs."}
	if lack := lacking(inst.Permissions); len(lack) > 0 {
		current.Met, current.Fix = false, FlowPermissions
		current.Words = "The installation lacks permissions this version needs: " + strings.Join(lack, ", ") + ". Approve them on GitHub."
	}
	return append(rows, current), nil
}

// checkError is a GitHub failure while checking, in words for the
// owner.
func checkError(err error) error {
	var ae *apiError
	var te *tokenError
	switch {
	case errors.As(err, &te):
		return errors.New(sentence(ownerWords(te.api)))
	case errors.As(err, &ae):
		return errors.New(sentence(ownerWords(ae)))
	}
	return err
}
