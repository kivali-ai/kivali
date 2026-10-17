package browserpod

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/kivali-ai/kivali/internal/clock"
)

// The pod runs a session only for a hello that carries core's token;
// a wrong, missing or unset token ends the run before Chromium starts.
func TestHelloMustCarryCoreToken(t *testing.T) {
	for _, tc := range []struct {
		name, want, sent string
	}{
		{"wrong", "ctok", "other"},
		{"missing", "ctok", ""},
		{"unset on the pod", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := newFakeCore(t)
			launched := false
			rt := &Runtime{
				Slug: "ada", Session: "s1", Token: "tok", CoreToken: tc.want, CoreUDS: fc.sock,
				Dir: t.TempDir(), Logger: log.New(io.Discard, "", 0), Clock: clock.NewFake(),
				launch: func(context.Context, chromeOptions) (browserProcess, error) {
					launched = true
					return nil, io.EOF
				},
			}
			done := make(chan error, 1)
			go func() { done <- rt.Run(context.Background()) }()
			core := fc.accept()
			core.send(Message{Type: TypeHello, CoreToken: tc.sent})
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("Run returned nil for a hello without core's token")
				}
			case <-time.After(failsafe):
				t.Fatal("Run did not refuse the hello")
			}
			if launched {
				t.Error("Chromium was launched")
			}
		})
	}
}
