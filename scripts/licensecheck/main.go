// Command licensecheck holds dependencies to Kivali's license policy.
// scripts/license-check.sh collects what ships and runs it.
//
//	licensecheck -policy scripts/license-policy.txt < deps
//
// Each line of deps is ECOSYSTEM|NAME|VERSION|LICENSE, LICENSE an SPDX
// expression (empty when none is known). Lines for the same component
// combine with AND: every license found applies. A component passes when
// its expression is satisfiable by the policy's allowed licenses, or
// when the policy lists it as an exception. The exit status is 1 when
// any component fails, 2 on bad input.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

func main() {
	policyPath := flag.String("policy", "scripts/license-policy.txt", "the policy file")
	flag.Parse()
	f, err := os.Open(*policyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "licensecheck:", err)
		os.Exit(2)
	}
	pol, err := parsePolicy(f)
	_ = f.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "licensecheck: %s: %v\n", *policyPath, err)
		os.Exit(2)
	}
	deps, err := parseDeps(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "licensecheck:", err)
		os.Exit(2)
	}
	if !report(os.Stdout, *policyPath, pol, deps) {
		os.Exit(1)
	}
}

type policy struct {
	allowed    map[string]bool   // upper-cased SPDX identifiers
	exceptions map[string]string // "ECOSYSTEM NAME" -> reason
}

// parsePolicy reads the policy file: "allow ID" and
// "exception ECOSYSTEM NAME REASON..." lines; # starts a comment.
func parsePolicy(r io.Reader) (policy, error) {
	pol := policy{allowed: map[string]bool{}, exceptions: map[string]string{}}
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line, _, _ := strings.Cut(sc.Text(), "#")
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0:
		case fields[0] == "allow" && len(fields) == 2:
			pol.allowed[strings.ToUpper(fields[1])] = true
		case fields[0] == "exception" && len(fields) >= 4:
			pol.exceptions[fields[1]+" "+fields[2]] = strings.Join(fields[3:], " ")
		default:
			return pol, fmt.Errorf("line %d: want \"allow ID\" or \"exception ECOSYSTEM NAME REASON\"", n)
		}
	}
	return pol, sc.Err()
}

type dep struct {
	ecosystem, name, version string
	licenses                 []string // distinct, in input order
}

func (d dep) expression() string {
	if len(d.licenses) == 1 {
		return d.licenses[0]
	}
	parts := make([]string, len(d.licenses))
	for i, l := range d.licenses {
		parts[i] = "(" + l + ")"
	}
	return strings.Join(parts, " AND ")
}

func parseDeps(r io.Reader) ([]dep, error) {
	byKey := map[string]*dep{}
	var keys []string
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		f := strings.Split(sc.Text(), "|")
		if len(f) != 4 || f[0] == "" || f[1] == "" {
			return nil, fmt.Errorf("input line %d: want ECOSYSTEM|NAME|VERSION|LICENSE, got %q", n, sc.Text())
		}
		key := f[0] + " " + f[1] + " " + f[2]
		d := byKey[key]
		if d == nil {
			d = &dep{ecosystem: f[0], name: f[1], version: f[2]}
			byKey[key] = d
			keys = append(keys, key)
		}
		lic := strings.TrimSpace(f[3])
		if lic != "" && !contains(d.licenses, lic) {
			d.licenses = append(d.licenses, lic)
		}
	}
	sort.Strings(keys)
	deps := make([]dep, len(keys))
	for i, k := range keys {
		deps[i] = *byKey[k]
	}
	return deps, sc.Err()
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// check returns why d fails the policy, or "" when it passes on its
// licenses alone.
func check(pol policy, d dep) string {
	if len(d.licenses) == 0 {
		return "no license found"
	}
	e, err := parseExpr(d.expression())
	if err != nil {
		return err.Error()
	}
	if !e.satisfied(func(id string) bool { return pol.allowed[strings.ToUpper(id)] }) {
		return "not satisfiable by the allowed licenses"
	}
	return ""
}

// report prints the outcome and returns whether every dependency passes.
func report(w io.Writer, policyPath string, pol policy, deps []dep) bool {
	counts := map[string]int{}
	var ecos, failures, excepted []string
	used := map[string]bool{}
	for _, d := range deps {
		if counts[d.ecosystem] == 0 {
			ecos = append(ecos, d.ecosystem)
		}
		counts[d.ecosystem]++
		why := check(pol, d)
		if why == "" {
			continue
		}
		key := d.ecosystem + " " + d.name
		lic := d.expression()
		if lic == "" {
			lic = "(none)"
		}
		if reason, ok := pol.exceptions[key]; ok {
			used[key] = true
			excepted = append(excepted, fmt.Sprintf("  %s %s %s: %s (exception: %s)", d.ecosystem, d.name, d.version, lic, reason))
			continue
		}
		failures = append(failures, fmt.Sprintf("  %-6s %s %s\n         license: %s\n         %s", d.ecosystem, d.name, d.version, lic, why))
	}
	sort.Strings(ecos)
	summary := make([]string, len(ecos))
	for i, e := range ecos {
		summary[i] = fmt.Sprintf("%s %d", e, counts[e])
	}
	_, _ = fmt.Fprintf(w, "license-check: %d dependencies (%s) against %s\n", len(deps), strings.Join(summary, ", "), policyPath)
	for _, l := range excepted {
		_, _ = fmt.Fprintln(w, l)
	}
	var stale []string
	for k := range pol.exceptions {
		if !used[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	for _, k := range stale {
		_, _ = fmt.Fprintf(w, "license-check: exception %q matches no failing dependency; remove it from %s\n", k, policyPath)
	}
	if len(failures) == 0 {
		_, _ = fmt.Fprintln(w, "license-check: PASS")
		return true
	}
	_, _ = fmt.Fprintf(w, "license-check: FAIL: %d dependencies outside the license policy:\n", len(failures))
	for _, l := range failures {
		_, _ = fmt.Fprintln(w, l)
	}
	_, _ = fmt.Fprintf(w, `
To fix each one, do one of:
  - replace the dependency (or pin a version under an allowed license);
  - if the license is acceptable for Kivali (Apache-2.0, shipped in
    binaries and images), have it reviewed, then add "allow <SPDX-ID>"
    to %[1]s;
  - for this one component only, add
    "exception <ecosystem> <name> <reason>" to %[1]s.
Then run: make licenses
`, policyPath)
	return false
}
