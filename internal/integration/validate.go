package integration

import (
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/kivali-ai/kivali/internal/connect"
	"github.com/kivali-ai/kivali/internal/debpkg"
)

// Level is how serious a finding is.
type Level string

const (
	// LevelError keeps the bundle from being stored.
	LevelError Level = "error"
	// LevelWarning is shown on the install screen; the bundle is stored.
	LevelWarning Level = "warning"
)

// Finding is one thing the bundle check reports, in words.
type Finding struct {
	Level Level
	Text  string
}

// Findings is everything the check reports, in the order it checked.
type Findings []Finding

// HasErrors reports whether any finding is an error.
func (f Findings) HasErrors() bool {
	return slices.ContainsFunc(f, func(x Finding) bool { return x.Level == LevelError })
}

// Texts is every finding's text, errors first.
func (f Findings) Texts() []string {
	out := make([]string, 0, len(f))
	for _, lvl := range []Level{LevelError, LevelWarning} {
		for _, x := range f {
			if x.Level == lvl {
				out = append(out, x.Text)
			}
		}
	}
	return out
}

// Warnings is every warning's text.
func (f Findings) Warnings() []string {
	out := []string{}
	for _, x := range f {
		if x.Level == LevelWarning {
			out = append(out, x.Text)
		}
	}
	return out
}

func (f *Findings) errorf(format string, a ...any) {
	*f = append(*f, Finding{Level: LevelError, Text: fmt.Sprintf(format, a...)})
}

func (f *Findings) warnf(format string, a ...any) {
	*f = append(*f, Finding{Level: LevelWarning, Text: fmt.Sprintf(format, a...)})
}

// Bundle limits.
const (
	// MaxBundleBytes caps a bundle's files, unpacked, in all.
	MaxBundleBytes = 64 << 20
	// MaxBundleFiles caps how many files a bundle holds.
	MaxBundleFiles = 5000
)

var (
	nameRE      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$|^[a-z0-9]$`)
	configKeyRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)
	toolNameRE  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,60}$`)
	// placeholderRE is one ${...} in an env value.
	placeholderRE = regexp.MustCompile(`\$\{([^}]*)\}`)
)

// ReservedToolPrefixes are the first segments of Kivali's own tool
// names. An integration's tools are named <prefix><tool>, so a prefix
// starting with one of these would make its tools read as Kivali's.
var ReservedToolPrefixes = []string{
	"file", "artifact", "agent", "get", "search", "list", "read", "graph", "assignment",
	"publish", "run", "share", "subagent", "web", "submit", "propose", "integration", "kivali",
}

// ValidName reports whether name is an integration name: lowercase
// letters, digits and hyphens, 1 to 32 characters, starting and ending
// with a letter or digit. It names a directory in the store.
func ValidName(name string) bool { return nameRE.MatchString(name) }

// ToolPrefix is what an integration's tools are named with: its name
// with hyphens as underscores, and an underscore ("hubspot" is
// "hubspot_", "my-crm" is "my_crm_").
func ToolPrefix(name string) string { return strings.ReplaceAll(name, "-", "_") + "_" }

// reservedPrefix is the reserved word name's tool prefix starts with,
// if any.
func reservedPrefix(name string) string {
	first, _, _ := strings.Cut(ToolPrefix(name), "_")
	if slices.Contains(ReservedToolPrefixes, first) {
		return first
	}
	return ""
}

// ValidHost reports whether h is a host pattern the egress proxy can
// match: not empty, not a comment, and with no scheme, path, port,
// user info, query or whitespace. It is the rule of
// store.ValidEgressHost, kept here so this package depends on nothing
// of the store's; a test in internal/store holds the two together.
func ValidHost(h string) bool {
	return h != "" && !strings.HasPrefix(h, "#") && !strings.ContainsAny(h, "/: \t\r\n@?")
}

// BundleFile is one file of a bundle: its path relative to the
// bundle's root (slash-separated, as the zip names it with the top
// folder stripped), its unpacked size and its mode.
type BundleFile struct {
	Path string
	Size int64
	Mode fs.FileMode
}

// Validate checks a manifest and the bundle's files against the rules
// docs/developers/integrations.md lists. An error keeps the bundle
// out of the store; a warning is shown on the install screen.
func Validate(m *Manifest, files []BundleFile) Findings {
	var f Findings
	validateBundle(files, &f)
	if m == nil {
		f.errorf("the bundle has no %s", ManifestName)
		return f
	}
	validateManifest(m, &f)
	validateServer(m, files, &f)
	tools := validateTools(m, &f)
	validateKivali(m, tools, false, &f)
	validatePackages(m, &f)
	return f
}

// validatePackages checks kivali.packages: each a Debian package name
// (debpkg.ValidName, the rule the agent shell's software is held to),
// each once. A name reaches apt-get as an argument.
func validatePackages(m *Manifest, f *Findings) {
	seen := map[string]bool{}
	for _, p := range m.Kivali.Packages {
		switch {
		case !debpkg.ValidName(p):
			f.errorf("kivali.packages: %q is not a Debian package name; give the package's name alone, lowercase, with no version, release or option", p)
		case seen[p]:
			f.errorf("kivali.packages: %s is listed twice", p)
		}
		seen[p] = true
	}
}

// ValidateBuiltin checks a built-in integration's manifest
// (internal/builtinintegrations) by the rules Validate holds a bundle's
// to, except what a built-in has no use for. It has no bundle and no
// server section, and declares no hosts and no packages: it runs inside
// Kivali and reaches the hosts its connection names. The names kivali.verify and
// kivali.access.choices give are passed to its Go functions, so they
// need not name a tool.
func ValidateBuiltin(m *Manifest) Findings {
	var f Findings
	if m == nil {
		f.errorf("the built-in integration has no manifest")
		return f
	}
	validateManifest(m, &f)
	s := m.Server
	if s.Type != "" || s.EntryPoint != "" || s.MCPConfig.Command != "" || len(s.MCPConfig.Args) > 0 || len(s.MCPConfig.Env) > 0 {
		f.errorf("server: a built-in integration has no server section")
	}
	if len(m.Kivali.Hosts) > 0 {
		f.errorf("kivali.hosts: a built-in integration declares no hosts; it reaches the hosts its connection names")
	}
	if len(m.Kivali.Packages) > 0 {
		f.errorf("kivali.packages: a built-in integration runs inside Kivali and installs no software")
	}
	tools := validateTools(m, &f)
	validateKivali(m, tools, true, &f)
	return f
}

// validateManifest checks the manifest's own fields and its connection
// form.
func validateManifest(m *Manifest, f *Findings) {
	switch {
	case m.ManifestVersion == "":
		f.errorf("manifest_version is missing")
	case !strings.HasPrefix(m.ManifestVersion, "0."):
		f.errorf("manifest_version %s is not one Kivali reads; it reads 0.x", m.ManifestVersion)
	}

	switch {
	case m.Name == "":
		f.errorf("name is missing")
	case !ValidName(m.Name):
		f.errorf("name %q must be 1 to 32 lowercase letters, digits and hyphens, starting and ending with a letter or digit", m.Name)
	default:
		if w := reservedPrefix(m.Name); w != "" {
			f.errorf("name %q gives its actions the prefix %s, and Kivali's own actions start with %s_; choose another name", m.Name, ToolPrefix(m.Name), w)
		}
	}

	if m.Version == "" {
		f.errorf("version is missing")
	} else if _, err := ParseVersion(m.Version); err != nil {
		f.errorf("%v", err)
	}

	if strings.TrimSpace(m.DisplayName) == "" {
		f.errorf("display_name is missing")
	}
	if strings.TrimSpace(m.Description) == "" {
		f.warnf("description is empty")
	}
	validateUserConfig(m, f)
}

func validateBundle(files []BundleFile, f *Findings) {
	var total int64
	for _, bf := range files {
		total += bf.Size
		switch {
		case strings.HasPrefix(bf.Path, "/"):
			f.errorf("bundle: %s is an absolute path", bf.Path)
		case bf.Path == ".." || strings.HasPrefix(bf.Path, "../") || strings.Contains(bf.Path, "/../"):
			f.errorf("bundle: %s leaves the bundle", bf.Path)
		}
		switch {
		case bf.Mode&fs.ModeSymlink != 0:
			f.errorf("bundle: %s is a symlink", bf.Path)
		case !bf.Mode.IsRegular():
			f.errorf("bundle: %s is not a regular file", bf.Path)
		}
	}
	if len(files) > MaxBundleFiles {
		f.errorf("bundle: %d files, more than the %d a bundle may hold", len(files), MaxBundleFiles)
	}
	if total > MaxBundleBytes {
		f.errorf("bundle: %d bytes unpacked, more than the %d MiB a bundle may hold", total, MaxBundleBytes>>20)
	}
}

func validateServer(m *Manifest, files []BundleFile, f *Findings) {
	s := m.Server
	if s.Type != "python" {
		f.errorf("server.type: Kivali runs Python integrations; server.type must be python")
	}
	switch {
	case s.EntryPoint == "":
		f.errorf("server.entry_point is missing")
	case !slices.ContainsFunc(files, func(bf BundleFile) bool {
		return bf.Mode.IsRegular() && bf.Path == path.Clean(s.EntryPoint)
	}):
		f.errorf("server.entry_point %s is not a file in the bundle", s.EntryPoint)
	}
	if c := s.MCPConfig.Command; c != "python3" && c != "python" {
		f.errorf("server.mcp_config.command must be python3 or python")
	}
	if len(s.MCPConfig.Args) == 0 {
		f.errorf("server.mcp_config.args is empty; name the entry point")
	}
	signIn := m.Kivali.Connection.SignIn()
	for _, k := range sortedKeys(s.MCPConfig.Env) {
		if signIn && k == m.Kivali.Connection.CredentialKey() {
			f.errorf("server.mcp_config.env.%s: Kivali gives the access token under that name, so the env cannot set it", k)
		}
		for _, ph := range placeholderRE.FindAllStringSubmatch(s.MCPConfig.Env[k], -1) {
			key, ok := strings.CutPrefix(ph[1], "user_config.")
			switch {
			case !ok:
				f.errorf("server.mcp_config.env.%s: ${%s} is not a connection field; only ${user_config.<key>} is replaced", k, ph[1])
			case signIn && key == FieldClientSecret:
				f.errorf("server.mcp_config.env.%s: the client secret is Kivali's; the server asks kivali/credential for the access token", k)
			case !slices.ContainsFunc(ConnectionFields(m), func(u UserConfigField) bool { return u.Key == key }):
				f.errorf("server.mcp_config.env.%s names user_config.%s, which user_config does not declare", k, key)
			}
		}
	}
}

func validateUserConfig(m *Manifest, f *Findings) {
	for _, u := range m.UserConfig {
		at := "user_config." + u.Key
		if !configKeyRE.MatchString(u.Key) {
			f.errorf("%s: the key must be lowercase letters, digits and underscores, starting with a letter", at)
		}
		if u.Key == connect.SecretKeyAccessToken || u.Key == connect.SecretKeyRefreshToken {
			f.errorf("%s: Kivali keeps a sign-in's tokens under that key; choose another", at)
		}
		switch u.Type {
		case "string", "number", "boolean":
		case "":
			f.errorf("%s: type is missing; a connection field is a string, a number or a boolean", at)
		default:
			f.errorf("%s: type %s is not supported; a connection field is a string, a number or a boolean", at, u.Type)
		}
		if strings.TrimSpace(u.Title) == "" {
			f.errorf("%s: title is missing", at)
		}
		for _, name := range u.notBool {
			f.errorf("%s: %s must be true or false", at, name)
		}
	}
}

// validateTools checks the tools and returns their names.
func validateTools(m *Manifest, f *Findings) []string {
	if len(m.Tools) == 0 {
		f.errorf("tools is empty; an integration gives agents at least one action")
		return nil
	}
	names := make([]string, 0, len(m.Tools))
	for i, t := range m.Tools {
		switch {
		case t.Name == "":
			f.errorf("tools[%d] has no name", i)
		case !toolNameRE.MatchString(t.Name):
			f.errorf("tools[%d] (%s): the name must be lowercase letters, digits and underscores, starting with a letter", i, t.Name)
		case slices.Contains(names, t.Name):
			f.errorf("tools[%d] (%s) is listed twice", i, t.Name)
		}
		if strings.TrimSpace(t.Description) == "" {
			f.warnf("tools[%d] (%s) has no description", i, t.Name)
		}
		names = append(names, t.Name)
	}
	return names
}

func validateKivali(m *Manifest, tools []string, builtin bool, f *Findings) {
	k := m.Kivali
	if k.Connection.SignIn() {
		f.errorf("kivali.connection.method: Kivali connects integrations with a pasted token; sign-in connections are not available")
	}
	validateConnection(m, f)
	if k.Connection.Multiple && !builtin {
		f.errorf("kivali.connection.multiple: a bundle's server takes one connection")
	}

	if len(k.Hosts) == 0 && !builtin {
		f.errorf("kivali.hosts is empty")
	}
	for _, h := range k.Hosts {
		switch {
		case h == "*":
			f.errorf("kivali.hosts: * would reach every host; name the hosts")
		case h != strings.ToLower(h):
			f.errorf("kivali.hosts: %s must be lowercase", h)
		case !ValidHost(h):
			f.errorf("kivali.hosts: %q is not a host name; give the host alone, with no scheme, port or path", h)
		}
	}

	if a := k.Access; a != nil {
		s, sf := parseSchema(a.Schema)
		*f = append(*f, sf...)
		for _, key := range sortedKeys(a.Choices) {
			tool := a.Choices[key]
			at := "kivali.access.choices." + key
			p, ok := s.prop(key)
			switch {
			case !ok:
				f.errorf("%s: the access form has no field %s", at, key)
			case p.typ == propBoolean:
				f.errorf("%s: only a text or list field can have its options listed", at)
			case len(p.enum) > 0:
				f.errorf("%s: the field has fixed options (enum); list its options one way, not both", at)
			}
			switch {
			case builtin && !toolNameRE.MatchString(tool):
				f.errorf("%s: %q must be lowercase letters, digits and underscores, starting with a letter", at, tool)
			case !builtin && !slices.Contains(tools, tool):
				f.errorf("%s names %s, which is not in tools", at, tool)
			}
		}
	}

	switch {
	case k.Verify == "":
	case builtin && !toolNameRE.MatchString(k.Verify):
		f.errorf("kivali.verify: %q must be lowercase letters, digits and underscores, starting with a letter", k.Verify)
	case !builtin && !slices.Contains(tools, k.Verify):
		f.errorf("kivali.verify names %s, which is not in tools", k.Verify)
	}
}

// reservedAuthorizationParams are the authorization address's
// parameters core sets itself, which extra_authorization_params may
// not name.
var reservedAuthorizationParams = []string{
	"client_id", "redirect_uri", "response_type", "scope", "state", "code_challenge", "code_challenge_method",
}

// envNameRE is a name an access token may be given under: an
// environment variable's.
var envNameRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// validateConnection checks the kivali.connection section for the
// method it names: a token connection gives no endpoints; a sign-in
// connection (device, browser) gives the endpoints its method needs,
// each an https:// address (plain http only on loopback), names the
// access token for the integration's calls and leaves the client id
// and secret to core.
func validateConnection(m *Manifest, f *Findings) {
	c := m.Kivali.Connection
	endpoints := []struct{ key, value string }{
		{"device_endpoint", c.DeviceEndpoint},
		{"authorization_endpoint", c.AuthorizationEndpoint},
		{"token_endpoint", c.TokenEndpoint},
		{"revocation_endpoint", c.RevocationEndpoint},
	}
	switch c.Method {
	case MethodToken:
		for _, e := range endpoints {
			if e.value != "" {
				f.errorf("kivali.connection.%s: a token connection has no endpoints", e.key)
			}
		}
	case MethodDevice, MethodBrowser:
		need := map[string][]string{
			MethodDevice:  {"device_endpoint", "token_endpoint"},
			MethodBrowser: {"authorization_endpoint", "token_endpoint"},
		}[c.Method]
		for _, e := range endpoints {
			if slices.Contains(need, e.key) && e.value == "" {
				f.errorf("kivali.connection.%s is missing; a %s connection needs it", e.key, c.Method)
			}
		}
		switch key := c.CredentialKey(); {
		case key == "":
			f.errorf("kivali.connection.credential_keys.access_token is missing; name what the access token is given as, such as GH_TOKEN")
		case !envNameRE.MatchString(key):
			f.errorf("kivali.connection.credential_keys.access_token: %q must be capital letters, digits and underscores, starting with a letter", key)
		}
		for _, key := range []string{FieldClientID, FieldClientSecret} {
			if _, ok := m.UserConfig.Field(key); ok {
				f.errorf("user_config.%s: Kivali adds the client id and secret to a sign-in connection's form, so user_config does not declare them", key)
			}
		}
	case "":
		f.errorf("kivali.connection.method is missing; Kivali connects integrations with a pasted token, so it must be token")
	default:
		f.errorf("kivali.connection.method %s is not one Kivali knows; it must be token", c.Method)
	}
	for _, e := range endpoints {
		if e.value != "" && !validEndpoint(e.value) {
			f.errorf("kivali.connection.%s: %q must be an https:// address", e.key, e.value)
		}
	}
	switch c.Client {
	case "", "operator":
	case "kivali":
		f.errorf("kivali.connection.client: a client Kivali ships is not in this version of Kivali; the owner registers the app and the client is operator")
	default:
		f.errorf("kivali.connection.client %s is not one Kivali knows; it is operator", c.Client)
	}
	for _, key := range sortedKeys(c.CredentialKeys) {
		if key != CredentialAccessToken {
			f.errorf("kivali.connection.credential_keys.%s: only access_token can be named", key)
		}
	}
	for i, s := range c.Scopes {
		if strings.TrimSpace(s) == "" {
			f.errorf("kivali.connection.scopes[%d] is empty", i)
		}
	}
	for _, key := range sortedKeys(c.ExtraAuthorizationParams) {
		if slices.Contains(reservedAuthorizationParams, key) {
			f.errorf("kivali.connection.extra_authorization_params.%s: Kivali sets %s itself", key, key)
		}
	}
}

// validEndpoint reports whether u is an absolute https:// address, or
// a plain http:// one on loopback (a vendor run in-process for tests).
func validEndpoint(u string) bool {
	p, err := url.Parse(u)
	if err != nil || p.Host == "" || p.User != nil || p.Fragment != "" {
		return false
	}
	switch p.Scheme {
	case "https":
		return true
	case "http":
		h := p.Hostname()
		return h == "localhost" || h == "127.0.0.1" || h == "::1"
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
