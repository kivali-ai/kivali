package web

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kivali-ai/kivali/internal/web/apitypes"
)

// squarePNG returns a dim×dim PNG with a recognizable two-tone pattern so
// downscaling has actual color content to average.
func squarePNG(t *testing.T, dim int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, dim, dim))
	for y := 0; y < dim; y++ {
		for x := 0; x < dim; x++ {
			if (x+y)%2 == 0 {
				img.Set(x, y, color.RGBA{0x33, 0x66, 0x99, 0xff})
			} else {
				img.Set(x, y, color.RGBA{0xff, 0xcc, 0x00, 0xff})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func nonSquarePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestBuildFaviconAssetsHappyPath(t *testing.T) {
	assets, err := buildFaviconAssets(squarePNG(t, 256))
	if err != nil {
		t.Fatalf("buildFaviconAssets: %v", err)
	}
	for _, name := range []string{"favicon.ico", "icon-32.png", "icon-180.png", "icon-512.png"} {
		if len(assets[name]) == 0 {
			t.Errorf("missing/empty asset %s", name)
		}
	}
	// favicon.ico must start with the ICONDIR header: reserved=0,
	// type=1, and a 3-image count (16/32/48).
	ico := assets["favicon.ico"]
	if len(ico) < 6 || ico[0] != 0 || ico[1] != 0 || ico[2] != 1 || ico[3] != 0 {
		t.Fatalf("bad ICO header: % x", ico[:min(6, len(ico))])
	}
	if count := int(ico[4]) | int(ico[5])<<8; count != 3 {
		t.Errorf("ICO image count = %d, want 3", count)
	}
	// The 32px PNG derivative must decode and be exactly 32×32.
	cfg, _, err := image.DecodeConfig(bytes.NewReader(assets["icon-32.png"]))
	if err != nil {
		t.Fatalf("decode icon-32: %v", err)
	}
	if cfg.Width != 32 || cfg.Height != 32 {
		t.Errorf("icon-32 = %d×%d, want 32×32", cfg.Width, cfg.Height)
	}
}

func TestBuildFaviconAssetsValidation(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"non-png", []byte("GIF89a not really"), "valid image"},
		{"non-square", nonSquarePNG(t, 64, 32), "square"},
		{"too-small", squarePNG(t, 32), "at least"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := buildFaviconAssets(c.data)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err.Error(), c.want)
			}
		})
	}
}

// An uploaded logo is derived into the favicon set and served, without
// a session, under /branding/ (the sign-in page shows the org's mark).
// /favicon.ico itself is the Kivali icon from the web app's build.
func TestLogoUploadServesTheDerivedAssets(t *testing.T) {
	srv := newTestServer(t)
	body, ct := orgMultipart(t, []multipartFile{{"logo", "logo.png", squarePNG(t, 256)}}, nil)
	if rr := orgDo(t, srv, "", http.MethodPost, "/api/v1/org/logo", ct, body); rr.Code != http.StatusOK {
		t.Fatalf("upload code = %d, body=%s", rr.Code, rr.Body.String())
	}
	br, _ := srv.Store.ReadBranding()
	if !br.HasFavicon {
		t.Fatal("HasFavicon not set after upload")
	}

	serve := func(path string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil)) // no cookie
		return rr
	}
	rr := serve("/branding/favicon.ico")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /branding/favicon.ico = %d", rr.Code)
	}
	if ct := rr.Header().Get("content-type"); ct != "image/x-icon" {
		t.Errorf("favicon content-type = %q", ct)
	}
	rr = serve("/branding/icon-180.png")
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /branding/icon-180.png = %d", rr.Code)
	}
	if _, _, err := image.DecodeConfig(bytes.NewReader(rr.Body.Bytes())); err != nil {
		t.Errorf("served icon-180 does not decode: %v", err)
	}
}

func TestLogoUploadRejectsBadImage(t *testing.T) {
	srv := newTestServer(t)
	body, ct := orgMultipart(t, []multipartFile{{"logo", "x.png", nonSquarePNG(t, 64, 32)}}, nil)
	assertAPIError(t, orgDo(t, srv, "", http.MethodPost, "/api/v1/org/logo", ct, body), http.StatusBadRequest)
	if br, _ := srv.Store.ReadBranding(); br.HasFavicon {
		t.Error("HasFavicon set despite invalid upload")
	}
}

func TestCompanyNameTooLongRejected(t *testing.T) {
	srv := newTestServer(t)
	assertAPIError(t, orgJSON(t, srv, http.MethodPut, "/api/v1/org", apitypes.OrgPut{Name: strings.Repeat("x", 65)}), http.StatusBadRequest)
	if br, _ := srv.Store.ReadBranding(); br.CompanyName != "" {
		t.Errorf("over-long name was saved: %q", br.CompanyName)
	}
}
