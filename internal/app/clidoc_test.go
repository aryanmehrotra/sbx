package app

// docs/CLI.md is the one page that lists every command and every `sbx serve` flag. A command
// added to the help and dispatch, but not to that page, passes every other test here and still
// leaves a reader unable to find it - the docs contract in AGENTS.md asks for the row, and this
// is what makes the ask stick.
//
// The command list comes from the top-level help (listedCommands, the same list the help tests
// walk), so a command is checked the moment it is advertised. The serve flags are read from
// the source of internal/daemon/serve.go with go/parser rather than by building the FlagSet:
// Serve builds it inline and then starts the daemon, and extracting it just for a test would
// be a refactor of production code for a docs check.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"testing"
)

const cliDoc = "../../docs/CLI.md"

func readCLIDoc(t *testing.T) string {
	t.Helper()

	b, err := os.ReadFile(cliDoc)
	if err != nil {
		t.Fatalf("read %s: %v", cliDoc, err)
	}

	return string(b)
}

func TestEveryCommandIsInTheCLIReference(t *testing.T) {
	doc := readCLIDoc(t)

	for name := range listedCommands(t) {
		// "`sbx fc" must not be satisfied by "`sbx fork": the name ends at a non-letter.
		re := regexp.MustCompile("`sbx " + regexp.QuoteMeta(name) + `([^a-z-]|$)`)
		if !re.MatchString(doc) {
			t.Errorf("docs/CLI.md has no row for `sbx %s`: add one (docs contract, AGENTS.md)", name)
		}
	}
}

// serveFlags are the names passed to fs.String, fs.Bool, fs.Duration, fs.Var... inside
// daemon.Serve: the first string literal argument of each call on the FlagSet.
func serveFlags(t *testing.T) []string {
	t.Helper()

	const src = "../daemon/serve.go"

	f, err := parser.ParseFile(token.NewFileSet(), src, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}

	var serve *ast.FuncDecl

	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "Serve" {
			serve = fd
		}
	}

	if serve == nil {
		t.Fatalf("%s has no func Serve; update this test to where the serve flags are defined", src)
	}

	// The FlagSet's variable: whatever `X := flag.NewFlagSet("serve", ...)` names it.
	fsVar := ""

	ast.Inspect(serve.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}

		if call, ok := as.Rhs[0].(*ast.CallExpr); ok && isSel(call.Fun, "flag", "NewFlagSet") {
			if id, ok := as.Lhs[0].(*ast.Ident); ok {
				fsVar = id.Name
			}
		}

		return true
	})

	if fsVar == "" {
		t.Fatalf("func Serve in %s builds no flag.NewFlagSet; update this test", src)
	}

	definers := map[string]bool{
		"String": true, "Bool": true, "Int": true, "Int64": true, "Uint": true, "Uint64": true,
		"Float64": true, "Duration": true, "Var": true, "Func": true, "BoolFunc": true, "TextVar": true,
		"StringVar": true, "BoolVar": true, "IntVar": true, "Int64Var": true, "UintVar": true,
		"Uint64Var": true, "Float64Var": true, "DurationVar": true,
	}

	var names []string

	ast.Inspect(serve.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !definers[sel.Sel.Name] {
			return true
		}

		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != fsVar {
			return true
		}

		for _, a := range call.Args {
			if lit, ok := a.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if s, err := strconv.Unquote(lit.Value); err == nil {
					names = append(names, s)
				}

				break
			}
		}

		return true
	})

	if len(names) < 10 {
		t.Fatalf("found only %d serve flags in %s, so this test is not reading it right", len(names), src)
	}

	return names
}

func isSel(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}

	id, ok := sel.X.(*ast.Ident)

	return ok && id.Name == pkg
}

func TestEveryServeFlagIsInTheCLIReference(t *testing.T) {
	doc := readCLIDoc(t)

	for _, name := range serveFlags(t) {
		// "`--osb-pool`" or "`--osb-pool IMAGE[=N]`", but not "`--osb-pool-freeze`".
		re := regexp.MustCompile("`--" + regexp.QuoteMeta(name) + "[` =]")
		if !re.MatchString(doc) {
			t.Errorf("docs/CLI.md's sbx serve table has no row for `--%s` (flag in internal/daemon/serve.go)", name)
		}
	}
}
