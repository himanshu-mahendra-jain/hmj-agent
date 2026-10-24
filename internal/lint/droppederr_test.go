package lint

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"slices"
	"strings"
	"testing"
)

const droppedErrSource = `package a

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

type myErr struct{}

func (*myErr) Error() string { return "" }

func fail() error            { return errors.New("x") }
func findErr() (*myErr, bool) { return nil, false }
func failTyped() *myErr      { return nil }
func pair() (int, error)     { return 0, nil }
func three() (int, int, int) { return 0, 0, 0 }

func reported(w io.Writer) {
	_ = fail()             // want
	n, _ := w.Write(nil)   // want
	_, _ = pair()          // want
	_ = failTyped()        // want
	var _ = os.Remove("x") // want
	_, _ = n, fail()       // want
	m, _ := pair()         // want
	_ = m
	fail()                 // want
	w.Write(nil)           // want
	os.Remove("x")         // want
}

func allowed(m map[string]int, v any, ch chan int) {
	_, _ = m["k"]
	_, _ = v.(string)
	_, _ = <-ch
	_, _, _ = strings.Cut("a b", " ")
	_, _, _ = three()
	_ = len(m)
	_, _ = findErr()
	var sb strings.Builder
	sb.WriteString("x")
	var buf bytes.Buffer
	buf.WriteByte('x')
	fmt.Println("x")
	fmt.Fprintf(os.Stderr, "x")
	os.Stdout.WriteString("x")
	strings.ToUpper("x")
}
`

func TestDroppedErrorsReportsOnlyDroppedErrors(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "a.go", droppedErrSource, 0)
	if err != nil {
		t.Fatal(err)
	}
	info := newInfo()
	if _, err := (&types.Config{Importer: importer.Default()}).Check("a", fset, []*ast.File{f}, info); err != nil {
		t.Fatal(err)
	}

	var want []int
	for i, line := range strings.Split(droppedErrSource, "\n") {
		if strings.HasSuffix(line, "// want") {
			want = append(want, i+1)
		}
	}
	found := DroppedErrors(fset, []*ast.File{f}, info)
	var got []int
	for _, finding := range found {
		var line, col int
		if _, err := fmt.Sscanf(finding, "a.go:%d:%d:", &line, &col); err != nil {
			t.Fatalf("finding %q: %v", finding, err)
		}
		got = append(got, line)
	}
	if !slices.Equal(got, want) {
		t.Errorf("findings on lines %v, want %v:\n%s", got, want, strings.Join(found, "\n"))
	}
}

func TestDroppedErrorsInListsModulePackages(t *testing.T) {
	found, err := DroppedErrorsIn("../..", "./internal/lint")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("internal/lint itself drops errors: %v", found)
	}
	if _, err := DroppedErrorsIn("../..", "./does/not/exist"); err == nil {
		t.Error("a pattern that matches nothing type-checks without error")
	}
}
