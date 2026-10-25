package web

import (
	"crypto/sha256"
	"encoding/base64"
	"html"
	"io/fs"
	"net/http"
	"strings"
	"sync"

	"github.com/kivali-ai/kivali/internal/web/ui"
)

// A connect page is one Kivali itself shows the owner's browser during
// an integration's setup, between the web app and the service: the one
// that posts a Go step's values to the service (writeAutoPost) and the
// one the callback answers a browser Kivali Desktop handed off
// (writeDesktopReturn). Each is one card on paper in the design
// system's tokens, type and button, with the lockup at the top, dark
// as the web app is dark (the same stored choice, else the device's),
// and loads only from Kivali's own origin: the lockup from /logos, the
// fonts from the build's /assets when the binary carries one. Its CSP
// starts from default-src 'none', allows its own stylesheet and
// scripts by hash, and names the one origin a page may post to or
// reach besides.
type connectPage struct {
	// Heading is the card's title, in the display face.
	Heading string
	// Words follow the heading: one or two sentences.
	Words string
	// Body is HTML after the words, already escaped: a form, or nothing.
	Body string
	// Script is the page's own script, run at the end of the body; its
	// hash goes into the CSP. Empty for none.
	Script string
	// FormAction is the one origin a form on the page may post to.
	FormAction string
	// ConnectSrc is the one origin the script may reach.
	ConnectSrc string
}

// connectThemeScript paints the page dark as the web app paints itself
// (web/src/app/theme.ts): the choice stored under the app's key on this
// origin, or the device's when none is stored. It runs in the head so
// the page never flashes light first.
const connectThemeScript = `try{var t=localStorage.getItem("kivali.theme");if(t==="dark"||(t!=="light"&&matchMedia("(prefers-color-scheme: dark)").matches))document.documentElement.setAttribute("data-theme","dark")}catch(e){}`

// connectPageCSS is the design system's share the pages need, as the
// web app's tokens have it (web/src/ds/tokens, components/kivali.css):
// paper and ink in both themes, the body and heading type, the card and
// the primary button.
const connectPageCSS = `:root{color-scheme:light;--paper:#F6F3EA;--paper-raised:#FCFAF5;--line:#E2DDD1;--ink:#24211C;--ink-muted:#615D55;--on-ink:#F6F3EA;--shadow-sm:0 1px 2px #24211C14;--focus-ring:0 0 0 2px #F6F3EA,0 0 0 4px #3F53A8}
[data-theme="dark"]{color-scheme:dark;--paper:#17150F;--paper-raised:#221F19;--line:#36322B;--ink:#F3EFE6;--ink-muted:#ABA69B;--on-ink:#17150F;--shadow-sm:0 1px 2px #00000080;--focus-ring:0 0 0 2px #17150F,0 0 0 4px #9AA7E0}
html,body{margin:0;background:var(--paper);color:var(--ink);font:400 15px/24px "IBM Plex Sans",ui-sans-serif,system-ui,sans-serif;-webkit-font-smoothing:antialiased}
body{min-height:100vh;box-sizing:border-box;display:grid;place-items:center;padding:32px 16px}
main{width:100%;max-width:400px;box-sizing:border-box;padding:24px;background:var(--paper-raised);border:1px solid var(--line);border-radius:16px;box-shadow:var(--shadow-sm)}
.logo{display:block;width:132px;height:auto;margin:0 0 24px}
.logo-dark,[data-theme="dark"] .logo-light{display:none}
[data-theme="dark"] .logo-dark{display:block}
h1{margin:0 0 8px;font:500 20px/28px "Space Grotesk",ui-sans-serif,system-ui,sans-serif;letter-spacing:-0.01em}
p{margin:0;color:var(--ink-muted)}
form{margin:24px 0 0}
button{display:inline-flex;align-items:center;justify-content:center;height:36px;padding:0 14px;border:1px solid transparent;border-radius:10px;background:var(--ink);color:var(--on-ink);font:500 14px/1 "IBM Plex Sans",ui-sans-serif,system-ui,sans-serif;cursor:pointer;transition:background-color 120ms cubic-bezier(.2,.7,.2,1),transform 120ms cubic-bezier(.2,.7,.2,1)}
button:hover{background:color-mix(in srgb,var(--ink) 86%,var(--paper))}
button:active{transform:translateY(1px)}
button:focus-visible{outline:none;box-shadow:var(--focus-ring)}
@media (prefers-reduced-motion:reduce){*{transition-duration:.01ms!important}}
`

// connectFontFiles are the faces the pages use, each the build file
// that holds it (web/src/ds/fonts, which Vite writes to /assets under a
// hashed name): the display face for the heading, the body face at its
// two weights for the words and the button.
var connectFontFiles = []struct{ family, weight, file string }{
	{"Space Grotesk", "500", "SpaceGrotesk-Medium"},
	{"IBM Plex Sans", "400", "IBMPlexSans-Regular"},
	{"IBM Plex Sans", "500", "IBMPlexSans-Medium"},
}

// connectFontFaces is the @font-face rules for the faces found in
// fsys, a web build at its root: one rule per face whose file is
// there, none for a face that is not, whose family then falls back
// down the stack. A binary built without the web app has none.
func connectFontFaces(fsys fs.FS) string {
	var b strings.Builder
	for _, f := range connectFontFiles {
		matches, _ := fs.Glob(fsys, "assets/"+f.file+"-*.woff2")
		if len(matches) == 0 {
			continue
		}
		b.WriteString(`@font-face{font-family:"` + f.family + `";src:url("/` + matches[0] + `") format("woff2");font-weight:` + f.weight + `;font-style:normal;font-display:swap}` + "\n")
	}
	return b.String()
}

// connectStyle is the pages' stylesheet with the build's fonts, read
// once from the embedded build.
var connectStyle = sync.OnceValue(func() string {
	return connectFontFaces(ui.Dist()) + connectPageCSS
})

// cspHash is s's CSP source expression: 'sha256-<base64>'.
func cspHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// write answers w with the page: its HTML, the headers that keep it
// out of caches and referrers, and its CSP.
func (p connectPage) write(w http.ResponseWriter) {
	style := connectStyle()
	scripts := cspHash(connectThemeScript)
	if p.Script != "" {
		scripts += " " + cspHash(p.Script)
	}
	csp := "default-src 'none'; img-src 'self'; font-src 'self'; style-src " + cspHash(style) + "; script-src " + scripts + "; base-uri 'none'; frame-ancestors 'none'"
	if p.FormAction != "" {
		csp += "; form-action " + p.FormAction
	}
	if p.ConnectSrc != "" {
		csp += "; connect-src " + p.ConnectSrc
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><meta name="referrer" content="no-referrer"><title>Kivali</title>`)
	b.WriteString(`<script>` + connectThemeScript + `</script>`)
	b.WriteString(`<style>` + style + `</style></head><body><main>`)
	b.WriteString(`<img class="logo logo-light" src="/logos/kivali-lockup.svg" alt="Kivali" width="132" height="32">`)
	b.WriteString(`<img class="logo logo-dark" src="/logos/kivali-lockup-dark.svg" alt="" width="132" height="32">`)
	b.WriteString(`<h1>` + html.EscapeString(p.Heading) + `</h1>`)
	b.WriteString(`<p>` + html.EscapeString(p.Words) + `</p>`)
	b.WriteString(p.Body)
	b.WriteString(`</main>`)
	if p.Script != "" {
		b.WriteString(`<script>` + p.Script + `</script>`)
	}
	b.WriteString(`</body></html>`)
	_, _ = w.Write([]byte(b.String()))
}
