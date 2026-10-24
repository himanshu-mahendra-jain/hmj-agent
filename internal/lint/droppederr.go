// Package lint holds hmj's own static checks, built on the standard library alone.
package lint

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var errorType = types.Universe.Lookup("error").Type().Underlying().(*types.Interface)

// DroppedErrors returns "file:line:col: ..." for each error that files drop: one assigned to
// the blank identifier, as in `_ = f()` or `n, _ := w.Write(p)`, or one returned by a call
// made as a statement, as in `f.Close()`. Comma-ok forms drop a bool or a typed value, not a
// failure, and are not reported, nor are calls that cannot fail in practice: fmt's Print and
// Fprint functions, writes to os.Stdout and os.Stderr, and strings.Builder and bytes.Buffer
// methods, which are documented to return a nil error. info needs Types, Uses and Selections.
func DroppedErrors(fset *token.FileSet, files []*ast.File, info *types.Info) []string {
	var found []string
	report := func(lhs, rhs []ast.Expr) {
		for i, l := range lhs {
			if id, ok := l.(*ast.Ident); !ok || id.Name != "_" {
				continue
			}
			if t := valueType(info, rhs, len(lhs), i); t != nil && types.Implements(t, errorType) {
				found = append(found, fmt.Sprintf("%s: error assigned to _", fset.Position(l.Pos())))
			}
		}
	}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				report(n.Lhs, n.Rhs)
			case *ast.ValueSpec:
				lhs := make([]ast.Expr, len(n.Names))
				for i, name := range n.Names {
					lhs[i] = name
				}
				report(lhs, n.Values)
			case *ast.ExprStmt:
				if call, ok := n.X.(*ast.CallExpr); ok && returnsError(info, call) && !cannotFail(info, call) {
					found = append(found, fmt.Sprintf("%s: error from %s not checked", fset.Position(call.Pos()), types.ExprString(call.Fun)))
				}
			}
			return true
		})
	}
	return found
}

// valueType returns the type of the i-th of n values that rhs produces: one value per
// expression, or the results of a single multi-value call. It returns nil for a comma-ok
// call, one whose last result is a bool, such as errors.AsType.
func valueType(info *types.Info, rhs []ast.Expr, n, i int) types.Type {
	if len(rhs) == n {
		return info.TypeOf(rhs[i])
	}
	if len(rhs) != 1 {
		return nil
	}
	tuple, ok := info.TypeOf(rhs[0]).(*types.Tuple)
	if !ok || i >= tuple.Len() {
		return nil
	}
	if b, ok := tuple.At(tuple.Len() - 1).Type().(*types.Basic); ok && b.Kind() == types.Bool {
		return nil
	}
	return tuple.At(i).Type()
}

// returnsError reports whether call's last result is an error.
func returnsError(info *types.Info, call *ast.CallExpr) bool {
	t := info.TypeOf(call)
	if tuple, ok := t.(*types.Tuple); ok {
		if tuple.Len() == 0 {
			return false
		}
		t = tuple.At(tuple.Len() - 1).Type()
	}
	return t != nil && types.Implements(t, errorType)
}

// cannotFail reports whether call is one whose error DroppedErrors does not ask for.
func cannotFail(info *types.Info, call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if fn, ok := info.Uses[sel.Sel].(*types.Func); ok && fn.Pkg() != nil && fn.Pkg().Path() == "fmt" &&
		(strings.HasPrefix(fn.Name(), "Print") || strings.HasPrefix(fn.Name(), "Fprint")) {
		return true
	}
	if x, ok := sel.X.(*ast.SelectorExpr); ok {
		if v, ok := info.Uses[x.Sel].(*types.Var); ok && v.Pkg() != nil && v.Pkg().Path() == "os" && (v.Name() == "Stdout" || v.Name() == "Stderr") {
			return true
		}
	}
	s, ok := info.Selections[sel]
	if !ok {
		return false
	}
	recv := s.Recv()
	if p, ok := recv.(*types.Pointer); ok {
		recv = p.Elem()
	}
	named, ok := recv.(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	switch named.Obj().Pkg().Path() + "." + named.Obj().Name() {
	case "strings.Builder", "bytes.Buffer":
		return true
	}
	return false
}

// listedPackage is the part of `go list -json` output DroppedErrorsIn reads.
type listedPackage struct {
	ImportPath string
	Dir        string
	GoFiles    []string
	Export     string
	DepOnly    bool
	Standard   bool
	Error      *struct{ Err string }
}

// DroppedErrorsIn runs DroppedErrors over the non-test files of the packages that patterns
// name, resolved in dir. Imports are read from the export data `go list -export` builds.
func DroppedErrorsIn(dir string, patterns ...string) ([]string, error) {
	cmd := exec.Command("go", append([]string{"list", "-e", "-export", "-deps", "-json=ImportPath,Dir,GoFiles,Export,DepOnly,Standard,Error"}, patterns...)...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	exports := map[string]string{}
	var targets []listedPackage
	dec := jsontext.NewDecoder(bytes.NewReader(out))
	for {
		var p listedPackage
		if err := json.UnmarshalDecode(dec, &p); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("go list output: %w", err)
		}
		if p.Error != nil {
			return nil, fmt.Errorf("%s: %s", p.ImportPath, p.Error.Err)
		}
		exports[p.ImportPath] = p.Export
		if !p.DepOnly && !p.Standard {
			targets = append(targets, p)
		}
	}

	fset := token.NewFileSet()
	imp := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		if exports[path] == "" {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(exports[path])
	})
	var found []string
	for _, p := range targets {
		var files []*ast.File
		for _, name := range p.GoFiles {
			f, err := parser.ParseFile(fset, filepath.Join(p.Dir, name), nil, parser.SkipObjectResolution)
			if err != nil {
				return nil, err
			}
			files = append(files, f)
		}
		info := newInfo()
		if _, err := (&types.Config{Importer: imp}).Check(p.ImportPath, fset, files, info); err != nil {
			return nil, fmt.Errorf("type-checking %s: %w", p.ImportPath, err)
		}
		found = append(found, DroppedErrors(fset, files, info)...)
	}
	return found, nil
}

// newInfo returns the type information DroppedErrors needs.
func newInfo() *types.Info {
	return &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
}
