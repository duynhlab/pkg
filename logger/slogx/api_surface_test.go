package slogx

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExportedAPI_UsesStdlibTypesOnly pins ADR-070's central promise at the
// facade's own surface: a service imports this package and nothing else for
// logging, so no exported signature or struct field may name a type from a
// logging library, from the OTel SDK, or from the bridge this package happens
// to be built on. The API a call site sees is log/slog and the standard
// library — that is what makes the sink replaceable without touching ten
// services.
//
// The one allowlisted entry is deliberate and documented at its declaration.
// Removing an entry here is the intended way to retire the exception.
func TestExportedAPI_UsesStdlibTypesOnly(t *testing.T) {
	// Package qualifiers that must not appear in the exported surface.
	forbidden := []string{
		"zap.", "zapcore.", "zerolog.", "clog.", // logging libraries
		"sdklog.", "sdktrace.", "sdkmetric.", "resource.", // the OTel SDK
		"otelslog.", "global.", // the bridge and the OTel globals
		"log.", // the OTel Logs API — see the allowlist
	}
	allow := map[string]string{
		// The seam main() uses to hand the facade a provider instead of the
		// OTel global: obsx owns the provider, and a service that lets obsx
		// install the global never names this type. Retire it if the global
		// ever becomes the only supported path.
		"Config.LoggerProvider": "log.",
	}

	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var violations []string
	check := func(name string, node ast.Node) {
		var sb strings.Builder
		ast.Inspect(node, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok {
					sb.WriteString(id.Name + "." + sel.Sel.Name + " ")
				}
			}
			return true
		})
		// Leading space so a qualifier matches whole: "log." must not be
		// found inside "slog.Attr", which is the type this API is built on.
		sig := " " + sb.String()
		for _, f := range forbidden {
			if strings.Contains(sig, " "+f) && allow[name] != f {
				violations = append(violations, name+" uses "+f)
			}
		}
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if !d.Name.IsExported() {
					continue
				}
				name := d.Name.Name
				if d.Recv != nil && len(d.Recv.List) == 1 {
					recv := d.Recv.List[0].Type
					if star, ok := recv.(*ast.StarExpr); ok {
						recv = star.X
					}
					id, ok := recv.(*ast.Ident)
					if !ok || !id.IsExported() {
						continue
					}
					name = id.Name + "." + name
				}
				check(name, d.Type)
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok || !ts.Name.IsExported() {
						continue
					}
					switch tt := ts.Type.(type) {
					case *ast.StructType:
						for _, fld := range tt.Fields.List {
							for _, n := range fld.Names {
								if n.IsExported() {
									check(ts.Name.Name+"."+n.Name, fld.Type)
								}
							}
						}
					case *ast.FuncType, *ast.InterfaceType:
						check(ts.Name.Name, tt)
					}
				}
			}
		}
	}
	if len(violations) > 0 {
		t.Errorf("exported API must speak log/slog and the standard library only:\n\t%s",
			strings.Join(violations, "\n\t"))
	}
}

// TestNoSDKImports proves the layering claim the package doc makes: no
// non-test file links the OpenTelemetry SDK or a logging library. The fleet
// lint policy enforces the same rule from outside; this keeps the module
// honest on its own.
func TestNoSDKImports(t *testing.T) {
	banned := []string{
		"go.opentelemetry.io/otel/sdk",
		"go.uber.org/zap",
		"github.com/rs/zerolog",
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for _, b := range banned {
				if p == b || strings.HasPrefix(p, b+"/") {
					t.Errorf("%s imports %s", path, p)
				}
			}
		}
	}
}
