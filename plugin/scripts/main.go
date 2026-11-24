// Command toomanyclients adds up the Postgres connections that Go programs
// can open across their Kubernetes pods, at full scale and during a
// rolling update, and compares the total with max_connections.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

const version = "0.1.0"

const footer = "toomanyclients reads Go source and Kubernetes manifests only. Clients in other languages, poolers such as PgBouncer, and connections from outside these files aren't counted."

const usageText = `Usage: toomanyclients [--pg-config FILE | --max-connections N] [--reserve N]
                      [--node-cpus N] [--nodes N] [--workload PROGRAM=WORKLOAD]...
                      [--format text|json] [--fail-on warn|error|never]
                      PATH...

PATH is a Go file, a manifest (.yaml, .yml) or a folder, read recursively.
Pass the code of the programs that use the database and the rendered
manifests that deploy them. vendor, testdata, hidden folders, test files,
Helm charts and kustomizations are skipped in folders: render those first.

Exit codes: 0 no finding at the --fail-on level, 1 findings, 2 bad options,
3 the input can't be trusted or has nothing to compare (no result).
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type report struct {
	Version  string    `json:"version"`
	Programs []string  `json:"programs"`
	Budget   *Budget   `json:"budget"`
	Findings []Finding `json:"findings"`
	Result   string    `json:"result"`
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(s string) error { *m = append(*m, s); return nil }

func run(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("toomanyclients", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	format := fl.String("format", "text", "")
	failOn := fl.String("fail-on", "warn", "")
	pgConf := fl.String("pg-config", "", "")
	maxConns := fl.Int("max-connections", 0, "")
	superuser := fl.Int("superuser-reserved", -1, "")
	reserve := fl.Int("reserve", 0, "")
	nodeCPUs := fl.Int("node-cpus", 0, "")
	nodes := fl.Int("nodes", 0, "")
	var workloads multiFlag
	fl.Var(&workloads, "workload", "")
	showVersion := fl.Bool("version", false, "")
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usageText)
			return 0
		}
		fmt.Fprintf(stderr, "toomanyclients: %v\n\n%s", err, usageText)
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, "toomanyclients", version)
		return 0
	}
	bad := func(msg string) int {
		fmt.Fprintf(stderr, "toomanyclients: %s\n", msg)
		return 2
	}
	if *format != "text" && *format != "json" {
		return bad("--format must be text or json")
	}
	var failLevel level
	switch *failOn {
	case "warn":
		failLevel = levelWarn
	case "error":
		failLevel = levelError
	case "never":
		failLevel = levelError + 1
	default:
		return bad("--fail-on must be warn, error or never")
	}
	if *maxConns < 0 || *reserve < 0 || *nodeCPUs < 0 || *nodes < 0 || *superuser < -1 {
		return bad("numbers can't be negative")
	}
	if *pgConf != "" && *maxConns > 0 {
		return bad("pass --pg-config or --max-connections, not both")
	}
	opt := options{nodeCPUs: *nodeCPUs, nodes: *nodes, overrides: map[string][]string{}}
	for _, w := range workloads {
		p, n, ok := strings.Cut(w, "=")
		if !ok || p == "" || n == "" {
			return bad("--workload takes PROGRAM=WORKLOAD or PROGRAM=WORKLOAD/CONTAINER")
		}
		opt.overrides[p] = append(opt.overrides[p], n)
	}
	if fl.NArg() == 0 {
		fmt.Fprintf(stderr, "toomanyclients: no PATH given\n\n%s", usageText)
		return 2
	}
	lim := limitFlags{conf: *pgConf, max: *maxConns, superuser: *superuser, reserve: *reserve}
	rep, err := analyze(fl.Args(), lim, opt)
	if err != nil {
		var ie *inputError
		if errors.As(err, &ie) {
			fmt.Fprintf(stderr, "toomanyclients: can't trust this input, so there is no result.\n%s\n", ie.msg)
			return 3
		}
		fmt.Fprintf(stderr, "toomanyclients: %v\n", err)
		return 3
	}
	failing := 0
	for _, f := range rep.Findings {
		if f.lvl >= failLevel {
			failing++
		}
	}
	if *format == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return 3
		}
	} else {
		writeText(stdout, rep)
	}
	if failing > 0 {
		return 1
	}
	return 0
}

type limitFlags struct {
	conf      string
	max       int
	superuser int
	reserve   int
}

func analyze(paths []string, lf limitFlags, opt options) (*report, error) {
	in, err := collectFiles(paths)
	if err != nil {
		return nil, err
	}
	if len(in.goFiles) == 0 {
		return nil, refuse("no Go files in %s, so there is no program to count. Pass the code of the programs that connect to Postgres", strings.Join(paths, " "))
	}
	if len(in.yamlFiles) == 0 {
		msg := "no .yaml or .yml manifests in " + strings.Join(paths, " ") + ", so there is no pod count to multiply by. Pass the rendered manifests that deploy the programs"
		if len(in.skipped) > 0 {
			msg += ". Skipped " + strings.Join(in.skipped, ", ") + ": render them with helm template or kustomize build first"
		}
		return nil, refuse("%s", msg)
	}
	fset, parsed, err := parseGo(in.goFiles)
	if err != nil {
		return nil, err
	}
	named := map[string]bool{}
	for _, p := range paths {
		named[p] = true
	}
	objs, _, err := loadManifests(in.yamlFiles, named)
	if err != nil {
		return nil, err
	}
	m, err := readObjects(objs)
	if err != nil {
		return nil, err
	}
	if len(m.workloads) == 0 {
		return nil, refuse("no Deployment, StatefulSet, DaemonSet, Job, CronJob or Pod in the manifests, so there is no pod count. Pass the manifests that deploy the programs")
	}

	limit, err := pickLimit(lf, m)
	if err != nil {
		return nil, err
	}

	prog := newProgram(fset, parsed)
	progs := build(prog)
	if len(progs) == 0 {
		return nil, refuse("no main package in the Go files, so there is no program to match with a workload. Pass the folder of each program's main package along with the packages it imports")
	}
	withPools := 0
	var names []string
	for _, gp := range progs {
		if len(gp.pools) > 0 {
			withPools++
			names = append(names, gp.name)
		}
	}
	if withPools == 0 {
		return nil, refuse("none of the %d %s (%s) opens a Postgres connection this tool recognizes (database/sql, sqlx, gorm, pgxpool or pgx), so there is nothing to count", len(progs), plural(len(progs), "program", "programs"), programList(progs))
	}

	budget, findings := check(progs, m, limit, opt)
	for i := range findings {
		findings[i].Level = findings[i].lvl.String()
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].lvl != findings[j].lvl {
			return findings[i].lvl > findings[j].lvl
		}
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
	if findings == nil {
		findings = []Finding{}
	}
	rep := &report{Version: version, Programs: names, Budget: budget, Findings: findings}
	rep.Result = resultLine(budget, findings)
	return rep, nil
}

func programList(progs []*goProgram) string {
	var out []string
	for _, p := range progs {
		out = append(out, p.name)
	}
	return strings.Join(out, ", ")
}

// pickLimit decides which max_connections to compare with: a flag, a
// postgresql.conf file, a Postgres operator object in the manifests, or
// PostgreSQL's default.
func pickLimit(lf limitFlags, m *manifests) (dbLimit, error) {
	var l dbLimit
	switch {
	case lf.max > 0:
		l = dbLimit{max: lf.max, superuser: defaultSuperuser, source: "--max-connections"}
	case lf.conf != "":
		var err error
		if l, err = readPgConf(lf.conf); err != nil {
			return l, err
		}
	case len(m.servers) == 1:
		var err error
		if l, err = limitFromServer(m.servers[0]); err != nil {
			return l, err
		}
	case len(m.servers) > 1:
		var refs []string
		for _, s := range m.servers {
			refs = append(refs, s.obj.ref()+" at "+s.obj.where())
		}
		return l, refuse("the manifests define %d Postgres servers (%s), and the source doesn't show which one the programs use. Pass --max-connections or --pg-config for the one to check", len(m.servers), strings.Join(refs, ", "))
	default:
		l = dbLimit{max: defaultMaxConnections, superuser: defaultSuperuser, assumed: true,
			source: "no --pg-config, --max-connections or Postgres operator object, so PostgreSQL's default max_connections of 100 is assumed. Managed databases often set their own"}
	}
	if lf.superuser >= 0 {
		l.superuser = lf.superuser
	}
	l.extra = lf.reserve
	if l.available() < 1 {
		return l, refuse("%s leaves no connections for these programs", l.describe())
	}
	return l, nil
}

func resultLine(b *Budget, findings []Finding) string {
	problems, unread := 0, 0
	for _, f := range findings {
		if f.lvl == levelNote {
			continue
		}
		if strings.HasPrefix(f.Code, "TC9") {
			unread++
		} else {
			problems++
		}
	}
	switch {
	case problems > 0 && unread > 0:
		return fmt.Sprintf("%d %s where the pods can open more connections than Postgres accepts, and %d %s that couldn't be counted.", problems, plural(problems, "finding", "findings"), unread, plural(unread, "thing", "things"))
	case problems > 0:
		return fmt.Sprintf("%d %s where the pods can open more connections than Postgres accepts.", problems, plural(problems, "finding", "findings"))
	case unread > 0:
		return fmt.Sprintf("%d %s couldn't be counted, so this isn't a clean result.", unread, plural(unread, "thing", "things"))
	case len(b.Rows) == 0:
		return "no workload runs these programs, so nothing was counted."
	}
	return fmt.Sprintf("at most %d connections at full scale and %d during a rolling update, within the %d Postgres accepts.", b.Steady, b.Peak, b.Available)
}

func writeText(w io.Writer, rep *report) {
	b := rep.Budget
	fmt.Fprintf(w, "toomanyclients: %d %s (%s) in %d %s, against %s\n\n", len(rep.Programs), plural(len(rep.Programs), "program", "programs"), strings.Join(rep.Programs, ", "), len(b.Rows), plural(len(b.Rows), "workload", "workloads"), limitShort(b))
	if len(b.Rows) > 0 {
		fmt.Fprintln(w, "Budget")
		for _, r := range b.Rows {
			per := strconv.Itoa(r.PerPod)
			if r.Unlimited {
				per = "no limit"
			} else if r.AtLeast {
				per = ">=" + per
			}
			fmt.Fprintf(w, "  %s runs %s: %d %s (%s) x %s per pod, +%d %s in a rollout\n", r.Workload, r.Program, r.Pods, plural(r.Pods, "pod", "pods"), r.PodsFrom, per, r.Surge, plural(r.Surge, "pod", "pods"))
			for _, p := range r.Pools {
				fmt.Fprintf(w, "    %s\n", p)
			}
		}
		if b.Unlimited {
			fmt.Fprintln(w, "  Total: no limit")
		} else {
			atLeast := ""
			if b.AtLeast {
				atLeast = "at least "
			}
			fmt.Fprintf(w, "  Total: %s%d at full scale, %s%d during a rolling update\n", atLeast, b.Steady, atLeast, b.Peak)
		}
		fmt.Fprintf(w, "  Postgres: %s\n\n", b.LimitText)
	}
	for _, f := range rep.Findings {
		loc := ""
		if f.File != "" {
			loc = fmt.Sprintf(" %s:%d", f.File, f.Line)
		}
		fmt.Fprintf(w, "%s %s%s  %s\n", strings.ToUpper(f.Level), f.Code, loc, f.Message)
		for _, d := range f.Details {
			fmt.Fprintf(w, "  %s\n", d)
		}
		if f.Fix != "" {
			fmt.Fprintf(w, "  Fix: %s\n", f.Fix)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "Result: %s\n%s\n", rep.Result, footer)
}

func limitShort(b *Budget) string {
	if b.LimitGuess {
		return fmt.Sprintf("%d available connections (assumed)", b.Available)
	}
	return fmt.Sprintf("%d available connections (%s)", b.Available, b.LimitFrom)
}
