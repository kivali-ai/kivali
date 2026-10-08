package broker

import (
	"encoding/base64"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/kivali-ai/kivali/internal/supervisor/host"
)

// The name, MAC and console pipe all derive from the config directory's
// key, and match the vector host.DirKey pins. The Hyper-V backend
// derives the same on its side.
func TestDerivations(t *testing.T) {
	const dir = `C:\Users\Maya\AppData\Local\Kivali`
	key := host.DirKey(dir)
	if key != "96e3ef74" {
		t.Fatalf("key = %q", key)
	}
	if got := VMName(key); got != "kivali-96e3ef74" {
		t.Errorf("VMName = %q", got)
	}
	if got := ComPipe(key); got != `\\.\pipe\kivali-96e3ef74-com1` {
		t.Errorf("ComPipe = %q", got)
	}
	// 02:4b:56 then the key's first three bytes (96 e3 ef).
	if got := MAC(key); got != "024B5696E3EF" {
		t.Errorf("MAC = %q", got)
	}
	if err := ValidMAC(MAC(key)); err != nil {
		t.Errorf("derived MAC invalid: %v", err)
	}
	if KeyOf(VMName(key)) != key {
		t.Errorf("KeyOf round trip")
	}
}

func TestValidName(t *testing.T) {
	for _, ok := range []string{"kivali-96e3ef74", "kivali-00000000"} {
		if err := ValidName(ok); err != nil {
			t.Errorf("ValidName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "kivali-", "kivali-96E3EF74", "kivali-96e3ef7", "kivali-96e3ef74x", "other-96e3ef74", "kivali-xyz12345", `kivali-96e3ef74"`} {
		if err := ValidName(bad); err == nil {
			t.Errorf("ValidName(%q) accepted", bad)
		}
	}
}

func TestValidMAC(t *testing.T) {
	for _, ok := range []string{"024B5696E3EF", "02abcdef0123"} {
		if err := ValidMAC(ok); err != nil {
			t.Errorf("ValidMAC(%q) = %v", ok, err)
		}
	}
	bad := map[string]string{
		"024B56":         "too short",
		"024B5696E3EFAA": "too long",
		"014B5696E3EF":   "multicast (first byte 01)",
		"004B5696E3EF":   "not locally administered",
		"zz4B5696E3EF":   "not hex",
	}
	for mac, why := range bad {
		if err := ValidMAC(mac); err == nil {
			t.Errorf("ValidMAC(%q) accepted (%s)", mac, why)
		}
	}
}

func TestLocalAbs(t *testing.T) {
	for _, ok := range []string{`C:\Kivali`, `C:\Kivali\team1\data.vhdx`, `d:\x`} {
		if err := localAbs("p", ok); err != nil {
			t.Errorf("localAbs(%q) = %v", ok, err)
		}
	}
	bad := []string{
		``, `\\server\share\x`, `\\?\C:\x`, `relative\path`, `C:x`, `C:/x`,
		`C:\a\..\b`, `C:\a\.\b`, `C:\a\\b`, "C:\\a\x00b", `C:\a*b`, `C:\a|b`,
	}
	for _, p := range bad {
		if err := localAbs("p", p); err == nil {
			t.Errorf("localAbs(%q) accepted", p)
		}
	}
}

func TestUnder(t *testing.T) {
	dir := `C:\Kivali\team1`
	for _, in := range []string{`C:\Kivali\team1\data.vhdx`, `C:\Kivali\team1\vm\root-child.vhdx`, `c:\kivali\TEAM1\x`} {
		if !Under(dir, in) {
			t.Errorf("Under(%q, %q) = false", dir, in)
		}
	}
	for _, out := range []string{`C:\Kivali\team1`, `C:\Kivali\team10\x`, `C:\Kivali`, `C:\Kivali\team1\..\team2\x`, `C:\Other\x`, `C:\Kivali\team1\a\.\b`} {
		if Under(dir, out) {
			t.Errorf("Under(%q, %q) = true", dir, out)
		}
	}
}

func TestOwnerOf(t *testing.T) {
	sid := "S-1-5-21-111-222-333-1001"
	if got, ok := OwnerOf(OwnerNotes(sid)); !ok || got != sid {
		t.Fatalf("OwnerOf(OwnerNotes) = %q, %v", got, ok)
	}
	if _, ok := OwnerOf("no owner here"); ok {
		t.Error("notes without an owner reported one")
	}
	// Two different owner lines: refuse (no single owner).
	two := OwnerPrefix + sid + "\n" + OwnerPrefix + "S-1-5-21-111-222-333-1002"
	if _, ok := OwnerOf(two); ok {
		t.Error("two owner lines reported a single owner")
	}
	// A malformed SID is no owner.
	if _, ok := OwnerOf(OwnerPrefix + "not-a-sid"); ok {
		t.Error("a malformed owner SID was accepted")
	}
}

func TestVMRequestValidate(t *testing.T) {
	cfg := `C:\Kivali\team1`
	key := host.DirKey(cfg)
	good := VMRequest{
		Name: VMName(key), ConfigDir: cfg,
		RootVHDX: `C:\Program Files\Kivali\vm\root.vhdx`,
		DataVHDX: `C:\Kivali\team1\data.vhdx`,
		CPUs:     4, MemoryMB: 4096, MAC: MAC(key), ComPipe: ComPipe(key),
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("a good request did not validate: %v", err)
	}
	mut := func(f func(*VMRequest)) VMRequest { r := good; f(&r); return r }
	bad := map[string]VMRequest{
		"cpus 0":        mut(func(r *VMRequest) { r.CPUs = 0 }),
		"cpus huge":     mut(func(r *VMRequest) { r.CPUs = 1000 }),
		"memory small":  mut(func(r *VMRequest) { r.MemoryMB = 512 }),
		"memory odd":    mut(func(r *VMRequest) { r.MemoryMB = 4097 }),
		"root not vhdx": mut(func(r *VMRequest) { r.RootVHDX = `C:\x\root.img` }),
		"data unc":      mut(func(r *VMRequest) { r.DataVHDX = `\\srv\share\data.vhdx` }),
		"pipe wrong":    mut(func(r *VMRequest) { r.ComPipe = `\\.\pipe\evil` }),
		"name form":     mut(func(r *VMRequest) { r.Name = "kivali-XYZ" }),
	}
	for why, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("request accepted but should fail: %s", why)
		}
	}
}

func TestVHDRequestValidate(t *testing.T) {
	ok := VHDRequest{Path: `C:\Kivali\team1\data.vhdx`, SizeBytes: 64 << 30}
	if err := ok.Validate(); err != nil {
		t.Fatalf("good VHD request: %v", err)
	}
	for why, r := range map[string]VHDRequest{
		"wrong name": {Path: `C:\Kivali\team1\root.vhdx`, SizeBytes: 64 << 30},
		"too small":  {Path: `C:\Kivali\team1\data.vhdx`, SizeBytes: 1 << 20},
		"not MiB":    {Path: `C:\Kivali\team1\data.vhdx`, SizeBytes: 64<<30 + 1},
		"relative":   {Path: `data.vhdx`, SizeBytes: 64 << 30},
	} {
		if err := r.Validate(); err == nil {
			t.Errorf("VHD request accepted but should fail: %s", why)
		}
	}
}

// EncodedCommand wraps only fixed script text; it decodes back to the
// prelude around the named script, and the script name is checked.
func TestEncodedCommand(t *testing.T) {
	enc, err := EncodedCommand(scriptGetVM)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	u := make([]uint16, len(raw)/2)
	for i := range u {
		u[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	src := string(utf16.Decode(u))
	if !strings.Contains(src, "ConvertFrom-Json") || !strings.Contains(src, "Get-VM") {
		t.Errorf("encoded command missing prelude or script body:\n%s", src)
	}
	if _, err := EncodedCommand("../evil"); err == nil {
		t.Error("a script name with path characters was accepted")
	}
	if _, err := Script("nonesuch"); err == nil {
		t.Error("an unknown script name was accepted")
	}
}

// Every operation has a script and a timeout, so a run never blocks
// without bound and no name is mistyped.
func TestEveryScriptHasATimeout(t *testing.T) {
	for _, name := range []string{scriptGetVM, scriptConfigureVM, scriptStartVM, scriptStopVM, scriptRemoveVM, scriptNewVHD, scriptSetupStatus, scriptSetupApply} {
		if _, err := Script(name); err != nil {
			t.Errorf("missing script %q: %v", name, err)
		}
		if scriptTimeouts[name] == 0 {
			t.Errorf("script %q has no timeout", name)
		}
	}
}
