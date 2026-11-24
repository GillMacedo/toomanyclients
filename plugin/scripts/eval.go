package main

import (
	"bytes"
	"go/ast"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

// pkg is the files of one Go package: one folder and one package name.
type pkg struct {
	dir        string
	name       string
	importPath string // empty when no go.mod was found
	modPath    string
	files      []*sourceFile
	// decls holds package-level constants, and variables that are never
	// assigned again, by name.
	decls    map[string]ast.Expr
	declFile map[string]*sourceFile
	imports  map[string]bool
}

// funcInfo is one function or method declaration.
type funcInfo struct {
	decl *ast.FuncDecl
	file *sourceFile
	pkg  *pkg
}

// program is every package in the input, with the lookups the evaluator
// needs. It isn't one Go program: main packages are found later.
type program struct {
	fset     *token.FileSet
	pkgs     []*pkg
	byImport map[string]*pkg
	pkgOf    map[*sourceFile]*pkg
	funcs    map[string][]*funcInfo // by name, across packages
}

func newProgram(fset *token.FileSet, files []*sourceFile) *program {
	prog := &program{fset: fset, byImport: map[string]*pkg{}, pkgOf: map[*sourceFile]*pkg{}, funcs: map[string][]*funcInfo{}}
	index := map[string]*pkg{}
	modCache := map[string][2]string{}
	for _, f := range files {
		key := f.dir + "\x00" + f.ast.Name.Name
		p := index[key]
		if p == nil {
			p = &pkg{dir: f.dir, name: f.ast.Name.Name, decls: map[string]ast.Expr{}, declFile: map[string]*sourceFile{}, imports: map[string]bool{}}
			if root, mod := findModule(f.dir, modCache); mod != "" {
				p.modPath = mod
				if rel, err := filepath.Rel(root, f.dir); err == nil {
					if rel == "." {
						p.importPath = mod
					} else {
						p.importPath = mod + "/" + filepath.ToSlash(rel)
					}
				}
			}
			index[key] = p
			prog.pkgs = append(prog.pkgs, p)
			if p.importPath != "" {
				prog.byImport[p.importPath] = p
			}
		}
		p.files = append(p.files, f)
		prog.pkgOf[f] = p
		for _, imp := range f.ast.Imports {
			p.imports[strings.Trim(imp.Path.Value, `"`)] = true
		}
		for _, d := range f.ast.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				prog.funcs[fd.Name.Name] = append(prog.funcs[fd.Name.Name], &funcInfo{decl: fd, file: f, pkg: p})
			}
		}
	}
	for _, p := range prog.pkgs {
		p.collectDecls()
	}
	return prog
}

// findModule returns the folder of the nearest go.mod above dir and its
// module path.
func findModule(dir string, cache map[string][2]string) (string, string) {
	if v, ok := cache[dir]; ok {
		return v[0], v[1]
	}
	var root, mod string
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "module") {
				mod = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "module")), `"`)
				root = dir
				break
			}
		}
	} else if parent := filepath.Dir(dir); parent != dir {
		root, mod = findModule(parent, cache)
	}
	cache[dir] = [2]string{root, mod}
	return root, mod
}

func (p *pkg) collectDecls() {
	assigned := map[string]bool{}
	for _, f := range p.files {
		ast.Inspect(f.ast, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				if n.Tok != token.DEFINE {
					for _, l := range n.Lhs {
						if id, ok := l.(*ast.Ident); ok {
							assigned[id.Name] = true
						}
					}
				}
			case *ast.UnaryExpr:
				if id, ok := n.X.(*ast.Ident); ok && n.Op == token.AND {
					assigned[id.Name] = true
				}
			}
			return true
		})
	}
	for _, f := range p.files {
		for _, d := range f.ast.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || (gd.Tok != token.CONST && gd.Tok != token.VAR) {
				continue
			}
			for _, s := range gd.Specs {
				vs := s.(*ast.ValueSpec)
				if len(vs.Values) != len(vs.Names) {
					continue
				}
				for i, name := range vs.Names {
					if gd.Tok == token.VAR && assigned[name.Name] {
						continue
					}
					p.decls[name.Name] = vs.Values[i]
					p.declFile[name.Name] = f
				}
			}
		}
	}
}

func importPathOf(f *ast.File, name string) string {
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		local := path[strings.LastIndex(path, "/")+1:]
		if imp.Name != nil {
			local = imp.Name.Name
		} else if isMajorVersion(local) {
			// github.com/jackc/pgx/v5/pgxpool is pgxpool, but
			// github.com/jackc/pgx/v5 is pgx.
			trimmed := path[:strings.LastIndex(path, "/")]
			local = trimmed[strings.LastIndex(trimmed, "/")+1:]
		} else if strings.HasPrefix(local, "go-") {
			local = strings.TrimPrefix(local, "go-")
		}
		if local == name {
			return path
		}
	}
	return ""
}

func isMajorVersion(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	_, err := strconv.Atoi(s[1:])
	return err == nil
}

// pkgCall returns the import path and function name of a call such as
// sql.Open(...), or "" when the call isn't to a package-level function.
func pkgCall(call *ast.CallExpr, f *ast.File) (string, string) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", ""
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", ""
	}
	return importPathOf(f, id.Name), sel.Sel.Name
}

// scope is where an expression appears: its file and the enclosing
// function, if any.
type scope struct {
	file *sourceFile
	fn   *ast.FuncDecl
	lit  *ast.FuncLit // innermost function literal, if any
	// args binds the parameters of fn when it is being evaluated for one
	// call, so a small helper's result follows that call's arguments.
	args map[string]boundArg
}

type boundArg struct {
	expr ast.Expr
	sc   scope
}

func (sc scope) body() *ast.BlockStmt {
	if sc.lit != nil {
		return sc.lit.Body
	}
	if sc.fn != nil {
		return sc.fn.Body
	}
	return nil
}

func (sc scope) params() map[string]int {
	var t *ast.FuncType
	if sc.lit != nil {
		t = sc.lit.Type
	} else if sc.fn != nil {
		t = sc.fn.Type
	}
	out := map[string]int{}
	if t == nil || t.Params == nil {
		return out
	}
	i := 0
	for _, field := range t.Params.List {
		if len(field.Names) == 0 {
			i++
			continue
		}
		for _, n := range field.Names {
			out[n.Name] = i
			i++
		}
	}
	return out
}

// amtKind says where a number comes from.
type amtKind int

const (
	amtKnown   amtKind = iota // a constant in the source
	amtEnv                    // an environment variable, maybe with a default
	amtFlag                   // a command-line flag, maybe with a default
	amtUnknown                // anything else: config files, computed values
)

// amount is the evaluated value of an expression, or how to find it in a
// container's environment or arguments.
type amount struct {
	kind amtKind
	n    int64
	name string // environment variable or flag name
	def  *amount
	// suffix means the variable may carry a prefix, as kelseyhightower
	// envconfig adds one: any variable ending in _name matches.
	suffix bool
	// required means the program refuses to start without the variable,
	// so no other value is a fallback.
	required bool
	why      string // for amtUnknown: the expression and where it comes from
	note     string // extra detail, such as "highest of 2 values"
}

func known(n int64) amount { return amount{kind: amtKnown, n: n} }

func unknownAmt(why string) amount { return amount{kind: amtUnknown, why: why} }

const maxDepth = 12

// evalInt evaluates an integer expression.
func (prog *program) evalInt(e ast.Expr, sc scope, depth int) amount {
	if depth > maxDepth {
		return unknownAmt(prog.text(e) + " (too many levels to follow)")
	}
	switch e := e.(type) {
	case *ast.ParenExpr:
		return prog.evalInt(e.X, sc, depth+1)
	case *ast.BasicLit:
		if e.Kind == token.INT {
			if n, err := strconv.ParseInt(strings.ReplaceAll(e.Value, "_", ""), 0, 64); err == nil {
				return known(n)
			}
		}
		if e.Kind == token.STRING {
			if s, err := strconv.Unquote(e.Value); err == nil {
				if n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
					return known(n)
				}
			}
		}
	case *ast.UnaryExpr:
		v := prog.evalInt(e.X, sc, depth+1)
		if v.kind != amtKnown {
			return v
		}
		switch e.Op {
		case token.SUB:
			return known(-v.n)
		case token.ADD:
			return v
		}
	case *ast.StarExpr:
		return prog.evalPointer(e.X, sc, depth+1)
	case *ast.BinaryExpr:
		x := prog.evalInt(e.X, sc, depth+1)
		y := prog.evalInt(e.Y, sc, depth+1)
		// v & math.MaxInt32 keeps any pool size as it is.
		if e.Op == token.AND && y.kind == amtKnown && y.n >= 1<<16-1 && y.n&(y.n+1) == 0 && x.kind != amtUnknown {
			return x
		}
		if x.kind != amtKnown || y.kind != amtKnown {
			bad := x
			if x.kind == amtKnown {
				bad = y
			}
			return unknownAmt(prog.text(e) + " (computed from " + bad.describe() + ")")
		}
		switch e.Op {
		case token.AND:
			return known(x.n & y.n)
		case token.MUL:
			return known(x.n * y.n)
		case token.ADD:
			return known(x.n + y.n)
		case token.SUB:
			return known(x.n - y.n)
		case token.QUO:
			if y.n != 0 {
				return known(x.n / y.n)
			}
		}
	case *ast.CallExpr:
		return prog.evalIntCall(e, sc, depth)
	case *ast.SelectorExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			if path := importPathOf(sc.file.ast, id.Name); path != "" {
				if path == "math" {
					if n, ok := mathConsts[e.Sel.Name]; ok {
						return known(n)
					}
				}
				if other := prog.byImport[path]; other != nil {
					if expr, ok := other.decls[e.Sel.Name]; ok {
						return prog.evalInt(expr, scope{file: other.declFile[e.Sel.Name]}, depth+1)
					}
				}
				return unknownAmt(prog.text(e) + " (from package " + path + ", which isn't in the input or isn't a constant)")
			}
		}
		return prog.evalField(e, sc, depth+1)
	case *ast.Ident:
		return prog.evalIdent(e, sc, depth, prog.evalInt)
	}
	return unknownAmt(prog.text(e))
}

var mathConsts = map[string]int64{
	"MaxInt8": 1<<7 - 1, "MaxInt16": 1<<15 - 1, "MaxInt32": 1<<31 - 1, "MaxInt64": 1<<63 - 1, "MaxInt": 1<<63 - 1,
	"MaxUint8": 1<<8 - 1, "MaxUint16": 1<<16 - 1, "MaxUint32": 1<<32 - 1,
}

type evalFunc func(ast.Expr, scope, int) amount

// evalIdent follows a name to its local definitions, the enclosing
// function's callers, or a package-level constant.
func (prog *program) evalIdent(e *ast.Ident, sc scope, depth int, eval evalFunc) amount {
	if i, ok := sc.params()[e.Name]; ok {
		if b, ok := sc.args[e.Name]; ok && sc.lit == nil {
			return eval(b.expr, b.sc, depth+1)
		}
		return prog.evalParam(e.Name, i, sc, depth, eval)
	}
	if body := sc.body(); body != nil {
		defs, addrTaken := localDefs(body, e.Name)
		if addrTaken {
			return unknownAmt(e.Name + " (its address is taken, so anything can set it)")
		}
		if len(defs) > 0 {
			var vals []amount
			for _, d := range defs {
				if d == nil {
					return unknownAmt(e.Name + " (declared without a value this tool can read)")
				}
				vals = append(vals, eval(d, sc, depth+1))
			}
			return combine(vals, e.Name)
		}
		if sc.lit != nil && sc.fn != nil {
			return prog.evalIdent(e, scope{file: sc.file, fn: sc.fn, args: sc.args}, depth+1, eval)
		}
	}
	if p := prog.pkgOf[sc.file]; p != nil {
		if expr, ok := p.decls[e.Name]; ok {
			return eval(expr, scope{file: p.declFile[e.Name]}, depth+1)
		}
	}
	return unknownAmt(e.Name + " (not a constant this tool can read)")
}

// evalParam evaluates a function parameter from every call of the
// function in the input.
func (prog *program) evalParam(name string, index int, sc scope, depth int, eval evalFunc) amount {
	if sc.lit != nil || sc.fn == nil {
		return unknownAmt(name + " (a parameter of a function literal)")
	}
	calls := prog.callsOf(sc.fn, prog.pkgOf[sc.file])
	if len(calls) == 0 {
		return unknownAmt(name + " (a parameter of " + sc.fn.Name.Name + ", which is never called in these files)")
	}
	var vals []amount
	for _, c := range calls {
		if index >= len(c.call.Args) || c.call.Ellipsis != token.NoPos {
			return unknownAmt(name + " (a parameter of " + sc.fn.Name.Name + ")")
		}
		vals = append(vals, eval(c.call.Args[index], c.sc, depth+1))
	}
	return combine(vals, name)
}

// combine merges the values one name can have. Constants give their
// highest value, since the budget must hold in the worst case. One
// environment variable or flag with constants for its fallback gives that
// variable with the highest constant as its default.
func combine(vals []amount, name string) amount {
	if len(vals) == 1 {
		return vals[0]
	}
	var best *amount
	var source *amount
	for i := range vals {
		v := vals[i]
		switch v.kind {
		case amtUnknown:
			return v
		case amtKnown:
			if best == nil || v.n > best.n {
				best = &vals[i]
			}
		default:
			if source != nil && (source.kind != v.kind || source.name != v.name) {
				return unknownAmt(name + " (set from more than one variable or flag)")
			}
			source = &vals[i]
		}
	}
	if source == nil {
		out := *best
		out.note = "highest of " + strconv.Itoa(len(vals)) + " values"
		return out
	}
	out := *source
	if best != nil && !out.required {
		if out.def == nil || out.def.kind != amtKnown || out.def.n < best.n {
			d := *best
			d.note = ""
			out.def = &d
		}
	}
	return out
}

func (prog *program) evalIntCall(e *ast.CallExpr, sc scope, depth int) amount {
	if len(e.Args) == 1 && isConversion(e.Fun) {
		return prog.evalInt(e.Args[0], sc, depth+1)
	}
	path, name := pkgCall(e, sc.file.ast)
	switch {
	case path == "strconv" && (name == "Atoi" || name == "ParseInt" || name == "ParseUint") && len(e.Args) >= 1:
		return prog.evalStr(e.Args[0], sc, depth+1)
	case (path == "flag" || path == "github.com/spf13/pflag") && flagFuncs[name] > 0:
		return prog.flagAmount(e, flagFuncs[name], sc, depth)
	}
	if h, i := prog.envHelper(e, sc); h != nil {
		return prog.helperAmount(e, h, i, sc, depth)
	}
	if fi := prog.funcFor(e, sc); fi != nil {
		if ret := finalReturn(fi.decl); ret != nil {
			params := scope{fn: fi.decl}.params()
			if len(params) == len(e.Args) && e.Ellipsis == token.NoPos {
				args := map[string]boundArg{}
				for name, i := range params {
					args[name] = boundArg{e.Args[i], sc}
				}
				return prog.evalInt(ret, scope{file: fi.file, fn: fi.decl, args: args}, depth+1)
			}
		}
	}
	return unknownAmt(prog.text(e) + " (a function result)")
}

// funcFor finds the declaration of a function called by name, f(...) in
// the same package or pkg.f(...) from another package of the input.
func (prog *program) funcFor(call *ast.CallExpr, sc scope) *funcInfo {
	var name string
	want := prog.pkgOf[sc.file]
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		name = fun.Name
	case *ast.SelectorExpr:
		id, ok := fun.X.(*ast.Ident)
		if !ok {
			return nil
		}
		path := importPathOf(sc.file.ast, id.Name)
		if path == "" {
			return nil
		}
		if want = prog.byImport[path]; want == nil {
			if want = prog.bySuffix(path); want == nil {
				return nil
			}
		}
		name = fun.Sel.Name
	default:
		return nil
	}
	for _, fi := range prog.funcs[name] {
		if fi.pkg == want && fi.decl.Recv == nil {
			return fi
		}
	}
	return nil
}

// finalReturn is the single result of a function's last statement, for
// small helpers such as conversions and clamps.
func finalReturn(fd *ast.FuncDecl) ast.Expr {
	if fd.Body == nil || len(fd.Body.List) == 0 || len(fd.Body.List) > 6 {
		return nil
	}
	ret, ok := fd.Body.List[len(fd.Body.List)-1].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return nil
	}
	return ret.Results[0]
}

// evalStr evaluates an expression that yields the text of a number, such
// as os.Getenv("DB_MAX_CONNS").
func (prog *program) evalStr(e ast.Expr, sc scope, depth int) amount {
	if depth > maxDepth {
		return unknownAmt(prog.text(e) + " (too many levels to follow)")
	}
	switch e := e.(type) {
	case *ast.ParenExpr:
		return prog.evalStr(e.X, sc, depth+1)
	case *ast.BasicLit:
		return prog.evalInt(e, sc, depth+1)
	case *ast.Ident:
		return prog.evalIdent(e, sc, depth, prog.evalStr)
	case *ast.CallExpr:
		path, name := pkgCall(e, sc.file.ast)
		if path == "os" && (name == "Getenv" || name == "LookupEnv") && len(e.Args) == 1 {
			key := prog.evalString(e.Args[0], sc, depth+1)
			if key == "" {
				return unknownAmt(prog.text(e) + " (the variable name isn't a constant)")
			}
			return amount{kind: amtEnv, name: key}
		}
		if path == "strings" && name == "TrimSpace" && len(e.Args) == 1 {
			return prog.evalStr(e.Args[0], sc, depth+1)
		}
		if path == "strconv" && name == "Itoa" && len(e.Args) == 1 {
			return prog.evalInt(e.Args[0], sc, depth+1)
		}
		if h, i := prog.envHelper(e, sc); h != nil {
			return prog.helperAmount(e, h, i, sc, depth)
		}
		return unknownAmt(prog.text(e) + " (a function result)")
	case *ast.SelectorExpr:
		return prog.evalInt(e, sc, depth+1)
	}
	return unknownAmt(prog.text(e))
}

// evalString returns a constant string, or "".
func (prog *program) evalString(e ast.Expr, sc scope, depth int) string {
	if depth > maxDepth {
		return ""
	}
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING {
			s, err := strconv.Unquote(e.Value)
			if err == nil {
				return s
			}
		}
	case *ast.ParenExpr:
		return prog.evalString(e.X, sc, depth+1)
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			x := prog.evalString(e.X, sc, depth+1)
			y := prog.evalString(e.Y, sc, depth+1)
			if x != "" && y != "" {
				return x + y
			}
		}
	case *ast.Ident:
		if body := sc.body(); body != nil {
			defs, addr := localDefs(body, e.Name)
			if addr || len(defs) > 1 {
				return ""
			}
			if len(defs) == 1 && defs[0] != nil {
				return prog.evalString(defs[0], sc, depth+1)
			}
		}
		if p := prog.pkgOf[sc.file]; p != nil {
			if expr, ok := p.decls[e.Name]; ok {
				return prog.evalString(expr, scope{file: p.declFile[e.Name]}, depth+1)
			}
		}
	case *ast.SelectorExpr:
		if id, ok := e.X.(*ast.Ident); ok {
			if other := prog.byImport[importPathOf(sc.file.ast, id.Name)]; other != nil {
				if expr, ok := other.decls[e.Sel.Name]; ok {
					return prog.evalString(expr, scope{file: other.declFile[e.Sel.Name]}, depth+1)
				}
			}
		}
	}
	return ""
}

// flagFuncs maps flag constructors to the index of their default value
// argument plus one. Var forms take a pointer first.
var flagFuncs = map[string]int{
	"Int": 2, "Int64": 2, "Uint": 2, "Uint64": 2, "Int32": 2,
	"IntP": 3, "Int64P": 3, "Int32P": 3, "UintP": 3,
	"IntVar": 3, "Int64Var": 3, "UintVar": 3, "Uint64Var": 3, "Int32Var": 3,
	"IntVarP": 4, "Int64VarP": 4, "Int32VarP": 4, "UintVarP": 4,
}

// flagAmount builds the amount for a flag definition call.
func (prog *program) flagAmount(call *ast.CallExpr, defPlusOne int, sc scope, depth int) amount {
	name := ""
	nameIdx := 0
	if strings.Contains(prog.text(call.Fun), "Var") {
		nameIdx = 1
	}
	if len(call.Args) < defPlusOne {
		return unknownAmt(prog.text(call) + " (a flag)")
	}
	name = prog.evalString(call.Args[nameIdx], sc, depth+1)
	if name == "" {
		return unknownAmt(prog.text(call) + " (a flag whose name isn't a constant)")
	}
	def := prog.evalInt(call.Args[defPlusOne-1], sc, depth+1)
	return amount{kind: amtFlag, name: name, def: &def}
}

// evalPointer evaluates *p where p comes from flag.Int and similar.
func (prog *program) evalPointer(e ast.Expr, sc scope, depth int) amount {
	var defs []ast.Expr
	switch x := e.(type) {
	case *ast.Ident:
		if body := sc.body(); body != nil {
			d, _ := localDefs(body, x.Name)
			defs = d
		}
		if len(defs) == 0 {
			if p := prog.pkgOf[sc.file]; p != nil {
				if expr, ok := p.decls[x.Name]; ok {
					defs = []ast.Expr{expr}
					sc = scope{file: p.declFile[x.Name]}
				}
			}
		}
	case *ast.SelectorExpr:
		// cfg.MaxConns where the field is *int set by flag.Int.
		return prog.evalField(x, sc, depth+1)
	}
	if len(defs) != 1 || defs[0] == nil {
		return unknownAmt("*" + prog.text(e) + " (a pointer this tool can't follow)")
	}
	if call, ok := defs[0].(*ast.CallExpr); ok {
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if n := flagFuncs[sel.Sel.Name]; n > 0 && !strings.Contains(sel.Sel.Name, "Var") {
				return prog.flagAmount(call, n, sc, depth)
			}
		}
	}
	return unknownAmt("*" + prog.text(e) + " (a pointer this tool can't follow)")
}

// envHelper finds a function in the input that reads an environment
// variable named by one of its parameters, such as getEnvInt("X", 10). It
// returns the function and the index of the name parameter.
func (prog *program) envHelper(call *ast.CallExpr, sc scope) (*funcInfo, int) {
	var name string
	var want *pkg
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		name = fun.Name
		want = prog.pkgOf[sc.file]
	case *ast.SelectorExpr:
		name = fun.Sel.Name
		if id, ok := fun.X.(*ast.Ident); ok {
			if path := importPathOf(sc.file.ast, id.Name); path != "" {
				want = prog.byImport[path]
				if want == nil {
					return nil, -1
				}
			}
		}
	default:
		return nil, -1
	}
	for _, fi := range prog.funcs[name] {
		if want != nil && fi.pkg != want {
			continue
		}
		fsc := scope{file: fi.file, fn: fi.decl}
		params := fsc.params()
		found := -1
		ast.Inspect(fi.decl.Body, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok || found >= 0 {
				return found < 0
			}
			path, fn := pkgCall(c, fi.file.ast)
			if path == "os" && (fn == "Getenv" || fn == "LookupEnv") && len(c.Args) == 1 {
				if id, ok := c.Args[0].(*ast.Ident); ok {
					if i, ok := params[id.Name]; ok {
						found = i
					}
				}
			}
			return true
		})
		if found >= 0 {
			return fi, found
		}
	}
	return nil, -1
}

func (prog *program) helperAmount(call *ast.CallExpr, h *funcInfo, keyIdx int, sc scope, depth int) amount {
	if keyIdx >= len(call.Args) {
		return unknownAmt(prog.text(call))
	}
	key := prog.evalString(call.Args[keyIdx], sc, depth+1)
	if key == "" {
		return unknownAmt(prog.text(call) + " (the variable name isn't a constant)")
	}
	out := amount{kind: amtEnv, name: key}
	// The default is the other argument, when there is exactly one more.
	if len(call.Args) == 2 {
		d := prog.evalInt(call.Args[1-keyIdx], sc, depth+1)
		if d.kind == amtUnknown {
			d = prog.evalStr(call.Args[1-keyIdx], sc, depth+1)
		}
		out.def = &d
	} else if len(call.Args) > 2 {
		return unknownAmt(prog.text(call) + " (a helper with more than one other argument)")
	}
	return out
}

// evalField evaluates a struct field such as cfg.DB.MaxOpenConns: from a
// flag bound to it, its env struct tags, or the constants assigned to it.
func (prog *program) evalField(e *ast.SelectorExpr, sc scope, depth int) amount {
	field := e.Sel.Name
	if depth > maxDepth {
		return unknownAmt(prog.text(e) + " (too many levels to follow)")
	}
	var vals []amount
	for _, p := range prog.pkgs {
		for _, f := range p.files {
			walkScopes(f, func(n ast.Node, fsc scope) {
				switch n := n.(type) {
				case *ast.CallExpr:
					sel, ok := n.Fun.(*ast.SelectorExpr)
					if !ok || flagFuncs[sel.Sel.Name] == 0 || !strings.Contains(sel.Sel.Name, "Var") || len(n.Args) == 0 {
						return
					}
					if u, ok := n.Args[0].(*ast.UnaryExpr); ok && u.Op == token.AND {
						if s, ok := u.X.(*ast.SelectorExpr); ok && s.Sel.Name == field {
							vals = append(vals, prog.flagAmount(n, flagFuncs[sel.Sel.Name], fsc, depth+1))
						}
					}
				case *ast.AssignStmt:
					if len(n.Lhs) != len(n.Rhs) {
						return
					}
					for i, l := range n.Lhs {
						if s, ok := l.(*ast.SelectorExpr); ok && s.Sel.Name == field && n.Tok == token.ASSIGN {
							vals = append(vals, prog.evalInt(n.Rhs[i], fsc, depth+1))
						}
					}
				case *ast.KeyValueExpr:
					if k, ok := n.Key.(*ast.Ident); ok && k.Name == field {
						vals = append(vals, prog.evalInt(n.Value, fsc, depth+1))
					}
				case *ast.Field:
					for _, nm := range n.Names {
						if nm.Name == field && n.Tag != nil {
							if a, ok := tagAmount(n.Tag.Value); ok {
								vals = append(vals, a)
							}
						}
					}
				}
			})
		}
	}
	if len(vals) == 0 {
		return unknownAmt(prog.text(e) + " (a field set at run time, such as from a config file)")
	}
	return combine(vals, prog.text(e))
}

// tagAmount reads environment variable struct tags: caarlos0/env
// (env, envDefault), sethvargo/go-envconfig (env:"X, default=N"),
// ilyakaznacheev/cleanenv (env-default) and kelseyhightower/envconfig
// (envconfig, default).
func tagAmount(raw string) (amount, bool) {
	s, err := strconv.Unquote(raw)
	if err != nil {
		return amount{}, false
	}
	tag := reflect.StructTag(s)
	if v, ok := tag.Lookup("envconfig"); ok {
		name, _, _ := strings.Cut(v, ",")
		out := amount{kind: amtEnv, name: strings.TrimSpace(name), suffix: true}
		if d, ok := tag.Lookup("default"); ok {
			out.def = defAmount(d)
		}
		if r, _ := tag.Lookup("required"); r == "true" {
			out.required = true
		}
		return out, out.name != ""
	}
	v, ok := tag.Lookup("env")
	if !ok {
		return amount{}, false
	}
	parts := strings.Split(v, ",")
	out := amount{kind: amtEnv, name: strings.TrimSpace(parts[0])}
	if out.name == "" || out.name == "-" {
		return amount{}, false
	}
	for _, p := range parts[1:] {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "default=") {
			out.def = defAmount(strings.TrimPrefix(p, "default="))
		}
		if p == "required" {
			out.required = true
		}
	}
	for _, k := range []string{"envDefault", "env-default"} {
		if d, ok := tag.Lookup(k); ok {
			out.def = defAmount(d)
		}
	}
	if r, _ := tag.Lookup("env-required"); r == "true" {
		out.required = true
	}
	return out, true
}

func defAmount(s string) *amount {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		a := unknownAmt("default " + strconv.Quote(s) + " (not a number)")
		return &a
	}
	a := known(n)
	return &a
}

var conversionTypes = map[string]bool{
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
}

func isConversion(fun ast.Expr) bool {
	switch fun := fun.(type) {
	case *ast.Ident:
		return conversionTypes[fun.Name]
	case *ast.ParenExpr:
		return isConversion(fun.X)
	}
	return false
}

// localDefs finds every assignment to name inside a function body. A
// declaration without a value gives a nil entry. addrTaken is true when
// &name appears, so anything could change it.
func localDefs(body *ast.BlockStmt, name string) (defs []ast.Expr, addrTaken bool) {
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return true
		case *ast.AssignStmt:
			for i, l := range n.Lhs {
				id, isID := l.(*ast.Ident)
				if !isID || id.Name != name {
					continue
				}
				switch {
				case n.Tok != token.DEFINE && n.Tok != token.ASSIGN:
					defs = append(defs, nil)
				case len(n.Rhs) == len(n.Lhs):
					defs = append(defs, n.Rhs[i])
				case len(n.Rhs) == 1 && i == 0:
					// n, err := strconv.Atoi(...): the first result.
					defs = append(defs, n.Rhs[0])
				default:
					defs = append(defs, nil)
				}
			}
		case *ast.ValueSpec:
			for i, id := range n.Names {
				if id.Name != name {
					continue
				}
				if len(n.Values) == len(n.Names) {
					defs = append(defs, n.Values[i])
				} else if len(n.Values) == 1 && i == 0 {
					defs = append(defs, n.Values[0])
				} else {
					defs = append(defs, nil)
				}
			}
		case *ast.UnaryExpr:
			if id, isID := n.X.(*ast.Ident); isID && n.Op == token.AND && id.Name == name {
				addrTaken = true
			}
		case *ast.RangeStmt:
			for _, x := range []ast.Expr{n.Key, n.Value} {
				if id, ok := x.(*ast.Ident); ok && id.Name == name {
					defs = append(defs, nil)
				}
			}
		}
		return true
	})
	return defs, addrTaken
}

// walkScopes calls visit for every node in a file with its scope.
func walkScopes(f *sourceFile, visit func(ast.Node, scope)) {
	for _, d := range f.ast.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok {
			ast.Inspect(d, func(n ast.Node) bool {
				if n != nil {
					visit(n, scope{file: f})
				}
				return true
			})
			continue
		}
		if fd.Body == nil {
			continue
		}
		var walk func(n ast.Node, sc scope)
		walk = func(n ast.Node, sc scope) {
			ast.Inspect(n, func(m ast.Node) bool {
				if m == nil {
					return false
				}
				if lit, ok := m.(*ast.FuncLit); ok && m != n {
					inner := sc
					inner.lit = lit
					walk(lit, inner)
					return false
				}
				visit(m, sc)
				return true
			})
		}
		walk(fd, scope{file: f, fn: fd})
	}
}

// callSite is one call of a function, with its scope.
type callSite struct {
	call *ast.CallExpr
	sc   scope
	pkg  *pkg
}

// callsOf finds calls of a function or method declared in pkg p: f(...)
// in p, pkgname.f(...) in packages that import p, and x.f(...) anywhere
// for methods.
func (prog *program) callsOf(fd *ast.FuncDecl, p *pkg) []callSite {
	var out []callSite
	name := fd.Name.Name
	isMethod := fd.Recv != nil
	for _, q := range prog.pkgs {
		for _, f := range q.files {
			walkScopes(f, func(n ast.Node, sc scope) {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return
				}
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					if !isMethod && q == p && fun.Name == name {
						out = append(out, callSite{call, sc, q})
					}
				case *ast.SelectorExpr:
					if fun.Sel.Name != name {
						return
					}
					if isMethod {
						if id, ok := fun.X.(*ast.Ident); ok && importPathOf(f.ast, id.Name) != "" {
							return
						}
						out = append(out, callSite{call, sc, q})
						return
					}
					if id, ok := fun.X.(*ast.Ident); ok && p.importPath != "" && importPathOf(f.ast, id.Name) == p.importPath {
						out = append(out, callSite{call, sc, q})
					}
				}
			})
		}
	}
	return out
}

func (a amount) describe() string {
	switch a.kind {
	case amtKnown:
		return strconv.FormatInt(a.n, 10)
	case amtEnv:
		s := "environment variable " + a.name
		if a.def != nil {
			s += " (default " + a.def.describe() + ")"
		}
		return s
	case amtFlag:
		s := "flag -" + a.name
		if a.def != nil {
			s += " (default " + a.def.describe() + ")"
		}
		return s
	}
	return a.why
}

func (prog *program) text(e ast.Node) string {
	var buf bytes.Buffer
	if err := printer.Fprint(&buf, prog.fset, e); err != nil {
		return "?"
	}
	s := strings.Join(strings.Fields(buf.String()), " ")
	return truncate(s, 100)
}

func (prog *program) pos(n ast.Node) token.Position {
	return prog.fset.Position(n.Pos())
}
