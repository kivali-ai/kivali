//go:build darwin && cgo

package main

import (
	"github.com/kivali-ai/kivali/internal/supervisor"
	"github.com/kivali-ai/kivali/internal/supervisor/vz"
)

// vmImageHint names the VM image artifacts serve looks for, for the
// "no VM image found" message.
const vmImageHint = "Image, initramfs.gz and root.squashfs"

// newBackend is Virtualization.framework.
func newBackend() supervisor.Backend { return vz.Backend{} }
