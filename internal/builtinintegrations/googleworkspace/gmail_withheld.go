package googleworkspace

import (
	"context"
	stdhtml "html"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Kivali withholds from agents every message that hands over access to
// an account: a sign-in or verification code, a sign-in link, a
// password reset, or a link that confirms an email address for an
// account. Whoever holds one can sign in as the owner, or take an
// account over, so an agent never reads one, whatever its access: an
// agent misled by an email it read could otherwise pass one on, or use
// it in its browser. A withheld message is listed by Kivali's heading
// and provenance lines alone, with a line saying why, and an agent may
// not save its attachments, change its labels or reply to it.
//
// accessKind decides, and errs towards showing mail: each kind needs
// both a phrase that names its purpose and what it hands over (a code
// beside the phrase, or a link), and nothing a person writes in a
// conversation (a reply or a forward, mail the owner sent or is
// drafting) is withheld.

// The kinds of access a withheld message carries, as the agent is told.
const (
	accessCode    = "a sign-in or verification code"
	accessLink    = "a sign-in link"
	accessReset   = "a password reset"
	accessConfirm = "an email address confirmation"
)

// emailWithheld is in the description of every action that returns an
// email's text, before emailUntrusted.
const emailWithheld = ` A message that carries a sign-in or verification code, a sign-in link, a password reset or an email address confirmation is withheld: it is listed by its id, when Gmail received it and who sent it, with a line saying why, and you may not save its attachments, change its labels or reply to it. Keys, tokens and private keys in an email's text are shown as "` + secretRemoved + `".`

// withheldNever is what the agent is told of every withheld message.
const withheldNever = "Agents never see sign-in or verification codes, sign-in links, password resets or email address confirmations."

// withheldLine is the line a withheld message is read as, after its
// heading and provenance lines.
func withheldLine(id, kind string) string {
	return "Kivali withheld message " + id + ": it carries " + kind + ". " + withheldNever + " If your work needs it, ask the owner."
}

// refuseWithheld is the refusal of what an action would do with a
// withheld message: doing names it ("save its attachments").
func refuseWithheld(id, kind, doing string) error {
	return refuse("Kivali withheld message "+id+": it carries "+kind+", so you may not "+doing+". "+withheldNever+" If your work needs it, ask the owner.",
		"message "+id+" carries "+kind)
}

// withholding is the kind of access m carries, read from its body;
// "" when Kivali shows it. A body Kivali cannot read (it does not
// decode, or Gmail does not answer for it) is judged by Gmail's snippet,
// so that one such message does not fail a search that lists it.
func (c *call) withholding(ctx context.Context, m gmailMessage) (string, error) {
	b, err := c.body(ctx, m, newLinkList())
	if err != nil {
		if ctx.Err() != nil {
			return "", err
		}
		return accessKind(m, ""), nil
	}
	return accessKind(m, b.text), nil
}

// conversationSubject is a subject a reply or a forward starts with, in
// the languages mail readers commonly write one.
var conversationSubject = regexp.MustCompile(`(?i)^(?:re|fw|fwd|aw|wg|sv|vs|rv|tr|antw|odp|vb|r|i)\s*(?:\[\d+\]|\(\d+\))?\s*:`)

// accessKind is the kind of access m carries, judged from its subject
// and body, the body as Kivali renders it (each link a "[link" marker);
// Gmail's snippet stands in for a body with no text. "" for none.
func accessKind(m gmailMessage, body string) string {
	if slices.Contains(m.LabelIDs, "SENT") || slices.Contains(m.LabelIDs, "DRAFT") || header(m.Payload.Headers, "In-Reply-To") != "" {
		return ""
	}
	subject := normalForm(decodeHeader(header(m.Payload.Headers, "Subject")))
	if conversationSubject.MatchString(subject) {
		return ""
	}
	if strings.TrimSpace(body) == "" {
		body = stdhtml.UnescapeString(m.Snippet)
	}
	linked := strings.Contains(body, "[link")
	text := subject + " \n " + normalForm(body)
	switch {
	case hasCode(subject, text):
		return accessCode
	case linked && (signInLink.MatchString(text) || signInAction.MatchString(text) && singleUse.MatchString(text)):
		return accessLink
	case temporaryPassword.MatchString(text):
		return accessReset
	case alreadyDone.MatchString(subject) && !requested.MatchString(subject):
		return ""
	case resetSubject.MatchString(subject) && (linked || anyCode(text)):
		return accessReset
	case confirmsAccount(subject) && (linked || anyCode(text)):
		return accessConfirm
	}
	return ""
}

// normalForm is s as accessKind reads it: compatibility-folded (NFKC),
// every dash a hyphen and every run of white space one space. Case is
// kept: a code is told from a word partly by its capitals.
func normalForm(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case '‐', '‑', '‒', '–', '—', '―', '−':
			return '-'
		}
		return r
	}, norm.NFKC.String(s))
	return strings.Join(strings.Fields(s), " ")
}

// codePhrase names a code that signs in or confirms who someone is.
// Codes for other things, which share some of its words, are left out:
// a meeting's passcode, a booking's confirmation code, a card's
// authorization code, an event's registration code, a discount code.
var codePhrase = regexp.MustCompile(`(?i)\b(?:` +
	`(?:verification|security|sign-?in|sign in|log-?in|log in|one-time|one time|single-use|authentication|2fa|two-factor|two factor|2-step|two-step|2-factor|mfa|multi-factor|recovery|backup|reset|device|identity)` +
	`(?: verification| security| sign-in| login| authentication)?(?: \([a-z0-9]+\))? (?:code|codes|passcode|pass code|pin)` +
	`|one-time pass(?:code|word)|one time pass(?:code|word)|otp` +
	`|(?:enter|use|type) (?:this|the following|the) code (?:to (?:sign|log) ?in|to verify|on your (?:device|tv|screen))` +
	`)\b`)

// codeLead is where an email whose subject names a code gives it:
// "enter this code:", "your code is", "code:".
var codeLead = regexp.MustCompile(`(?i)\b(?:(?:enter|use|type) (?:this|the following|the) code|(?:code|passcode|pin) is|code:)`)

// brandCode is "your <name> code is", which signs in when the email
// also asks that the code not be shared (dontShare).
var (
	brandCode = regexp.MustCompile(`(?i)\byour (?:\S+ ){1,3}code(?: is)?:?`)
	dontShare = regexp.MustCompile(`(?i)\b(?:don't|do not|never) share\b`)
)

// confirmationCode is a confirmation code. It signs in when the code is
// digits alone, or of capitals in two groups ("QJS-PNT") in an email
// about signing in and not about a booking or an order.
var (
	confirmationCode = regexp.MustCompile(`(?i)\bconfirmation code\b`)
	letterCode       = regexp.MustCompile(`\b[A-Z]{3,4}-[A-Z]{3,4}\b`)
	signInWords      = regexp.MustCompile(`(?i)\b(?:sign(?:ed)? ?in|log(?:ged)? ?in|browser window)\b`)
	bookingWords     = regexp.MustCompile(`(?i)\b(?:booking|reservation|flight|hotel|order|ticket|trip|itinerary|purchase)\b`)
)

// codeBefore is a code a phrase follows ("482913 is your Acme code").
var codeBefore = regexp.MustCompile(`(?i)\b(\d{4,10}) is your (?:\S+ ){0,3}(?:code|passcode|pin)\b`)

// codeToken is a candidate code: letters and digits, hyphens between.
var codeToken = regexp.MustCompile(`[A-Za-z0-9]+(?:-[A-Za-z0-9]+)*`)

// spacedDigits is a code of digits in two groups ("482 913").
var spacedDigits = regexp.MustCompile(`\b\d{3} \d{3,4}\b`)

// notCode is digits that are never a code: a phone number, and the end
// of a card's or a phone's number ("ending in 4242").
var notCode = regexp.MustCompile(`(?i)(?:ending(?: in)?|last (?:4|four) digits(?: of)?|ext\.?|#|no\.)\s*[\d-]+` +
	`|\+?\(?\b\d{3}\)?[ .-]?\d{3}[ .-]\d{4}\b|\+\d[\d ().-]{7,}\d`)

// unitToken is a number with a unit or a suffix ("10AM", "1990S").
var unitToken = regexp.MustCompile(`(?i)^\d+(?:am|pm|s|th|nd|rd|st|h|hr|hrs|min|mins|px|kb|mb|gb|k|m)$`)

// The bytes after a code phrase, and before it, a code is looked for
// in, and after codeLead or brandCode.
const (
	codeAfter      = 150
	codeLookBehind = 60
	codeLeadAfter  = 30
)

// hasCode reports whether text, which starts with subject, holds a code
// beside a phrase that names it: within codeAfter bytes after one or
// codeLookBehind before; or after codeLead when the subject names a
// code; or after brandCode in an email that asks it not be shared.
func hasCode(subject, text string) bool {
	if codeBefore.MatchString(text) {
		return true
	}
	after := func(end, n int) string { return text[end:min(len(text), end+n)] }
	for _, loc := range codePhrase.FindAllStringIndex(text, -1) {
		if codeIn(after(loc[1], codeAfter), false) || codeIn(before(text, loc[0]), false) {
			return true
		}
	}
	signIn := signInWords.MatchString(text) && !bookingWords.MatchString(text)
	for _, loc := range confirmationCode.FindAllStringIndex(text, -1) {
		near := after(loc[1], codeAfter)
		if codeIn(near, true) || codeIn(before(text, loc[0]), true) || signIn && letterCode.MatchString(near) {
			return true
		}
	}
	if codePhrase.MatchString(subject) {
		for _, loc := range codeLead.FindAllStringIndex(text, -1) {
			if codeIn(after(loc[1], codeLeadAfter), false) {
				return true
			}
		}
	}
	if dontShare.MatchString(text) {
		for _, loc := range brandCode.FindAllStringIndex(text, -1) {
			if codeIn(after(loc[1], codeLeadAfter), false) {
				return true
			}
		}
	}
	return false
}

// before is the codeLookBehind bytes of text before end, starting at a
// token's start, so that a longer number is not read as its tail.
func before(text string, end int) string {
	start := max(0, end-codeLookBehind)
	for start > 0 && start < end && isAlnum(text[start-1]) && isAlnum(text[start]) {
		start++
	}
	return text[start:end]
}

func isAlnum(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// anyCode reports whether text holds a code anywhere.
func anyCode(text string) bool { return codeIn(text, false) }

// codeIn reports whether s holds a code: digits alone, or with
// digitsOnly false letters and digits too. Phone numbers and card
// endings are passed over.
func codeIn(s string, digitsOnly bool) bool {
	s = notCode.ReplaceAllString(s, " ")
	if spacedDigits.MatchString(s) {
		return true
	}
	for _, t := range codeToken.FindAllString(s, -1) {
		if isCode(t, digitsOnly) {
			return true
		}
	}
	return false
}

// isCode reports whether t, a run of letters and digits with hyphens
// between, reads as a code: 4 to 10 digits that are not a year (1900
// to 2099); or with digitsOnly false, 4 to 16 letters and digits
// holding at least two digits, not a number with a unit, and either
// with no small letters ("BDWP-HQ84", "G-482913") or in hyphenated
// groups of 4 to 6 ("a1b2c-3d4e5", a recovery code). A word such as
// "sha256" is not one.
func isCode(t string, digitsOnly bool) bool {
	groups := strings.Split(t, "-")
	whole := strings.Join(groups, "")
	digits, small := 0, false
	for _, r := range whole {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r >= 'a' && r <= 'z':
			small = true
		}
	}
	if digits == len(whole) {
		if len(whole) == 4 {
			if y, _ := strconv.Atoi(whole); y >= 1900 && y <= 2099 {
				return false
			}
		}
		return len(whole) >= 4 && len(whole) <= 10
	}
	if digitsOnly || digits < 2 || len(whole) < 4 || len(whole) > 16 || unitToken.MatchString(whole) {
		return false
	}
	if !small {
		return true
	}
	return len(groups) >= 2 && !slices.ContainsFunc(groups, func(g string) bool { return len(g) < 4 || len(g) > 6 })
}

// signInLink hands the reader a link that signs in, or asks them to
// approve a sign-in.
var signInLink = regexp.MustCompile(`(?i)\b(?:` +
	`(?:your|here is your|here's your|this|the following) (?:magic (?:sign-in |sign in |login |log-in |log in )?|sign-in |login |log-in )link` +
	`|(?:approve|confirm) (?:this|the|your) (?:sign-in|sign in|login|log-in|log in)(?: attempt| request)?` +
	`)\b`)

// signInAction asks the reader to sign in by a link, or names a sign-in
// link; singleUse says the link works once or for a short while, which
// a link to a sign-in page, or one an email mentions, does not.
var (
	signInAction = regexp.MustCompile(`(?i)\b(?:` +
		`magic (?:sign-in |sign in |login |log-in |log in )?link|(?:sign-in|login|log-in) link` +
		`|(?:click|tap|use|follow|press|select|open) (?:on )?(?:the|this) (?:link|button)(?: below| above)? to (?:securely )?(?:sign|log) ?(?:in|into|on)` +
		`|(?:sign|log) ?in(?: to \S+)? (?:with|using) (?:this|the) (?:link|button)` +
		`|(?:sign|log) ?in to \S+(?: \S+)? \[link` +
		`|sign ?in.{0,80}?, (?:click|tap|follow) (?:on )?(?:this|the) link` +
		`)\b`)
	singleUse = regexp.MustCompile(`(?i)\b(?:(?:link|button) (?:will |is only |is )?(?:expire|expires|valid|works once|can only be used)` +
		`|expires? (?:in|after) \d+ (?:minutes?|hours?)|valid for (?:the next )?\d+ (?:minutes?|hours?)|can only be used once|single-use|one-time` +
		`|(?:did not|didn't) request this (?:link|email))`)
)

// temporaryPassword is a password given in the email.
var temporaryPassword = regexp.MustCompile(`(?i)\b(?:temporary|one-time|one time) password(?: is)?:? \S*[0-9!@#$%^&*]\S*`)

// alreadyDone is a subject that says a change was made, which an
// email that makes one does not; unless it names a request (requested),
// which may have been received and still be waiting.
var (
	alreadyDone = regexp.MustCompile(`(?i)\b(?:was|were|has been|have been|successful|successfully|complete|completed|changed|updated|confirmed|verified)\b`)
	requested   = regexp.MustCompile(`(?i)\brequest\b`)
)

// subjectEnd is what follows a subject's phrase in an email that does
// what the phrase says: the subject's end, punctuation, or a word of
// the email's purpose. A person's subject goes on about the phrase
// ("password reset flow redesign", "verify your email preferences").
const subjectEnd = `(?:$|[.!?:;,|)\]]| -| \|| \[| \(| (?:for|now|to|and|at|with|on|request|instructions|link|code|email|required)\b)`

// resetSubject is the subject of a password reset or account recovery.
var resetSubject = regexp.MustCompile(`(?i)\b(?:reset (?:your |the )?(?:\S+ ){0,3}password|password reset|forgot(?:ten)? (?:your )?password|password recovery|account recovery` +
	`|recover (?:your )?(?:\S+ ){0,3}(?:account|password)|set (?:up )?your (?:\S+ ){0,3}password|create your (?:\S+ ){0,3}password)` + subjectEnd)

// confirmSubject is the subject of an email address's or a new
// account's confirmation; moneyAccount an account that is a bank's or
// a payment service's, which is confirmed to be paid, not to sign in.
var (
	confirmSubject = regexp.MustCompile(`(?i)\b(?:(?:confirm|verify|validate|activate) (?:your |the )?(?:new )?(?:email|e-mail)(?: address)?` +
		`|(?:email|e-mail)(?: address)? (?:verification|confirmation|validation)` +
		`|(?:confirm|verify|validate|activate) (?:your |the )?(?:new )?(?:\S+ ){0,3}account` +
		`|(?:confirm|verify) your (?:registration|sign-up|signup|sign up))` + subjectEnd)
	moneyAccount = regexp.MustCompile(`(?i)\b(?:bank|payment|billing|payout|business|tax)\b`)
)

// confirmsAccount reports whether subject asks to confirm an email
// address or a new account.
func confirmsAccount(subject string) bool {
	m := confirmSubject.FindString(subject)
	return m != "" && !moneyAccount.MatchString(m)
}

// secretRemoved is what a secret is replaced with.
const secretRemoved = "[secret removed by Kivali]"

// secretPatterns are secrets in formats nothing else takes: a private
// key, and the keys and tokens of services agents commonly reach. A
// private key cut off before its end is removed to the end of the text,
// and a key or token cut off where the text was clipped as far as it goes.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN[A-Z0-9 ]* PRIVATE KEY(?: BLOCK)?-----(?s:.*?)(?:-----END[A-Z0-9 ]* PRIVATE KEY(?: BLOCK)?-----|\z)`),
	regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,255}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,255}`),
	regexp.MustCompile(`\b[rs]k_live_[A-Za-z0-9]{10,247}\b`),
	regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,250}`),
	regexp.MustCompile(`\bsk-(?:ant|proj)-[A-Za-z0-9_-]{20,300}`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`),
	// A key or token cut short where Kivali or Gmail clipped the text.
	regexp.MustCompile(`\b(?:gh[pousr]_|github_pat_|[rs]k_live_|xox[abposr]-|sk-(?:ant|proj)-|AIza|AKIA|ASIA)[A-Za-z0-9_-]{8,}(?:\.\.\.|…|\z)`),
}

// redactSecrets is s with each secret replaced by secretRemoved, and
// how many it replaced.
func redactSecrets(s string) (string, int) {
	n := 0
	for _, p := range secretPatterns {
		s = p.ReplaceAllStringFunc(s, func(string) string {
			n++
			return secretRemoved
		})
	}
	return s, n
}

// redactSecrets replaces the secrets in each URL of the list, answering
// how many it replaced.
func (l *linkList) redactSecrets() int {
	n := 0
	for i, u := range l.urls {
		var k int
		l.urls[i], k = redactSecrets(u)
		n += k
	}
	return n
}
