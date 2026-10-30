package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// scripts/hdiutil-retry.sh, the hdiutil the dmg step of `tauri bundle`
// finds first on PATH (desktop-app/Makefile), run against a stand-in
// hdiutil that answers from a queue per subcommand and a stand-in sleep
// that only records how long it was asked to wait.

// fakeHdiutil answers create and detach from the queues in its
// directory ("busy", "full" or, when empty, success), records every call,
// and, like hdiutil without -ov, refuses to create over an existing file.
const fakeHdiutil = `#!/usr/bin/env bash
printf '%s\n' "$*" >>"$FAKE/calls"
next() {
  local f="$FAKE/$1" first
  if [ -s "$f" ]; then
    first="$(head -n 1 "$f")"
    tail -n +2 "$f" >"$f.rest" && mv "$f.rest" "$f"
    echo "$first"
  else
    echo ok
  fi
}
case "$1" in
  create)
    out="${!#}"
    if [ -e "$out" ]; then echo "hdiutil: create failed - File exists" >&2; exit 1; fi
    case "$(next create)" in
      busy) : >"$out"; echo "hdiutil: create failed - Resource busy" >&2; exit 1 ;;
      full) echo "hdiutil: create failed - No space left on device" >&2; exit 1 ;;
      *) : >"$out"; echo "created: $out" ;;
    esac ;;
  detach)
    case "$(next detach)" in
      busy) echo 'hdiutil: couldn'"'"'t unmount "disk4" - Resource busy' >&2; exit 16 ;;
      *) echo '"disk4" ejected.' ;;
    esac ;;
  info) cat "$FAKE/info" ;;
  attach) printf '/dev/disk4\tGUID_partition_scheme\t\n/dev/disk4s1\tApple_HFS\t/Volumes/Kivali\n' ;;
esac
`

const fakeSleep = `#!/usr/bin/env bash
printf '%s\n' "$1" >>"$FAKE/sleeps"
`

type hdiutilFake struct {
	dir   string
	image string // the path create writes, under a real directory
}

// newHdiutilFake sets up the stand-ins and an `hdiutil info` listing,
// captured from a real one, in which the image being created is attached
// as /dev/disk4 and an unrelated image as /dev/disk5.
func newHdiutilFake(t *testing.T) *hdiutilFake {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"hdiutil": fakeHdiutil, "sleep": fakeSleep} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bundle := filepath.Join(dir, "bundle", "dmg")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	// The script compares physical paths (pwd -P); macOS's temporary
	// directory is under a symlink.
	bundle, err := filepath.EvalSymlinks(bundle)
	if err != nil {
		t.Fatal(err)
	}
	f := &hdiutilFake{dir: dir, image: filepath.Join(bundle, "rw.4242.Kivali_1.3.0_aarch64.dmg")}
	info, err := os.ReadFile("scripts/testdata/hdiutil-retry/info.txt")
	if err != nil {
		t.Fatal(err)
	}
	listing := strings.ReplaceAll(string(info),
		"/Users/runner/work/kivali/kivali/desktop-app/src-tauri/target/release/bundle/dmg/rw.4242.Kivali_1.3.0_aarch64.dmg", f.image)
	if err := os.WriteFile(filepath.Join(dir, "info"), []byte(listing), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *hdiutilFake) queue(t *testing.T, sub string, results ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, sub), []byte(strings.Join(results, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// run runs the script with args and returns its stdout, stderr and exit
// status.
func (f *hdiutilFake) run(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"scripts/hdiutil-retry.sh"}, args...)...)
	cmd.Env = append(os.Environ(),
		"FAKE="+f.dir,
		"HDIUTIL_REAL="+filepath.Join(f.dir, "hdiutil"),
		"HDIUTIL_RETRY_SLEEP="+filepath.Join(f.dir, "sleep"),
		"HDIUTIL_RETRY_LOG="+filepath.Join(f.dir, "log"),
	)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return stdout.String(), stderr.String(), 0
	case errors.As(err, &exit):
		return stdout.String(), stderr.String(), exit.ExitCode()
	}
	t.Fatalf("hdiutil-retry.sh %s: %v", strings.Join(args, " "), err)
	return "", "", 0
}

// lines returns the lines of one of the fake's records (calls, sleeps,
// log), none when it was never written.
func (f *hdiutilFake) lines(t *testing.T, name string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func (f *hdiutilFake) create() []string {
	return []string{"create", "-srcfolder", "Kivali.app", "-volname", "Kivali", "-fs", "HFS+", "-format", "UDRW", f.image}
}

// The v1.3.0 release failed here: the dmg script tauri runs gives up on
// the first `hdiutil create` that fails with "Resource busy", a failure
// the macOS runners hit now and then. Without the retry, one busy volume
// fails the release build again. The fake refuses to create over an
// existing file, so passing also shows the failed attempt's file was
// removed; and the failed attempt's image, still attached, is detached,
// while the unrelated image is left alone.
func TestHdiutilRetryTriesABusyCreateAgain(t *testing.T) {
	f := newHdiutilFake(t)
	f.queue(t, "create", "busy", "busy")
	stdout, stderr, code := f.run(t, f.create()...)
	if code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if want := "created: " + f.image; !strings.Contains(stdout, want) {
		t.Errorf("stdout %q, want %q", stdout, want)
	}
	create := strings.Join(f.create(), " ")
	want := []string{create, "info", "detach /dev/disk4", create, "info", "detach /dev/disk4", create}
	if got := f.lines(t, "calls"); !reflect.DeepEqual(got, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := f.lines(t, "sleeps"); !reflect.DeepEqual(got, []string{"2", "4"}) {
		t.Errorf("sleeps %q, want [2 4]", got)
	}
	log := strings.Join(f.lines(t, "log"), "\n")
	for _, want := range []string{"create failed - Resource busy", "exit 0 (attempt 3 of 5)", "removed " + f.image} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}

// A failed create's image that will not detach (the volume is still busy)
// must still go: left attached, it is a second volume named like the dmg,
// and the script's Finder step, which finds the volume by name, could lay
// out the wrong one. The image is the failed attempt's own, so forcing is
// safe.
func TestHdiutilRetryForcesALeftoverImageThatWillNotDetach(t *testing.T) {
	f := newHdiutilFake(t)
	f.queue(t, "create", "busy")
	f.queue(t, "detach", "busy")
	if _, stderr, code := f.run(t, f.create()...); code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	create := strings.Join(f.create(), " ")
	want := []string{create, "info", "detach /dev/disk4", "detach -force /dev/disk4", create}
	if got := f.lines(t, "calls"); !reflect.DeepEqual(got, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// A volume that stays busy must still fail the build, after a bounded
// wait (five attempts, 30 seconds of waiting), with hdiutil's own message
// and exit status, rather than hang the release job.
func TestHdiutilRetryGivesUpOnACreateThatStaysBusy(t *testing.T) {
	f := newHdiutilFake(t)
	f.queue(t, "create", "busy", "busy", "busy", "busy", "busy")
	_, stderr, code := f.run(t, f.create()...)
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "create failed - Resource busy") {
		t.Errorf("stderr lacks hdiutil's message:\n%s", stderr)
	}
	if got := f.lines(t, "sleeps"); !reflect.DeepEqual(got, []string{"2", "4", "8", "16"}) {
		t.Errorf("sleeps %q, want [2 4 8 16]", got)
	}
}

// Only "Resource busy" is worth waiting out. Any other failure is real
// and must fail the build at once, as it would without this script.
func TestHdiutilRetryReturnsOtherFailuresAtOnce(t *testing.T) {
	f := newHdiutilFake(t)
	f.queue(t, "create", "full")
	_, stderr, code := f.run(t, f.create()...)
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr, "No space left on device") {
		t.Errorf("stderr lacks hdiutil's message:\n%s", stderr)
	}
	if got := f.lines(t, "calls"); len(got) != 1 {
		t.Errorf("calls %q, want the one create", got)
	}
	if got := f.lines(t, "sleeps"); got != nil {
		t.Errorf("slept %q", got)
	}
}

// The dmg script tries a busy detach only three times over six seconds;
// a volume Finder has just laid out can stay busy longer. Without this,
// that detach fails the build.
func TestHdiutilRetryTriesABusyDetachAgain(t *testing.T) {
	f := newHdiutilFake(t)
	f.queue(t, "detach", "busy")
	if _, stderr, code := f.run(t, "detach", "/dev/disk4"); code != 0 {
		t.Fatalf("exit %d, stderr:\n%s", code, stderr)
	}
	if got := f.lines(t, "calls"); !reflect.DeepEqual(got, []string{"detach /dev/disk4", "detach /dev/disk4"}) {
		t.Errorf("calls %q", got)
	}
	if got := f.lines(t, "sleeps"); !reflect.DeepEqual(got, []string{"2"}) {
		t.Errorf("sleeps %q, want [2]", got)
	}
}

// -quiet hides the message the retry looks for, so the script drops it
// for hdiutil and keeps the caller's output quiet itself; a quiet create
// that was busy must still be tried again.
func TestHdiutilRetryKeepsAQuietCreateQuiet(t *testing.T) {
	f := newHdiutilFake(t)
	f.queue(t, "create", "busy")
	args := append([]string{"create", "-quiet"}, f.create()[1:]...)
	stdout, _, code := f.run(t, args...)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if stdout != "" {
		t.Errorf("stdout %q, want none", stdout)
	}
	create := strings.Join(f.create(), " ")
	want := []string{create, "info", "detach /dev/disk4", create}
	if got := f.lines(t, "calls"); !reflect.DeepEqual(got, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// The dmg script reads `hdiutil attach`'s output to find the device it
// lays out and detaches, so every subcommand other than create and detach
// must reach hdiutil and come back untouched.
func TestHdiutilRetryPassesOtherSubcommandsThrough(t *testing.T) {
	f := newHdiutilFake(t)
	stdout, _, code := f.run(t, "attach", "-quiet", "-nobrowse", "rw.dmg")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if want := "/dev/disk4\tGUID_partition_scheme\t\n/dev/disk4s1\tApple_HFS\t/Volumes/Kivali\n"; stdout != want {
		t.Errorf("stdout %q, want %q", stdout, want)
	}
	if got := f.lines(t, "calls"); !reflect.DeepEqual(got, []string{"attach -quiet -nobrowse rw.dmg"}) {
		t.Errorf("calls %q", got)
	}
}
