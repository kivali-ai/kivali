package web

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"

	"github.com/kivali-ai/kivali/internal/store"
)

// maxFaviconBytes caps the favicon upload buffered into memory. A source
// PNG large enough to derive a 512px icon is well under this; the cap
// just stops a hostile or fat-fingered multi-MB upload from ballooning
// memory.
const maxFaviconBytes = 5 << 20

// minFaviconDim is the smallest square source we accept. Below this the
// 180/512 derivatives would be upscaled blur; 512×512 is recommended.
const minFaviconDim = 64

// maxCompanyNameRunes bounds the company name so it can't blow out the
// app's org switcher or the browser title. Rejecting absurd input up
// front gives a clear error instead of a silently-clipped name.
const maxCompanyNameRunes = 64

// setCompanyName saves, or with a blank name clears, the organization's
// name. Over maxCompanyNameRunes is refused.
func (s *Server) setCompanyName(name string) error {
	name = strings.TrimSpace(name)
	if len([]rune(name)) > maxCompanyNameRunes {
		return refuse(http.StatusBadRequest, fmt.Sprintf("company name must be %d characters or fewer", maxCompanyNameRunes))
	}
	return s.Store.WriteCompanyName(name)
}

// SeedBranding applies what the process was started with
// (KIVALI_SEED_ORG_NAME, KIVALI_TEAM_KIND) at boot: name becomes the
// org's name when none is stored, through setCompanyName so its rules
// apply, and kind is stored when no kind is. Neither overwrites what is
// stored; empty arguments do nothing.
func (s *Server) SeedBranding(name, kind string) error {
	br, err := s.Store.ReadBranding()
	if err != nil {
		return err
	}
	var errs []error
	if strings.TrimSpace(name) != "" && br.CompanyName == "" {
		if err := s.setCompanyName(name); err != nil {
			errs = append(errs, fmt.Errorf("seed org name: %w", err))
		}
	}
	if kind != "" && br.TeamKind == "" {
		if err := s.Store.WriteTeamKind(kind); err != nil {
			errs = append(errs, fmt.Errorf("seed team kind: %w", err))
		}
	}
	return errors.Join(errs...)
}

// SeedOwnerName applies KIVALI_OWNER_NAME at boot: name becomes what
// agents and pages call the person, the first time only. Once a name
// was ever stored or cleared (OwnerNameSet, or a name stored before
// that flag existed) boot leaves it alone: the env var stays in the
// deployment, and a name the person cleared or changed in the app must
// not come back at the next restart. An empty name does nothing.
func (s *Server) SeedOwnerName(name string) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	br, err := s.Store.ReadBranding()
	if err != nil {
		return err
	}
	if br.OwnerNameSet || br.OwnerName != "" {
		return nil
	}
	return s.Store.WriteOwnerName(name)
}

// teamKind is the org's stored kind: store.TeamKindWork,
// store.TeamKindPersonal, or "" when never set (which behaves as work).
func (s *Server) teamKind() string {
	br, _ := s.Store.ReadBranding()
	return br.TeamKind
}

// setTeamKind changes the org's kind. Anything but work or personal is
// refused.
func (s *Server) setTeamKind(kind string) error {
	if !store.ValidTeamKind(kind) {
		return refuse(http.StatusBadRequest, "the kind must be work or personal")
	}
	return s.Store.WriteTeamKind(kind)
}

// setLogo validates an uploaded logo and derives the favicon asset set
// from it: a square PNG at least minFaviconDim on a side, at most
// maxFaviconBytes. An invalid image is refused with what is wrong.
func (s *Server) setLogo(raw []byte) error {
	if len(raw) > maxFaviconBytes {
		return refuse(http.StatusRequestEntityTooLarge, fmt.Sprintf("the logo is over %d MB", maxFaviconBytes>>20))
	}
	assets, err := buildFaviconAssets(raw)
	if err != nil {
		return refuse(http.StatusBadRequest, err.Error())
	}
	return s.Store.WriteFaviconAssets(assets)
}

// handleFaviconAsset serves a derived favicon asset by its public name.
// Registered unauthenticated so the sign-in screen can show the org's
// mark too. The store validates name against a
// fixed allowlist, so an unknown or traversal name just 404s.
func (s *Server) handleFaviconAsset(w http.ResponseWriter, r *http.Request, name string) {
	data, mime, err := s.Store.ReadFaviconAsset(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("content-type", mime)
	// Match the project's static-asset policy: embed.FS-style zero
	// modtimes make stale caches likely, and a re-uploaded favicon must
	// take effect immediately.
	w.Header().Set("cache-control", "no-store, must-revalidate")
	_, _ = w.Write(data)
}

// buildFaviconAssets validates a source PNG and derives the full favicon
// asset set: a multi-size favicon.ico (16/32/48, PNG-encoded entries)
// for legacy /favicon.ico fetchers, plus 32/180/512 PNGs for modern,
// retina, and Apple-touch use. Returns a filename→bytes map ready for
// store.WriteFaviconAssets.
//
// Validation: the source must decode as PNG, be square, and be at least
// minFaviconDim on a side. Derivatives are only ever downscaled — a
// target larger than the source is clamped to the source size so we
// never invent detail (upscale blur).
func buildFaviconAssets(raw []byte) (map[string][]byte, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("not a valid image: %v", err)
	}
	if format != "png" {
		return nil, fmt.Errorf("favicon must be a PNG (got %s)", format)
	}
	if cfg.Width != cfg.Height {
		return nil, fmt.Errorf("favicon must be square (got %d×%d)", cfg.Width, cfg.Height)
	}
	if cfg.Width < minFaviconDim {
		return nil, fmt.Errorf("favicon must be at least %d×%d (got %d×%d); 512×512 recommended", minFaviconDim, minFaviconDim, cfg.Width, cfg.Height)
	}
	src, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("not a valid PNG: %v", err)
	}
	srcDim := cfg.Width

	pngAt := func(size int) ([]byte, error) {
		if size > srcDim {
			size = srcDim
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, downscale(src, size)); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}

	assets := map[string][]byte{}
	for _, size := range []int{32, 180, 512} {
		b, err := pngAt(size)
		if err != nil {
			return nil, err
		}
		assets[fmt.Sprintf("icon-%d.png", size)] = b
	}

	// Multi-size .ico embedding PNG-encoded 16/32/48 entries.
	icoSizes := []int{16, 32, 48}
	icoPNGs := make([][]byte, 0, len(icoSizes))
	for _, size := range icoSizes {
		b, err := pngAt(size)
		if err != nil {
			return nil, err
		}
		icoPNGs = append(icoPNGs, b)
	}
	ico, err := encodeICO(icoSizes, icoPNGs)
	if err != nil {
		return nil, err
	}
	assets["favicon.ico"] = ico
	return assets, nil
}

// downscale resizes src down to size×size using box (area-average)
// sampling: each destination pixel is the mean of the source region it
// covers. Box sampling is the right filter for the large reductions a
// favicon needs — it avoids the aliasing a single-tap nearest/bilinear
// would show on logos with thin strokes. Averaging is done on the
// alpha-premultiplied 16-bit values RGBA() returns, which is the
// correct way to downscale an image with transparency. If size exceeds
// the source (callers clamp, but be defensive), the inner spans collapse
// to a single source pixel — nearest, never blur.
func downscale(src image.Image, size int) *image.RGBA {
	if size < 1 {
		size = 1
	}
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	for dy := 0; dy < size; dy++ {
		sy0 := b.Min.Y + dy*sh/size
		sy1 := b.Min.Y + (dy+1)*sh/size
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for dx := 0; dx < size; dx++ {
			sx0 := b.Min.X + dx*sw/size
			sx1 := b.Min.X + (dx+1)*sw/size
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var rs, gs, bs, as, n uint64
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					r, g, bb, a := src.At(sx, sy).RGBA()
					rs += uint64(r)
					gs += uint64(g)
					bs += uint64(bb)
					as += uint64(a)
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			dst.Set(dx, dy, color.RGBA64{
				R: uint16(rs / n),
				G: uint16(gs / n),
				B: uint16(bs / n),
				A: uint16(as / n),
			})
		}
	}
	return dst
}

// encodeICO assembles a multi-image .ico from per-size PNG payloads.
// Browsers and Windows (Vista+) accept PNG-encoded image data inside an
// ICO directory entry, so we embed the PNGs verbatim rather than
// re-encoding to BMP — no decoder dependency, and the alpha channel
// survives intact. sizes[i] is the pixel dimension of pngs[i], used only
// for the directory entry's width/height bytes (0 means 256).
func encodeICO(sizes []int, pngs [][]byte) ([]byte, error) {
	if len(sizes) != len(pngs) {
		return nil, fmt.Errorf("encodeICO: %d sizes but %d images", len(sizes), len(pngs))
	}
	var buf bytes.Buffer
	// ICONDIR header: reserved=0, type=1 (icon), image count.
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(len(pngs)))
	// Image data starts after the 6-byte header and all 16-byte entries.
	offset := 6 + 16*len(pngs)
	for i, p := range pngs {
		var wb, hb byte // 0 encodes 256
		if sizes[i] < 256 {
			wb, hb = byte(sizes[i]), byte(sizes[i])
		}
		buf.WriteByte(wb)                                           // width
		buf.WriteByte(hb)                                           // height
		buf.WriteByte(0)                                            // palette size (0 = no palette)
		buf.WriteByte(0)                                            // reserved
		_ = binary.Write(&buf, binary.LittleEndian, uint16(1))      // color planes
		_ = binary.Write(&buf, binary.LittleEndian, uint16(32))     // bits per pixel
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(p))) // image byte size
		_ = binary.Write(&buf, binary.LittleEndian, uint32(offset)) // image offset
		offset += len(p)
	}
	for _, p := range pngs {
		buf.Write(p)
	}
	return buf.Bytes(), nil
}
