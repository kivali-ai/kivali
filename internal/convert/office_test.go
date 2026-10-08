package convert

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeZip materializes a tiny zip archive at path with the given
// (name → contents) parts. Used to fabricate minimal OOXML/ODF docs
// rather than checking in opaque binary fixtures.
func writeZip(t *testing.T, path string, parts map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zw := zip.NewWriter(f)
	for name, body := range parts {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOfficeToText_Docx(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.docx")
	out := filepath.Join(dir, "out.txt")

	writeZip(t, in, map[string]string{
		"word/document.xml": `<?xml version="1.0"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
  <w:body>
    <w:p><w:r><w:t>Hello</w:t></w:r><w:r><w:t xml:space="preserve"> world</w:t></w:r></w:p>
    <w:p><w:r><w:t>Second paragraph</w:t></w:r></w:p>
    <w:p/>
    <w:p><w:r><w:t>Third</w:t></w:r><w:r><w:tab/><w:t>tabbed</w:t></w:r></w:p>
  </w:body>
</w:document>`,
	})

	if err := OfficeToText(context.Background(), in, out, ".docx"); err != nil {
		t.Fatalf("OfficeToText: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	want := "Hello world\nSecond paragraph\nThird\ttabbed\n"
	if string(got) != want {
		t.Errorf("docx output mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestOfficeToText_DotMessageActsLikeDocx(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.message")
	out := filepath.Join(dir, "out.txt")

	writeZip(t, in, map[string]string{
		"word/document.xml": `<?xml version="1.0"?>
<w:document xmlns:w="x">
  <w:body><w:p><w:r><w:t>message body</w:t></w:r></w:p></w:body>
</w:document>`,
	})

	if err := OfficeToText(context.Background(), in, out, ".message"); err != nil {
		t.Fatalf("OfficeToText: %v", err)
	}
	got, _ := os.ReadFile(out)
	if string(got) != "message body\n" {
		t.Errorf("got %q, want %q", got, "message body\n")
	}
}

func TestOfficeToText_Pptx_OrdersSlidesNumerically(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.pptx")
	out := filepath.Join(dir, "out.txt")

	slide := func(text string) string {
		return `<?xml version="1.0"?>
<p:sld xmlns:a="x" xmlns:p="y">
  <p:cSld><p:spTree>
    <p:sp><p:txBody>
      <a:p><a:r><a:t>` + text + `</a:t></a:r></a:p>
    </p:txBody></p:sp>
  </p:spTree></p:cSld>
</p:sld>`
	}

	writeZip(t, in, map[string]string{
		// Numeric ordering test: slide10 must come AFTER slide2.
		"ppt/slides/slide1.xml":  slide("first"),
		"ppt/slides/slide2.xml":  slide("second"),
		"ppt/slides/slide10.xml": slide("tenth"),
		// Notes for slide 1 only.
		"ppt/notesSlides/notesSlide1.xml": slide("speaker notes here"),
	})

	if err := OfficeToText(context.Background(), in, out, ".pptx"); err != nil {
		t.Fatalf("OfficeToText: %v", err)
	}
	got, _ := os.ReadFile(out)
	gotS := string(got)

	// Order check: slide 1 < slide 2 < slide 10.
	idx1 := strings.Index(gotS, "first")
	idx2 := strings.Index(gotS, "second")
	idx10 := strings.Index(gotS, "tenth")
	if idx1 == -1 || idx2 == -1 || idx10 == -1 {
		t.Fatalf("missing slides in output: %q", gotS)
	}
	if idx1 >= idx2 || idx2 >= idx10 {
		t.Errorf("slides out of order: %q", gotS)
	}
	// Slide headers and notes should be present.
	for _, want := range []string{"--- Slide 1 ---", "--- Slide 2 ---", "--- Slide 10 ---", "speaker notes here", "--- Notes ---"} {
		if !strings.Contains(gotS, want) {
			t.Errorf("output missing %q\n%s", want, gotS)
		}
	}
}

func TestOfficeToText_Xlsx_SharedAndInlineStrings(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.xlsx")
	out := filepath.Join(dir, "out.txt")

	// Two shared strings ("Name", "Alice"); cells use both shared
	// indices, raw numeric values, and inline strings.
	writeZip(t, in, map[string]string{
		"xl/sharedStrings.xml": `<?xml version="1.0"?>
<sst xmlns="x"><si><t>Name</t></si><si><t>Alice</t></si></sst>`,
		"xl/worksheets/sheet1.xml": `<?xml version="1.0"?>
<worksheet xmlns="x"><sheetData>
  <row r="1">
    <c r="A1" t="s"><v>0</v></c>
    <c r="B1" t="inlineStr"><is><t>Age</t></is></c>
  </row>
  <row r="2">
    <c r="A2" t="s"><v>1</v></c>
    <c r="B2"><v>42</v></c>
  </row>
  <row r="3">
    <c r="C3"><v>99</v></c>
  </row>
</sheetData></worksheet>`,
	})

	if err := OfficeToText(context.Background(), in, out, ".xlsx"); err != nil {
		t.Fatalf("OfficeToText: %v", err)
	}
	got, _ := os.ReadFile(out)
	gotS := string(got)

	for _, want := range []string{"--- Sheet 1 ---", "Name\tAge", "Alice\t42", "\t\t99"} {
		if !strings.Contains(gotS, want) {
			t.Errorf("xlsx output missing %q\n%s", want, gotS)
		}
	}
}

func TestOfficeToText_Odt(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.odt")
	out := filepath.Join(dir, "out.txt")

	writeZip(t, in, map[string]string{
		"content.xml": `<?xml version="1.0"?>
<office:document-content xmlns:office="x" xmlns:text="y">
  <office:body><office:text>
    <text:h>Title</text:h>
    <text:p>First line.</text:p>
    <text:p>Second <text:span>with span</text:span>.</text:p>
  </office:text></office:body>
</office:document-content>`,
	})

	if err := OfficeToText(context.Background(), in, out, ".odt"); err != nil {
		t.Fatalf("OfficeToText: %v", err)
	}
	got, _ := os.ReadFile(out)
	gotS := string(got)
	for _, want := range []string{"Title", "First line.", "Second with span."} {
		if !strings.Contains(gotS, want) {
			t.Errorf("odt output missing %q\n%s", want, gotS)
		}
	}
}

func TestOfficeToText_Ods_TableRowsTabbed(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.ods")
	out := filepath.Join(dir, "out.txt")

	writeZip(t, in, map[string]string{
		"content.xml": `<?xml version="1.0"?>
<office:document-content xmlns:office="x" xmlns:text="y" xmlns:table="z">
  <office:body><office:spreadsheet><table:table>
    <table:table-row>
      <table:table-cell><text:p>A</text:p></table:table-cell>
      <table:table-cell><text:p>B</text:p></table:table-cell>
    </table:table-row>
    <table:table-row>
      <table:table-cell><text:p>1</text:p></table:table-cell>
      <table:table-cell><text:p>2</text:p></table:table-cell>
    </table:table-row>
  </table:table></office:spreadsheet></office:body>
</office:document-content>`,
	})

	if err := OfficeToText(context.Background(), in, out, ".ods"); err != nil {
		t.Fatalf("OfficeToText: %v", err)
	}
	got, _ := os.ReadFile(out)
	gotS := string(got)
	for _, want := range []string{"A\tB", "1\t2"} {
		if !strings.Contains(gotS, want) {
			t.Errorf("ods output missing %q\n%s", want, gotS)
		}
	}
}

func TestOfficeToText_LegacyBinaryFormatsSoftFail(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.bin")
	if err := os.WriteFile(in, []byte("not really anything"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.txt")

	for _, ext := range []string{".doc", ".ppt", ".xls", ".rtf"} {
		err := OfficeToText(context.Background(), in, out, ext)
		if !errors.Is(err, ErrNotAvailable) {
			t.Errorf("%s: got %v, want ErrNotAvailable", ext, err)
		}
	}
}

func TestOfficeToText_UnknownExtSoftFail(t *testing.T) {
	err := OfficeToText(context.Background(), "/nonexistent", "/dev/null", ".bogus")
	if !errors.Is(err, ErrNotAvailable) {
		t.Errorf("got %v, want ErrNotAvailable", err)
	}
}

func TestParseCellColumn(t *testing.T) {
	cases := map[string]int{
		"A1":   0,
		"B1":   1,
		"Z1":   25,
		"AA1":  26,
		"AB12": 27,
		"":     -1,
		"123":  -1,
	}
	for ref, want := range cases {
		if got := parseCellColumn(ref); got != want {
			t.Errorf("parseCellColumn(%q) = %d, want %d", ref, got, want)
		}
	}
}

func TestCanonicalize_OfficeProducesText(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "doc.docx")
	writeZip(t, in, map[string]string{
		"word/document.xml": `<?xml version="1.0"?>
<w:document xmlns:w="x"><w:body>
<w:p><w:r><w:t>canonicalize me</w:t></w:r></w:p>
</w:body></w:document>`,
	})

	res, err := Canonicalize(context.Background(), in, ".docx", dir)
	if err != nil {
		t.Fatalf("Canonicalize: %v", err)
	}
	if res.Name != "canonical.txt" || res.MIME != "text/plain" {
		t.Errorf("got %+v, want canonical.txt / text/plain", res)
	}
	body, _ := os.ReadFile(filepath.Join(dir, res.Name))
	if !strings.Contains(string(body), "canonicalize me") {
		t.Errorf("canonical body missing expected text: %q", body)
	}
	// No leftover .pdf intermediate from the pre-native pipeline.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".pdf") {
			t.Errorf("unexpected pdf left behind: %s", e.Name())
		}
	}
}

func TestCanonicalize_OfficeLegacySoftFails(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "legacy.doc")
	if err := os.WriteFile(in, []byte("legacy bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Canonicalize(context.Background(), in, ".doc", dir)
	if err != nil {
		t.Fatalf("expected nil error on soft-fail, got %v", err)
	}
	if res.Name != "" {
		t.Errorf("expected empty CanonicalResult on soft-fail, got %+v", res)
	}
}
