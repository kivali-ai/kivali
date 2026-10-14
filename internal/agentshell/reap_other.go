//go:build !linux

package agentshell

// BecomeSubreaper is Linux-only; elsewhere it does nothing.
func BecomeSubreaper() error { return nil }

// KillDescendants is Linux-only; elsewhere it does nothing.
func KillDescendants() {}
