package googleworkspace

import "strings"

// testingAdvice follows core's words of a sign-in Google no longer
// accepts. Google ends every sign-in of an External app left in
// Testing seven days after it was made, refusing the renewal with
// invalid_grant, which is also how it refuses a sign-in the owner
// revoked: so it names the likely cause and its fix, not the only one.
const testingAdvice = "If this came about a week after you signed in, the likely cause is the app being in Testing, where Google ends every sign-in after seven days: publish it under Audience in Google's Auth Platform, then sign in again."

// SignInAdvice is Google's likely cause of a refused renewal, for the
// Sign-in requirement: the app left in Testing, for invalid_grant, and
// nothing for any other refusal.
func (g *GoogleWorkspace) SignInAdvice(refusal string) string {
	if strings.Contains(refusal, "invalid_grant") {
		return testingAdvice
	}
	return ""
}
