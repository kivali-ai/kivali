package convert

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
)

// OfficeToText extracts plain text from an office document at inputPath
// and writes UTF-8 text to outPath. It handles Office Open XML (docx,
// pptx, xlsx, the Kivali .message variant) and OpenDocument formats
// (odt, odp, ods) natively, with no external binaries.
//
// Returns ErrNotAvailable for legacy binary formats (.doc, .ppt, .xls,
// .rtf): callers treat that as soft-fail — same contract as missing
// libreoffice in the prior pipeline.
func OfficeToText(ctx context.Context, inputPath, outPath, ext string) error {
	switch strings.ToLower(ext) {
	case ".docx", ".message":
		return ooxmlExtract(ctx, inputPath, outPath, extractDocx)
	case ".pptx":
		return ooxmlExtract(ctx, inputPath, outPath, extractPptx)
	case ".xlsx":
		return ooxmlExtract(ctx, inputPath, outPath, extractXlsx)
	case ".odt", ".odp", ".ods":
		return odfExtract(ctx, inputPath, outPath)
	case ".doc", ".ppt", ".xls", ".rtf":
		return fmt.Errorf("%w: legacy binary office format %s not supported", ErrNotAvailable, ext)
	}
	return fmt.Errorf("%w: unrecognized office extension %s", ErrNotAvailable, ext)
}

// ooxmlExtract is the shared entry point for OOXML formats. The
// per-format extract function takes a *zip.Reader and an output writer
// and writes the text representation. We open the zip and the output
// file here so each extractor stays focused on parsing.
func ooxmlExtract(ctx context.Context, inputPath, outPath string,
	extract func(context.Context, *zip.Reader, io.Writer) error) error {
	zr, err := zip.OpenReader(inputPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", inputPath, err)
	}
	defer func() { _ = zr.Close() }()
	out, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", outPath, err)
	}
	defer func() { _ = out.Close() }()
	if err := extract(ctx, &zr.Reader, out); err != nil {
		return err
	}
	return out.Close()
}

// extractDocx reads word/document.xml and writes paragraph-separated
// text. docx body text lives in <w:t> runs inside <w:p> paragraphs;
// <w:tab/> and <w:br/> map to "\t" and newline respectively.
func extractDocx(ctx context.Context, zr *zip.Reader, out io.Writer) error {
	f := zipFile(zr, "word/document.xml")
	if f == nil {
		return fmt.Errorf("docx: missing word/document.xml")
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()

	dec := xml.NewDecoder(rc)
	bw := newBufferedWriter(out)
	defer func() { _ = bw.Flush() }()

	inParagraph := false
	paragraphHasText := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("docx: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p":
				inParagraph = true
				paragraphHasText = false
			case "t":
				txt, err := readCharData(dec)
				if err != nil {
					return fmt.Errorf("docx: %w", err)
				}
				if txt != "" {
					_, _ = bw.WriteString(txt)
					paragraphHasText = true
				}
			case "tab":
				_ = bw.WriteByte('\t')
			case "br":
				_ = bw.WriteByte('\n')
			}
		case xml.EndElement:
			if t.Name.Local == "p" && inParagraph {
				if paragraphHasText {
					_ = bw.WriteByte('\n')
				}
				inParagraph = false
				paragraphHasText = false
			}
		}
	}
	return bw.Flush()
}

// extractPptx walks ppt/slides/slide*.xml in numeric order, then the
// matching ppt/notesSlides/notesSlide*.xml if present. Slides are
// separated by a "--- Slide N ---" header so the LLM sees structure
// instead of one mush.
func extractPptx(ctx context.Context, zr *zip.Reader, out io.Writer) error {
	bw := newBufferedWriter(out)
	defer func() { _ = bw.Flush() }()

	slides := numericallySortedFiles(zr, "ppt/slides/slide", ".xml")
	notes := indexedFiles(zr, "ppt/notesSlides/notesSlide", ".xml")

	for _, s := range slides {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(bw, "--- Slide %d ---\n", s.idx)
		if err := writePptxXMLText(s.f, bw); err != nil {
			return fmt.Errorf("pptx slide %d: %w", s.idx, err)
		}
		if note := notes[s.idx]; note != nil {
			_, _ = bw.WriteString("--- Notes ---\n")
			if err := writePptxXMLText(note, bw); err != nil {
				return fmt.Errorf("pptx notes %d: %w", s.idx, err)
			}
		}
		_ = bw.WriteByte('\n')
	}
	return bw.Flush()
}

// writePptxXMLText copies <a:t> text out of a slide or notesSlide XML
// part. <a:p> paragraphs map to newlines.
func writePptxXMLText(f *zip.File, bw *bufferedWriter) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	dec := xml.NewDecoder(rc)
	paragraphHasText := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				txt, err := readCharData(dec)
				if err != nil {
					return err
				}
				if txt != "" {
					_, _ = bw.WriteString(txt)
					paragraphHasText = true
				}
			case "br":
				_ = bw.WriteByte('\n')
			}
		case xml.EndElement:
			if t.Name.Local == "p" && paragraphHasText {
				_ = bw.WriteByte('\n')
				paragraphHasText = false
			}
		}
	}
	if paragraphHasText {
		_ = bw.WriteByte('\n')
	}
	return nil
}

// extractXlsx writes one TSV-shaped section per worksheet. Cells
// reference xl/sharedStrings.xml by index when t="s"; inline values
// otherwise. Sheet ordering follows the worksheet file numbering,
// which mirrors the workbook's sheet order in practice — for the LLM
// "feed text" use case that's sufficient.
func extractXlsx(ctx context.Context, zr *zip.Reader, out io.Writer) error {
	bw := newBufferedWriter(out)
	defer func() { _ = bw.Flush() }()

	sst, err := readSharedStrings(zr)
	if err != nil {
		return fmt.Errorf("xlsx: %w", err)
	}
	sheets := numericallySortedFiles(zr, "xl/worksheets/sheet", ".xml")
	for _, s := range sheets {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(bw, "--- Sheet %d ---\n", s.idx)
		if err := writeXlsxSheet(s.f, sst, bw); err != nil {
			return fmt.Errorf("xlsx sheet %d: %w", s.idx, err)
		}
		_ = bw.WriteByte('\n')
	}
	return bw.Flush()
}

// readSharedStrings parses xl/sharedStrings.xml into an index → string
// table. The file is optional — if absent, all cells must use inline
// values.
func readSharedStrings(zr *zip.Reader) ([]string, error) {
	f := zipFile(zr, "xl/sharedStrings.xml")
	if f == nil {
		return nil, nil
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	dec := xml.NewDecoder(rc)
	var out []string
	var current strings.Builder
	inSI := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "si":
				inSI = true
				current.Reset()
			case "t":
				if !inSI {
					continue
				}
				txt, err := readCharData(dec)
				if err != nil {
					return nil, err
				}
				current.WriteString(txt)
			}
		case xml.EndElement:
			if t.Name.Local == "si" {
				out = append(out, current.String())
				inSI = false
			}
		}
	}
	return out, nil
}

// writeXlsxSheet streams cells from a single worksheet, emitting one
// row per <row>, with cells separated by tabs. Empty cells between
// populated ones are filled in based on the cell reference (e.g. "C1"
// after "A1" means one empty cell of padding) so the layout is
// preserved enough to be readable.
func writeXlsxSheet(f *zip.File, sst []string, bw *bufferedWriter) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	dec := xml.NewDecoder(rc)

	var (
		rowCells    []string
		expectedCol int
		cellType    string
		cellRefCol  int
		inSheetData bool
	)
	flushRow := func() {
		if len(rowCells) == 0 {
			_ = bw.WriteByte('\n')
			return
		}
		_, _ = bw.WriteString(strings.Join(rowCells, "\t"))
		_ = bw.WriteByte('\n')
		rowCells = rowCells[:0]
		expectedCol = 0
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "sheetData":
				inSheetData = true
			case "row":
				if !inSheetData {
					continue
				}
				rowCells = rowCells[:0]
				expectedCol = 0
			case "c":
				if !inSheetData {
					continue
				}
				cellType = ""
				cellRefCol = -1
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "t":
						cellType = a.Value
					case "r":
						cellRefCol = parseCellColumn(a.Value)
					}
				}
				if cellRefCol >= 0 {
					for expectedCol < cellRefCol {
						rowCells = append(rowCells, "")
						expectedCol++
					}
				}
				rowCells = append(rowCells, "")
				expectedCol++
			case "v":
				if !inSheetData {
					continue
				}
				txt, err := readCharData(dec)
				if err != nil {
					return err
				}
				val := xlsxCellValue(cellType, txt, sst)
				if len(rowCells) > 0 {
					rowCells[len(rowCells)-1] = val
				}
			case "is":
				// Inline string: <c t="inlineStr"><is><t>...</t></is></c>
				if !inSheetData {
					continue
				}
				txt, err := readInlineString(dec)
				if err != nil {
					return err
				}
				if len(rowCells) > 0 {
					rowCells[len(rowCells)-1] = txt
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "row":
				if inSheetData {
					flushRow()
				}
			case "sheetData":
				inSheetData = false
			}
		}
	}
	return nil
}

// xlsxCellValue resolves a cell value according to its declared type.
// "s" = shared-string index; everything else (numbers, booleans,
// dates-as-numbers) is emitted as the raw string — the LLM doesn't
// need formatted dates and this avoids dragging in the xlsx format
// system.
func xlsxCellValue(cellType, raw string, sst []string) string {
	if cellType == "s" {
		if i, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && i >= 0 && i < len(sst) {
			return sst[i]
		}
		return ""
	}
	return raw
}

// readInlineString collects all <t> chardata until the matching </is>.
func readInlineString(dec *xml.Decoder) (string, error) {
	var b strings.Builder
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if t.Name.Local == "t" {
				txt, err := readCharData(dec)
				if err != nil {
					return "", err
				}
				b.WriteString(txt)
				depth--
			}
		case xml.EndElement:
			depth--
		}
	}
	return b.String(), nil
}

// parseCellColumn converts a cell reference like "AB12" to a 0-indexed
// column number (AB → 27). Returns -1 if the reference is unusable so
// the caller falls back to sequential placement.
func parseCellColumn(ref string) int {
	col := 0
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		if c >= 'A' && c <= 'Z' {
			col = col*26 + int(c-'A'+1)
		} else if c >= 'a' && c <= 'z' {
			col = col*26 + int(c-'a'+1)
		} else {
			break
		}
	}
	if col == 0 {
		return -1
	}
	return col - 1
}

// odfExtract reads content.xml from an ODF zip and writes its text.
// ODF text lives in <text:p>, <text:span>, <text:h>, etc.; we collect
// any chardata under those nodes and break paragraphs at <text:p> /
// <text:h> ends. Spreadsheet cells (<table:table-cell>) get tab
// separation and rows newline.
func odfExtract(ctx context.Context, inputPath, outPath string) error {
	zr, err := zip.OpenReader(inputPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", inputPath, err)
	}
	defer func() { _ = zr.Close() }()
	f := zipFile(&zr.Reader, "content.xml")
	if f == nil {
		return fmt.Errorf("odf: missing content.xml")
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	bw := newBufferedWriter(out)
	defer func() { _ = bw.Flush() }()

	dec := xml.NewDecoder(rc)
	// Cell-aware bookkeeping: when we're inside a table cell we hold
	// its text in cellBuf and emit it joined with tabs at row end.
	var (
		row         []string
		cellBuf     strings.Builder
		inCell      bool
		paraNonZero bool
	)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("odf: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "table-cell":
				inCell = true
				cellBuf.Reset()
				row = append(row, "")
			case "tab":
				if inCell {
					cellBuf.WriteByte('\t')
				} else {
					_ = bw.WriteByte('\t')
				}
			case "line-break":
				if inCell {
					cellBuf.WriteByte(' ')
				} else {
					_ = bw.WriteByte('\n')
				}
			}
		case xml.CharData:
			if inCell {
				cellBuf.Write([]byte(t))
			} else {
				if len(t) > 0 {
					_, _ = bw.Write([]byte(t))
					paraNonZero = true
				}
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "p", "h":
				if !inCell && paraNonZero {
					_ = bw.WriteByte('\n')
					paraNonZero = false
				}
			case "table-cell":
				if len(row) > 0 {
					row[len(row)-1] = cellBuf.String()
				}
				inCell = false
			case "table-row":
				_, _ = bw.WriteString(strings.Join(row, "\t"))
				_ = bw.WriteByte('\n')
				row = row[:0]
			}
		}
	}
	return bw.Flush()
}

// readCharData consumes the content of the currently-open element up to
// its matching EndElement, returning the concatenated chardata. Tracks
// depth so a stray nested element (OOXML rich text inside <t> is
// pathological but possible) doesn't terminate early.
func readCharData(dec *xml.Decoder) (string, error) {
	var b strings.Builder
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err == io.EOF {
			return b.String(), nil
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			_ = t
			depth++
		case xml.CharData:
			b.Write([]byte(t))
		case xml.EndElement:
			depth--
		}
	}
	return b.String(), nil
}

// zipFile returns the *zip.File whose Name matches exactly, or nil.
// OOXML/ODF parts use forward-slash paths regardless of platform.
func zipFile(zr *zip.Reader, name string) *zip.File {
	for _, f := range zr.File {
		if f.Name == name {
			return f
		}
	}
	return nil
}

// indexedFile pairs a zip part with the integer suffix from its name
// (e.g. ppt/slides/slide7.xml → idx=7). PPTX and XLSX both encode
// ordering this way; alphabetical sort would put slide10 before slide2
// so callers need the parsed integer to sort and to label.
type indexedFile struct {
	idx int
	f   *zip.File
}

// numericallySortedFiles returns indexed files whose name has the
// shape `<prefix><N><suffix>` for some integer N, sorted by N
// ascending.
func numericallySortedFiles(zr *zip.Reader, prefix, suffix string) []indexedFile {
	var matches []indexedFile
	for _, f := range zr.File {
		base := path.Base(f.Name)
		fullPrefix := path.Base(prefix)
		if !strings.HasPrefix(f.Name, prefix) || !strings.HasSuffix(base, suffix) {
			continue
		}
		mid := strings.TrimSuffix(strings.TrimPrefix(base, fullPrefix), suffix)
		n, err := strconv.Atoi(mid)
		if err != nil {
			continue
		}
		matches = append(matches, indexedFile{idx: n, f: f})
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].idx < matches[j].idx })
	return matches
}

// indexedFiles is numericallySortedFiles' sibling that returns a map
// keyed by the numeric index. Used for notes slides which align by
// index with primary slides but may be missing for some.
func indexedFiles(zr *zip.Reader, prefix, suffix string) map[int]*zip.File {
	out := map[int]*zip.File{}
	for _, f := range zr.File {
		base := path.Base(f.Name)
		fullPrefix := path.Base(prefix)
		if !strings.HasPrefix(f.Name, prefix) || !strings.HasSuffix(base, suffix) {
			continue
		}
		mid := strings.TrimSuffix(strings.TrimPrefix(base, fullPrefix), suffix)
		n, err := strconv.Atoi(mid)
		if err != nil {
			continue
		}
		out[n] = f
	}
	return out
}

// bufferedWriter is a tiny shim over io.Writer so we can write byte and
// string fragments without each call hitting the os.File. Avoids a
// dependency on bufio just for this — the writes are short and frequent.
type bufferedWriter struct {
	w   io.Writer
	buf []byte
}

func newBufferedWriter(w io.Writer) *bufferedWriter {
	return &bufferedWriter{w: w, buf: make([]byte, 0, 4096)}
}

func (b *bufferedWriter) WriteByte(c byte) error {
	b.buf = append(b.buf, c)
	if len(b.buf) >= cap(b.buf) {
		return b.Flush()
	}
	return nil
}

func (b *bufferedWriter) WriteString(s string) (int, error) {
	b.buf = append(b.buf, s...)
	if len(b.buf) >= cap(b.buf) {
		return len(s), b.Flush()
	}
	return len(s), nil
}

func (b *bufferedWriter) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if len(b.buf) >= cap(b.buf) {
		return len(p), b.Flush()
	}
	return len(p), nil
}

func (b *bufferedWriter) Flush() error {
	if len(b.buf) == 0 {
		return nil
	}
	_, err := b.w.Write(b.buf)
	b.buf = b.buf[:0]
	return err
}
