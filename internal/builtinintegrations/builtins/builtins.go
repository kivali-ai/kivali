// Package builtins is the list of the integrations that ship with
// Kivali: the built-ins whose actions are Go functions (All) and the
// bundles embedded in the binary (Shipped). Each built-in's package
// imports builtinintegrations for its types, so the lists that name
// them all live here.
package builtins

import (
	"github.com/kivali-ai/kivali/internal/builtinintegrations"
	"github.com/kivali-ai/kivali/internal/builtinintegrations/gitintegration"
)

// All is the built-in integrations compiled into this binary.
func All() builtinintegrations.Registry {
	return builtinintegrations.Registry{gitintegration.New()}
}

// Shipped is the integrations that ship with Kivali as bundles
// embedded in this binary, each run in a pod of its own. A name is in
// All or here, never both.
func Shipped() builtinintegrations.ShippedBundles {
	return builtinintegrations.ShippedBundles{}
}
