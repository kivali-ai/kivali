package claudeauth

import (
	"bufio"
	"bytes"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// ModelHosts are the exact hostnames the CLI's model calls reach for a
// sign-in: st (what `claude auth status --json` reports), settings
// (the bytes of its settings.json, whose env block holds a cloud
// provider's region or resource) and awsConfig (the bytes of
// $HOME/.aws/config, nil for none, read for an SSO profile's region).
// The egress proxy leaves traffic to them out of an agent's upload
// count.
//
// A host the sign-in needs but whose region or resource cannot be read
// is left out, and missing names it in words: counted traffic is the
// safe side of a wrong guess. Not signed in, or a provider Kivali does
// not know, is no hosts and one missing entry.
//
//   - Anthropic: api.anthropic.com, platform.claude.com, and the host
//     of ANTHROPIC_BASE_URL when set.
//   - Amazon Bedrock: bedrock-runtime and bedrock in AWS_REGION (and in
//     ANTHROPIC_SMALL_FAST_MODEL_AWS_REGION), sts.amazonaws.com and the
//     regional STS, the host of ANTHROPIC_BEDROCK_BASE_URL, and, for an
//     AWS_PROFILE that signs in through IAM Identity Center, its SSO
//     portal and OIDC hosts.
//   - Google Vertex AI: aiplatform in CLOUD_ML_REGION and in each
//     VERTEX_REGION_CLAUDE_* region (aiplatform.googleapis.com for
//     global, aiplatform.<us|eu>.rep.googleapis.com for a
//     multi-region), the host of ANTHROPIC_VERTEX_BASE_URL, and the OAuth,
//     STS and IAM credentials hosts Application Default Credentials
//     refresh through.
//   - Microsoft Foundry: <resource>.services.ai.azure.com (or the host
//     of ANTHROPIC_FOUNDRY_BASE_URL) and login.microsoftonline.com.
func ModelHosts(st Status, settings, awsConfig []byte) (hosts, missing []string) {
	if !st.LoggedIn {
		return nil, []string{"the CLI is not signed in"}
	}
	f, err := readSettings(settings)
	if err != nil {
		return nil, []string{"the settings file: " + err.Error()}
	}
	env := f.strings()
	set := map[string]bool{}
	add := func(h string) {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			set[h] = true
		}
	}
	addURLHost := func(key string) {
		if raw := env[key]; raw != "" {
			if h := urlHost(raw); h != "" {
				add(h)
			} else {
				missing = append(missing, key+" is not a URL")
			}
		}
	}

	switch st.APIProvider {
	case "firstParty", "":
		add("api.anthropic.com")
		add("platform.claude.com")
		addURLHost("ANTHROPIC_BASE_URL")
	case "bedrock":
		region := firstOf(env, "AWS_REGION", "AWS_DEFAULT_REGION")
		if validRegion(region) {
			add("bedrock-runtime." + region + ".amazonaws.com")
			add("bedrock." + region + ".amazonaws.com")
			add("sts." + region + ".amazonaws.com")
		} else {
			missing = append(missing, "the Bedrock region (AWS_REGION)")
		}
		if small := env["ANTHROPIC_SMALL_FAST_MODEL_AWS_REGION"]; validRegion(small) {
			add("bedrock-runtime." + small + ".amazonaws.com")
		}
		add("sts.amazonaws.com")
		addURLHost("ANTHROPIC_BEDROCK_BASE_URL")
		if profile := env["AWS_PROFILE"]; profile != "" {
			if sso, ok := ssoRegion(awsConfig, profile); ok {
				if validRegion(sso) {
					add("portal.sso." + sso + ".amazonaws.com")
					add("oidc." + sso + ".amazonaws.com")
				} else {
					missing = append(missing, "the SSO region of AWS profile "+profile)
				}
			}
		}
	case "vertex":
		regions := []string{}
		if r := env["CLOUD_ML_REGION"]; r != "" {
			regions = append(regions, r)
		} else {
			missing = append(missing, "the Vertex AI region (CLOUD_ML_REGION)")
		}
		for k, r := range env {
			if strings.HasPrefix(k, "VERTEX_REGION_CLAUDE_") && r != "" {
				regions = append(regions, r)
			}
		}
		for _, r := range regions {
			switch {
			case r == "global":
				add("aiplatform.googleapis.com")
			case r == "us" || r == "eu":
				add("aiplatform." + r + ".rep.googleapis.com")
			case validRegion(r):
				add(r + "-aiplatform.googleapis.com")
			default:
				missing = append(missing, "Vertex AI region "+r)
			}
		}
		addURLHost("ANTHROPIC_VERTEX_BASE_URL")
		for _, h := range []string{"oauth2.googleapis.com", "sts.googleapis.com", "iamcredentials.googleapis.com", "www.googleapis.com"} {
			add(h)
		}
	case "foundry":
		switch res := env["ANTHROPIC_FOUNDRY_RESOURCE"]; {
		case res != "" && resourceName.MatchString(res):
			add(res + ".services.ai.azure.com")
		case env["ANTHROPIC_FOUNDRY_BASE_URL"] != "":
			addURLHost("ANTHROPIC_FOUNDRY_BASE_URL")
		default:
			missing = append(missing, "the Foundry resource (ANTHROPIC_FOUNDRY_RESOURCE)")
		}
		add("login.microsoftonline.com")
	default:
		return nil, []string{"the hosts of model provider " + st.APIProvider}
	}

	hosts = make([]string, 0, len(set))
	for h := range set {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	sort.Strings(missing)
	return hosts, missing
}

// regionPattern is a cloud region's shape: lowercase letters, digits
// and hyphens ("us-east-1", "europe-west4").
var regionPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func validRegion(r string) bool { return r != "" && len(r) <= 32 && regionPattern.MatchString(r) }

func firstOf(env map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := env[k]; v != "" {
			return v
		}
	}
	return ""
}

// urlHost is the hostname of raw, lowercased; empty when raw is not an
// absolute URL.
func urlHost(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// ssoRegion is the IAM Identity Center region profile signs in through,
// from an AWS config file: the profile's own sso_region, or that of the
// sso-session it names. ok is false when the profile does not use SSO.
func ssoRegion(config []byte, profile string) (region string, ok bool) {
	sections := parseINI(config)
	name := "profile " + profile
	if profile == "default" {
		name = "default"
	}
	p, found := sections[name]
	if !found {
		return "", false
	}
	if r := p["sso_region"]; r != "" {
		return r, true
	}
	if s := p["sso_session"]; s != "" {
		return sections["sso-session "+s]["sso_region"], true
	}
	if p["sso_start_url"] != "" || p["sso_account_id"] != "" {
		return "", true
	}
	return "", false
}

// parseINI reads an AWS config file: "[section]" headers and
// "key = value" lines; comments ("#", ";") and anything else ignored.
func parseINI(b []byte) map[string]map[string]string {
	out := map[string]map[string]string{}
	var cur map[string]string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			name := strings.Join(strings.Fields(line[1:len(line)-1]), " ")
			cur = map[string]string{}
			out[name] = cur
		case cur != nil:
			if k, v, ok := strings.Cut(line, "="); ok {
				cur[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}
	return out
}
