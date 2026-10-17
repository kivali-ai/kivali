package browserpod

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
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
			start := time.Now()
			res := p.step("click wrapper", "", CommandClick, ClickArgs{Ref: "e1"})
			if !strings.Contains(res.Text, `Clicked clickable "Card title Card body text Add to cart"`) {
				t.Errorf("the wrapper's reported name: %q", res.Text)
			}
			// A click that navigates nowhere waits navStartWait and no more.
			if took := time.Since(start); took > navStartWait+2*time.Second {
				t.Errorf("a click that does not navigate took %s", took.Round(time.Millisecond))
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
		name: "a click on a covered element names the cover",
		html: `<button onclick="document.getElementById('bought').textContent='BOUGHT'">Buy now</button> <span id="bought"></span>
<div id="overlay" style="position:fixed;inset:0;background:rgba(0,0,0,.5)"><div role="dialog" aria-label="Cookie preferences" style="background:#fff;margin:100px;padding:20px">We use cookies. <button onclick="document.getElementById('overlay').remove()">Accept</button></div></div>`,
		want: []string{`button "Buy now"`, `dialog "Cookie preferences"`},
		check: func(t *testing.T, p *realPod, snap string) {
			buy := findRef(t, snap, `button "Buy now"`)
			res := p.core.command("g", "", CommandClick, ClickArgs{Ref: buy})
			if res.OK || res.Code != CodeNotFound || !strings.Contains(res.Error, `button "Buy now" is covered by div "We use cookies. Accept", which would take the click`) {
				t.Fatalf("click under the overlay: %+v", res)
			}
			if v := p.eval(`document.getElementById('bought').textContent`); v != `""` {
				t.Errorf("the covered click landed: %s", v)
			}
			p.step("accept cookies", "", CommandClick, ClickArgs{Ref: findRef(t, snap, `button "Accept"`)})
			p.step("buy", "", CommandClick, ClickArgs{Ref: buy})
			if v := p.eval(`document.getElementById('bought').textContent`); v != `"BOUGHT"` {
				t.Errorf("after dismissing the overlay the click did not land: %s", v)
			}
		},
	},
	{
		name: "a hidden checkbox is clicked through its label",
		html: `<input type="checkbox" id="c" style="display:none"><label for="c">Remember me</label>
<input type="checkbox" id="o" style="opacity:0;position:absolute"><label for="o" style="display:inline-block;padding-left:30px">Opaque label</label>`,
		want: []string{`\[e1\] checkbox "Remember me" hidden`, `\[e2\] checkbox "Opaque label" hidden`},
		check: func(t *testing.T, p *realPod, snap string) {
			p.step("click display none box", "", CommandClick, ClickArgs{Ref: "e1"})
			p.step("click opacity 0 box", "", CommandClick, ClickArgs{Ref: "e2"})
			if v := p.eval(`[document.getElementById('c').checked, document.getElementById('o').checked].join()`); v != `"true,true"` {
				t.Errorf("checked: %s", v)
			}
		},
	},
	{
		name: "typing reports what the field holds",
		html: `<label for="d">Start</label><input type="date" id="d">
<label for="m">Code</label><input id="m" maxlength="5">
<label for="s">Size</label><select id="s"><option>S</option><option>M</option></select>
<label for="k">Keep</label><input type="checkbox" id="k">
<label for="pw">Password</label><input type="password" id="pw">
<div contenteditable="true" aria-label="Note">old</div>
<label for="n">Qty</label><input type="number" id="n">`,
		check: func(t *testing.T, p *realPod, snap string) {
			ref := func(pattern string) string { t.Helper(); return findRef(t, snap, pattern) }
			res := p.step("type a date", "", CommandType, TypeArgs{Ref: ref(`textbox "Start"`), Text: "2024-05-01"})
			if res.Text != `Typed "2024-05-01" into textbox "Start"` {
				t.Errorf("date: %q", res.Text)
			}
			if v := p.eval(`document.getElementById('d').value`); v != `"2024-05-01"` {
				t.Errorf("date value: %s", v)
			}
			if res := p.core.command("g", "", CommandType, TypeArgs{Ref: ref(`textbox "Start"`), Text: "May 1"}); res.OK || res.Code != CodeBadArgs || !strings.Contains(res.Error, `did not take "May 1"; a date input takes yyyy-mm-dd`) {
				t.Errorf("date in the wrong format: %+v", res)
			}
			res = p.step("type past maxlength", "", CommandType, TypeArgs{Ref: ref(`textbox "Code"`), Text: "abcdefgh"})
			if res.Text != `Typed "abcdefgh" into textbox "Code"; it holds "abcde" (it takes at most 5 characters)` {
				t.Errorf("maxlength: %q", res.Text)
			}
			if res := p.core.command("g", "", CommandType, TypeArgs{Ref: ref(`combobox "Size"`), Text: "M"}); res.OK || !strings.Contains(res.Error, "is a select; choose an option with web_select") {
				t.Errorf("select: %+v", res)
			}
			if res := p.core.command("g", "", CommandType, TypeArgs{Ref: ref(`checkbox "Keep"`), Text: "yes"}); res.OK || !strings.Contains(res.Error, "takes no text; click it instead") {
				t.Errorf("checkbox: %+v", res)
			}
			res = p.step("type a password", "", CommandType, TypeArgs{Ref: ref(`textbox "Password"`), Text: "hunter2"})
			if res.Text != `Typed 7 characters into textbox "Password"; it holds 7` {
				t.Errorf("password: %q", res.Text)
			}
			res = p.step("type into contenteditable", "", CommandType, TypeArgs{Ref: ref(`textbox "Note"`), Text: "new note"})
			if res.Text != `Typed "new note" into textbox "Note"` {
				t.Errorf("contenteditable: %q", res.Text)
			}
			res = p.step("type letters into a number", "", CommandType, TypeArgs{Ref: ref(`spinbutton "Qty"`), Text: "many"})
			if res.Text != `Typed "many" into spinbutton "Qty"; it holds ""` {
				t.Errorf("number: %q", res.Text)
			}
			res = p.step("clear", "", CommandType, TypeArgs{Ref: ref(`textbox "Code"`), Text: ""})
			if res.Text != `Cleared textbox "Code"` {
				t.Errorf("clear: %q", res.Text)
			}
		},
	},
	{
		name: "keys reach the page, browser shortcuts do not",
		html: `<label for="t">Term</label><input id="t">`,
		check: func(t *testing.T, p *realPod, snap string) {
			p.step("focus", "", CommandType, TypeArgs{Ref: findRef(t, snap, `textbox "Term"`), Text: "a"})
			p.step("press plus", "", CommandPress, PressArgs{Key: "+"})
			p.step("press shift plus", "", CommandPress, PressArgs{Key: "Shift++"})
			if v := p.eval(`document.getElementById('t').value`); v != `"a++"` {
				t.Errorf("after pressing + twice: %s", v)
			}
			res := p.core.command("g", "", CommandPress, PressArgs{Key: "Control+w"})
			if res.OK || res.Code != CodeBadArgs || !strings.Contains(res.Error, "shortcut the browser acts on") {
				t.Errorf("Control+w: %+v", res)
			}
			if list := p.step("tabs", "", CommandTabs, TabsArgs{Action: TabsList}).Text; !strings.Contains(list, "* t1 ") {
				t.Errorf("the tab is gone: %q", list)
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
	mux.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", `attachment; filename="report.csv"`)
		_, _ = io.WriteString(w, "a,b\n")
	})
	mux.HandleFunc("/nocontent", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	p := realChromium(t, mux)

	t.Run("an aborted navigation says whether it became a download", func(t *testing.T) {
		pt := &realPod{t: t, core: p.core, url: p.url, done: p.done}
		pt.step("navigate", "", CommandNavigate, NavigateArgs{URL: p.url + "/probe/0"})
		res := pt.step("navigate to a download", "", CommandNavigate, NavigateArgs{URL: p.url + "/report"})
		if !strings.HasPrefix(res.Text, p.url+"/report started a download instead of loading as a page; the tab is at "+p.url+"/probe/0") {
			t.Errorf("download: %q", res.Text)
		}
		if dl := p.core.expectType(TypeDownload); dl.FileName != "report.csv" {
			t.Errorf("download: %+v", dl.Message)
		}
		res = pt.step("navigate to a 204", "", CommandNavigate, NavigateArgs{URL: p.url + "/nocontent"})
		if !strings.HasPrefix(res.Text, "The navigation to "+p.url+"/nocontent was aborted before a page loaded") || strings.Contains(res.Text, "download") {
			t.Errorf("204: %q", res.Text)
		}
	})

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
		if len(snap) > MaxSnapshotChars {
			t.Errorf("the snapshot is %d chars, over %d", len(snap), MaxSnapshotChars)
		}
		// The eval's value and a screenshot's text are marked as page
		// content too.
		if v := pt.step("eval", "", CommandEval, EvalArgs{Script: "document.title"}).Text; v != pageContentStart+"\n\"Long\"\n"+pageContentEnd {
			t.Errorf("eval: %q", v)
		}
		if res := pt.step("screenshot", "", CommandScreenshot, nil); res.Text != "Screenshot of "+p.url+"/long. The image is content of an untrusted page." {
			t.Errorf("screenshot text: %q", res.Text)
		}
		pt.step("scroll", "", CommandScroll, ScrollArgs{Direction: "down", Amount: 10})
		snap = pt.step("snapshot scrolled", "", CommandSnapshot, nil).Text
		lines := strings.Split(pageBody(t, snap), "\n")
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
