package browserpod

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// probe is one page the snapshot rules are checked on: regular
// expressions the snapshot must match and strings it must not contain,
// and an optional check that acts on the page.
type probe struct {
	name    string
	html    string
	want    []string
	notWant []string
	check   func(t *testing.T, p *realPod, snap string)
}

var snapshotProbes = []probe{
	{
		name: "a clickable wrapper lists its content, a control its controls",
		html: `<div onclick="void 0"><h3>Card title</h3><p>Card body text</p><button>Add to cart</button></div>
<div tabindex="0">Lone menu</div>
<a href="/l"><span>Open</span><button>Save</button></a>
<button>Plain button</button>`,
		want: []string{
			"\n\\[e1\\] clickable\n  heading 3 \"Card title\"\n  text \"Card body text\"\n  \\[e2\\] button \"Add to cart\"\n",
			"\n\\[e3\\] clickable \"Lone menu\"\n",
			"\n\\[e4\\] link → /l\n  text \"Open\"\n  \\[e5\\] button \"Save\"\n",
			"\n\\[e6\\] button \"Plain button\"",
		},
		check: func(t *testing.T, p *realPod, snap string) {
			res := p.step("click wrapper", "", CommandClick, ClickArgs{Ref: "e1"})
			if !strings.Contains(res.Text, `Clicked clickable "Card title Card body text Add to cart"`) {
				t.Errorf("the wrapper's reported name: %q", res.Text)
			}
		},
	},
	{
		name: "text on an image or gradient background shows",
		html: `<div style="background-image: linear-gradient(#333, #000); color: #fff"><h1>GRADIENT-HEADING</h1></div>
<button style="background: linear-gradient(#06c, #039); color: #fff">GRADIENT-BUTTON</button>
<div style="background: #fff; color: #fff">HIDDEN-SAME-COLOUR</div>`,
		want:    []string{`heading 1 "GRADIENT-HEADING"`, `button "GRADIENT-BUTTON"`},
		notWant: []string{"HIDDEN-SAME-COLOUR"},
	},
	{
		name: "hidden form controls with a label are listed as hidden",
		html: `<style>.sr{position:absolute;width:1px;height:1px;overflow:hidden;clip:rect(0,0,0,0)}</style>
<label for="cv">Upload CV</label><input type="file" id="cv" accept=".pdf" style="display:none">
<input type="checkbox" id="agree" style="opacity:0;position:absolute"><label for="agree">I agree</label>
<input type="radio" id="ra" name="r" class="sr"><label for="ra">Option A</label>
<input type="checkbox" id="tiny" style="position:absolute;width:0;height:0"><label for="tiny">Tiny box</label>
<input type="checkbox" id="gone" style="display:none"><label for="gone">Display none box</label>
<input style="opacity:0" value="NOT-LISTED-INPUT">
<input style="display:none" id="nt"><label for="nt">No typing here</label>
<input type="file" style="opacity:0;position:absolute;width:0.1px;height:0.1px;z-index:-1" id="drop"><label for="drop">Drop files</label>`,
		want: []string{
			`\[e1\] file "Upload CV" empty accept=".pdf" hidden`,
			`\[e2\] checkbox "I agree" hidden`,
			`\[e3\] radio "Option A" hidden`,
			`\[e4\] checkbox "Tiny box" hidden`,
			`\[e5\] checkbox "Display none box" hidden`,
			`\[e6\] file "Drop files" empty hidden`,
		},
		notWant: []string{"NOT-LISTED-INPUT", "No typing here\" hidden", "textbox"},
		check: func(t *testing.T, p *realPod, snap string) {
			p.core.sendBytes(Message{Type: TypeFile, Group: "g", FileName: "cv.pdf"}, []byte("%PDF"))
			p.step("upload to a hidden input", "", CommandUpload, UploadArgs{Ref: "e1", File: "cv.pdf"})
			if v := p.eval(`document.getElementById('cv').files[0].name`); v != `"cv.pdf"` {
				t.Errorf("uploaded file: %s", v)
			}
		},
	},
	{
		name: "visibility and overflow clipping are judged per element",
		html: `<div style="visibility:hidden">HIDDEN-PARENT <span style="visibility:visible">VISIBLE-CHILD</span></div>
<div style="overflow:hidden;width:300px"><div style="display:flex;width:900px"><div style="width:300px;flex:none">SLIDE-ONE</div><div style="width:300px;flex:none">SLIDE-TWO</div></div></div>
<div style="overflow:hidden;height:40px"><div style="position:absolute;top:300px">ESCAPED-MENU</div></div>
<div style="overflow:hidden;height:40px;position:relative"><div style="position:absolute;top:300px">CLIPPED-MENU</div></div>
<div style="overflow-x:hidden;width:300px;height:40px"><div style="height:200px"><p style="margin-top:100px">BELOW-SCROLLABLE</p></div></div>`,
		want:    []string{"VISIBLE-CHILD", "SLIDE-ONE", "ESCAPED-MENU", "BELOW-SCROLLABLE"},
		notWant: []string{"HIDDEN-PARENT", "SLIDE-TWO", "CLIPPED-MENU"},
	},
}

// TestRealChromiumPageRules checks the snapshot rules and the commands
// against probe pages in a real browser. It runs under the same
// condition as TestRealChromium.
func TestRealChromiumPageRules(t *testing.T) {
	mux := http.NewServeMux()
	for i, pr := range snapshotProbes {
		mux.HandleFunc(fmt.Sprintf("/probe/%d", i), htmlPage("<!doctype html><html><head><title>Probe</title></head><body>"+pr.html+"</body></html>"))
	}
	var long strings.Builder
	long.WriteString(`<!doctype html><html><head><title>Long</title><style>p{height:20px;margin:0;line-height:20px}</style></head><body>`)
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&long, "<p>PARA-%04d</p>", i)
	}
	long.WriteString("</body></html>")
	mux.HandleFunc("/long", htmlPage(long.String()))
	p := realChromium(t, mux)

	for i, pr := range snapshotProbes {
		t.Run(pr.name, func(t *testing.T) {
			pt := &realPod{t: t, core: p.core, url: p.url, done: p.done}
			pt.step("navigate", "", CommandNavigate, NavigateArgs{URL: fmt.Sprintf("%s/probe/%d", p.url, i)})
			snap := pt.step("snapshot", "", CommandSnapshot, nil).Text
			t.Logf("snapshot:\n%s", snap)
			for _, w := range pr.want {
				if !regexp.MustCompile(w).MatchString(snap) {
					t.Errorf("snapshot lacks %s", w)
				}
			}
			for _, nw := range pr.notWant {
				if strings.Contains(snap, nw) {
					t.Errorf("snapshot shows %q", nw)
				}
			}
			if pr.check != nil {
				pr.check(t, pt, snap)
			}
		})
	}

	t.Run("a long page is cut at the scroll position", func(t *testing.T) {
		pt := &realPod{t: t, core: p.core, url: p.url, done: p.done}
		pt.step("navigate", "", CommandNavigate, NavigateArgs{URL: p.url + "/long"})
		snap := pt.step("snapshot at the top", "", CommandSnapshot, nil).Text
		if !strings.Contains(snap, "\ntext \"PARA-0000\"\n") || !strings.Contains(snap, " lines below not shown") || strings.Contains(snap, "above the current scroll position") {
			t.Errorf("at the top:\n%s", firstLines(snap, 4))
		}
		pt.step("scroll", "", CommandScroll, ScrollArgs{Direction: "down", Amount: 10})
		snap = pt.step("snapshot scrolled", "", CommandSnapshot, nil).Text
		lines := strings.Split(snap, "\n")
		m := regexp.MustCompile(`scrolled (\d+) of`).FindStringSubmatch(lines[1])
		if m == nil {
			t.Fatalf("viewport line: %q", lines[1])
		}
		var scrollY int
		_, _ = fmt.Sscan(m[1], &scrollY)
		// Paragraph i starts at 8 + 20 i (the body's margin, then 20 px
		// each); the first shown is the first at or below scrollY.
		first := (scrollY - 8 + 19) / 20
		wantAbove := fmt.Sprintf("… %d lines above the current scroll position not shown", first)
		wantFirst := fmt.Sprintf("text \"PARA-%04d\"", first)
		if !strings.HasPrefix(lines[2], wantAbove) || lines[3] != wantFirst {
			t.Errorf("scrolled to %d: lines 2 and 3 are %q, %q; want %q, %q", scrollY, lines[2], lines[3], wantAbove, wantFirst)
		}
		if !strings.Contains(snap, " lines below not shown") || strings.Contains(snap, "PARA-0000") {
			t.Errorf("scrolled:\n%s", firstLines(snap, 4))
		}
	})
}
