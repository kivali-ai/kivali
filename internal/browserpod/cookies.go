package browserpod

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"
)

// Cookie snapshots. A site session's profile is saved when its browser
// closes cleanly; between saves the pod sends core the session's
// cookies whenever they have changed, so a browser that dies first
// loses minutes of cookies, not a sign-in. The snapshot is the cookies
// as Storage.setCookies takes them, in a fixed order, so an unchanged
// jar gives the same bytes and nothing is sent.

// snapshotEvery is how often the pod looks at the cookies.
const snapshotEvery = 5 * time.Minute

// MaxCookiesBytes bounds a cookie snapshot in either direction.
const MaxCookiesBytes = 8 << 20

// cookieParam is one cookie as Storage.setCookies takes it. It reads
// Storage.getCookies' cookies too: the fields the two share.
type cookieParam struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	Domain   string `json:"domain,omitempty"`
	Path     string `json:"path,omitempty"`
	Secure   bool   `json:"secure,omitempty"`
	HTTPOnly bool   `json:"httpOnly,omitempty"`
	SameSite string `json:"sameSite,omitempty"`
	// Expires is seconds since the epoch; a session cookie has none
	// (getCookies says -1) and is restored as a session cookie.
	Expires      float64         `json:"expires,omitempty"`
	Priority     string          `json:"priority,omitempty"`
	SourceScheme string          `json:"sourceScheme,omitempty"`
	SourcePort   int             `json:"sourcePort,omitempty"`
	PartitionKey json.RawMessage `json:"partitionKey,omitempty"`
}

// cookies returns the browser's cookies as a snapshot: every cookie,
// whatever its domain. A site session's sign-in may go through an
// identity provider on another site, whose cookies carry the single
// sign-on the next sign-in to the site reuses.
func (b *browser) cookies(ctx context.Context) ([]byte, error) {
	raw, err := b.conn.Call(ctx, "", "Storage.getCookies", map[string]any{})
	if err != nil {
		return nil, err
	}
	var res struct {
		Cookies []cookieParam `json:"cookies"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("Storage.getCookies: %w", err)
	}
	jar := res.Cookies
	if jar == nil {
		jar = []cookieParam{}
	}
	for i := range jar {
		if jar[i].Expires <= 0 {
			jar[i].Expires = 0
		}
	}
	sort.SliceStable(jar, func(i, j int) bool {
		a, b := jar[i], jar[j]
		if a.Domain != b.Domain {
			return a.Domain < b.Domain
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Name < b.Name
	})
	return json.Marshal(jar)
}

// setCookies restores a snapshot: data is the array a snapshot carries.
func (b *browser) setCookies(ctx context.Context, data []byte) error {
	var jar []cookieParam
	if err := json.Unmarshal(data, &jar); err != nil {
		return fmt.Errorf("cookie snapshot: %w", err)
	}
	if len(jar) == 0 {
		return nil
	}
	if _, err := b.conn.Call(ctx, "", "Storage.setCookies", map[string]any{"cookies": jar}); err != nil {
		return fmt.Errorf("Storage.setCookies: %w", err)
	}
	return nil
}

// receiveCookies keeps the snapshot core sent before start for
// Chromium to set once it is up.
func (s *session) receiveCookies(msg Message, body io.Reader) {
	if msg.Bytes <= 0 || msg.Bytes > MaxCookiesBytes {
		s.log.Printf("cookies of %d bytes refused", msg.Bytes)
		return
	}
	data, err := io.ReadAll(io.LimitReader(body, msg.Bytes))
	if err != nil {
		s.log.Printf("reading cookies: %v", err)
		return
	}
	s.cookies = data
}

// restoreCookies sets the snapshot core sent, if any, on the running
// browser. A snapshot that cannot be set is logged, not fatal: the
// session still starts from its profile.
func (s *session) restoreCookies(ctx context.Context) {
	if len(s.cookies) == 0 {
		return
	}
	if err := s.b.setCookies(ctx, s.cookies); err != nil {
		s.log.Printf("restore cookies: %v", err)
	} else {
		s.log.Printf("cookies: %d bytes restored", len(s.cookies))
	}
	s.cookies = nil
}

// snapshots sends core the cookies whenever they have changed: every
// snapshotEvery, and at once when core asks. The first look, right
// after start, is the baseline, so a jar nobody touched sends nothing.
func (s *session) snapshots(ctx context.Context) {
	ticker := s.r.Clock.NewTicker(snapshotEvery)
	defer ticker.Stop()
	var last [sha256.Size]byte
	if data, err := s.b.cookies(ctx); err == nil {
		last = sha256.Sum256(data)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
		case <-s.snapshotReq:
		}
		data, err := s.b.cookies(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.log.Printf("cookies: %v", err)
			}
			continue
		}
		sum := sha256.Sum256(data)
		if sum == last {
			continue
		}
		if int64(len(data)) > MaxCookiesBytes {
			s.log.Printf("cookies: %d bytes, over the limit; not sent", len(data))
			continue
		}
		if err := s.w.sendBytes(context.Background(), Message{Type: TypeCookies, Bytes: int64(len(data))}, data); err != nil {
			s.log.Printf("send cookies: %v", err)
			return
		}
		last = sum
		s.log.Printf("cookies sent: %d bytes", len(data))
	}
}
