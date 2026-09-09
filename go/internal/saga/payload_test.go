package saga

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// go/docs/adr/0001 turns on the orchestrator never reading a published
// payload. Avoiding it is not the same as being unable to do it, so this file
// asserts the property two ways: the envelope has nowhere to put a payload,
// and no source file in the package mentions one.

func TestEnvelopeCannotHoldAPayload(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeOf(envelope{})
	for i := range typ.NumField() {
		field := typ.Field(i)
		name := strings.ToLower(field.Name + " " + string(field.Tag))
		if strings.Contains(name, "payload") {
			t.Errorf("envelope has field %s (tag %q); the published payload must have nowhere to land",
				field.Name, field.Tag)
		}
	}
}

// A source-level check, because the structural one above only covers the shape
// this package parses today. Somebody adding a base64 decode, or a second
// struct with a payload field, should fail here rather than quietly reverse a
// decision three documents rest on.
//
// Comments are deliberately not scanned — the package explains at length why it
// does not read the payload, and saying so must stay allowed.
func TestPackageNeverDecodesPayload(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	adapter, err := filepath.Glob(filepath.Join("kafkareader", "*.go"))
	if err != nil {
		t.Fatalf("glob kafkareader: %v", err)
	}
	files = append(files, adapter...)

	fset := token.NewFileSet()
	scanned := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		scanned++

		// Parsed without ParseComments, so the AST holds code only.
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.Ident:
				reportForbidden(t, fset, node.Pos(), node.Name)
			case *ast.BasicLit:
				reportForbidden(t, fset, node.Pos(), node.Value)
			}
			return true
		})
	}
	if scanned == 0 {
		t.Fatal("scanned no source files; the check would pass vacuously")
	}
}

// forbidden names what reading the payload would have to look like: the field
// itself, or the base64 decoding that turns it back into a protobuf.
var forbidden = []string{"payload", "base64"}

func reportForbidden(t *testing.T, fset *token.FileSet, pos token.Pos, text string) {
	t.Helper()
	lower := strings.ToLower(text)
	for _, word := range forbidden {
		if strings.Contains(lower, word) {
			t.Errorf("%s: %q references %q; the orchestrator must never read the published payload (go/docs/adr/0001)",
				fset.Position(pos), text, word)
		}
	}
}
