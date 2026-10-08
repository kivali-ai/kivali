package apitypes

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// TestStringEnumsAreUnions guards the TypeScript wire contract: tygo
// (enum_style: union) emits a string-literal union for a Go string type
// only when every constant of that type is named with the type name as a
// prefix. One off-pattern constant silently widens the TypeScript type to
// plain `string`, and the web app loses exhaustiveness checking.
//
// The test also fails when a string type has no constants at all but is
// carried on a JSON field, because it reaches TypeScript as `string` too.
func TestStringEnumsAreUnions(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}

	// Every `type X string`.
	stringTypes := map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				if id, ok := ts.Type.(*ast.Ident); ok && id.Name == "string" && ts.Assign == 0 {
					stringTypes[ts.Name.Name] = true
				}
			}
		}
	}

	// Typed constants, including grouped consts that inherit the type of
	// the previous spec (iota-style implicit repetition).
	consts := map[string][]string{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			var current string
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				switch {
				case vs.Type != nil:
					current = ""
					if id, ok := vs.Type.(*ast.Ident); ok {
						current = id.Name
					}
				case len(vs.Values) > 0:
					// An untyped constant with its own value breaks the chain.
					current = ""
				}
				if current == "" || !stringTypes[current] {
					continue
				}
				for _, n := range vs.Names {
					consts[current] = append(consts[current], n.Name)
				}
			}
		}
	}

	// String types referenced by a struct field with a json tag.
	jsonUsed := map[string]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range st.Fields.List {
				if field.Tag == nil || !strings.Contains(field.Tag.Value, "json:") {
					continue
				}
				ast.Inspect(field.Type, func(m ast.Node) bool {
					if id, ok := m.(*ast.Ident); ok && stringTypes[id.Name] {
						jsonUsed[id.Name] = true
					}
					return true
				})
			}
			return true
		})
	}

	names := make([]string, 0, len(stringTypes))
	for n := range stringTypes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, typ := range names {
		cs := consts[typ]
		if len(cs) == 0 {
			if jsonUsed[typ] {
				t.Errorf("%s is a string type carried on a JSON field but declares no constants; tygo emits plain `string`", typ)
			}
			continue
		}
		for _, c := range cs {
			if !strings.HasPrefix(c, typ) {
				t.Errorf("%s: constant %s lacks the %q prefix; tygo emits plain `string` for the whole type", typ, c, typ)
			}
		}
	}
}
