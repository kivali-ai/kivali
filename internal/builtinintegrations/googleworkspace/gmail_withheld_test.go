package googleworkspace

import (
	"io"
	"strings"
	"testing"
	"time"
)

// withheldCase is a message as accessKind reads it: its subject, its
// body as Kivali renders it (links as markers), and what it holds.
type withheldCase struct {
	name, subject, body string
	labels              []string
	inReplyTo           string
	want                string
}

func (wc withheldCase) message() gmailMessage {
	hs := []gmailHeader{{"From", "Acme <no-reply@acme.example>"}, {"Subject", wc.subject}}
	if wc.inReplyTo != "" {
		hs = append(hs, gmailHeader{"In-Reply-To", wc.inReplyTo})
	}
	return gmailMessage{ID: "m1", LabelIDs: wc.labels, Payload: gmailPart{Headers: hs}}
}

// Why this test exists: an email that hands over access to an account
// (a code, a sign-in link, a password reset, an email confirmation) is
// what an agent misled by another email would pass on or use in its
// browser to sign in as the owner, so each of the shapes these emails
// come in must be withheld. And withholding hides mail the agent may
// need, so the emails that read most like them and carry no access (a
// security alert, a meeting's passcode, a booking's confirmation code,
// a promotion's code, a card receipt's authorization code, a phone
// number, a newsletter about magic links, a footer, a person's own
// thread about the same words) must be shown.
func TestAccessKind(t *testing.T) {
	cases := []withheldCase{
		{name: "verification code", subject: "Your verification code", body: "Your verification code is 482913. It expires in 10 minutes.", want: accessCode},
		{name: "code first in the subject", subject: "482913 is your Acme code", want: accessCode},
		{name: "code on its own line", subject: "Account security code", body: "Please use the following security code for your account.\n\nSecurity code: 4539\n\nThanks, the account team", want: accessCode},
		{name: "prefixed code", subject: "Sign in", body: "G-482913 is your verification code.", want: accessCode},
		{name: "confirmation code of digits", subject: "Acme confirmation code: 482-913", body: "Your confirmation code is below.\n\n482-913", want: accessCode},
		{name: "spaced code", subject: "Your login code", body: "Your login code: 482 913", want: accessCode},
		{name: "recovery code", subject: "Backup codes", body: "Keep this backup code somewhere safe: a1b2c-3d4e5", want: accessCode},
		{name: "device code", subject: "Device sign-in", body: "Enter this code on your device: BDWP-HQ84", want: accessCode},
		{name: "one-time password", subject: "Your OTP", body: "Your one-time password is 90817364.", want: accessCode},
		{name: "magic link", subject: "Your sign-in link", body: "Click the button below to sign in to Acme.\n[link 1]", want: accessLink},
		{name: "sign-in link that expires", subject: "Sign in to Acme", body: "Click the link below to sign in. [link 1]\nThis link will expire in 24 hours and can only be used once.", want: accessLink},
		{name: "sign-in approval", subject: "Is this you?", body: "Someone is signing in to your account. Approve this sign-in: [link 1]", want: accessLink},
		{name: "password reset", subject: "Reset your Acme password", body: "Someone asked to reset the password for your account.\n[link 1]", want: accessReset},
		{name: "password reset with a tag", subject: "[Acme] Please reset your password", body: "[link 1]", want: accessReset},
		{name: "temporary password", subject: "Welcome to Acme", body: "Your temporary password is: Xk9!mPq2", want: accessReset},
		{name: "set your password", subject: "Set up your password", body: "Your administrator made you an account. [link 1]", want: accessReset},
		{name: "email confirmation", subject: "Please confirm your email address", body: "Confirm your address to finish signing up: [link 1]", want: accessConfirm},
		{name: "account activation", subject: "Activate your account", body: "[link 1]", want: accessConfirm},
		{name: "email confirmation with a code", subject: "Verify your email", body: "Enter 739104 to verify your email.", want: accessConfirm},
		{name: "letter confirmation code to sign in", subject: "Acme confirmation code: QJS-PNT", body: "Your confirmation code is below. Enter it in your open browser window and we'll help you get signed in.\n\nQJS-PNT", want: accessCode},
		{name: "named code not to share", subject: "Your code", body: "Your Acme Account code is: 482913. Don't share it with anyone.", want: accessCode},
		{name: "hyphenated named code not to share", subject: "Acme", body: "Your Acme code 482-913. Don't share this code with others.", want: accessCode},
		{name: "code far below a code subject", subject: "Your Acme verification code", body: "Hi Dana, Someone tried to sign in to your Acme account from a new browser in Lisbon, using the address you gave us when you made the account. To finish signing in, enter this code: 482913", want: accessCode},
		{name: "parenthesised 2FA", subject: "Sign in", body: "Your two-factor authentication (2FA) code is 482913", want: accessCode},
		{name: "two-word brand reset", subject: "Reset your Acme Cloud account password", body: "[link 1]", want: accessReset},
		{name: "two-word brand activation", subject: "Activate your Acme Cloud account", body: "[link 1]", want: accessConfirm},
		{name: "reset request received", subject: "Your password reset request has been received", body: "Use this link to choose a new password: [link 1]", want: accessReset},
		{name: "reset subject with a bar", subject: "Reset your password | Acme", body: "[link 1]", want: accessReset},
		{name: "email-link sign-in", subject: "Sign in to Acme", body: "Hello, We received a request to sign in to Acme using this email address. If you want to sign in with your dana@example.com account, click this link: [link 1] If you did not request this link, you can safely ignore this email.", want: accessLink},
		{name: "button-only magic link", subject: "Sign in", body: "Sign in to Acme [link 1] This link expires in 24 hours.", want: accessLink},

		{name: "security alert", subject: "New sign-in to your account", body: "We noticed a new sign-in to your Acme account on a Windows device. If this was you, you don't need to do anything. If not, we'll help you secure your account.\n[link 1] Check activity"},
		{name: "password changed alert", subject: "Your password was changed", body: "The password for your account was changed. If you didn't change it, reset your password: [link 1]"},
		{name: "password reset done", subject: "Password reset successful", body: "You can now sign in with your new password. [link 1]"},
		{name: "email verified", subject: "Your email has been verified", body: "Thanks for confirming your email address. [link 1]"},
		{name: "two-step turned on", subject: "2-Step Verification turned on", body: "2-Step Verification is now on for your account. From now on, you'll get a verification code when you sign in. © 2026 Acme"},
		{name: "announcement with a year", subject: "Changes to sign-in", body: "Starting March 2026, verification codes will be sent by text message."},
		{name: "card security code", subject: "Update your payment details", body: "Enter the 3-digit security code on the back of your card. [link 1]"},
		{name: "meeting passcode", subject: "Invitation: Weekly sync", body: "Join the meeting [link 1]\nMeeting ID: 812 3456 7890\nPasscode: 482913"},
		{name: "booking confirmation code", subject: "Your booking", body: "Thanks for booking. Confirmation code: QX7R2B"},
		{name: "promotion code", subject: "20% off this week", body: "Use code SAVE2026 at checkout. [link 1]"},
		{name: "footer", subject: "Weekly digest", body: "Here is what happened this week. [link 1]\nForgot your password? Reset it anytime in settings. [link 2]"},
		{name: "notification sign-in", subject: "You have 3 new messages", body: "Click here to sign in and read them. [link 1]"},
		{name: "report sign-in", subject: "Your weekly summary", body: "Click the button below to sign in and see your report. [link 1]"},
		{name: "a person's thread", subject: "Password reset flow redesign", body: "Here are the mockups for the new flow: [link 1]"},
		{name: "a person's question", subject: "Verify your email preferences", body: "Can you check these before Monday? [link 1]"},
		{name: "reply", subject: "Re: Your verification code", body: "I got 482913, is that right?"},
		{name: "forward", subject: "Fwd: Reset your password", body: "Can you look at this? [link 1]"},
		{name: "reply without Re", subject: "Your verification code", body: "Your verification code is 482913.", inReplyTo: "<a@acme.example>"},
		{name: "sent", subject: "Your verification code", body: "Your verification code is 482913.", labels: []string{"SENT"}},
		{name: "newsletter on magic links", subject: "Product update", body: "Why magic links are replacing passwords: read our new post. [link 1]"},
		{name: "a person's login link", subject: "Staging access", body: "Here's the login link for the staging server: [link 1]"},
		{name: "card receipt", subject: "Your receipt", body: "Visa 4242\nAuth code: 012345\nTotal $4.50 [link 1]"},
		{name: "event registration code", subject: "See you Thursday", body: "Your registration code: 7F3K9Q. Show it at the door."},
		{name: "hash name", subject: "Changelog", body: "The verification code path now uses sha256 for hashing."},
		{name: "time", subject: "About your sign-in", body: "Your verification code arrives by 10am tomorrow."},
		{name: "decade", subject: "Blog", body: "Verification codes sent by SMS were introduced in the 1990s and remain weak."},
		{name: "phone number", subject: "Changes to sign-in", body: "Starting March 2026, verification codes will be sent by text message to (415) 555-0100."},
		{name: "phone ending", subject: "Security alert", body: "A one-time passcode was sent to the phone number ending in 1234."},
		{name: "card ending", subject: "Update your card", body: "Please update the security code (CVV) for your Visa ending in 4242. [link 1]"},
		{name: "bank account verification", subject: "Verify your bank account", body: "We sent two small deposits. [link 1]"},
	}
	for _, wc := range cases {
		if got := accessKind(wc.message(), wc.body); got != wc.want {
			t.Errorf("%s: accessKind = %q, want %q", wc.name, got, wc.want)
		}
	}
}

// Why this test exists: Gmail's snippet is all a search shows of a
// message's body, and a message with no text part is judged by it, so
// a code in the snippet, with HTML's characters escaped as Gmail
// escapes them, must withhold the message.
func TestAccessKindReadsTheSnippet(t *testing.T) {
	m := withheldCase{subject: "Your code"}.message()
	m.Snippet = "Your verification code is 482913 &amp; it expires soon"
	if got := accessKind(m, ""); got != accessCode {
		t.Errorf("accessKind = %q", got)
	}
}

// Why this test exists: an email may carry a key or a token pasted
// into it, which an agent misled by the email could pass on; each
// format Kivali knows must be removed, and text that only looks like
// one (a test key, a public key, a short or misspelt token, the word
// AKIA in prose) must be left as it is.
func TestRedactSecrets(t *testing.T) {
	// A private key cut off before its end is removed to the text's end.
	if got, n := redactSecrets("before -----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmU after"); got != "before "+secretRemoved || n != 1 {
		t.Errorf("a cut-off private key: %q, %d", got, n)
	}
	secrets := []string{
		"-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn\n-----END RSA PRIVATE KEY-----",
		"AKIAIOSFODNN7EXAMPLE",
		"ghp_" + strings.Repeat("a1B2", 9),
		"github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz",
		"sk_live_" + strings.Repeat("x9", 12),
		"xoxb-1234567890-abcdefghij",
		"sk-ant-api03-" + strings.Repeat("Ab3_", 8),
		"sk-proj-" + strings.Repeat("Zz9-", 8),
		"AIza" + strings.Repeat("Sy0_", 8) + "abc",
	}
	for _, s := range secrets {
		got, n := redactSecrets("before " + s + " after")
		if got != "before "+secretRemoved+" after" || n != 1 {
			t.Errorf("redactSecrets(%q) = %q, %d", s, got, n)
		}
	}
	kept := []string{
		"-----BEGIN PUBLIC KEY-----\nMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8A\n-----END PUBLIC KEY-----",
		"sk_test_" + strings.Repeat("x9", 12),
		"ghp_short",
		"Rotate the AKIA keys before Friday.",
		"Our sk-ant- prefix docs.",
	}
	// A token cut short where the text was clipped ("..." after it, or
	// the text's end) is removed too: its first part may be all that is
	// needed, and the rest of a header was cut away.
	for _, s := range []string{"ghp_a1B2a1B2a1B2a1B2...", "token ghp_a1B2a1B2a1B2"} {
		if got, n := redactSecrets(s); strings.Contains(got, "ghp_") || n != 1 {
			t.Errorf("redactSecrets(%q) = %q, %d", s, got, n)
		}
	}
	for _, s := range kept {
		if got, n := redactSecrets(s); got != s || n != 0 {
			t.Errorf("redactSecrets(%q) = %q, %d", s, got, n)
		}
	}
}

// withheldMail is the account's mail for the withholding tests: a
// sign-in code with an attachment, an ordinary message, and a message
// carrying a key in its text and in a link, all in one thread.
func withheldMail(f *fakeGoogle) {
	withMail(f,
		mailMsg{id: "w1", thread: "tw", labels: []string{"INBOX"}, at: monday, from: "Acme <no-reply@acme.example>", to: "dana@example.com",
			subject: "482913 is your Acme code", snippet: "Your verification code is 482913", auth: gmailAuth,
			plain: "Your verification code is 482913. It expires in 10 minutes.",
			files: []mailFile{{name: "code.png", mimeType: "image/png", data: []byte("png")}}},
		mailMsg{id: "w2", thread: "tw", labels: []string{"INBOX"}, at: monday.Add(-time.Hour), from: "Alice <alice@example.org>", to: "dana@example.com",
			subject: "Lunch", snippet: "Tuesday?", auth: gmailAuth, plain: "Tuesday?"},
		mailMsg{id: "w3", thread: "tw", labels: []string{"INBOX"}, at: monday.Add(-2 * time.Hour), from: "Carol <carol@example.net>", to: "dana@example.com",
			subject: "Deploy", snippet: "Here is the token", plain: "Here is the token: ghp_" + strings.Repeat("a1B2", 9) + "\nDashboard: https://maps.example.org/embed?key=AIza" + strings.Repeat("Sy0_", 8) + "abc"},
	)
}

// Why this test exists: a withheld message must not reach the agent by
// any read: a search lists its subject and snippet, where a code
// usually is, get_message and get_thread its body. Each must show the
// message by Kivali's own lines and say why it is withheld, so the
// agent knows it is there and can ask the owner, while the other
// messages read as ever; the audit names what was withheld, and the
// withheld sender, whose text the agent never read, is not in
// Result.Outside.
func TestWithheldMessageReads(t *testing.T) {
	f, g := newFakeGoogle(t)
	withheldMail(f)
	caller := agent(map[string]any{})
	line := withheldLine("w1", accessCode)

	res := gmailAnswer(t, g, caller, "search_messages", `{"query":""}`)
	if got := res.Content[0]; !strings.Contains(got, line) || strings.Contains(got, "482913") || !strings.Contains(got, "Subject: Lunch") {
		t.Errorf("search:\n%s", got)
	}
	if res.Audit != "searched mail, listing 3 messages: w1, w2, w3, withholding 1: w1" || strings.Contains(strings.Join(res.Outside, ","), "acme.example") {
		t.Errorf("search audit %q, outside %v", res.Audit, res.Outside)
	}

	res = gmailAnswer(t, g, caller, "get_message", `{"id":"w1"}`)
	want := "Message w1 in thread tw, received 2026-10-26 09:00 UTC. Labels: Inbox (INBOX).\n" +
		"Sender: no-reply@acme.example, from outside your domain. Gmail's sender checks: SPF pass, DKIM pass, DMARC pass.\n" + line
	if res.Content[0] != want || res.Audit != "withheld message w1: it carries "+accessCode || len(res.Outside) != 0 {
		t.Errorf("get_message:\n%s\naudit %q, outside %v", res.Content[0], res.Audit, res.Outside)
	}

	res = gmailAnswer(t, g, caller, "get_thread", `{"id":"tw"}`)
	if got := res.Content[0]; !strings.Contains(got, line) || strings.Contains(got, "482913") || !strings.Contains(got, "Tuesday?") {
		t.Errorf("get_thread:\n%s", got)
	}
	if !strings.HasSuffix(res.Audit, ", withholding 1: w1") {
		t.Errorf("get_thread audit %q", res.Audit)
	}
}

// Why this test exists: a search shows only a message's subject and
// snippet, but must judge it by its body, or a code below an innocent
// subject and opening would be withheld by get_message yet show in the
// snippet of a later search page. And since a search now reads bodies,
// a message whose body does not decode must not fail the whole page: it
// is judged by its snippet and listed as before.
func TestSearchJudgesTheBody(t *testing.T) {
	f, g := newFakeGoogle(t)
	withMail(f, mailMsg{id: "b1", thread: "b1", labels: []string{"INBOX"}, at: monday, from: "Acme <no-reply@acme.example>", to: "dana@example.com",
		subject: "Account update", snippet: "Hi Dana, here is the update you asked for.",
		plain: "Hi Dana, here is the update you asked for.\n\nYour verification code is 482913."})
	broken := mailMsg{id: "b2", thread: "b2", labels: []string{"INBOX"}, at: monday.Add(-time.Hour), from: "Alice <alice@example.org>", to: "dana@example.com",
		subject: "Lunch", snippet: "Tuesday?", plain: "Tuesday?"}.full(f)
	broken.Payload.Body.Data = "%%% not base64 %%%"
	f.mail = append(f.mail, broken)

	res := gmailAnswer(t, g, agent(map[string]any{}), "search_messages", `{"query":""}`)
	if got := res.Content[0]; !strings.Contains(got, withheldLine("b1", accessCode)) || strings.Contains(got, "update you asked") || !strings.Contains(got, "Snippet: Tuesday?") {
		t.Errorf("search:\n%s", got)
	}
}

// Why this test exists: what an agent may do with a message must not
// reach a withheld one either: its attachments may hold its code, a
// reply's draft carries its subject, and archiving it or marking it
// read would hide a password reset from the owner. Each is refused
// saying why.
func TestWithheldMessageActions(t *testing.T) {
	f, g := newFakeGoogle(t)
	withheldMail(f)
	caller := agent(map[string]any{})
	caller.Save = func(string, string, io.Reader, int64) (string, error) {
		t.Error("an attachment of a withheld message was saved")
		return "", nil
	}
	refusedWith(t, g, caller, "get_attachment", `{"message_id":"w1","attachment_id":"1"}`,
		"Kivali withheld message w1: it carries "+accessCode+", so you may not save its attachments. "+withheldNever+" If your work needs it, ask the owner.")

	f, _, g = newFakeGmail(t)
	withheldMail(f)
	writer := agent(map[string]any{"gmail_write": true})
	writeRefused(t, g, writer, "modify_labels", `{"message_id":"w1","remove":["INBOX"]}`,
		"Kivali withheld message w1: it carries "+accessCode+", so you may not change its labels. "+withheldNever+" If your work needs it, ask the owner.")
	writeRefused(t, g, writer, "create_draft", `{"in_reply_to_message_id":"w1","body":"thanks"}`,
		"Kivali withheld message w1: it carries "+accessCode+", so you may not reply to it. "+withheldNever+" If your work needs it, ask the owner.")
	writeRefused(t, g, writer, "create_draft", `{"thread_id":"tw","body":"thanks"}`,
		"Kivali withheld message w1: it carries "+accessCode+", so you may not reply to it. "+withheldNever+" If your work needs it, ask the owner.")
	for _, r := range f.requests() {
		if strings.Contains(r, "POST") {
			t.Errorf("a refused action changed Gmail: %s", r)
		}
	}
}

// Why this test exists: a key pasted into an ordinary email must be
// removed wherever the agent would read it, the text and the link
// listing alike, with a note saying so, while the rest of the email
// reads as ever.
func TestSecretsRemovedFromMail(t *testing.T) {
	f, g := newFakeGoogle(t)
	withheldMail(f)
	got := gmailAnswer(t, g, agent(map[string]any{}), "get_message", `{"id":"w3"}`).Content[0]
	if strings.Contains(got, "ghp_") || strings.Contains(got, "AIza") || strings.Count(got, secretRemoved) < 3 ||
		!strings.Contains(got, "Here is the token: "+secretRemoved) || !strings.Contains(got, "maps.example.org") || !strings.Contains(got, secretsNote(2)) {
		t.Errorf("get_message:\n%s", got)
	}
}
