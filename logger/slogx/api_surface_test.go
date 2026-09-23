package slogx

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// allowedForeign maps an exported declaration to the one import path it may
// name. Entries are deliberate and documented at their declaration; removing
// one here is the intended way to retire the exception.
var allowedForeign = map[string]string{
	// The seam main() uses to hand the facade a provider instead of the OTel
	// global: obsx owns the provider, and a service that lets obsx install
	// the global never names this type. Retire it if the global ever becomes
	// the only supported path.
	"Config.LoggerProvider": "go.opentelemetry.io/otel/log",
}

// TestExportedAPI_UsesStdlibTypesOnly pins ADR-070's central promise at the
// facade's own surface: a service imports this package and nothing else for
// logging, so no exported signature, field, variable or type may name a type
// from ANY module outside the standard library — not a logging library, not
// the OTel SDK, not the bridge this package happens to be built on. That is
// what makes the sink replaceable without touching ten services.
//
// The check resolves each qualifier through the file's own imports rather
// than matching package names, so an import alias, a type alias, an exported
// variable or an embedded field cannot walk past it.
func TestExportedAPI_UsesStdlibTypesOnly(t *testing.T) {
	var violations []string
	forEachSourceFile(t, func(path string, f *ast.File) {
		imports := importsOf(f)
		check := func(name string, node ast.Node) {
			for _, qualifier := range qualifiersIn(node) {
				pkg, ok := imports[qualifier]
				if !ok || isStdlib(pkg) || allowedForeign[name] == pkg {
					continue
				}
				violations = append(violations, name+" names "+pkg+" ("+path+")")
			}
		}
		for _, d := range f.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				if name, ok := exportedFuncName(d); ok {
					check(name, d.Type)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch sp := spec.(type) {
					case *ast.TypeSpec: // includes aliases: check the whole RHS
						if !sp.Name.IsExported() {
							continue
						}
						if st, ok := sp.Type.(*ast.StructType); ok {
							for _, fld := range st.Fields.List {
								if len(fld.Names) == 0 { // embedded
									check(sp.Name.Name+".<embedded>", fld.Type)
									continue
								}
								for _, n := range fld.Names {
									if n.IsExported() {
										check(sp.Name.Name+"."+n.Name, fld.Type)
									}
								}
							}
							continue
						}
						check(sp.Name.Name, sp.Type)
					case *ast.ValueSpec: // exported var and const
						for i, n := range sp.Names {
							if !n.IsExported() {
								continue
							}
							if sp.Type != nil {
								check(n.Name, sp.Type)
							}
							if i < len(sp.Values) {
								check(n.Name, sp.Values[i])
							}
						}
					}
				}
			}
		}
	})
	if len(violations) > 0 {
		t.Errorf("the exported API must name standard-library types only:\n\t%s",
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
		"github.com/sirupsen/logrus",
	}
	forEachSourceFile(t, func(path string, f *ast.File) {
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			for _, b := range banned {
				if p == b || strings.HasPrefix(p, b+"/") {
					t.Errorf("%s imports %s", path, p)
				}
			}
		}
	})
}

// forEachSourceFile walks the module's non-test Go files.
func forEachSourceFile(t *testing.T, fn func(path string, f *ast.File)) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return perr
		}
		fn(path, f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// importsOf maps the qualifier a file uses to the import path behind it,
// honouring aliases.
func importsOf(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		name := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		out[name] = path
	}
	return out
}

// qualifiersIn returns the package qualifiers a declaration mentions.
func qualifiersIn(node ast.Node) []string {
	var out []string
	ast.Inspect(node, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok {
				out = append(out, id.Name)
			}
		}
		return true
	})
	return out
}

// isStdlib reports an import path with no module domain in its first segment,
// which is the standard library's shape ("log/slog", "context").
func isStdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

func exportedFuncName(d *ast.FuncDecl) (string, bool) {
	if !d.Name.IsExported() {
		return "", false
	}
	if d.Recv == nil || len(d.Recv.List) != 1 {
		return d.Name.Name, true
	}
	recv := d.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	id, ok := recv.(*ast.Ident)
	if !ok || !id.IsExported() {
		return "", false
	}
	return id.Name + "." + d.Name.Name, true
}
