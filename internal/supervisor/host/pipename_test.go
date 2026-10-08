package host

import "testing"

// The shell derives the same pipe name; this vector pins the rule on
// every OS: sha256("c:\users\maya\appdata\local\kivali") starts 96e3ef74.
func TestPipeName(t *testing.T) {
	const want = `\\.\pipe\kivali-96e3ef74`
	if got := PipeName(`C:\Users\Maya\AppData\Local\Kivali`); got != want {
		t.Fatalf("PipeName = %q, want %q", got, want)
	}
	if PipeName(`C:\Users\Maya\AppData\Local\kivali`) != want {
		t.Fatal("PipeName is case-sensitive")
	}
}

// DirKey is the same rule without the pipe prefix; the broker and the
// Hyper-V backend use it for the VM's name, console pipe and MAC.
func TestDirKey(t *testing.T) {
	if got := DirKey(`C:\Users\Maya\AppData\Local\Kivali`); got != "96e3ef74" {
		t.Fatalf("DirKey = %q, want 96e3ef74", got)
	}
}
