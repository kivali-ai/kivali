package main

import (
	"fmt"
	"strings"
	"unicode"
)

// expr is a parsed SPDX license expression.
type expr interface {
	// satisfied reports whether a licensee may take the component under
	// licenses allowed accepts: one side of an OR, both sides of an AND.
	satisfied(allowed func(id string) bool) bool
}

// license is one license identifier, with the exception a WITH names
// (an exception only grants permissions, so it never decides).
type license struct{ id, exception string }

type and struct{ l, r expr }

type or struct{ l, r expr }

func (e license) satisfied(allowed func(string) bool) bool {
	// "ID+" is ID or any later version; ID itself is one of them.
	return allowed(strings.TrimSuffix(e.id, "+"))
}

func (e and) satisfied(allowed func(string) bool) bool {
	return e.l.satisfied(allowed) && e.r.satisfied(allowed)
}

func (e or) satisfied(allowed func(string) bool) bool {
	return e.l.satisfied(allowed) || e.r.satisfied(allowed)
}

// parseExpr parses an SPDX license expression:
//
//	or   = and { "OR" and }
//	and  = atom { "AND" atom }
//	atom = "(" or ")" | ID [ "WITH" ID ]
//
// Operators are matched without regard to case, and "/" is OR (Cargo's
// legacy "MIT/Apache-2.0").
func parseExpr(s string) (expr, error) {
	p := &parser{toks: tokenize(s)}
	if len(p.toks) == 0 {
		return nil, fmt.Errorf("no license declared")
	}
	e, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.toks) {
		return nil, fmt.Errorf("unexpected %q in %q", p.toks[p.pos], s)
	}
	return e, nil
}

func tokenize(s string) []string {
	var toks []string
	cur := strings.Builder{}
	flush := func() {
		if cur.Len() > 0 {
			toks = append(toks, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			flush()
		case r == '(' || r == ')':
			flush()
			toks = append(toks, string(r))
		case r == '/':
			flush()
			toks = append(toks, "OR")
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return toks
}

type parser struct {
	toks []string
	pos  int
}

func (p *parser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

// op reports whether the next token is the operator name, and consumes it.
func (p *parser) op(name string) bool {
	if strings.EqualFold(p.peek(), name) {
		p.pos++
		return true
	}
	return false
}

func (p *parser) or() (expr, error) {
	l, err := p.and()
	if err != nil {
		return nil, err
	}
	for p.op("OR") {
		r, err := p.and()
		if err != nil {
			return nil, err
		}
		l = or{l, r}
	}
	return l, nil
}

func (p *parser) and() (expr, error) {
	l, err := p.atom()
	if err != nil {
		return nil, err
	}
	for p.op("AND") {
		r, err := p.atom()
		if err != nil {
			return nil, err
		}
		l = and{l, r}
	}
	return l, nil
}

func (p *parser) atom() (expr, error) {
	if p.op("(") {
		e, err := p.or()
		if err != nil {
			return nil, err
		}
		if !p.op(")") {
			return nil, fmt.Errorf("missing )")
		}
		return e, nil
	}
	id, err := p.id()
	if err != nil {
		return nil, err
	}
	l := license{id: id}
	if p.op("WITH") {
		if l.exception, err = p.id(); err != nil {
			return nil, err
		}
	}
	return l, nil
}

func (p *parser) id() (string, error) {
	t := p.peek()
	switch {
	case t == "":
		return "", fmt.Errorf("expression ends early")
	case t == "(" || t == ")" || isOperator(t):
		return "", fmt.Errorf("license identifier expected, found %q", t)
	}
	for _, r := range t {
		if !idRune(r) {
			return "", fmt.Errorf("%q is not a license identifier", t)
		}
	}
	p.pos++
	return t, nil
}

// idRune reports whether r may appear in a license identifier (letters,
// digits, ".", "-", "+" and the ":" of a DocumentRef).
func idRune(r rune) bool {
	return r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune(".-+:", r))
}

func isOperator(t string) bool {
	for _, o := range []string{"AND", "OR", "WITH"} {
		if strings.EqualFold(t, o) {
			return true
		}
	}
	return false
}
