package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type level int

const (
	levelNote level = iota
	levelWarn
	levelError
)

func (l level) String() string {
	switch l {
	case levelError:
		return "error"
	case levelWarn:
		return "warn"
	}
	return "note"
}

// Finding is one result. lvl orders and filters; Level is for JSON.
type Finding struct {
	Code    string   `json:"code"`
	Level   string   `json:"level"`
	File    string   `json:"file"`
	Line    int      `json:"line"`
	Message string   `json:"message"`
	Details []string `json:"details,omitempty"`
	Fix     string   `json:"fix,omitempty"`
	lvl     level
}

// options are the command-line settings that change the budget.
type options struct {
	nodeCPUs  int // 0: unknown
	nodes     int // 0: unknown
	overrides map[string][]string
}

// Row is one workload's share of the budget.
type Row struct {
	Workload string `json:"workload"`
	Where    string `json:"where"`
	Program  string `json:"program"`
	Pods     int    `json:"pods"`
	PodsFrom string `json:"podsFrom"`
	Surge    int    `json:"rolloutExtraPods"`
	PerPod   int    `json:"connectionsPerPod"`
	// AtLeast is true when PerPod is a lower bound: a pgxpool default
	// that depends on the node's CPU count, or a pool that can't be read.
	AtLeast   bool     `json:"perPodIsLowerBound"`
	Unlimited bool     `json:"unlimited"`
	Pools     []string `json:"pools"`
	pgxDef    int      // pgxpool pools on the default, per pod
	fixed     int      // connections per pod from every other pool
	unknown   bool
}

// Budget is the total compared with what Postgres accepts.
type Budget struct {
	Rows       []Row  `json:"workloads"`
	Steady     int    `json:"atFullScale"`
	Peak       int    `json:"duringRollout"`
	AtLeast    bool   `json:"totalsAreLowerBounds"`
	Unlimited  bool   `json:"unlimited"`
	Available  int    `json:"available"`
	LimitText  string `json:"limit"`
	LimitFrom  string `json:"limitSource"`
	LimitGuess bool   `json:"limitAssumed"`
}

// deployment is a program running in one container of a workload.
type deployment struct {
	prog *goProgram
	w    *workload
	c    *container
	how  string
}

func check(progs []*goProgram, m *manifests, limit dbLimit, opt options) (*Budget, []Finding) {
	var findings []Finding
	b := &Budget{Available: limit.available(), LimitText: limit.describe(), LimitFrom: limit.source, LimitGuess: limit.assumed}
	if limit.assumed {
		findings = append(findings, Finding{Code: "TC905", lvl: levelWarn, Message: "The Postgres limit is assumed: " + limit.source,
			Fix: "Pass --pg-config with the server's postgresql.conf, or --max-connections with the value from SHOW max_connections."})
	}

	deps, unmatched := match(progs, m, opt)
	for _, gp := range unmatched {
		pl := gp.pools[0].pool
		findings = append(findings, Finding{Code: "TC902", lvl: levelWarn, File: pl.pos.Filename, Line: pl.pos.Line,
			Message: fmt.Sprintf("Program %s opens Postgres connections, but no workload in the manifests runs it, so its connections aren't counted", gp.name),
			Details: []string{fmt.Sprintf("A workload runs %s when a container's name, image name or command is %q, or the workload is named %q.", gp.name, gp.name, gp.name)},
			Fix:     fmt.Sprintf("Pass the manifests that deploy %s, or name its workload with --workload %s=WORKLOAD.", gp.name, gp.name)})
	}

	// Scalers: each target's pod count at full scale.
	scaled := map[*workload]*scaler{}
	for _, s := range m.scalers {
		var target *workload
		for _, w := range m.workloads {
			if w.obj.name == s.targetName && sameNS(w.obj.ns, s.obj.ns) && (s.targetKind == "" || s.targetKind == w.obj.kind) {
				target = w
			}
		}
		if target == nil {
			findings = append(findings, Finding{Code: "TC906", lvl: levelWarn, File: s.obj.file, Line: s.obj.line,
				Message: fmt.Sprintf("%s scales %s %s, which isn't in these files", s.obj.ref(), s.targetKind, s.targetName),
				Fix:     "Pass the manifest of the workload it scales, so its pods are counted at the scaler's maximum."})
			continue
		}
		if prev := scaled[target]; prev == nil || s.max > prev.max {
			scaled[target] = s
		}
	}

	byWorkload := map[*workload][]deployment{}
	var order []*workload
	for _, d := range deps {
		if _, ok := byWorkload[d.w]; !ok {
			order = append(order, d.w)
		}
		byWorkload[d.w] = append(byWorkload[d.w], d)
	}
	seenUnknown := map[string]bool{}
	pgxDefaultAt := map[string]bool{}
	for _, w := range order {
		row := Row{Workload: w.obj.ref(), Where: w.obj.where()}
		var names []string
		for _, d := range byWorkload[w] {
			names = append(names, d.prog.name)
			for _, use := range d.prog.pools {
				r := resolvePool(use.pool, d.c, w, m, opt)
				line := fmt.Sprintf("%s at %s: %s", use.pool.call, use.pool.where(), r.text)
				if use.copies > 1 {
					line += fmt.Sprintf(", x%d (%s)", use.copies, use.why)
				}
				row.Pools = append(row.Pools, line)
				switch {
				case r.unlimited:
					row.Unlimited = true
					key := "TC103" + use.pool.where()
					if !seenUnknown[key] {
						seenUnknown[key] = true
						findings = append(findings, unlimitedFinding(d.prog, use.pool, r))
					}
				case r.unknown != "":
					row.unknown = true
					key := "TC901" + use.pool.where() + r.unknown
					if !seenUnknown[key] {
						seenUnknown[key] = true
						findings = append(findings, Finding{Code: "TC901", lvl: levelWarn, File: use.pool.pos.Filename, Line: use.pool.pos.Line,
							Message: fmt.Sprintf("The size of the %s pool %s opens in %s can't be read, so the total is a lower bound", use.pool.kind, d.prog.name, w.obj.ref()),
							Details: []string{"Pool size: " + r.unknown},
							Fix:     "Set the value in the manifest's env or args, as a constant in the code, or pass the file that sets it."})
					}
				case r.pgxDefault:
					row.pgxDef += use.copies
					pgxDefaultAt[use.pool.where()] = true
				default:
					row.fixed += r.n * use.copies
				}
			}
		}
		row.Program = strings.Join(names, ", ")
		pods, from := w.replicas, "replicas"
		if !w.hasReplica && !w.daemon {
			from = "replicas not set, so 1"
		}
		if s := scaled[w]; s != nil {
			pods, from = s.max, fmt.Sprintf("%s max", s.obj.ref())
		}
		if w.daemon {
			if opt.nodes > 0 {
				pods, from = opt.nodes, "one per node, --nodes"
			} else {
				row.unknown = true
				pods, from = 1, "one per node, counted as 1"
				findings = append(findings, Finding{Code: "TC904", lvl: levelWarn, File: w.obj.file, Line: w.obj.line,
					Message: fmt.Sprintf("%s runs one pod per node, and the node count isn't known, so it is counted as 1 pod", w.obj.ref()),
					Fix:     "Pass --nodes with the largest node count the cluster scales to."})
			}
		}
		row.Pods, row.PodsFrom = pods, from
		surge, err := w.surgePods(pods)
		if err != nil {
			surge = 0
			row.unknown = true
			findings = append(findings, Finding{Code: "TC901", lvl: levelWarn, File: w.obj.file, Line: w.obj.line,
				Message: fmt.Sprintf("%s has a maxSurge of %q, which isn't a number or a percentage", w.obj.ref(), w.surge)})
		}
		row.Surge = surge
		cpus := 4
		if opt.nodeCPUs > 4 {
			cpus = opt.nodeCPUs
		}
		row.PerPod = row.fixed + row.pgxDef*cpus
		row.AtLeast = row.unknown || (row.pgxDef > 0 && opt.nodeCPUs == 0)
		b.Rows = append(b.Rows, row)
	}

	// Workloads that look like Postgres clients but whose connections
	// weren't counted: they run no program here, or one with no pool this
	// tool recognizes.
	matchedW := map[*workload]bool{}
	for _, w := range order {
		matchedW[w] = true
	}
	for _, w := range m.workloads {
		if matchedW[w] {
			continue
		}
		v := dbEnvName(w, m)
		if v == "" {
			continue
		}
		f := Finding{Code: "TC903", lvl: levelWarn, File: w.obj.file, Line: w.obj.line,
			Message: fmt.Sprintf("%s sets %s, but none of the Go programs in these files opens a Postgres pool in it, so its connections aren't counted", w.obj.ref(), v),
			Fix:     "If it connects to this database, pass its code, or keep connections for it with --reserve."}
		if b.LimitText != "" && strings.Contains(b.LimitText, "--reserve") {
			f.lvl = levelNote
			f.Message = fmt.Sprintf("%s sets %s and isn't counted; check that --reserve covers its connections", w.obj.ref(), v)
			f.Fix = ""
		}
		findings = append(findings, f)
	}

	if len(b.Rows) == 0 {
		return b, findings
	}
	fixedSteady, fixedPeak, defSteady, defPeak := 0, 0, 0, 0
	for _, r := range b.Rows {
		fixedSteady += r.Pods * r.fixed
		fixedPeak += (r.Pods + r.Surge) * r.fixed
		defSteady += r.Pods * r.pgxDef
		defPeak += (r.Pods + r.Surge) * r.pgxDef
		if r.Unlimited {
			b.Unlimited = true
		}
		if r.AtLeast {
			b.AtLeast = true
		}
	}
	cpus := 4
	if opt.nodeCPUs > 4 {
		cpus = opt.nodeCPUs
	}
	b.Steady = fixedSteady + defSteady*cpus
	b.Peak = fixedPeak + defPeak*cpus
	if b.Unlimited {
		return b, findings
	}
	avail := b.Available
	anchorFile, anchorLine := b.Rows[0].Where, 0
	if i := strings.LastIndex(anchorFile, ":"); i >= 0 {
		anchorLine, _ = strconv.Atoi(anchorFile[i+1:])
		anchorFile = anchorFile[:i]
	}
	atLeast := ""
	if b.AtLeast {
		atLeast = "at least "
	}
	details := budgetLines(b)
	switch {
	case b.Steady > avail:
		findings = append(findings, Finding{Code: "TC101", lvl: levelError, File: anchorFile, Line: anchorLine,
			Message: fmt.Sprintf("At full scale the pods can open %s%d connections, and Postgres accepts %d", atLeast, b.Steady, avail),
			Details: append(details, "When the pools fill, under load or with slow queries, new connections fail with SQLSTATE 53300: FATAL: remaining connection slots are reserved for roles with the SUPERUSER attribute, or FATAL: sorry, too many clients already."),
			Fix:     fixText(b, avail, false)})
	case b.Peak > avail:
		findings = append(findings, Finding{Code: "TC102", lvl: levelWarn, File: anchorFile, Line: anchorLine,
			Message: fmt.Sprintf("Pods fit at full scale (%s%d of %d), but a rolling update adds pods and can open %s%d", atLeast, b.Steady, avail, atLeast, b.Peak),
			Details: append(details, "During a rollout, old and new pods run at the same time, each with a full pool. Pods that are shutting down can hold theirs a little longer."),
			Fix:     fixText(b, avail, true)})
	}
	// With --node-cpus, default pools were counted exactly in resolvePool.
	if defPeak > 0 && b.Peak <= avail {
		breakAt := 5
		for fixedPeak+defPeak*breakAt <= avail {
			breakAt++
		}
		var where []string
		for w := range pgxDefaultAt {
			where = append(where, w)
		}
		sort.Strings(where)
		file, line := splitPos(where[0])
		findings = append(findings, Finding{Code: "TC104", lvl: levelWarn, File: file, Line: line,
			Message: fmt.Sprintf("pgxpool's default MaxConns is the node's CPU count when above 4, so the budget breaks on nodes with %d or more CPUs", breakAt),
			Details: []string{
				fmt.Sprintf("Pools on the default: %s. With 4 per pod the peak is %d of %d.", strings.Join(where, ", "), b.Peak, avail),
				"runtime.NumCPU counts the CPUs the process may run on, which in a container is usually every CPU of the node. CPU limits don't change it.",
			},
			Fix: "Set pool_max_conns in the DSN or config.MaxConns in code, or pass --node-cpus with the largest node's CPU count."})
	}
	return b, findings
}

func splitPos(s string) (string, int) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return s, 0
	}
	n, _ := strconv.Atoi(s[i+1:])
	return s[:i], n
}

func budgetLines(b *Budget) []string {
	var out []string
	for _, r := range b.Rows {
		per := strconv.Itoa(r.PerPod)
		if r.AtLeast {
			per = "at least " + per
		}
		out = append(out, fmt.Sprintf("%s (%s) runs %s: %d pods (%s) x %s = %d; a rollout adds %d %s", r.Workload, r.Where, r.Program, r.Pods, r.PodsFrom, per, r.Pods*r.PerPod, r.Surge, plural(r.Surge, "pod", "pods")))
	}
	out = append(out, "Postgres: "+b.LimitText+" (from "+b.LimitFrom+")")
	return out
}

func fixText(b *Budget, avail int, rollout bool) string {
	pods := 0
	for _, r := range b.Rows {
		pods += r.Pods
		if rollout {
			pods += r.Surge
		}
	}
	per := 0
	if pods > 0 {
		per = avail / pods
	}
	s := fmt.Sprintf("Lower the pool sizes so the total fits, for example %d per pod across %d pods, or lower the maximum replicas, or raise max_connections", per, pods)
	if rollout {
		s += ", or set maxSurge to 0 with a maxUnavailable above 0"
	}
	return s + ". With many pods, put PgBouncer in transaction mode in front of Postgres."
}

type resolved struct {
	n          int
	text       string
	unlimited  bool
	pgxDefault bool
	unknown    string
}

// resolvePool works out one pool's maximum size in one container.
func resolvePool(pl *pool, c *container, w *workload, m *manifests, opt options) resolved {
	at := ""
	if len(pl.limitPos) > 0 {
		var ps []string
		for _, p := range pl.limitPos {
			ps = append(ps, posString(p))
		}
		at = " at " + strings.Join(ps, ", ")
	}
	switch pl.kind {
	case kindConn:
		return resolved{n: 1, text: "1 connection"}
	case kindSQL:
		if pl.limit == nil {
			return resolved{unlimited: true, text: "no SetMaxOpenConns, so no limit"}
		}
		n, how, why := resolveAmount(*pl.limit, c, w, m)
		if why != "" {
			return resolved{unknown: why, text: "SetMaxOpenConns" + at + " from " + why}
		}
		if n <= 0 {
			return resolved{unlimited: true, text: fmt.Sprintf("SetMaxOpenConns(%d)%s%s, and 0 or less means no limit", n, at, how)}
		}
		return resolved{n: int(n), text: fmt.Sprintf("SetMaxOpenConns(%d)%s%s", n, at, how)}
	case kindPgxpool:
		if pl.limit != nil {
			n, how, why := resolveAmount(*pl.limit, c, w, m)
			if why != "" {
				return resolved{unknown: why, text: "MaxConns" + at + " from " + why}
			}
			if n < 1 {
				return resolved{n: 0, text: fmt.Sprintf("MaxConns %d%s%s, which pgxpool rejects, so the program fails to start", n, at, how)}
			}
			return resolved{n: int(n), text: fmt.Sprintf("MaxConns %d%s%s", n, at, how)}
		}
		if pl.dsnUnknown != "" {
			return resolved{unknown: pl.dsnUnknown, text: "DSN " + pl.dsnUnknown}
		}
		if pl.dsnEnv != "" {
			v, how, why := lookupEnv(pl.dsnEnv, false, c, w, m)
			if why != "" {
				return resolved{unknown: "the DSN in " + why, text: "DSN from " + why}
			}
			if v == nil {
				return pgxDefault(opt, " ("+pl.dsnEnv+" not set in "+w.obj.ref()+")")
			}
			if a, ok := poolMaxFromDSN(*v); ok {
				if a.kind != amtKnown {
					return resolved{unknown: a.why, text: "DSN " + a.why}
				}
				return resolved{n: int(a.n), text: fmt.Sprintf("pool_max_conns=%d in %s%s", a.n, pl.dsnEnv, how)}
			}
			return pgxDefault(opt, " (DSN in "+pl.dsnEnv+how+" has no pool_max_conns)")
		}
		return pgxDefault(opt, "")
	}
	return resolved{unknown: "unknown pool kind"}
}

func pgxDefault(opt options, why string) resolved {
	if opt.nodeCPUs > 0 {
		n := opt.nodeCPUs
		if n < 4 {
			n = 4
		}
		return resolved{n: n, text: fmt.Sprintf("default MaxConns, %d with --node-cpus %d%s", n, opt.nodeCPUs, why)}
	}
	return resolved{pgxDefault: true, text: "default MaxConns: the larger of 4 and the node's CPU count" + why}
}

// resolveAmount turns an amount into a number for one container. how
// says where the value came from; why is set when it can't be read.
func resolveAmount(a amount, c *container, w *workload, m *manifests) (int64, string, string) {
	switch a.kind {
	case amtKnown:
		if a.note != "" {
			return a.n, " (" + a.note + ")", ""
		}
		return a.n, "", ""
	case amtEnv:
		v, how, why := lookupEnv(a.name, a.suffix, c, w, m)
		if why != "" {
			return 0, "", why
		}
		if v == nil {
			if a.required {
				return 0, "", a.name + ", which the code requires and the container doesn't set, so the program would fail to start"
			}
			if a.def == nil {
				return 0, "", a.name + ", which the container doesn't set, and the code has no default this tool can see"
			}
			n, _, why := resolveAmount(*a.def, c, w, m)
			if why != "" {
				return 0, "", a.name + ", not set, and its default " + why
			}
			return n, fmt.Sprintf(" (%s not set in %s, so the default)", a.name, w.obj.ref()), ""
		}
		n, err := strconv.ParseInt(strings.TrimSpace(*v), 10, 64)
		if err != nil {
			return 0, "", fmt.Sprintf("%s=%q, which isn't a number", a.name, *v)
		}
		return n, fmt.Sprintf(" (%s=%s%s)", a.name, *v, how), ""
	case amtFlag:
		v, found, why := lookupFlag(a.name, c)
		if why != "" {
			return 0, "", why
		}
		if !found {
			if a.def == nil {
				return 0, "", "flag -" + a.name + " with no default"
			}
			n, _, why := resolveAmount(*a.def, c, w, m)
			if why != "" {
				return 0, "", "flag -" + a.name + ", not set, and its default " + why
			}
			return n, fmt.Sprintf(" (flag -%s not set in %s, so the default)", a.name, w.obj.ref()), ""
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, "", fmt.Sprintf("flag -%s=%q, which isn't a number", a.name, v)
		}
		return n, fmt.Sprintf(" (flag -%s=%s in %s)", a.name, v, w.obj.ref()), ""
	}
	return 0, "", a.why
}

// lookupEnv finds a variable's value in a container. A nil value with no
// reason means the container doesn't set it.
func lookupEnv(name string, suffix bool, c *container, w *workload, m *manifests) (*string, string, string) {
	matches := func(n string) bool { return n == name || (suffix && strings.HasSuffix(n, "_"+name)) }
	var hit *envVar
	for i := range c.env {
		if matches(c.env[i].name) {
			hit = &c.env[i]
		}
	}
	if hit != nil {
		switch {
		case hit.secret != "":
			return nil, "", fmt.Sprintf("%s, which comes from Secret %s", hit.name, hit.secret)
		case hit.other != "":
			return nil, "", fmt.Sprintf("%s, which comes from a %s", hit.name, hit.other)
		case hit.cmName != "":
			cm := findConfigMap(m, hit.cmName, w.obj.ns)
			if cm == nil {
				return nil, "", fmt.Sprintf("%s, from ConfigMap %s, which isn't in these files", hit.name, hit.cmName)
			}
			v, ok := cm.data[hit.cmKey]
			if !ok {
				return nil, "", fmt.Sprintf("%s, from key %s of ConfigMap %s, which has no such key", hit.name, hit.cmKey, hit.cmName)
			}
			return checkExpand(hit.name, v, fmt.Sprintf(" from ConfigMap %s at %s", hit.cmName, cm.obj.where()))
		}
		return checkExpand(hit.name, hit.value, " in "+w.obj.ref())
	}
	var secrets []string
	for i := len(c.envFrom) - 1; i >= 0; i-- {
		ef := c.envFrom[i]
		if ef.secret != "" {
			secrets = append(secrets, ef.secret)
			continue
		}
		if ef.cm == "" {
			continue
		}
		cm := findConfigMap(m, ef.cm, w.obj.ns)
		if cm == nil {
			return nil, "", fmt.Sprintf("%s, which may come from ConfigMap %s (envFrom), which isn't in these files", name, ef.cm)
		}
		for k, v := range cm.data {
			if matches(ef.prefix + k) {
				return checkExpand(ef.prefix+k, v, fmt.Sprintf(" from ConfigMap %s at %s", ef.cm, cm.obj.where()))
			}
		}
	}
	if len(secrets) > 0 {
		return nil, "", fmt.Sprintf("%s, which may come from Secret %s (envFrom)", name, strings.Join(secrets, ", "))
	}
	return nil, "", ""
}

func checkExpand(name, v, how string) (*string, string, string) {
	if strings.Contains(v, "$(") {
		return nil, "", fmt.Sprintf("%s=%q, which refers to another variable", name, v)
	}
	return &v, how, ""
}

func findConfigMap(m *manifests, name, ns string) *configMap {
	for _, cm := range m.configMaps {
		if cm.obj.name == name && sameNS(cm.obj.ns, ns) {
			return cm
		}
	}
	return nil
}

// lookupFlag finds -name=value, --name=value, -name value or --name value
// in a container's command and args, also inside sh -c strings.
func lookupFlag(name string, c *container) (string, bool, string) {
	var toks []string
	for _, t := range append(append([]string{}, c.command...), c.args...) {
		toks = append(toks, strings.Fields(t)...)
	}
	val, found := "", false
	for i, t := range toks {
		if !strings.HasPrefix(t, "-") {
			continue
		}
		k := strings.TrimLeft(t, "-")
		if k == name && i+1 < len(toks) {
			val, found = toks[i+1], true
		} else if strings.HasPrefix(k, name+"=") {
			val, found = strings.TrimPrefix(k, name+"="), true
		}
	}
	val = strings.Trim(val, `'"`)
	if found && strings.Contains(val, "$(") {
		ref := strings.TrimSuffix(strings.TrimPrefix(val, "$("), ")")
		for _, e := range c.env {
			if e.name == ref && e.hasValue && e.secret == "" && e.cmName == "" && e.other == "" {
				return e.value, true, ""
			}
		}
		return "", false, fmt.Sprintf("flag -%s=%s, which refers to a variable this tool can't read", name, val)
	}
	return val, found, ""
}

func sameNS(a, b string) bool { return a == "" || b == "" || a == b }

// dbEnvName returns the first environment variable that suggests a
// workload talks to Postgres, or "". Postgres and pooler images are left
// out: they are the server, not a client.
func dbEnvName(w *workload, m *manifests) string {
	for _, c := range w.containers {
		img := strings.ToLower(imageName(c.image))
		if strings.Contains(img, "postgres") || strings.Contains(img, "pgbouncer") || strings.Contains(img, "pgpool") || strings.Contains(img, "spilo") {
			return ""
		}
	}
	for _, c := range w.containers {
		for _, e := range c.env {
			if dbLike(e.name) {
				return e.name
			}
		}
		for _, ef := range c.envFrom {
			if cm := findConfigMap(m, ef.cm, w.obj.ns); ef.cm != "" && cm != nil {
				var keys []string
				for k := range cm.data {
					if dbLike(ef.prefix + k) {
						keys = append(keys, ef.prefix+k)
					}
				}
				if len(keys) > 0 {
					sort.Strings(keys)
					return keys[0]
				}
			}
		}
	}
	return ""
}

func dbLike(name string) bool {
	u := strings.ToUpper(name)
	return strings.Contains(u, "DATABASE") || strings.Contains(u, "POSTGRES") || strings.HasPrefix(u, "PG") || strings.HasPrefix(u, "DB_")
}

// match finds the workloads that run each program with pools. A container
// runs a program when its name, image name or command is the program's
// name, or the workload has that name and one container. --workload
// overrides this for a program.
func match(progs []*goProgram, m *manifests, opt options) ([]deployment, []*goProgram) {
	var deps []deployment
	var unmatched []*goProgram
	for _, gp := range progs {
		if len(gp.pools) == 0 {
			continue
		}
		found := false
		if names, ok := opt.overrides[gp.name]; ok {
			for _, want := range names {
				wname, cname, _ := strings.Cut(want, "/")
				for _, w := range m.workloads {
					if w.obj.name != wname {
						continue
					}
					for _, c := range w.containers {
						if cname == "" && len(w.containers) > 1 && !containerRuns(c, gp.name) {
							continue
						}
						if cname != "" && c.name != cname {
							continue
						}
						deps = append(deps, deployment{gp, w, c, "--workload"})
						found = true
					}
				}
			}
		} else {
			for _, w := range m.workloads {
				for _, c := range w.containers {
					short, _, _ := strings.Cut(gp.name, ".")
					if containerRuns(c, gp.name) || ((w.obj.name == gp.name || w.obj.name == short) && len(w.containers) == 1) {
						deps = append(deps, deployment{gp, w, c, "name"})
						found = true
					}
				}
			}
		}
		if !found {
			unmatched = append(unmatched, gp)
		}
	}
	return deps, unmatched
}

func containerRuns(c *container, name string) bool {
	// A module named miniflux.app builds a binary of that name, and
	// manifests call it miniflux.
	if short, _, ok := strings.Cut(name, "."); ok && short != "" && containerRuns(c, short) {
		return true
	}
	if c.name == name || imageName(c.image) == name {
		return true
	}
	if len(c.command) > 0 && baseName(c.command[0]) == name {
		return true
	}
	return false
}

func unlimitedFinding(gp *goProgram, pl *pool, r resolved) Finding {
	return Finding{Code: "TC103", lvl: levelError, File: pl.pos.Filename, Line: pl.pos.Line,
		Message: fmt.Sprintf("%s opens a database/sql pool with no connection limit (%s), so one pod alone can use every connection Postgres has", gp.name, pl.call),
		Details: []string{r.text, "database/sql opens a new connection whenever every open one is busy, unless SetMaxOpenConns is above 0. Under load or with slow queries, each pod keeps opening connections until Postgres refuses them."},
		Fix:     "Call db.SetMaxOpenConns with a value that fits the budget, such as the available connections divided by the most pods."}
}
