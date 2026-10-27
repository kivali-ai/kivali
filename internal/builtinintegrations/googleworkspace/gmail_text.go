package googleworkspace

import (
	"bytes"
	"cmp"
	"fmt"
	"io"
	"mime"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/net/html/charset"
	"golang.org/x/net/idna"
	"golang.org/x/text/unicode/norm"
)

// An email's own text reaches the agent between these two lines, so
// that it can tell what Kivali says from what the email says, as a
// page's text reaches a web task between the browser pod's
// (internal/browserpod/snapshot.go).
const (
	emailContentStart = "Content of an untrusted email follows."
	emailContentEnd   = "End of email content."
)

// framingLetters are the letters of every framing line Kivali writes,
// an email's, an event's (calendar.go) and a document's
// (docs_text.go), lower-cased: a line of untrusted text that starts
// with the same letters reads as one of them.
var framingLetters = []string{
	lettersOf(emailContentStart), lettersOf(emailContentEnd),
	lettersOf(eventContentStart), lettersOf(eventContentEnd),
	lettersOf(documentContentStart), lettersOf(documentContentEnd),
}

// lettersOf is s's letters, lower-cased, after Unicode compatibility
// folding (NFKC), so a full-width letter or a ligature reads as the
// plain letters it shows. Letters of another script that look like
// Latin ones (a Cyrillic U+0435 for an "e") are not mapped to Latin, so a
// line spelled with them does not read as a framing line.
func lettersOf(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, norm.NFKC.String(s))
}

// delimitEmail puts an email's own text between the two lines.
func delimitEmail(s string) string { return delimit(s, emailContentStart, emailContentEnd) }

// delimit puts untrusted text between the lines start and end. A line
// of it that starts as any framing line Kivali writes reads, whatever
// its spaces, case, punctuation and what follows, is quoted with "> ",
// so the text cannot end its own framing and go on as Kivali.
func delimit(s, start, end string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		letters := lettersOf(l)
		if slices.ContainsFunc(framingLetters, func(f string) bool { return strings.HasPrefix(letters, f) }) {
			lines[i] = "> " + l
		}
	}
	return start + "\n" + strings.Join(lines, "\n") + "\n" + end
}

// cleanText is untrusted text as the agent is given it: valid UTF-8,
// line ends as "\n" (a carriage return and Unicode's line and paragraph
// separators each one), no control character but tabs and line ends,
// and no format character (Unicode's Cf: a zero-width space, a
// right-to-left override, a tag character), which no one sees and a
// model reads, but the zero-width joiner emoji are built with. Tag
// characters (U+E0000 to U+E007F) are dropped whether or not Unicode
// assigns them. It answers how many format characters it dropped.
func cleanText(s string) (string, int) {
	s = strings.ReplaceAll(strings.ToValidUTF8(s, "\uFFFD"), "\r\n", "\n")
	dropped := 0
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r == '\r' || r == '\u2028' || r == '\u2029':
			return '\n'
		case r >= 0xE0000 && r <= 0xE007F:
			dropped++
			return -1
		case unicode.IsControl(r):
			return -1
		case r != '\u200D' && unicode.Is(unicode.Cf, r):
			dropped++
			return -1
		}
		return r
	}, s)
	return s, dropped
}

// oneLine is cleanText of a header's value with its line ends and runs
// of white space made one space, cut at most characters.
func oneLine(s string, most int) string {
	s, _ = cleanText(s)
	s = strings.Join(strings.Fields(s), " ")
	if cut, clipped := clip(s, most); clipped {
		s = cut + "..."
	}
	return s
}

// charCount is the characters of s a reader sees: each run of white
// space one.
func charCount(s string) int {
	return utf8.RuneCountInString(strings.Join(strings.Fields(s), " "))
}

// headerDecoder decodes a header's encoded words (RFC 2047) in any
// character set the HTML package knows.
var headerDecoder = &mime.WordDecoder{CharsetReader: charset.NewReaderLabel}

// decodeHeader is a header's value with its encoded words decoded, or
// as it is when they do not decode.
func decodeHeader(v string) string {
	if d, err := headerDecoder.DecodeHeader(v); err == nil {
		return d
	}
	return v
}

// decodeCharset is a body part's bytes as text, from the character set
// its Content-Type names; UTF-8 when it names none or one unknown.
func decodeCharset(b []byte, contentType string) string {
	_, params, _ := mime.ParseMediaType(contentType)
	if cs := params["charset"]; cs != "" && !strings.EqualFold(cs, "utf-8") && !strings.EqualFold(cs, "us-ascii") {
		if r, err := charset.NewReaderLabel(cs, bytes.NewReader(b)); err == nil {
			if d, err := io.ReadAll(r); err == nil {
				return string(d)
			}
		}
	}
	return string(b)
}

// account is the signed-in Gmail account (users/me/profile): what "your
// domain" means.
type account struct{ address, domain string }

// publicDomains are mail domains anyone may have an address at: for an
// account at one, only its own address is from the owner's domain.
var publicDomains = []string{"gmail.com", "googlemail.com"}

// owns reports whether addr, a sender's lower-cased address, is from
// the account's domain; never for "".
func (a account) owns(addr string) bool {
	at := strings.LastIndex(addr, "@")
	switch {
	case at < 0:
		return false
	case slices.Contains(publicDomains, a.domain):
		return addr == a.address
	}
	return addr[at+1:] == a.domain
}

// addressChars is a sender's address Kivali writes in its own words:
// these characters alone, at most 254 of them. Any other (a quoted local
// part holding spaces, say) is the sender's text, and Kivali writes none
// of it.
var addressChars = regexp.MustCompile(`^[a-z0-9.!#$%&'*+/=?^_{|}~-]+@[a-z0-9-]+(\.[a-z0-9-]+)+$`)

// senderOf is the address of a From header's value, lower-cased; ""
// when Kivali cannot read one.
func senderOf(from string) string {
	a, err := mail.ParseAddress(from)
	if err != nil {
		a, err = mail.ParseAddress(decodeHeader(from))
	}
	if err != nil {
		list, lerr := mail.ParseAddressList(from)
		if lerr != nil || len(list) == 0 {
			return ""
		}
		a = list[0]
	}
	return plainAddress(a)
}

// plainAddress is a parsed address lower-cased, when it is one Kivali
// writes in its own words (addressChars); "" when it is not.
func plainAddress(a *mail.Address) string {
	addr := strings.ToLower(a.Address)
	if len(addr) > 254 || !addressChars.MatchString(addr) {
		return ""
	}
	return addr
}

// domainOf is an address's domain, after its last "@"; "" for none.
func domainOf(addr string) string {
	at := strings.LastIndex(addr, "@")
	if at < 0 {
		return ""
	}
	return addr[at+1:]
}

// unreadAddress names an address Kivali cannot read in its own words.
const unreadAddress = "an address Kivali could not read"

// replyElsewhere is where a Reply-To header's value sends replies when
// that is not the sender's domain: its first address at another
// domain, or unreadAddress when Kivali cannot read one of its
// addresses; "" when there is no Reply-To, or every address in it is
// at the sender's domain.
func replyElsewhere(replyTo, sender string) string {
	if strings.TrimSpace(replyTo) == "" {
		return ""
	}
	list, err := mail.ParseAddressList(replyTo)
	if err != nil {
		list, err = mail.ParseAddressList(decodeHeader(replyTo))
	}
	if err != nil || len(list) == 0 {
		return unreadAddress
	}
	for _, a := range list {
		addr := plainAddress(a)
		switch {
		case addr == "":
			return unreadAddress
		case domainOf(addr) != domainOf(sender):
			return addr
		}
	}
	return ""
}

// checks is Gmail's checks of a message's sender, from the
// Authentication-Results header Gmail wrote: the SPF, DKIM and DMARC
// results, "" for a method it did not report, and the domains of the
// identities that passed: each passing DKIM signature's (header.d, or
// header.i's after its "@"), the passing SPF check's envelope sender's
// (smtp.mailfrom's after its "@", or all of it), and the From domain a
// passing DMARC check names (header.from). seen is false when there is
// no such header.
type checks struct {
	spf, dkim, dmarc             string
	dkimPass, spfPass, dmarcPass []string
	seen                         bool
}

// gmailAuthServ is the authserv-id Gmail's own Authentication-Results
// header starts with.
const gmailAuthServ = "mx.google.com"

// resultWord is a result Kivali writes as it is ("pass", "softfail");
// any other is "unknown".
var resultWord = regexp.MustCompile(`^[a-z]{1,16}$`)

// readChecks reads Gmail's checks from a message's headers, which Gmail
// gives top first. Gmail writes its Authentication-Results above every
// header the message came with, so only the first one is read, and only
// when its authserv-id is Gmail's: a sender can write one of its own
// lower down, claiming anything. A method reported more than once (a
// message signed twice) is pass when any one passed, else its first
// result. Its results are split where Gmail ends one (authResults), so
// a quoted envelope sender holding ";" cannot add a result of its own.
func readChecks(headers []gmailHeader) checks {
	for _, h := range headers {
		if !strings.EqualFold(h.Name, "Authentication-Results") {
			continue
		}
		parts := authResults(h.Value)
		if f := strings.Fields(parts[0]); len(f) == 0 || !strings.EqualFold(f[0], gmailAuthServ) {
			return checks{}
		}
		out := checks{seen: true}
		pick := func(cur, r string) string {
			if cur == "" || r == "pass" {
				return r
			}
			return cur
		}
		for _, p := range parts[1:] {
			f := quotedFields(p)
			if len(f) == 0 {
				continue
			}
			method, result, ok := strings.Cut(strings.ToLower(f[0]), "=")
			if !ok {
				continue
			}
			if !resultWord.MatchString(result) {
				result = "unknown"
			}
			props := map[string]string{}
			for _, prop := range f[1:] {
				if k, v, ok := strings.Cut(prop, "="); ok {
					props[strings.ToLower(k)] = v
				}
			}
			switch method {
			case "spf":
				out.spf = pick(out.spf, result)
				if d := identityDomain(props["smtp.mailfrom"]); result == "pass" && d != "" {
					out.spfPass = append(out.spfPass, d)
				}
			case "dkim":
				out.dkim = pick(out.dkim, result)
				if d := identityDomain(cmp.Or(props["header.d"], props["header.i"])); result == "pass" && d != "" {
					out.dkimPass = append(out.dkimPass, d)
				}
			case "dmarc":
				out.dmarc = pick(out.dmarc, result)
				if d := identityDomain(props["header.from"]); result == "pass" && d != "" {
					out.dmarcPass = append(out.dmarcPass, d)
				}
			}
		}
		return out
	}
	return checks{}
}

// authResults is an Authentication-Results value split into its parts
// at each ";" outside a quoted string, its comments (in parentheses,
// nested, outside quoted strings) each made one space.
func authResults(v string) []string {
	var parts []string
	var b strings.Builder
	depth, quoted, escaped := 0, false, false
	for _, r := range v {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && (quoted || depth > 0):
			escaped = true
		case depth > 0:
			switch r {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					b.WriteByte(' ')
				}
			}
			continue
		case quoted:
			quoted = r != '"'
		case r == '"':
			quoted = true
		case r == '(':
			depth = 1
			continue
		case r == ';':
			parts = append(parts, b.String())
			b.Reset()
			continue
		}
		if depth == 0 {
			b.WriteRune(r)
		}
	}
	return append(parts, b.String())
}

// quotedFields is s split at white space outside quoted strings.
func quotedFields(s string) []string {
	var out []string
	start, quoted, escaped := -1, false, false
	for i, r := range s {
		switch {
		case escaped:
			escaped = false
		case quoted && r == '\\':
			escaped = true
		case r == '"':
			quoted = !quoted
		case !quoted && unicode.IsSpace(r):
			if start >= 0 {
				out = append(out, s[start:i])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, s[start:])
	}
	return out
}

// domainName is a domain as Kivali compares one: letters, digits and
// dashes, in dot-separated parts.
var domainName = regexp.MustCompile(`^[a-z0-9-]+(\.[a-z0-9-]+)*$`)

// identityDomain is the domain of an identity Gmail checked, as an
// Authentication-Results property gives it ("@example.org",
// "x@example.org", "example.org"): after its last "@", lower-cased,
// without a final dot; "" for one that is not a plain domain.
func identityDomain(v string) string {
	d := strings.TrimSuffix(strings.ToLower(domainOf("@"+strings.Trim(v, "<>"))), ".")
	if !domainName.MatchString(d) {
		return ""
	}
	return d
}

// confirms reports whether Gmail's checks confirm a message came from
// domain, its From address's: DMARC passed for it, or a DKIM signature
// or the SPF check passed for it or a subdomain of it. Any other
// identity that passed (a relay's, a sender's own domain) says nothing
// of domain.
func (k checks) confirms(domain string) bool {
	if domain == "" {
		return false
	}
	aligned := func(d string) bool { return d == domain || strings.HasSuffix(d, "."+domain) }
	return slices.Contains(k.dmarcPass, domain) || slices.ContainsFunc(k.dkimPass, aligned) || slices.ContainsFunc(k.spfPass, aligned)
}

func (k checks) words() string {
	if !k.seen {
		return "Gmail's sender checks: not checked."
	}
	w := func(r string) string { return cmp.Or(r, "not checked") }
	return fmt.Sprintf("Gmail's sender checks: SPF %s, DKIM %s, DMARC %s.", w(k.spf), w(k.dkim), w(k.dmarc))
}

// provenance is the line Kivali writes before an email's own text: its
// sender's address, whether that is the owner's domain, and Gmail's
// checks of it.
func provenance(sender string, own bool, k checks) string {
	where := "from outside your domain"
	if own {
		where = "from your domain"
	}
	return senderLine(sender, where, k)
}

// senderLine is a provenance line: the sender's address, where, in
// Kivali's words, it is from, and Gmail's checks of it.
func senderLine(sender, where string, k checks) string {
	return "Sender: " + cmp.Or(sender, unreadAddress) + ", " + where + ". " + k.words()
}

// The links of an email's text are listed after it, at most maxLinks,
// each cut at linkClip characters.
const (
	maxLinks = 50
	linkClip = 200
)

// linkList is an email's links in the order its text names them, each
// once: the text names each by its marker, [link N].
type linkList struct {
	urls  []string
	index map[string]int
	more  map[string]bool
}

func newLinkList() *linkList {
	return &linkList{index: map[string]int{}, more: map[string]bool{}}
}

// urlPattern is an http or https URL in text: up to white space, a
// quote or an angle bracket.
var urlPattern = regexp.MustCompile("(?i)\\bhttps?://[^\\s<>\"'`]+")

// markerLike is text of the email's that reads as a marker's start.
var markerLike = regexp.MustCompile(`(?i)\[(\s*)link`)

// replace is s with each URL in it replaced by its marker. Text of the
// email's that reads like a marker ("[link 3]") is first spaced apart
// ("[ link 3]"), so every marker the agent reads is Kivali's.
func (l *linkList) replace(s string) string {
	s = markerLike.ReplaceAllString(s, "[ link")
	return urlPattern.ReplaceAllStringFunc(s, func(u string) string {
		u, tail := trimURL(u)
		return l.marker(u) + tail
	})
}

// trimURL is u without the punctuation that ends the sentence around
// it: full stops, commas and the like, and a closing bracket it does
// not open; tail is what it took off.
func trimURL(u string) (string, string) {
	end := len(u)
	for end > 0 {
		c := u[end-1]
		switch {
		case strings.IndexByte(".,;:!?*'\"", c) >= 0:
		case c == ')' && strings.Count(u[:end], "(") < strings.Count(u[:end], ")"):
		case c == ']' && strings.Count(u[:end], "[") < strings.Count(u[:end], "]"):
		case c == '}' && strings.Count(u[:end], "{") < strings.Count(u[:end], "}"):
		default:
			return u[:end], u[end:]
		}
		end--
	}
	return u[:end], u[end:]
}

// marker is u's marker: [link N], N its place in the list, or [link]
// for a link past the first maxLinks, which is not listed.
func (l *linkList) marker(u string) string {
	if i, ok := l.index[u]; ok {
		return "[link " + strconv.Itoa(i+1) + "]"
	}
	if len(l.urls) >= maxLinks {
		l.more[u] = true
		return "[link]"
	}
	l.urls = append(l.urls, u)
	l.index[u] = len(l.urls) - 1
	return "[link " + strconv.Itoa(len(l.urls)) + "]"
}

// listing is the first n links, one a line under "Links:", each its
// number, its host and the whole URL, each cut at linkClip characters;
// "" for none.
func (l *linkList) listing(n int) string {
	var nums []int
	for i := range max(min(n, len(l.urls)), 0) {
		nums = append(nums, i+1)
	}
	return l.listingOf(nums)
}

// listingOf is listing of the links numbered nums, in that order; a
// number the list does not hold is passed over.
func (l *linkList) listingOf(nums []int) string {
	var b strings.Builder
	for _, n := range nums {
		if n < 1 || n > len(l.urls) {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("Links:")
		}
		u := l.urls[n-1]
		shown := u
		if cut, clipped := clip(u, linkClip); clipped {
			shown = cut + "..."
		}
		fmt.Fprintf(&b, "\n%d. %s %s", n, linkHost(u), shown)
	}
	return b.String()
}

// moreNote says how many links of an email went unlisted; "" for none.
func (l *linkList) moreNote() string { return l.moreNoteOf("email") }

// moreNoteOf is moreNote for what holds the links ("email",
// "document").
func (l *linkList) moreNoteOf(what string) string {
	if len(l.more) == 0 {
		return ""
	}
	return fmt.Sprintf("The %s has %d more links, each shown as [link], that Kivali does not list.", what, len(l.more))
}

// linkHost is a URL's host as the agent reads it: lower-cased, as ASCII
// (an international name in its xn-- form, so a look-alike shows as
// one), cut at linkClip characters.
func linkHost(raw string) string {
	p, err := url.Parse(raw)
	if err != nil || p.Hostname() == "" {
		return "(no host)"
	}
	h := strings.ToLower(p.Hostname())
	if a, err := idna.ToASCII(h); err == nil {
		h = a
	}
	h, _ = clip(h, linkClip)
	return h
}

// plainText is a text/plain body as the agent reads it: cleaned, its
// URLs replaced by markers, and the tag characters cleanText dropped.
func plainText(s string, links *linkList) (string, int) {
	s, tags := cleanText(s)
	return trimBody(links.replace(s)), tags
}

// htmlToText is an HTML body as the agent reads it, as a reader of the
// email would see it, and how many characters of hidden text it left
// out (hidden). It drops what a reader never sees: the head, scripts,
// styles and other content that is not text, and images (not counted);
// comments, other than Outlook's conditional ones, which hold markup;
// and text styled out of sight by its <style> rules and inline styles
// (htmlConverter), both counted in hidden. Links become markers, block
// elements line ends, list items "- " lines. The page behind the text
// is white until an element gives it a background, as a mail reader's
// is.
func htmlToText(src string, links *linkList) (string, int) {
	doc, err := html.ParseWithOptions(strings.NewReader(src), html.ParseOptionEnableScripting(false))
	if err != nil {
		// The parser fails only on a failing reader, which a string's
		// is not.
		return plainText(src, links)
	}
	c := &htmlConverter{links: links, sheet: sheetOf(doc)}
	c.walk(doc, textStyle{fontPx: -1, background: "#ffffff"})
	return trimBody(c.out.b.String()), c.hidden
}

// trimBody is a body without blank lines before it or white space after
// it; a first line's indent stays.
func trimBody(s string) string {
	return strings.TrimRight(strings.TrimLeft(s, "\r\n"), " \t\r\n")
}

// htmlConverter walks an HTML body into text.
type htmlConverter struct {
	out    textOut
	links  *linkList
	sheet  *styleSheet
	hidden int
	// image is set when an image a reader sees was walked, so a link
	// around one still gets its marker.
	image bool
}

// textStyle is what the styles above a text leave it: its colour and
// the background behind it, normalized (normColor; a colour "" where
// none is given, the background the white page's where none is given),
// its font size in pixels (-1 where none is given), and whether
// visibility hides it or it is preformatted.
type textStyle struct {
	color, background string
	fontPx            float64
	invisible         bool
	pre               bool
}

// tinyFontPx is the largest font size, in pixels, of text a reader
// cannot read.
const tinyFontPx = 3

// hides reports whether text in this style is out of sight: hidden by
// visibility, in a font of tinyFontPx or less, or in a colour that is
// transparent or the background's.
func (st textStyle) hides() bool {
	return st.invisible || (st.fontPx >= 0 && st.fontPx <= tinyFontPx) || st.color == "transparent" ||
		(st.color != "" && st.background != "" && st.color == st.background)
}

// skipped are elements whose content is not text a reader sees, left
// out and not counted as hidden.
var skipped = map[atom.Atom]bool{
	atom.Head: true, atom.Title: true, atom.Script: true, atom.Style: true, atom.Template: true,
	atom.Meta: true, atom.Link: true, atom.Object: true, atom.Embed: true, atom.Iframe: true,
	atom.Svg: true, atom.Math: true,
}

// paragraphBlocks end with a blank line, lineBlocks with a line end.
var (
	paragraphBlocks = map[atom.Atom]bool{
		atom.P: true, atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
		atom.Blockquote: true, atom.Pre: true, atom.Ul: true, atom.Ol: true, atom.Dl: true, atom.Hr: true,
	}
	lineBlocks = map[atom.Atom]bool{
		atom.Div: true, atom.Tr: true, atom.Li: true, atom.Dt: true, atom.Dd: true, atom.Table: true,
		atom.Section: true, atom.Article: true, atom.Header: true, atom.Footer: true, atom.Center: true,
		atom.Address: true, atom.Figure: true, atom.Figcaption: true, atom.Form: true, atom.Nav: true,
		atom.Main: true, atom.Aside: true, atom.Details: true, atom.Summary: true, atom.Caption: true,
	}
)

func (c *htmlConverter) walk(n *html.Node, st textStyle) {
	switch n.Type {
	case html.DocumentNode:
		for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
			c.walk(ch, st)
		}
		return
	case html.TextNode:
		c.text(n.Data, st)
		return
	case html.CommentNode:
		c.hidden += commentChars(n.Data)
		return
	case html.ElementNode:
	default:
		return
	}
	if skipped[n.DataAtom] {
		return
	}
	css := c.sheet.style(n)
	if elementHidden(n, css) {
		c.hidden += hiddenChars(n)
		return
	}
	st = st.under(n, css)
	switch n.DataAtom {
	case atom.Br:
		c.out.lineBreak()
		return
	case atom.Img:
		c.image = true
		return
	}
	block := 0
	switch {
	case paragraphBlocks[n.DataAtom]:
		block = 2
	case lineBlocks[n.DataAtom]:
		block = 1
	}
	c.out.block(block)
	if n.DataAtom == atom.Li {
		c.out.bullet = true
	}
	href := ""
	if n.DataAtom == atom.A {
		href = httpHref(n)
	}
	seen, image := c.out.written, c.image
	c.image = false
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		c.walk(ch, st)
	}
	if href != "" && (c.out.written > seen || c.image) {
		if m := c.links.marker(href); !strings.HasSuffix(c.out.b.String(), m) {
			c.out.word(m)
		}
	}
	c.image = c.image || image
	c.out.block(block)
	if n.DataAtom == atom.Td || n.DataAtom == atom.Th {
		c.out.gap()
	}
}

// text writes a text node of the body, or counts it as hidden when its
// style hides it.
func (c *htmlConverter) text(s string, st textStyle) {
	s, tags := cleanText(s)
	c.hidden += tags
	if st.hides() {
		c.hidden += charCount(s)
		return
	}
	s = c.links.replace(s)
	if st.pre {
		c.out.pre(s)
		return
	}
	c.out.inline(s)
}

// commentChars is a comment's characters as hidden text: none for one
// of Outlook's conditional comments ("[if mso]>...<![endif]"), which
// hold markup.
func commentChars(data string) int {
	t := strings.TrimSpace(data)
	if strings.HasPrefix(t, "[if") || strings.HasPrefix(t, "<![endif]") || strings.HasPrefix(t, "[endif]") || strings.HasSuffix(t, "<![endif]") {
		return 0
	}
	return charCount(t)
}

// hiddenChars counts the text under an element left out whole, outside
// the elements whose content is not text.
func hiddenChars(n *html.Node) int {
	total := 0
	for ch := n.FirstChild; ch != nil; ch = ch.NextSibling {
		switch {
		case ch.Type == html.TextNode:
			s, _ := cleanText(ch.Data)
			total += charCount(s)
		case ch.Type == html.ElementNode && !skipped[ch.DataAtom]:
			total += hiddenChars(ch)
		}
	}
	return total
}

func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Namespace == "" && strings.EqualFold(a.Key, key) {
			return a.Val, true
		}
	}
	return "", false
}

// httpHref is an anchor's href when it is an http or https URL; "" for
// any other.
func httpHref(n *html.Node) string {
	v, _ := attr(n, "href")
	v, _ = cleanText(strings.TrimSpace(v))
	if l := strings.ToLower(v); !strings.HasPrefix(l, "http://") && !strings.HasPrefix(l, "https://") {
		return ""
	}
	if strings.ContainsAny(v, " \t\n") {
		return ""
	}
	return v
}

// elementHidden reports whether an element and all it holds are out of
// sight, whatever is inside, by its attributes and the properties that
// apply to it (styleSheet.style): the hidden attribute; display:none;
// Outlook's mso-hide:all; an opacity of 0.05 or less; no height (or
// width, or their maximum) with its overflow hidden; moved 999px or
// more off the page to the left or the top (text-indent, left, top or
// margin-left of -999px or less, positioned or not); or clipped to
// nothing (clip:rect with no area, clip-path:inset of 50% or more).
//
// Not covered, so text hidden these ways reaches the agent: a transform
// (scale(0), a translate off the page), right, bottom or another
// margin off the page, an element laid over another (z-index, a
// negative margin onto it), a height or line-height of 0 without
// overflow hidden, clip-path shapes other than inset, filter:opacity, a
// colour near the background's but not equal to it, or one Kivali does
// not read (hsl(), a variable), text on a background image of its own
// colour, and the style rules styleSheet does not apply.
func elementHidden(n *html.Node, css map[string]string) bool {
	if _, ok := attr(n, "hidden"); ok {
		return true
	}
	if css["display"] == "none" || css["mso-hide"] == "all" {
		return true
	}
	if v, ok := css["opacity"]; ok {
		if f, ok := cssNumber(v); ok {
			if strings.HasSuffix(v, "%") {
				f /= 100
			}
			if f <= 0.05 {
				return true
			}
		}
	}
	if css["overflow"] == "hidden" {
		for _, k := range []string{"height", "max-height", "width", "max-width"} {
			if f, ok := cssNumber(css[k]); ok && f == 0 {
				return true
			}
		}
	}
	for _, k := range []string{"text-indent", "left", "top", "margin-left"} {
		if px, ok := lengthPx(css[k]); ok && px <= -offPagePx {
			return true
		}
	}
	return clippedAway(css["clip"], css["clip-path"])
}

// offPagePx is how far, in pixels, an element moved to the left or the
// top is off the page.
const offPagePx = 999

// lengthPx is a CSS length in pixels: px, or a number without a unit
// as mail readers take it; pt; em and rem against 16px. ok is false for
// a value it does not read (a percentage, auto).
func lengthPx(v string) (float64, bool) {
	f, ok := cssNumber(v)
	if !ok {
		return 0, false
	}
	unit := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(v), "+-.0123456789"))
	switch unit {
	case "", "px":
		return f, true
	case "pt":
		return f * 4 / 3, true
	case "em", "rem":
		return f * 16, true
	}
	return 0, false
}

// clippedAway reports whether a clip or clip-path value leaves nothing
// of an element: clip:rect(top, right, bottom, left) with its bottom
// at or above its top or its right at or left of its left
// (rect(0,0,0,0), rect(1px,1px,1px,1px)), or clip-path:inset() of one
// percentage, 50% or more, cut from every side.
func clippedAway(clip, clipPath string) bool {
	if in, ok := cssCall(clip, "rect"); ok {
		var side [4]float64
		parts := strings.FieldsFunc(in, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
		if len(parts) == 4 {
			read := 0
			for i, p := range parts {
				if f, ok := lengthPx(p); ok {
					side[i] = f
					read++
				}
			}
			if read == 4 && (side[2] <= side[0] || side[1] <= side[3]) {
				return true
			}
		}
	}
	if in, ok := cssCall(clipPath, "inset"); ok {
		if parts := strings.Fields(in); len(parts) == 1 && strings.HasSuffix(parts[0], "%") {
			if f, ok := cssNumber(parts[0]); ok && f >= 50 {
				return true
			}
		}
	}
	return false
}

// cssCall is what a CSS function value of the name holds between its
// parentheses: "0, 0, 0, 0" for "rect(0, 0, 0, 0)".
func cssCall(v, name string) (string, bool) {
	v = strings.TrimSpace(v)
	rest, ok := strings.CutPrefix(v, name+"(")
	if !ok || !strings.HasSuffix(rest, ")") {
		return "", false
	}
	return strings.TrimSuffix(rest, ")"), true
}

// cssNumber is a CSS value's number, with its unit or percent sign
// dropped.
func cssNumber(v string) (float64, bool) {
	v = strings.TrimSpace(v)
	end := 0
	for end < len(v) && (v[end] == '.' || v[end] == '-' || v[end] == '+' || (v[end] >= '0' && v[end] <= '9')) {
		end++
	}
	f, err := strconv.ParseFloat(v[:end], 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// under is the style of the text inside n: n's own background, colour,
// font size, visibility and white space where it gives them, by the
// properties that apply to it or by the old attributes (bgcolor, a
// background image, a font's color, the body's text), and the style
// above it where it does not. CSS takes the place of an attribute.
func (st textStyle) under(n *html.Node, css map[string]string) textStyle {
	if v, ok := attr(n, "bgcolor"); ok {
		st.background = backgroundOf(v, st.background)
	}
	if v, ok := attr(n, "background"); ok && strings.TrimSpace(v) != "" {
		st.background = "image"
	}
	for _, k := range []string{"background", "background-color", "background-image"} {
		if v, ok := css[k]; ok {
			st.background = backgroundOf(v, st.background)
		}
	}
	if v, ok := attr(n, "color"); ok && n.DataAtom == atom.Font {
		st.color = cmp.Or(normColor(v), st.color)
	}
	if v, ok := attr(n, "text"); ok && n.DataAtom == atom.Body {
		st.color = cmp.Or(normColor(v), st.color)
	}
	if v, ok := css["color"]; ok {
		st.color = cmp.Or(normColor(v), st.color)
	}
	if v, ok := css["font-size"]; ok {
		st.fontPx = fontPx(v, st.fontPx)
	}
	switch css["visibility"] {
	case "hidden", "collapse":
		st.invisible = true
	case "visible":
		st.invisible = false
	}
	if n.DataAtom == atom.Pre || strings.HasPrefix(css["white-space"], "pre") {
		st.pre = true
	}
	return st
}

// backgroundOf is the background a background value gives: its colour,
// "image" for an image (whose colours Kivali does not know, so no text
// matches it), or above, the background showing through, for none.
func backgroundOf(v, above string) string {
	if strings.Contains(v, "url(") {
		return "image"
	}
	for _, f := range cssFields(v) {
		if c := normColor(f); c != "" && c != "transparent" {
			return c
		}
	}
	return above
}

// cssFields is a CSS value's parts, a function's arguments kept with
// it: "#fff url(a b) no-repeat" is "#fff", "url(a b)", "no-repeat".
func cssFields(v string) []string {
	var out []string
	depth, start := 0, -1
	for i, r := range v {
		switch {
		case r == '(':
			depth++
		case r == ')':
			depth--
		case unicode.IsSpace(r) && depth == 0:
			if start >= 0 {
				out = append(out, v[start:i])
				start = -1
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	if start >= 0 {
		out = append(out, v[start:])
	}
	return out
}

// fontPx is a font-size value in pixels, relative ones taken against
// parent (16 where none is given above); parent again for a value it
// does not read (a keyword such as small, which is never tiny).
func fontPx(v string, parent float64) float64 {
	base := parent
	if base < 0 {
		base = 16
	}
	f, ok := cssNumber(v)
	if !ok {
		return parent
	}
	switch {
	case strings.HasSuffix(v, "px"):
		return f
	case strings.HasSuffix(v, "pt"):
		return f * 4 / 3
	case strings.HasSuffix(v, "rem"):
		return f * 16
	case strings.HasSuffix(v, "em"):
		return f * base
	case strings.HasSuffix(v, "%"):
		return f / 100 * base
	case f == 0:
		return 0
	}
	return parent
}

// namedColors are the colour names emails use most, as #rrggbb; a name
// not here is compared as written.
var namedColors = map[string]string{
	"white": "#ffffff", "black": "#000000", "red": "#ff0000", "lime": "#00ff00", "green": "#008000",
	"blue": "#0000ff", "yellow": "#ffff00", "cyan": "#00ffff", "aqua": "#00ffff", "magenta": "#ff00ff",
	"fuchsia": "#ff00ff", "silver": "#c0c0c0", "gray": "#808080", "grey": "#808080", "maroon": "#800000",
	"olive": "#808000", "purple": "#800080", "teal": "#008080", "navy": "#000080", "orange": "#ffa500",
	"whitesmoke": "#f5f5f5", "snow": "#fffafa", "ivory": "#fffff0", "ghostwhite": "#f8f8ff",
	"lightgray": "#d3d3d3", "lightgrey": "#d3d3d3", "darkgray": "#a9a9a9", "darkgrey": "#a9a9a9",
}

// normColor is a colour as #rrggbb, or "transparent" for one with no
// alpha, or a name not known as written; "" for a value that is not a
// colour Kivali reads (inherit, a variable).
func normColor(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch {
	case v == "transparent":
		return v
	case strings.HasPrefix(v, "#"):
		h := v[1:]
		if !isHex(h) {
			return ""
		}
		switch len(h) {
		case 3, 4:
			if len(h) == 4 && h[3] == '0' {
				return "transparent"
			}
			return "#" + strings.Repeat(h[0:1], 2) + strings.Repeat(h[1:2], 2) + strings.Repeat(h[2:3], 2)
		case 6, 8:
			if len(h) == 8 && h[6:] == "00" {
				return "transparent"
			}
			return "#" + h[:6]
		}
		return ""
	case strings.HasPrefix(v, "rgb"):
		open, end := strings.IndexByte(v, '('), strings.LastIndexByte(v, ')')
		if open < 0 || end < open {
			return ""
		}
		parts := strings.FieldsFunc(v[open+1:end], func(r rune) bool { return r == ',' || r == '/' || unicode.IsSpace(r) })
		if len(parts) < 3 {
			return ""
		}
		if len(parts) >= 4 {
			if a, ok := cssNumber(parts[3]); ok && a == 0 {
				return "transparent"
			}
		}
		out := "#"
		for _, p := range parts[:3] {
			f, ok := cssNumber(p)
			if !ok {
				return ""
			}
			if strings.HasSuffix(p, "%") {
				f = f * 255 / 100
			}
			out += fmt.Sprintf("%02x", int(min(max(f, 0), 255)+0.5))
		}
		return out
	case v == "" || v == "inherit" || v == "initial" || v == "unset" || v == "currentcolor" || strings.HasPrefix(v, "var("):
		return ""
	}
	if hex, ok := namedColors[v]; ok {
		return hex
	}
	return v
}

func isHex(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return s != ""
}

// textOut is the text an HTML body converts to, as it is written: words
// with one space between them, line ends where blocks end.
type textOut struct {
	b strings.Builder
	// nl is the line ends owed before the next word, at most two; space
	// a space owed; bullet a list item's "- " owed.
	nl     int
	space  bool
	bullet bool
	// written counts the characters written.
	written int
}

// inline writes text whose white space is the HTML's own: each run is
// one space.
func (o *textOut) inline(s string) {
	words := strings.Fields(s)
	if len(words) == 0 {
		if s != "" {
			o.space = true
		}
		return
	}
	if r, _ := utf8.DecodeRuneInString(s); unicode.IsSpace(r) {
		o.space = true
	}
	o.emit(strings.Join(words, " "))
	if r, _ := utf8.DecodeLastRuneInString(s); unicode.IsSpace(r) {
		o.space = true
	}
}

// pre writes preformatted text as it is.
func (o *textOut) pre(s string) {
	if s != "" {
		o.emit(s)
	}
}

// word writes one word after a space.
func (o *textOut) word(s string) {
	o.space = true
	o.emit(s)
}

func (o *textOut) emit(s string) {
	if o.b.Len() > 0 {
		switch {
		case o.nl > 0:
			o.b.WriteString(strings.Repeat("\n", o.nl))
		case o.space:
			o.b.WriteByte(' ')
		}
	}
	if o.bullet {
		o.b.WriteString("- ")
		o.bullet = false
	}
	o.b.WriteString(s)
	o.written += utf8.RuneCountInString(s)
	o.nl, o.space = 0, false
}

// block owes n line ends (1 a line, 2 a paragraph) before the next word.
func (o *textOut) block(n int) {
	if n == 0 || o.b.Len() == 0 {
		return
	}
	o.nl = max(o.nl, n)
	o.space = false
}

// lineBreak owes one more line end, as a br does, up to a blank line.
func (o *textOut) lineBreak() {
	if o.b.Len() == 0 {
		return
	}
	o.nl = min(o.nl+1, 2)
	o.space = false
}

// gap owes a space between table cells.
func (o *textOut) gap() {
	if o.nl == 0 {
		o.space = true
	}
}
