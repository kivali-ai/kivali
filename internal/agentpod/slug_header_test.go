package agentpod

import (
	"net/http"
	"testing"
)

func TestSlugFromHeader(t *testing.T) {
	cases := []struct {
		name string
		set  map[string]string
		want string
	}{
		{"new header", map[string]string{SlugHeader: "alice"}, "alice"},
		{"neither", nil, ""},
	}
	for _, c := range cases {
		h := http.Header{}
		for k, v := range c.set {
			h.Set(k, v)
		}
		if got := SlugFromHeader(h); got != c.want {
			t.Errorf("%s: SlugFromHeader = %q, want %q", c.name, got, c.want)
		}
	}
}
