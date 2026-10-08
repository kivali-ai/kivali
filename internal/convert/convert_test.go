package convert

import "testing"

func TestClassify(t *testing.T) {
	cases := map[string]Kind{
		".md":    KindText,
		".txt":   KindText,
		".MD":    KindText,
		".csv":   KindText,
		".json":  KindText,
		".js":    KindText,
		".yaml":  KindText,
		".pdf":   KindPDF,
		".png":   KindImage,
		".docx":  KindOffice,
		".pptx":  KindOffice,
		".bogus": KindUnknown,
	}
	for ext, want := range cases {
		if got := Classify(ext); got != want {
			t.Errorf("Classify(%q) = %v, want %v", ext, got, want)
		}
	}
}

func TestMIMEFor(t *testing.T) {
	cases := map[string]string{
		".md":   "text/markdown",
		".txt":  "text/plain",
		".csv":  "text/plain",
		".js":   "text/plain",
		".pdf":  "application/pdf",
		".png":  "image/png",
		".jpg":  "image/jpeg",
		".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
		".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		".odt":  "application/vnd.oasis.opendocument.text",
		".doc":  "application/msword",
		".rtf":  "application/rtf",
		".xyz":  "application/octet-stream",
	}
	for ext, want := range cases {
		if got := MIMEFor(ext); got != want {
			t.Errorf("MIMEFor(%q) = %q, want %q", ext, got, want)
		}
	}
}

func TestPdfToTextAvailableFalseWithBogusBin(t *testing.T) {
	orig := PdfToTextBin
	defer func() { PdfToTextBin = orig }()
	PdfToTextBin = "definitely-not-a-real-binary-kivali-xyzzy"
	if PdfToTextAvailable() {
		t.Error("expected false for bogus bin")
	}
}
