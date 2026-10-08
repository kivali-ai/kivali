package apitypes

import (
	"reflect"
	"strconv"
	"strings"
)

// TB is the part of testing.TB NoNilSlices needs. Declared here so the
// package does not import "testing" into the server binary.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
}

// NoNilSlices fails t for every nil slice reachable from v, naming its
// path ("agents[2].workers"). A response with a nil slice marshals as
// null, which the generated TypeScript types say cannot happen.
//
// Nil pointers and nil maps are allowed: an optional field is a pointer
// with omitempty, and absent is its documented meaning.
func NoNilSlices(t TB, v any) {
	t.Helper()
	for _, p := range nilSlicePaths(reflect.ValueOf(v), "$") {
		t.Errorf("nil slice at %s (marshals as null; make it an empty slice)", p)
	}
}

func nilSlicePaths(v reflect.Value, path string) []string {
	switch v.Kind() {
	case reflect.Invalid:
		return nil
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return nilSlicePaths(v.Elem(), path)
	case reflect.Slice:
		if v.IsNil() {
			return []string{path}
		}
		var out []string
		for i := 0; i < v.Len(); i++ {
			out = append(out, nilSlicePaths(v.Index(i), path+"["+strconv.Itoa(i)+"]")...)
		}
		return out
	case reflect.Array:
		var out []string
		for i := 0; i < v.Len(); i++ {
			out = append(out, nilSlicePaths(v.Index(i), path+"["+strconv.Itoa(i)+"]")...)
		}
		return out
	case reflect.Map:
		var out []string
		iter := v.MapRange()
		for iter.Next() {
			out = append(out, nilSlicePaths(iter.Value(), path+"["+iter.Key().String()+"]")...)
		}
		return out
	case reflect.Struct:
		var out []string
		ty := v.Type()
		for i := 0; i < v.NumField(); i++ {
			f := ty.Field(i)
			if !f.IsExported() {
				continue
			}
			name := f.Name
			if tag := f.Tag.Get("json"); tag != "" {
				if tag == "-" {
					continue
				}
				if n, _, _ := strings.Cut(tag, ","); n != "" {
					name = n
				}
			}
			sub := path + "." + name
			if f.Anonymous {
				sub = path
			}
			out = append(out, nilSlicePaths(v.Field(i), sub)...)
		}
		return out
	}
	return nil
}
