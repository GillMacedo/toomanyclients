package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Pool kinds.
const (
	kindSQL     = "database/sql"
	kindPgxpool = "pgxpool"
	kindConn    = "single connection"
)

// pool is one place in the source that opens connections to Postgres.
type pool struct {
	kind string
	call string // such as sql.Open
	pos  token.Position
	pkg  *pkg
	fn   *ast.FuncDecl // nil when at package level
	// driver is the database/sql driver name, when it is a constant.
	driver string

	// For database/sql: the SetMaxOpenConns calls that apply.
	limit    *amount
	limitPos []token.Position
	// For pgxpool: pool_max_conns read from the DSN, or MaxConns set on
	// the config. dsnEnv is the variable the DSN comes from, whose value
	// may carry pool_max_conns. dsnUnknown says why the DSN can't be read.
	dsnEnv     string
	dsnUnknown string
}

func (p *pool) where() string { return posString(p.pos) }

// goProgram is one main package and the packages of the input it imports.
type goProgram struct {
	name  string
	pkg   *pkg
	pkgs  map[*pkg]bool
	pools []poolUse
	// skipped are pools for other databases, such as MySQL.
	skipped []*pool
}

// poolUse is a pool as a program uses it: how many times its code runs.
type poolUse struct {
	pool   *pool
	copies int
	why    string // how copies was counted, when it isn't 1
}

var sqlOpeners = map[string]map[string]int{
	// import path: function name -> index of the driver name argument, or -1
	"database/sql":                   {"Open": 0, "OpenDB": -1},
	"github.com/jmoiron/sqlx":        {"Open": 0, "Connect": 0, "MustConnect": 0, "MustOpen": 0, "ConnectContext": 1},
	"gorm.io/gorm":                   {"Open": -2},
	"github.com/jackc/pgx/v5/stdlib": {"OpenDB": -1},
	"github.com/jackc/pgx/v4/stdlib": {"OpenDB": -1},
}

var pgxpoolPaths = map[string]bool{"github.com/jackc/pgx/v5/pgxpool": true, "github.com/jackc/pgx/v4/pgxpool": true}

var singleConnPaths = map[string]bool{"github.com/jackc/pgx/v5": true, "github.com/jackc/pgx/v4": true, "github.com/jackc/pgconn": true, "github.com/jackc/pgx/v5/pgconn": true}

var postgresDrivers = map[string]bool{"postgres": true, "postgresql": true, "pgx": true, "pgx/v5": true, "pgx/v4": true, "cloudsqlpostgres": true, "nrpostgres": true, "pq": true}

// build finds every pool in the input and the programs that use them.
func build(prog *program) []*goProgram {
	var pools []*pool
	var maxCalls []callSite
	for _, p := range prog.pkgs {
		for _, f := range p.files {
			walkScopes(f, func(n ast.Node, sc scope) {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "SetMaxOpenConns" && len(call.Args) == 1 {
					if id, ok := sel.X.(*ast.Ident); !ok || importPathOf(f.ast, id.Name) == "" {
						maxCalls = append(maxCalls, callSite{call, sc, p})
					}
				}
				path, name := pkgCall(call, f.ast)
				if path == "" {
					return
				}
				if fns, ok := sqlOpeners[path]; ok {
					if idx, ok := fns[name]; ok {
						pl := &pool{kind: kindSQL, call: lastElem(path) + "." + name, pos: prog.pos(call), pkg: p, fn: sc.fn}
						pl.driver = prog.driverOf(call, idx, sc)
						pools = append(pools, pl)
					}
				}
				if pgxpoolPaths[path] && (name == "New" || name == "NewWithConfig" || name == "Connect" || name == "ConnectConfig") {
					pl := &pool{kind: kindPgxpool, call: "pgxpool." + name, pos: prog.pos(call), pkg: p, fn: sc.fn, driver: "pgx"}
					prog.pgxLimit(pl, call, name, sc)
					pools = append(pools, pl)
				}
				if singleConnPaths[path] && (name == "Connect" || name == "ConnectConfig") {
					one := known(1)
					pools = append(pools, &pool{kind: kindConn, call: lastElem(path) + "." + name, pos: prog.pos(call), pkg: p, fn: sc.fn, driver: "pgx", limit: &one})
				}
			})
		}
	}
	for _, pl := range pools {
		if pl.kind == kindSQL {
			prog.sqlLimit(pl, maxCalls)
		}
	}

	var progs []*goProgram
	for _, p := range prog.pkgs {
		if p.name != "main" {
			continue
		}
		gp := &goProgram{name: programName(p), pkg: p, pkgs: prog.reachable(p)}
		for _, pl := range pools {
			if !gp.pkgs[pl.pkg] {
				continue
			}
			if pl.driver != "" && !postgresDrivers[pl.driver] {
				gp.skipped = append(gp.skipped, pl)
				continue
			}
			use := poolUse{pool: pl, copies: 1}
			if pl.fn != nil && !(pl.pkg == p && (pl.fn.Name.Name == "main" || pl.fn.Name.Name == "init")) {
				calls := prog.callsOf(pl.fn, pl.pkg)
				inProg := 0
				for _, c := range calls {
					if gp.pkgs[c.pkg] {
						inProg++
					}
				}
				switch {
				case len(calls) > 0 && inProg == 0:
					continue // this program never calls the function
				case inProg > 1:
					use.copies = inProg
					use.why = fmt.Sprintf("%s is called in %d places", pl.fn.Name.Name, inProg)
				}
			}
			gp.pools = append(gp.pools, use)
		}
		progs = append(progs, gp)
	}
	sort.Slice(progs, func(i, j int) bool { return progs[i].name < progs[j].name })
	return progs
}

func lastElem(path string) string {
	e := path[strings.LastIndex(path, "/")+1:]
	if isMajorVersion(e) {
		t := path[:strings.LastIndex(path, "/")]
		e = t[strings.LastIndex(t, "/")+1:]
	}
	return e
}

// programName is the name go build gives the binary: the folder name, or
// the module path's last element at the module root.
func programName(p *pkg) string {
	if p.importPath != "" {
		return lastElem(p.importPath)
	}
	return filepath.Base(p.dir)
}

// reachable is the main package and every package of the input it
// imports, directly or not.
func (prog *program) reachable(main *pkg) map[*pkg]bool {
	out := map[*pkg]bool{main: true}
	queue := []*pkg{main}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for imp := range p.imports {
			q := prog.byImport[imp]
			if q == nil {
				q = prog.bySuffix(imp)
			}
			if q != nil && !out[q] {
				out[q] = true
				queue = append(queue, q)
			}
		}
	}
	return out
}

// bySuffix finds the package for an import path when the input has no
// go.mod to give import paths: the one package whose folder ends with the
// most elements of the path, at least two. Standard library and other
// short paths never match.
func (prog *program) bySuffix(imp string) *pkg {
	parts := strings.Split(imp, "/")
	if len(parts) < 3 || !strings.Contains(parts[0], ".") {
		return nil
	}
	for n := len(parts); n >= 2; n-- {
		suffix := string(filepath.Separator) + filepath.Join(parts[len(parts)-n:]...)
		var found []*pkg
		for _, p := range prog.pkgs {
			if p.importPath == "" && p.name != "main" && strings.HasSuffix(p.dir, suffix) {
				found = append(found, p)
			}
		}
		if len(found) == 1 {
			return found[0]
		}
		if len(found) > 1 {
			return nil
		}
	}
	return nil
}

// driverOf returns the database/sql driver name, "" when unknown.
func (prog *program) driverOf(call *ast.CallExpr, idx int, sc scope) string {
	switch {
	case idx >= 0 && idx < len(call.Args):
		return prog.evalString(call.Args[idx], sc, 0)
	case idx == -2 && len(call.Args) > 0:
		// gorm.Open(postgres.Open(dsn)): the dialector's package.
		if inner, ok := call.Args[0].(*ast.CallExpr); ok {
			path, _ := pkgCall(inner, sc.file.ast)
			switch {
			case strings.HasPrefix(path, "gorm.io/driver/postgres"):
				return "postgres"
			case strings.HasPrefix(path, "gorm.io/driver/"):
				return lastElem(path)
			}
		}
	case idx == -1:
		if strings.Contains(prog.text(call.Fun), "stdlib") {
			return "pgx"
		}
	}
	return ""
}

// sqlLimit finds the SetMaxOpenConns calls for a database/sql pool: in
// the same function first, then the same package, then any package of the
// input. Several calls give the highest value.
func (prog *program) sqlLimit(pl *pool, calls []callSite) {
	var pick []callSite
	for _, c := range calls {
		if pl.fn != nil && c.sc.fn == pl.fn {
			pick = append(pick, c)
		}
	}
	if len(pick) == 0 {
		for _, c := range calls {
			if c.pkg == pl.pkg {
				pick = append(pick, c)
			}
		}
	}
	if len(pick) == 0 {
		pick = calls
	}
	if len(pick) == 0 {
		return
	}
	var vals []amount
	for _, c := range pick {
		vals = append(vals, prog.evalInt(c.call.Args[0], c.sc, 0))
		pl.limitPos = append(pl.limitPos, prog.pos(c.call))
	}
	a := combine(vals, "SetMaxOpenConns")
	pl.limit = &a
}

// pgxLimit works out where a pgxpool's MaxConns comes from.
func (prog *program) pgxLimit(pl *pool, call *ast.CallExpr, name string, sc scope) {
	if len(call.Args) < 2 {
		pl.dsnUnknown = prog.text(call)
		return
	}
	arg := call.Args[1]
	if name == "New" || name == "Connect" {
		prog.dsnLimit(pl, arg, sc)
		return
	}
	// NewWithConfig(ctx, cfg): cfg.MaxConns = n in the same function, then
	// the package; otherwise the DSN given to ParseConfig.
	var vals []amount
	for _, scopeOnly := range []bool{true, false} {
		for _, f := range pl.pkg.files {
			walkScopes(f, func(n ast.Node, fsc scope) {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Lhs) != len(as.Rhs) {
					return
				}
				if scopeOnly && fsc.fn != sc.fn {
					return
				}
				for i, l := range as.Lhs {
					if s, ok := l.(*ast.SelectorExpr); ok && s.Sel.Name == "MaxConns" {
						vals = append(vals, prog.evalInt(as.Rhs[i], fsc, 0))
						pl.limitPos = append(pl.limitPos, prog.pos(as))
					}
				}
			})
		}
		if len(vals) > 0 {
			break
		}
	}
	if len(vals) > 0 {
		a := combine(vals, "MaxConns")
		pl.limit = &a
		return
	}
	id, ok := arg.(*ast.Ident)
	if !ok || sc.body() == nil {
		pl.dsnUnknown = prog.text(arg) + " (a config this tool can't follow)"
		return
	}
	defs, _ := localDefs(sc.body(), id.Name)
	if len(defs) != 1 || defs[0] == nil {
		pl.dsnUnknown = prog.text(arg) + " (a config this tool can't follow)"
		return
	}
	pc, ok := defs[0].(*ast.CallExpr)
	if ok {
		if path, fn := pkgCall(pc, sc.file.ast); pgxpoolPaths[path] && fn == "ParseConfig" && len(pc.Args) == 1 {
			prog.dsnLimit(pl, pc.Args[0], sc)
			return
		}
	}
	pl.dsnUnknown = prog.text(arg) + " (a config this tool can't follow)"
}

var poolMaxRe = regexp.MustCompile(`(?:^|[?&\s])pool_max_conns=([^&\s]*)`)

// dsnLimit reads pool_max_conns from a DSN expression.
func (prog *program) dsnLimit(pl *pool, e ast.Expr, sc scope) {
	if lit, ok := e.(*ast.BasicLit); ok && (lit.Value == `""` || lit.Value == "``") {
		return // an empty DSN: libpq defaults and the default pool size
	}
	if s := prog.evalString(e, sc, 0); s != "" {
		if a, ok := poolMaxFromDSN(s); ok {
			pl.limit = &a
			pl.limitPos = append(pl.limitPos, prog.pos(e))
		}
		return
	}
	switch x := e.(type) {
	case *ast.CallExpr:
		path, name := pkgCall(x, sc.file.ast)
		if path == "fmt" && name == "Sprintf" && len(x.Args) > 0 {
			format := prog.evalString(x.Args[0], sc, 0)
			if format == "" {
				break
			}
			if i := strings.Index(format, "pool_max_conns=%"); i >= 0 {
				arg := strings.Count(strings.ReplaceAll(format[:i], "%%", ""), "%") + 1
				if arg < len(x.Args) {
					a := prog.evalInt(x.Args[arg], sc, 0)
					pl.limit = &a
					pl.limitPos = append(pl.limitPos, prog.pos(x))
					return
				}
				break
			}
			if a, ok := poolMaxFromDSN(format); ok {
				pl.limit = &a
				pl.limitPos = append(pl.limitPos, prog.pos(x))
			}
			return
		}
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			if s := prog.evalString(x.Y, sc, 0); s != "" {
				if a, ok := poolMaxFromDSN(s); ok {
					pl.limit = &a
					pl.limitPos = append(pl.limitPos, prog.pos(x))
					return
				}
			}
			prog.dsnLimit(pl, x.X, sc)
			return
		}
	}
	src := prog.evalStr(e, sc, 0)
	if src.kind == amtEnv {
		pl.dsnEnv = src.name
		return
	}
	pl.dsnUnknown = prog.text(e) + " (a connection string this tool can't read)"
}

// poolMaxFromDSN finds pool_max_conns in a URL or key=value DSN.
func poolMaxFromDSN(s string) (amount, bool) {
	if u, err := url.Parse(s); err == nil && u.RawQuery != "" {
		if v := u.Query().Get("pool_max_conns"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				return known(n), true
			}
			return unknownAmt("pool_max_conns=" + v + " (not a number)"), true
		}
	}
	m := poolMaxRe.FindStringSubmatch(s)
	if m == nil {
		return amount{}, false
	}
	n, err := strconv.ParseInt(strings.Trim(m[1], `'"`), 10, 64)
	if err != nil {
		return unknownAmt("pool_max_conns=" + m[1] + " (not a number)"), true
	}
	return known(n), true
}

func posString(p token.Position) string {
	return fmt.Sprintf("%s:%d", p.Filename, p.Line)
}
