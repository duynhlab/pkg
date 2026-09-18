package obsx

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExportedAPI_UsesOTelAPITypesOnly pins the RFC-0031 shared-package rule
// (ADR-072) at obsx's own surface: a service imports obsx and the OpenTelemetry
// API, never the SDK or a logging library, so no exported function signature,
// method signature or exported struct field may name a type from an SDK or Zap
// package. The two allowlisted exceptions are deliberate and documented at
// their declarations; removing an entry here is the intended way to retire one.
func TestExportedAPI_UsesOTelAPITypesOnly(t *testing.T) {
	// Package qualifiers that must not appear in the exported surface.
	forbidden := []string{"sdktrace.", "sdkmetric.", "sdklog.", "resource.", "zap.", "zapcore."}
	allow := map[string]string{
		// The one seam that forwards SDK options to a constructor outside
		// obsx (temporalx.NewReplaySafeTracerProvider); callers forward it
		// variadically and never import the SDK. Retire with the seam.
		"TracerProviderConfig.SDKOptions": "sdktrace.",
		// Deprecated: leave with the slogx facade (ADR-070, RFC-0031 Task 1.1c-B).
		"Observability.ZapCore": "zapcore.",
		"TraceContext":          "zap.",
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
		sig := sb.String()
		for _, f := range forbidden {
			if strings.Contains(sig, f) && allow[name] != f {
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
					if id, ok := recv.(*ast.Ident); ok {
						if !id.IsExported() {
							continue
						}
						name = id.Name + "." + name
					}
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
		t.Errorf("exported obsx API names SDK or Zap types (RFC-0031 shared-package rule):\n  %s", strings.Join(violations, "\n  "))
	}
}
