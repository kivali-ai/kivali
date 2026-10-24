// Package builtins is the list of the integrations that ship with
// Kivali, each a built-in whose actions are Go functions. Each
// built-in's package imports builtinintegrations for its types, so the
// list that names them all lives here.
package builtins

import (
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/githubintegration"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/gitintegration"
)

// All is the built-in integrations compiled into this binary.
func All() builtinintegrations.Registry {
	return builtinintegrations.Registry{gitintegration.New(), githubintegration.New()}
}
