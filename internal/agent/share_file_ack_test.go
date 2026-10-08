package agent

import (
	"reflect"
	"testing"
)

func TestParseShareFileAck(t *testing.T) {
	cases := []struct {
		name string
		ack  string
		want []SharedFile
	}{
		{
			name: "single file",
			ack: "Shared in this conversation:\n" +
				"- report.pdf (sha=abc123)",
			want: []SharedFile{{SHA: "abc123", Name: "report.pdf"}},
		},
		{
			name: "single file with caption suffix",
			ack: "Shared in this conversation:\n" +
				"- report.pdf (sha=abc123)\n" +
				"Caption: see chart 4",
			want: []SharedFile{{SHA: "abc123", Name: "report.pdf"}},
		},
		{
			name: "multiple files",
			ack: "Shared in this conversation:\n" +
				"- report.pdf (sha=abc123)\n" +
				"- chart.png (sha=def456)\n" +
				"- summary.md (sha=ghi789)",
			want: []SharedFile{
				{SHA: "abc123", Name: "report.pdf"},
				{SHA: "def456", Name: "chart.png"},
				{SHA: "ghi789", Name: "summary.md"},
			},
		},
		{
			name: "name with parentheses",
			ack: "Shared in this conversation:\n" +
				"- funky (name).zip (sha=deadbeef)",
			want: []SharedFile{{SHA: "deadbeef", Name: "funky (name).zip"}},
		},
		{
			name: "empty ack",
			ack:  "",
			want: nil,
		},
		{
			name: "garbage ack with no lines",
			ack:  "garbage",
			want: nil,
		},
		{
			name: "header but no entries",
			ack:  "Shared in this conversation:\n",
			want: nil,
		},
		{
			name: "skips malformed line, keeps the good one",
			ack: "Shared in this conversation:\n" +
				"- broken-no-sha\n" +
				"- ok.txt (sha=cafef00d)",
			want: []SharedFile{{SHA: "cafef00d", Name: "ok.txt"}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseShareFileAck(c.ack)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("parseShareFileAck:\n  got  %+v\n  want %+v", got, c.want)
			}
		})
	}
}
