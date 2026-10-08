//go:build !(darwin && cgo) && !windows

package main

import "github.com/kivali-ai/kivali/internal/supervisor"

// vmImageHint names the VM image artifact serve looks for, for the
// "no VM image found" message (there is no backend here, but the string
// keeps the message well formed).
const vmImageHint = "the VM image"

// newBackend has no backend here: macOS (Virtualization.framework, with
// cgo) and Windows (Hyper-V) have one. Every client subcommand still
// works against a serve running elsewhere on the same machine.
func newBackend() supervisor.Backend { return nil }
