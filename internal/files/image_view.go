package files

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/kivali-ai/kivali/internal/convert"
)

// ImageInlineCap bounds the bytes a file_view of an image returns
// inline. Anthropic's vision API rejects images over 5 MiB; we cap a
// touch lower to leave headroom for base64 expansion + JSON-RPC
// framing. Files over the cap surface as an error so the agent can
// stage them via run_shell.inputs to operate on the bytes directly.
const ImageInlineCap = 4 << 20

// ImageView is the result of TryImageView when modelPath resolved
// to an image file. Header is the human-readable line the model
// gets alongside the bytes (e.g. "[image: /files/foo.png, image/png,
// 67 bytes]"); MIME is the image content type; Data is the raw bytes.
type ImageView struct {
	Header string
	MIME   string
	Data   []byte
}

// TryImageView inspects modelPath and, if it points to a regular
// image file, reads the bytes and returns an ImageView. ok=false
// means "not an image, take the text path." A non-nil error means
// the path resolved to an image but we couldn't deliver it (cap
// exceeded, read failure) — the caller surfaces that as a tool-
// level error.
//
// Run calls it ahead of Dispatch for every file_view.
func TryImageView(b *Backend, modelPath string) (*ImageView, bool, error) {
	mime := convert.MIMEFor(strings.ToLower(filepath.Ext(modelPath)))
	if !IsImageMIME(mime) {
		return nil, false, nil
	}
	abs, _, err := b.resolveExisting(modelPath)
	if err != nil {
		return nil, false, err
	}
	// Follow symlinks (project/, attachments/ are symlink farms), as
	// far as the backend's read roots.
	f, info, err := b.openRead(abs, modelPath)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	if info.IsDir() {
		// Image-named directory is silly but well-defined: fall
		// through to the text path so the agent gets a listing.
		return nil, false, nil
	}
	if info.Size() > ImageInlineCap {
		return nil, false, fmt.Errorf("image %s is %d bytes (cap %d) — too large to inline; reach for run_shell on the same path to operate on the bytes",
			modelPath, info.Size(), ImageInlineCap)
	}
	data, err := io.ReadAll(io.LimitReader(f, ImageInlineCap+1))
	if err != nil {
		return nil, false, err
	}
	return &ImageView{
		Header: fmt.Sprintf("[image: %s, %s, %d bytes]", modelPath, mime, len(data)),
		MIME:   mime,
		Data:   data,
	}, true, nil
}

// IsImageMIME reports whether mime names an image type that file_view
// can return inline as a vision content block. Centralized so
// file_view and list_project_files agree on what counts.
func IsImageMIME(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}
