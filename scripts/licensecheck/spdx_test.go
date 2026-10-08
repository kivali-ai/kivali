package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestSatisfied(t *testing.T) {
	allowed := map[string]bool{"MIT": true, "APACHE-2.0": true, "ISC": true, "MPL-2.0": true}
	ok := func(id string) bool { return allowed[strings.ToUpper(id)] }
	for _, tc := range []struct {
		expr string
		want bool
	}{
		{"MIT", true},
		{"mit", true},
		{"GPL-3.0-only", false},
		{"MIT OR GPL-3.0-only", true},
		{"GPL-3.0-only OR MIT", true},
		{"GPL-2.0-only OR LGPL-2.1-only", false},
		{"Apache-2.0 AND ISC", true},
		{"Apache-2.0 AND GPL-2.0-only", false},
		{"Apache-2.0 WITH LLVM-exception", true},
		{"GPL-2.0-only WITH Classpath-exception-2.0", false},
		{"(Apache-2.0 OR MIT) AND ISC", true},
		{"(GPL-3.0-only OR MIT) AND (Apache-2.0 OR LGPL-3.0-only)", true},
		{"(GPL-3.0-only OR BSD-3-Clause) AND MIT", false},
		{"MIT AND GPL-3.0-only OR ISC", true}, // AND binds tighter
		{"MIT OR GPL-3.0-only AND LGPL-3.0-only", true},
		{"ISC AND (GPL-3.0-only OR LGPL-3.0-only)", false},
		{"Apache-2.0 WITH LLVM-exception OR Apache-2.0 OR MIT", true},
		{"MIT/Apache-2.0", true},
		{"GPL-3.0/LGPL-3.0", false},
		{"MIT or Apache-2.0", true},
		{"MPL-2.0+", true},
		{"LicenseRef-Proprietary", false},
		{"Unicode-3.0", false},
	} {
		e, err := parseExpr(tc.expr)
		if err != nil {
			t.Errorf("parseExpr(%q): %v", tc.expr, err)
			continue
		}
		if got := e.satisfied(ok); got != tc.want {
			t.Errorf("%q satisfied = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, s := range []string{
		"",
		"   ",
		"MIT AND",
		"OR MIT",
		"(MIT OR Apache-2.0",
		"MIT)",
		"MIT Apache-2.0",
		"MIT WITH",
		"SEE LICENSE IN LICENSE.md",
		"Apache 2.0",
		"()",
	} {
		if _, err := parseExpr(s); err == nil {
			t.Errorf("parseExpr(%q) succeeded, want an error", s)
		}
	}
}

func TestReport(t *testing.T) {
	pol, err := parsePolicy(strings.NewReader(`
# comment
allow MIT
allow Apache-2.0   # trailing comment
exception npm odd-pkg reviewed: dual-licensed by its author's letter
exception cargo unused-crate stale
`))
	if err != nil {
		t.Fatal(err)
	}
	deps, err := parseDeps(strings.NewReader(`go|example.com/a|v1.0.0|MIT
go|example.com/a|v1.0.0|Apache-2.0
go|example.com/a|v1.0.0|MIT
npm|odd-pkg|1.0.0|SEE LICENSE IN LICENSE
npm|ok-pkg|2.0.0|MIT OR GPL-3.0-only
cargo|gpl-crate|0.1.0|GPL-3.0-only
cargo|bare|0.2.0|
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := deps[2].expression(); got != "(MIT) AND (Apache-2.0)" { // sorted: cargo bare, cargo gpl-crate, go a
		t.Errorf("go a expression = %q", got)
	}
	var out bytes.Buffer
	if report(&out, "policy.txt", pol, deps) {
		t.Fatal("report passed with a GPL dependency")
	}
	s := out.String()
	for _, want := range []string{
		"5 dependencies (cargo 2, go 1, npm 2)",
		"FAIL: 2 dependencies",
		"gpl-crate 0.1.0\n         license: GPL-3.0-only\n         not satisfiable",
		"bare 0.2.0\n         license: (none)\n         no license found",
		"odd-pkg 1.0.0: SEE LICENSE IN LICENSE (exception: reviewed: dual-licensed by its author's letter)",
		`exception "cargo unused-crate" matches no failing dependency`,
		"make licenses",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("report lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "ok-pkg") || strings.Contains(s, "example.com/a") {
		t.Errorf("report names a passing dependency:\n%s", s)
	}

	out.Reset()
	if !report(&out, "policy.txt", pol, deps[2:3]) {
		t.Errorf("report failed on passing dependencies:\n%s", out.String())
	}
}

func TestParsePolicyErrors(t *testing.T) {
	for _, s := range []string{"allow", "allow MIT Apache-2.0", "exception npm pkg", "permit MIT"} {
		if _, err := parsePolicy(strings.NewReader(s)); err == nil {
			t.Errorf("parsePolicy(%q) succeeded, want an error", s)
		}
	}
}
