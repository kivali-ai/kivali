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
// "" when Kivali shows it.
func (c *call) withholding(ctx context.Context, m gmailMessage) (string, error) {
	b, err := c.body(ctx, m, newLinkList())
	if err != nil {
		return "", err
	}
	return accessKind(m, b.text), nil
}

// conversationSubject is a subject a reply or a forward starts with, in
// the languages mail readers commonly write one.
var conversationSubject = regexp.MustCompile(`^(?:re|fw|fwd|aw|wg|sv|vs|rv|tr|antw|odp|vb|r|i)\s*(?:\[\d+\]|\(\d+\))?\s*:`)

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
	case hasCode(text):
		return accessCode
	case linked && (signInLink.MatchString(text) || signInAction.MatchString(text) && singleUse.MatchString(text)):
		return accessLink
	case temporaryPassword.MatchString(text):
		return accessReset
	case alreadyDone.MatchString(subject):
		return ""
	case resetSubject.MatchString(subject) && (linked || anyCode(text)):
		return accessReset
	case confirmSubject.MatchString(subject) && (linked || anyCode(text)):
		return accessConfirm
	}
	return ""
}

// normalForm is s as accessKind reads it: compatibility-folded (NFKC),
// lower-cased, every dash a hyphen and every run of white space one
// space.
func normalForm(s string) string {
	s = strings.ToLower(norm.NFKC.String(s))
	s = strings.Map(func(r rune) rune {
		switch r {
		case '‐', '‑', '‒', '–', '—', '―', '−':
			return '-'
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// codePhrase names a code that signs in or confirms who someone is.
// Codes for other things, which share some of its words, are left out:
// a meeting's passcode, a booking's confirmation code, a discount code.
var codePhrase = regexp.MustCompile(`\b(?:` +
	`(?:verification|security|sign-?in|sign in|log-?in|log in|one-time|one time|single-use|authentication|auth|2fa|two-factor|two factor|2-step|two-step|2-factor|mfa|multi-factor|recovery|backup|reset|device|registration|identity)` +
	`(?: verification| security| sign-in| login)? (?:code|codes|passcode|pass code|pin)` +
	`|one-time pass(?:code|word)|one time pass(?:code|word)|otp` +
	`|(?:enter|use|type) (?:this|the following|the) code (?:to (?:sign|log) ?in|to verify|on your (?:device|tv|screen))` +
	`)\b`)

// confirmationCode is a confirmation code, which signs in when the
// code is digits alone and is a booking's when it holds letters.
var confirmationCode = regexp.MustCompile(`\bconfirmation code\b`)

// codeBefore is a code a phrase follows ("482913 is your Acme code").
var codeBefore = regexp.MustCompile(`\b(\d{4,10}) is your (?:\S+ ){0,3}(?:code|passcode|pin)\b`)

// codeToken is a candidate code: letters and digits, hyphens between.
var codeToken = regexp.MustCompile(`[a-z0-9]+(?:-[a-z0-9]+)*`)

// spacedDigits is a code of digits in two groups ("482 913").
var spacedDigits = regexp.MustCompile(`\b\d{3} \d{3,4}\b`)

// The characters after a code phrase, and before it, a code is looked
// for in.
const (
	codeAfter      = 100
	codeLookBehind = 40
)

// hasCode reports whether text holds a code beside a phrase that names
// it: within codeAfter characters after one, or codeLookBehind before.
func hasCode(text string) bool {
	if codeBefore.MatchString(text) {
		return true
	}
	near := func(loc []int, digitsOnly bool) bool {
		after := text[loc[1]:min(len(text), loc[1]+codeAfter)]
		before := text[max(0, loc[0]-codeLookBehind):loc[0]]
		return codeIn(after, digitsOnly) || codeIn(before, digitsOnly)
	}
	for _, loc := range codePhrase.FindAllStringIndex(text, -1) {
		if near(loc, false) {
			return true
		}
	}
	for _, loc := range confirmationCode.FindAllStringIndex(text, -1) {
		if near(loc, true) {
			return true
		}
	}
	return false
}

// anyCode reports whether text holds a code anywhere.
func anyCode(text string) bool { return codeIn(text, false) }

// codeIn reports whether s holds a code: digits alone, or with
// digitsOnly false letters and digits too.
func codeIn(s string, digitsOnly bool) bool {
	if spacedDigits.MatchString(s) {
		return true
	}
	for _, t := range codeToken.FindAllString(s, -1) {
		if isCode(strings.ReplaceAll(t, "-", ""), digitsOnly) {
			return true
		}
	}
	return false
}

// isCode reports whether t, a token without its hyphens, reads as a
// code: 4 to 10 digits that are not a year (1900 to 2099), or with
// digitsOnly false, 4 to 16 letters and digits holding at least two
// digits ("abcd1234", a recovery code "a1b2c3d4e5").
func isCode(t string, digitsOnly bool) bool {
	digits := 0
	for _, r := range t {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	if digits == len(t) {
		if len(t) == 4 {
			if y, _ := strconv.Atoi(t); y >= 1900 && y <= 2099 {
				return false
			}
		}
		return len(t) >= 4 && len(t) <= 10
	}
	return !digitsOnly && digits >= 2 && len(t) >= 4 && len(t) <= 16
}

// signInLink names a link that signs in.
var signInLink = regexp.MustCompile(`\b(?:magic (?:sign-in |sign in |login |log-in |log in )?link|(?:sign-in|login|log-in) link` +
	`|(?:approve|confirm) (?:this|the|your) (?:sign-in|sign in|login|log-in|log in)(?: attempt| request)?\b)`)

// signInAction asks the reader to sign in by a link; singleUse says the
// link works once or for a short while, which a link to a sign-in page
// does not.
var (
	signInAction = regexp.MustCompile(`\b(?:(?:click|tap|use|follow|press|select|open) (?:on )?(?:the|this) (?:link|button)(?: below| above)? to (?:securely )?(?:sign|log) ?(?:in|into|on)\b` +
		`|(?:sign|log) ?in(?: to \S+)? (?:with|using) (?:this|the) (?:link|button)\b)`)
	singleUse = regexp.MustCompile(`\b(?:(?:link|button) (?:will |is only |is )?(?:expire|expires|valid|works once|can only be used)` +
		`|expires? (?:in|after) \d+ (?:minutes?|hours?)|valid for (?:the next )?\d+ (?:minutes?|hours?)|can only be used once|single-use|one-time)`)
)

// temporaryPassword is a password given in the email.
var temporaryPassword = regexp.MustCompile(`\b(?:temporary|one-time|one time) password(?: is)?:? \S*[0-9!@#$%^&*]\S*`)

// alreadyDone is a subject that says a change was made, which an
// email that makes one does not.
var alreadyDone = regexp.MustCompile(`\b(?:was|were|has been|have been|successful|successfully|complete|completed|changed|updated|confirmed|verified)\b`)

// subjectEnd is what follows a subject's phrase in an email that does
// what the phrase says: the subject's end, punctuation, or a word of
// the email's purpose. A person's subject goes on about the phrase
// ("password reset flow redesign", "verify your email preferences").
const subjectEnd = `(?:$|[.!?:;,)\]]| -| \[| \(|( (?:for|now|to|and|request|instructions|link|code|email|required)\b))`

// resetSubject is the subject of a password reset or account recovery.
var resetSubject = regexp.MustCompile(`\b(?:reset (?:your |the )?(?:\S+ )?password|password reset|forgot(?:ten)? (?:your )?password|password recovery|account recovery` +
	`|recover (?:your )?(?:\S+ )?(?:account|password)|set (?:up )?your (?:\S+ )?password|create your (?:\S+ )?password)` + subjectEnd)

// confirmSubject is the subject of an email address's or a new
// account's confirmation.
var confirmSubject = regexp.MustCompile(`\b(?:(?:confirm|verify|validate|activate) (?:your |the )?(?:new )?(?:email|e-mail)(?: address)?` +
	`|(?:email|e-mail)(?: address)? (?:verification|confirmation|validation)` +
	`|(?:confirm|verify|validate|activate) (?:your |the )?(?:new )?(?:\S+ )?account` +
	`|(?:confirm|verify) your (?:registration|sign-up|signup|sign up))` + subjectEnd)

// secretRemoved is what a secret is replaced with.
const secretRemoved = "[secret removed by Kivali]"

// secretPatterns are secrets in formats nothing else takes: a private
// key, and the keys and tokens of services agents commonly reach. A
// private key cut off before its end is removed to the end of the text.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN[A-Z0-9 ]* PRIVATE KEY(?: BLOCK)?-----(?s:.*?)(?:-----END[A-Z0-9 ]* PRIVATE KEY(?: BLOCK)?-----|\z)`),
	regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,255}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,255}`),
	regexp.MustCompile(`\b[rs]k_live_[A-Za-z0-9]{10,247}\b`),
	regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,250}`),
	regexp.MustCompile(`\bsk-(?:ant|proj)-[A-Za-z0-9_-]{20,300}`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`),
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
