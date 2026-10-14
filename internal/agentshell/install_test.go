package agentshell

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeAptScript is a stand-in for apt-get. Each run appends its
// arguments, one per line, then a "--" line, to $FAKE_APT_LOG, and its
// DEBIAN_FRONTEND and http_proxy to $FAKE_APT_ENV, and, when fd 3 is
// open in it, a line to $FAKE_APT_FD3 (if set). `update` fails when
// $FAKE_APT_UPDATE_FAILS is set; `install` fails, naming the package,
// when any argument is a name listed in $FAKE_APT_MISSING.
const fakeAptScript = `#!/bin/sh
for a in "$@"; do printf '%s\n' "$a" >> "$FAKE_APT_LOG"; done
echo -- >> "$FAKE_APT_LOG"
echo "DEBIAN_FRONTEND=$DEBIAN_FRONTEND http_proxy=$http_proxy" >> "$FAKE_APT_ENV"
if [ -n "$FAKE_APT_FD3" ] && (true >&3) 2>/dev/null; then echo "fd 3 open in apt-get $1" >> "$FAKE_APT_FD3"; fi
echo "Reading package lists..."
if [ "$1" = update ]; then
  if [ -n "$FAKE_APT_UPDATE_FAILS" ]; then
    echo "W: Failed to fetch http://deb.debian.org/debian/dists/trixie/InRelease  Temporary failure resolving 'deb.debian.org'" >&2
    echo "E: Some index files failed to download. They have been ignored, or old ones used instead." >&2
    exit 100
  fi
  if [ -n "$FAKE_APT_UPDATE_WARNS" ]; then
    echo "W: Failed to fetch http://deb.debian.org/debian/dists/trixie/InRelease  Temporary failure resolving 'deb.debian.org'" >&2
    echo "W: Some index files failed to download. They have been ignored, or old ones used instead." >&2
  fi
  exit 0
fi
if [ -n "$FAKE_APT_LINGER" ]; then
  # A maintainer script's service: left running, holding stderr open
  # unless FAKE_APT_LINGER_QUIET is set.
  if [ -n "$FAKE_APT_LINGER_QUIET" ]; then
    (sleep 300 >/dev/null 2>&1 </dev/null & echo $! > "$FAKE_APT_LINGER")
  else
    (sleep 300 >/dev/null </dev/null & echo $! > "$FAKE_APT_LINGER")
  fi
fi
for a in "$@"; do
  for m in $FAKE_APT_MISSING; do
    if [ "$a" = "$m" ]; then
      echo "E: Unable to locate package $a" >&2
      exit 100
    fi
  done
done
exit 0
`

// fakeAptCacheScript is a stand-in for `apt-cache pkgnames --all-names
// <prefix>`: it prints the known names that start with the prefix.
// The known names are $FAKE_APT_KNOWN when set; otherwise every name
// is known but those in $FAKE_APT_UNKNOWN, and each comes with a
// "<name>-doc" sibling, so only an exact match counts. It logs each
// run's arguments to $FAKE_APT_CACHE_LOG and fails when
// $FAKE_APT_CACHE_FAILS is set.
const fakeAptCacheScript = `#!/bin/sh
echo "$*" >> "$FAKE_APT_CACHE_LOG"
if [ "$1" != pkgnames ] || [ "$2" != --all-names ] || [ $# -ne 3 ]; then
  echo "E: unexpected arguments: $*" >&2
  exit 100
fi
if [ -n "$FAKE_APT_CACHE_FAILS" ]; then
  echo "E: The package cache file is corrupted" >&2
  exit 100
fi
p="$3"
if [ -n "$FAKE_APT_KNOWN" ]; then
  for n in $FAKE_APT_KNOWN; do
    case "$n" in "$p"*) echo "$n" ;; esac
  done
  exit 0
fi
echo "$p-doc"
for u in $FAKE_APT_UNKNOWN; do
  if [ "$u" = "$p" ]; then exit 0; fi
done
echo "$p"
`

type fakeApt struct {
	bin, cache, log, cacheLog, env string
}

func newFakeApt(t *testing.T) fakeApt {
	t.Helper()
	dir := t.TempDir()
	f := fakeApt{
		bin:      filepath.Join(dir, "apt-get"),
		cache:    filepath.Join(dir, "apt-cache"),
		log:      filepath.Join(dir, "argv.log"),
		cacheLog: filepath.Join(dir, "cache.log"),
		env:      filepath.Join(dir, "env.log"),
	}
	if err := os.WriteFile(f.bin, []byte(fakeAptScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.cache, []byte(fakeAptCacheScript), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

// envVars is the environment the fakes need.
func (f fakeApt) envVars() []string {
	return []string{"PATH=/usr/bin:/bin", "FAKE_APT_LOG=" + f.log, "FAKE_APT_ENV=" + f.env, "FAKE_APT_CACHE_LOG=" + f.cacheLog}
}

func (f fakeApt) installer(extraEnv ...string) Installer {
	return Installer{AptGet: f.bin, AptCache: f.cache, Env: append(f.envVars(), extraEnv...)}
}

// lookups returns the names apt-cache was asked about.
func (f fakeApt) lookups(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(f.cacheLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		out = append(out, strings.TrimPrefix(l, "pkgnames --all-names "))
	}
	return out
}

// runs returns each run's arguments.
func (f fakeApt) runs(t *testing.T) [][]string {
	t.Helper()
	b, err := os.ReadFile(f.log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	var cur []string
	for _, l := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if l == "--" {
			out = append(out, cur)
			cur = nil
			continue
		}
		cur = append(cur, l)
	}
	return out
}

var installArgs = []string{"install", "-y", "-q", "--no-install-recommends"}

func TestInstallerInstallsAllInOneRun(t *testing.T) {
	apt := newFakeApt(t)
	res := apt.installer("http_proxy=http://proxy:3128").Install(context.Background(), []string{"ffmpeg", "g++", "poppler-utils"})
	if want := []string{"ffmpeg", "g++", "poppler-utils"}; !reflect.DeepEqual(res.Installed, want) {
		t.Errorf("Installed = %v, want %v", res.Installed, want)
	}
	if len(res.Failed) != 0 {
		t.Errorf("Failed = %v, want none", res.Failed)
	}
	want := [][]string{{"update"}, append(append([]string{}, installArgs...), "ffmpeg", "g++", "poppler-utils")}
	if got := apt.runs(t); !reflect.DeepEqual(got, want) {
		t.Errorf("apt-get runs = %q, want %q", got, want)
	}
	if got, want := apt.lookups(t), []string{"ffmpeg", "g++", "poppler-utils"}; !reflect.DeepEqual(got, want) {
		t.Errorf("apt-cache lookups = %q, want %q", got, want)
	}
	env, _ := os.ReadFile(apt.env)
	if !strings.Contains(string(env), "DEBIAN_FRONTEND=noninteractive http_proxy=http://proxy:3128") {
		t.Errorf("apt-get environment = %q, want noninteractive with the proxy passed through", env)
	}
}

// When the one install fails, each package is installed on its own so
// the good ones land and the bad one is named with apt's reason.
func TestInstallerFallsBackToOnePackageAtATime(t *testing.T) {
	apt := newFakeApt(t)
	res := apt.installer("FAKE_APT_MISSING=nosuch").Install(context.Background(), []string{"jq", "nosuch", "zip"})
	if want := []string{"jq", "zip"}; !reflect.DeepEqual(res.Installed, want) {
		t.Errorf("Installed = %v, want %v", res.Installed, want)
	}
	if want := []PackageFailure{{Name: "nosuch", Error: "Unable to locate package nosuch"}}; !reflect.DeepEqual(res.Failed, want) {
		t.Errorf("Failed = %v, want %v", res.Failed, want)
	}
	runs := apt.runs(t)
	if len(runs) != 5 {
		t.Fatalf("apt-get runs = %q, want update, the batch, then one per package", runs)
	}
	for i, p := range []string{"jq", "nosuch", "zip"} {
		if want := append(append([]string{}, installArgs...), p); !reflect.DeepEqual(runs[2+i], want) {
			t.Errorf("run %d = %q, want %q", 2+i, runs[2+i], want)
		}
	}
}

// apt-get update failing does not stop the install: the lists already
// on disk may have the package. A package that then fails carries the
// update's reason beside its own.
func TestInstallerInstallsDespiteAFailedUpdate(t *testing.T) {
	apt := newFakeApt(t)
	res := apt.installer("FAKE_APT_UPDATE_FAILS=1", "FAKE_APT_MISSING=ffmpeg", "FAKE_APT_UNKNOWN=nosuch").
		Install(context.Background(), []string{"ffmpeg", "jq", "nosuch"})
	if want := []string{"jq"}; !reflect.DeepEqual(res.Installed, want) {
		t.Errorf("Installed = %v, want %v", res.Installed, want)
	}
	const update = "; the package lists could not be updated: Some index files failed to download. They have been ignored, or old ones used instead."
	want := []PackageFailure{
		{Name: "nosuch", Error: reasonNoSuchPackage + update},
		{Name: "ffmpeg", Error: "Unable to locate package ffmpeg" + update},
	}
	if !reflect.DeepEqual(res.Failed, want) {
		t.Errorf("Failed = %q, want %q", res.Failed, want)
	}
	runs := apt.runs(t)
	if len(runs) < 2 || !reflect.DeepEqual(runs[1], append(append([]string{}, installArgs...), "ffmpeg", "jq")) {
		t.Errorf("apt-get runs = %q, want the update then an install of the known names", runs)
	}
}

// A name that is not exactly a package name fails without reaching
// apt-get install, which would read it as a pattern: "python3.1"
// matches python3.11 as a regular expression.
func TestInstallerOnlyInstallsExactPackageNames(t *testing.T) {
	apt := newFakeApt(t)
	res := apt.installer("FAKE_APT_KNOWN=jq python3.11 python3.11-dev").
		Install(context.Background(), []string{"jq", "python3.1", "python3.11"})
	if want := []string{"jq", "python3.11"}; !reflect.DeepEqual(res.Installed, want) {
		t.Errorf("Installed = %v, want %v", res.Installed, want)
	}
	if want := []PackageFailure{{Name: "python3.1", Error: reasonNoSuchPackage}}; !reflect.DeepEqual(res.Failed, want) {
		t.Errorf("Failed = %v, want %v", res.Failed, want)
	}
	for _, run := range apt.runs(t) {
		for _, a := range run {
			if a == "python3.1" {
				t.Errorf("apt-get received python3.1: %q", run)
			}
		}
	}

	// Nothing known: apt-get never installs anything.
	apt2 := newFakeApt(t)
	res = apt2.installer("FAKE_APT_UNKNOWN=lib..").Install(context.Background(), []string{"lib.."})
	if len(res.Installed) != 0 || len(res.Failed) != 1 || res.Failed[0].Error != reasonNoSuchPackage {
		t.Errorf("result = %+v", res)
	}
	if runs := apt2.runs(t); len(runs) != 1 || runs[0][0] != "update" {
		t.Errorf("apt-get runs = %q, want only the update", runs)
	}
}

// apt-cache failing fails the package with its reason, and apt-get is
// not asked to install it.
func TestInstallerLookupFailure(t *testing.T) {
	apt := newFakeApt(t)
	res := apt.installer("FAKE_APT_CACHE_FAILS=1").Install(context.Background(), []string{"jq"})
	want := []PackageFailure{{Name: "jq", Error: "could not look the package up: The package cache file is corrupted"}}
	if len(res.Installed) != 0 || !reflect.DeepEqual(res.Failed, want) {
		t.Errorf("result = %+v, want %v", res, want)
	}
	if runs := apt.runs(t); len(runs) != 1 {
		t.Errorf("apt-get runs = %q, want only the update", runs)
	}
}

func TestValidPackageName(t *testing.T) {
	for _, ok := range []string{"g++", "python3.11", "jq", "libc6-dev", "c+", "a."} {
		if !ValidPackageName(ok) {
			t.Errorf("ValidPackageName(%q) = false", ok)
		}
	}
	for _, bad := range []string{"jq-", "a-", "-a", ".a", "x", "", "Jq", "jq=1.6", "jq/stable", "a b"} {
		if ValidPackageName(bad) {
			t.Errorf("ValidPackageName(%q) = true", bad)
		}
	}
}

// No network, as apt-get really behaves: update only warns and exits
// 0, then the install cannot find the package. The warning's reason
// goes with the failure.
func TestInstallerNamesTheUpdateWarning(t *testing.T) {
	apt := newFakeApt(t)
	res := apt.installer("FAKE_APT_UPDATE_WARNS=1", "FAKE_APT_MISSING=jq").Install(context.Background(), []string{"jq"})
	want := []PackageFailure{{Name: "jq", Error: "Unable to locate package jq; the package lists could not be updated: Temporary failure resolving 'deb.debian.org'"}}
	if !reflect.DeepEqual(res.Failed, want) {
		t.Errorf("Failed = %v, want %v", res.Failed, want)
	}
}

// A name that is not a Debian package name never reaches apt-get: one
// starting with '-' would be an option, and none is shell text.
func TestInstallerNeverPassesAnInvalidName(t *testing.T) {
	apt := newFakeApt(t)
	bad := []string{"-oAPT::Update::Pre-Invoke::=touch /tmp/x", "--allow-unauthenticated", "a;rm -rf /", "$(id)", "Upper", "x", "", "bash-"}
	res := apt.installer().Install(context.Background(), append([]string{"jq"}, bad...))
	if want := []string{"jq"}; !reflect.DeepEqual(res.Installed, want) {
		t.Errorf("Installed = %v, want %v", res.Installed, want)
	}
	if len(res.Failed) != len(bad) {
		t.Errorf("Failed = %v, want each invalid name", res.Failed)
	}
	for _, f := range res.Failed {
		if f.Error != reasonInvalidName {
			t.Errorf("%q: reason %q, want %q", f.Name, f.Error, reasonInvalidName)
		}
	}
	for _, run := range apt.runs(t) {
		for _, a := range run {
			for _, b := range bad {
				if a == b {
					t.Errorf("apt-get received %q", a)
				}
			}
		}
	}

	apt2 := newFakeApt(t)
	res = apt2.installer().Install(context.Background(), bad)
	if runs := apt2.runs(t); runs != nil {
		t.Errorf("apt-get ran for only invalid names: %q", runs)
	}
	if len(res.Installed) != 0 || len(res.Failed) != len(bad) {
		t.Errorf("result = %+v", res)
	}
}

// A process apt-get leaves running with its stderr open does not hold
// the install up past OutputGrace, nor turn a success into a failure.
func TestInstallerOutlivesALingeringProcess(t *testing.T) {
	apt := newFakeApt(t)
	pidFile := filepath.Join(t.TempDir(), "linger.pid")
	t.Cleanup(func() { killPidFile(pidFile) })
	in := apt.installer("FAKE_APT_LINGER=" + pidFile)
	in.OutputGrace = 50 * time.Millisecond
	res := in.Install(context.Background(), []string{"jq"})
	if !reflect.DeepEqual(res.Installed, []string{"jq"}) || len(res.Failed) != 0 {
		t.Errorf("result = %+v, want jq installed", res)
	}
}

// killPidFile kills the process whose pid is in path, if any.
func killPidFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	}
}

// apt-get missing altogether (or the binary failing to start) still
// accounts for every package.
func TestInstallerWithoutAptGet(t *testing.T) {
	apt := newFakeApt(t)
	in := apt.installer()
	in.AptGet = filepath.Join(t.TempDir(), "absent")
	res := in.Install(context.Background(), []string{"jq"})
	if len(res.Failed) != 1 || !strings.Contains(res.Failed[0].Error, "the package lists could not be updated") {
		t.Errorf("result = %+v", res)
	}
}

// A run cut off by the install's deadline says so.
func TestInstallerTimedOut(t *testing.T) {
	apt := newFakeApt(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := apt.installer().Install(ctx, []string{"jq"})
	if len(res.Failed) != 1 || !strings.Contains(res.Failed[0].Error, "timed out") {
		t.Errorf("result = %+v, want jq failed as timed out", res)
	}
}

func TestFailureReason(t *testing.T) {
	cases := []struct {
		name, stderr, want string
	}{
		{"error lines win", "W: something\nE: Unable to locate package x\nE: second\n", "Unable to locate package x; second"},
		{"else the last two lines", "one\ntwo\nthree\n", "two; three"},
		{"else the error", "", "exit status 100"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := failureReason(c.stderr, errExit100{}); got != c.want {
				t.Errorf("failureReason = %q, want %q", got, c.want)
			}
		})
	}
	long := "E: " + strings.Repeat("é", 400)
	got := failureReason(long, nil)
	if len(got) > failureReasonMax || !strings.HasSuffix(got, "…") {
		t.Errorf("long reason not capped: %d bytes", len(got))
	}
	if !strings.HasPrefix(got, "éé") || strings.ContainsRune(got, '�') {
		t.Errorf("cap split a character: %q", got[len(got)-8:])
	}
}

type errExit100 struct{}

func (errExit100) Error() string { return "exit status 100" }

func TestParseInstallList(t *testing.T) {
	got := ParseInstallList(" zip, ffmpeg,,zip ,jq")
	if want := []string{"ffmpeg", "jq", "zip"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ParseInstallList = %v, want %v", got, want)
	}
	if got := ParseInstallList(""); got != nil {
		t.Errorf("ParseInstallList(\"\") = %v, want nil", got)
	}
}

func TestReadInstallResult(t *testing.T) {
	requested := []string{"ffmpeg", "jq", "zip"}
	got := ReadInstallResult(strings.NewReader(`{"installed":["jq","bogus"],"failed":[{"name":"zip","error":"E"},{"name":"other","error":"x"}]}`), requested)
	want := InstallResult{
		Installed: []string{"jq"},
		Failed: []PackageFailure{
			{Name: "zip", Error: "E"},
			{Name: "ffmpeg", Error: "the installer did not report on it"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadInstallResult = %+v, want %+v", got, want)
	}

	got = ReadInstallResult(strings.NewReader(""), requested)
	if len(got.Installed) != 0 || len(got.Failed) != 3 || got.Failed[0].Error != "the installer stopped without reporting a result" {
		t.Errorf("empty result = %+v", got)
	}
}
