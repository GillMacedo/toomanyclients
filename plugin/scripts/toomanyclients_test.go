package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// write creates files under a new temporary folder and returns it.
func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func runTool(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

// runJSON runs with --format json and decodes the report.
func runJSON(t *testing.T, args ...string) (int, report) {
	t.Helper()
	code, out, errs := runTool(t, append([]string{"--format", "json"}, args...)...)
	var rep report
	if code == 3 {
		t.Fatalf("exit 3: %s", errs)
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("bad JSON (exit %d): %v\n%s\n%s", code, err, out, errs)
	}
	return code, rep
}

func codes(rep report) string {
	var c []string
	for _, f := range rep.Findings {
		c = append(c, f.Code+":"+f.Level)
	}
	return strings.Join(c, ",")
}

const goMod = "module example.com/app\n\ngo 1.22\n"

func sqlMain(limit string) string {
	return `package main

import (
	"database/sql"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		panic(err)
	}
	` + limit + `
	_ = db
}
`
}

func deployYAML(name string, replicas int, extra string) string {
	r := ""
	if replicas >= 0 {
		r = "  replicas: " + itoa(replicas) + "\n"
	}
	return `apiVersion: apps/v1
kind: Deployment
metadata:
  name: ` + name + `
spec:
` + r + `  selector:
    matchLabels:
      app: ` + name + `
  template:
    metadata:
      labels:
        app: ` + name + `
    spec:
      containers:
        - name: ` + name + `
          image: registry.example.com/` + name + `:1.0
` + extra
}

func itoa(n int) string { return strconv.Itoa(n) }

func hpa(target string, max int) string {
	return `apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: ` + target + `
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: ` + target + `
  maxReplicas: ` + itoa(max) + `
`
}

func TestUnlimitedSQLPool(t *testing.T) {
	dir := writeFiles(t, map[string]string{"go.mod": goMod, "cmd/api/main.go": sqlMain(""), "deploy.yaml": deployYAML("api", 2, "")})
	code, rep := runJSON(t, "--max-connections", "100", dir)
	if code != 1 || !strings.Contains(codes(rep), "TC103:error") || !rep.Budget.Unlimited {
		t.Fatalf("code %d, %s", code, codes(rep))
	}
}

func TestZeroMeansUnlimited(t *testing.T) {
	dir := writeFiles(t, map[string]string{"go.mod": goMod, "cmd/api/main.go": sqlMain("db.SetMaxOpenConns(0)"), "deploy.yaml": deployYAML("api", 2, "")})
	_, rep := runJSON(t, "--max-connections", "100", dir)
	if !strings.Contains(codes(rep), "TC103") {
		t.Fatal(codes(rep))
	}
}

func TestBoundary(t *testing.T) {
	// 97 available; Recreate means no surge pods.
	for _, tc := range []struct {
		per  string
		want string
		code int
	}{{"97", "", 0}, {"98", "TC101:error", 1}} {
		extra := ""
		dir := writeFiles(t, map[string]string{"go.mod": goMod, "cmd/api/main.go": sqlMain("db.SetMaxOpenConns(" + tc.per + ")"),
			"deploy.yaml": strings.Replace(deployYAML("api", 1, extra), "spec:\n  replicas: 1\n", "spec:\n  replicas: 1\n  strategy:\n    type: Recreate\n", 1)})
		code, rep := runJSON(t, "--max-connections", "100", dir)
		if code != tc.code || codes(rep) != tc.want {
			t.Fatalf("per %s: code %d, %s", tc.per, code, codes(rep))
		}
	}
}

func TestRolloutOnly(t *testing.T) {
	// 4 pods x 20 = 80 fits 97; maxSurge 25% of 4 rounds up to 1: 100.
	dir := writeFiles(t, map[string]string{"go.mod": goMod, "cmd/api/main.go": sqlMain("db.SetMaxOpenConns(20)"), "deploy.yaml": deployYAML("api", 4, "")})
	code, rep := runJSON(t, "--max-connections", "100", dir)
	if code != 1 || codes(rep) != "TC102:warn" || rep.Budget.Steady != 80 || rep.Budget.Peak != 100 {
		t.Fatalf("code %d, %s, %+v", code, codes(rep), rep.Budget)
	}
	code, _ = runJSON(t, "--max-connections", "100", "--fail-on", "error", dir)
	if code != 0 {
		t.Fatal("TC102 is a warning")
	}
}

func TestSurgeRounding(t *testing.T) {
	w := &workload{surge: "25%"}
	for pods, want := range map[int]int{1: 1, 4: 1, 5: 2, 8: 2, 10: 3} {
		if got, _ := w.surgePods(pods); got != want {
			t.Errorf("25%% of %d: got %d, want %d", pods, got, want)
		}
	}
	w.surge = "2"
	if got, _ := w.surgePods(10); got != 2 {
		t.Error(got)
	}
	w.recreate = true
	if got, _ := w.surgePods(10); got != 0 {
		t.Error(got)
	}
}

func TestHPAMaxAndStatefulSet(t *testing.T) {
	ss := strings.Replace(deployYAML("api", 2, ""), "kind: Deployment", "kind: StatefulSet", 1)
	dir := writeFiles(t, map[string]string{"go.mod": goMod, "cmd/api/main.go": sqlMain("db.SetMaxOpenConns(10)"),
		"deploy.yaml": strings.Replace(hpa("api", 12), "kind: Deployment", "kind: StatefulSet", 1) + "---\n" + ss})
	code, rep := runJSON(t, "--max-connections", "100", dir)
	if code != 1 || rep.Budget.Steady != 120 || rep.Budget.Peak != 120 || !strings.Contains(codes(rep), "TC101") {
		t.Fatalf("code %d %s %+v", code, codes(rep), rep.Budget)
	}
}

func TestKEDADefaultMax(t *testing.T) {
	keda := `apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: api
spec:
  scaleTargetRef:
    name: api
`
	dir := writeFiles(t, map[string]string{"go.mod": goMod, "cmd/api/main.go": sqlMain("db.SetMaxOpenConns(1)"), "deploy.yaml": keda + "---\n" + deployYAML("api", 1, "")})
	_, rep := runJSON(t, "--max-connections", "1000", dir)
	if rep.Budget.Rows[0].Pods != 100 {
		t.Fatalf("%+v", rep.Budget.Rows[0])
	}
}

func TestScalerTargetMissing(t *testing.T) {
	dir := writeFiles(t, map[string]string{"go.mod": goMod, "cmd/api/main.go": sqlMain("db.SetMaxOpenConns(1)"), "deploy.yaml": hpa("web", 5) + "---\n" + deployYAML("api", 1, "")})
	code, rep := runJSON(t, "--max-connections", "100", dir)
	if code != 1 || !strings.Contains(codes(rep), "TC906") {
		t.Fatal(codes(rep))
	}
}

func TestEnvValues(t *testing.T) {
	helper := `package main

import (
	"database/sql"
	"os"
	"strconv"
)

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		n, _ := strconv.Atoi(v)
		return n
	}
	return def
}

func main() {
	db, _ := sql.Open("postgres", "")
	db.SetMaxOpenConns(envInt("DB_MAX", 7))
}
`
	env := func(s string) string { return "          env:\n" + s }
	cm := `---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg
data:
  DB_MAX: "9"
`
	for name, tc := range map[string]struct {
		extra string
		per   int
		code  string
	}{
		"literal":    {env("            - name: DB_MAX\n              value: \"5\"\n"), 5, ""},
		"default":    {"", 7, ""},
		"configmap":  {env("            - name: DB_MAX\n              valueFrom:\n                configMapKeyRef:\n                  name: cfg\n                  key: DB_MAX\n") + cm, 9, ""},
		"envFrom":    {"          envFrom:\n            - configMapRef:\n                name: cfg\n" + cm, 9, ""},
		"secret":     {env("            - name: DB_MAX\n              valueFrom:\n                secretKeyRef:\n                  name: s\n                  key: k\n"), 0, "TC901"},
		"notNumber":  {env("            - name: DB_MAX\n              value: lots\n"), 0, "TC901"},
		"expansion":  {env("            - name: DB_MAX\n              value: $(OTHER)\n"), 0, "TC901"},
		"secretFrom": {"          envFrom:\n            - secretRef:\n                name: s\n", 0, "TC901"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeFiles(t, map[string]string{"go.mod": goMod, "cmd/api/main.go": helper, "deploy.yaml": deployYAML("api", 1, tc.extra)})
			code, rep := runJSON(t, "--max-connections", "100", dir)
			if tc.code != "" {
				if code != 1 || !strings.Contains(codes(rep), tc.code) || !rep.Budget.Rows[0].AtLeast {
					t.Fatalf("code %d %s", code, codes(rep))
				}
				return
			}
			if code != 0 || rep.Budget.Rows[0].PerPod != tc.per {
				t.Fatalf("code %d %s %+v", code, codes(rep), rep.Budget.Rows[0])
			}
		})
	}
}

func TestFallbackAssignment(t *testing.T) {
	src := `package main

import (
	"database/sql"
	"os"
	"strconv"
)

func main() {
	db, _ := sql.Open("pgx", "")
	max := 25
	if v := os.Getenv("MAX_CONNS"); v != "" {
		max, _ = strconv.Atoi(v)
	}
	db.SetMaxOpenConns(max)
}
`
	dir := writeFiles(t, map[string]string{"go.mod": goMod, "cmd/api/main.go": src, "deploy.yaml": deployYAML("api", 1, "")})
	_, rep := runJSON(t, "--max-connections", "100", dir)
	if rep.Budget.Rows[0].PerPod != 25 {
		t.Fatalf("%+v", rep.Budget.Rows[0])
	}
	dir = writeFiles(t, map[string]string{"go.mod": goMod, "cmd/api/main.go": src, "deploy.yaml": deployYAML("api", 1, "          env:\n            - name: MAX_CONNS\n              value: \"40\"\n")})
	_, rep = runJSON(t, "--max-connections", "100", dir)
	if rep.Budget.Rows[0].PerPod != 40 {
		t.Fatalf("%+v", rep.Budget.Rows[0])
	}
}

func TestFlags(t *testing.T) {
	src := `package main

import (
	"database/sql"
	"flag"
)

var maxConns = flag.Int("db-max-conns", 10, "pool size")

func main() {
	flag.Parse()
	db, _ := sql.Open("pgx", "")
	db.SetMaxOpenConns(*maxConns)
}
`
	for args, want := range map[string]int{
		"": 10,
		"          args: [\"-db-max-conns=30\"]\n":                         30,
		"          args: [\"--db-max-conns\", \"31\"]\n":                   31,
		"          command: [\"sh\", \"-c\", \"/api -db-max-conns 32\"]\n": 32,
	} {
		dir := writeFiles(t, map[string]string{"go.mod": goMod, "cmd/api/main.go": src, "deploy.yaml": deployYAML("api", 1, args)})
		_, rep := runJSON(t, "--max-connections", "100", dir)
		if rep.Budget.Rows[0].PerPod != want {
			t.Errorf("%q: %+v", args, rep.Budget.Rows[0])
		}
	}
}

func TestStructTags(t *testing.T) {
	for tag, want := range map[string]int{
		"`env:\"POOL\" envDefault:\"12\"`":    12,
		"`env:\"POOL, default=13\"`":          13,
		"`envconfig:\"POOL\" default:\"14\"`": 14,
		"`env:\"POOL\" env-default:\"15\"`":   15,
	} {
		src := `package main

import "database/sql"

type config struct {
	Pool int ` + tag + `
}

func main() {
	var cfg config
	db, _ := sql.Open("pgx", "")
	db.SetMaxOpenConns(cfg.Pool)
}
`
		dir := writeFiles(t, map[string]string{"go.mod": goMod, "cmd/api/main.go": src, "deploy.yaml": deployYAML("api", 1, "")})
		_, rep := runJSON(t, "--max-connections", "100", dir)
		if rep.Budget.Rows[0].PerPod != want {
			t.Errorf("%s: %+v", tag, rep.Budget.Rows[0])
		}
	}
	// kelseyhightower/envconfig adds a prefix to the variable name.
	src := "package main\n\nimport \"database/sql\"\n\ntype config struct {\n\tPool int `envconfig:\"POOL\" default:\"14\"`\n}\n\nfunc main() {\n\tvar cfg config\n\tdb, _ := sql.Open(\"pgx\", \"\")\n\tdb.SetMaxOpenConns(cfg.Pool)\n}\n"
	dir := writeFiles(t, map[string]string{"go.mod": goMod, "cmd/api/main.go": src, "deploy.yaml": deployYAML("api", 1, "          env:\n            - name: MYAPP_POOL\n              value: \"3\"\n")})
	_, rep := runJSON(t, "--max-connections", "100", dir)
	if rep.Budget.Rows[0].PerPod != 3 {
		t.Errorf("prefixed: %+v", rep.Budget.Rows[0])
	}
}

func TestRequiredTagNotSet(t *testing.T) {
	src := "package main\n\nimport \"database/sql\"\n\ntype config struct {\n\tPool int `env:\"POOL,required\"`\n}\n\nfunc main() {\n\tcfg := config{Pool: 1}\n\tdb, _ := sql.Open(\"pgx\", \"\")\n\tdb.SetMaxOpenConns(cfg.Pool)\n}\n"
	dir := writeFiles(t, map[string]string{"go.mod": goMod, "cmd/api/main.go": src, "deploy.yaml": deployYAML("api", 1, "")})
	code, rep := runJSON(t, "--max-connections", "100", dir)
	if code != 1 || !strings.Contains(codes(rep), "TC901") {
		t.Fatal(codes(rep))
	}
}

func pgxMain(body string) string {
	return `package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

var _ = fmt.Sprintf
var _ = os.Getenv

func main() {
	ctx := context.Background()
	` + body + `
}
`
}

func TestPgxpool(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		per  int
		code string
		args []string
	}{
		"default":       {`pool, _ := pgxpool.New(ctx, os.Getenv("DATABASE_URL")); _ = pool`, 4, "TC104:warn", nil},
		"defaultCPUs":   {`pool, _ := pgxpool.New(ctx, os.Getenv("DATABASE_URL")); _ = pool`, 16, "", []string{"--node-cpus", "16"}},
		"defaultSmall":  {`pool, _ := pgxpool.New(ctx, os.Getenv("DATABASE_URL")); _ = pool`, 4, "", []string{"--node-cpus", "2"}},
		"dsnLiteral":    {`pool, _ := pgxpool.New(ctx, "postgres://db/app?pool_max_conns=6"); _ = pool`, 6, "", nil},
		"dsnKeyValue":   {`pool, _ := pgxpool.New(ctx, "host=db pool_max_conns=7 dbname=app"); _ = pool`, 7, "", nil},
		"sprintf":       {`pool, _ := pgxpool.New(ctx, fmt.Sprintf("postgres://%s/app?pool_max_conns=%d", os.Getenv("H"), 8)); _ = pool`, 8, "", nil},
		"config":        {`cfg, _ := pgxpool.ParseConfig(os.Getenv("DATABASE_URL")); cfg.MaxConns = 9; pool, _ := pgxpool.NewWithConfig(ctx, cfg); _ = pool`, 9, "", nil},
		"configDSN":     {`cfg, _ := pgxpool.ParseConfig("postgres://db/app?pool_max_conns=11"); pool, _ := pgxpool.NewWithConfig(ctx, cfg); _ = pool`, 11, "", nil},
		"configDefault": {`cfg, _ := pgxpool.ParseConfig("postgres://db/app"); pool, _ := pgxpool.NewWithConfig(ctx, cfg); _ = pool`, 4, "TC104:warn", nil},
		"dsnUnreadable": {`pool, _ := pgxpool.New(ctx, dsn()); _ = pool`, 0, "TC901:warn", nil},
	} {
		t.Run(name, func(t *testing.T) {
			files := map[string]string{"go.mod": goMod, "cmd/api/main.go": pgxMain(tc.body), "deploy.yaml": deployYAML("api", 1, "")}
			if name == "dsnUnreadable" {
				files["cmd/api/dsn.go"] = "package main\n\nimport \"os\"\n\nfunc dsn() string {\n\tu := os.Getenv(\"A\")\n\tu += os.Getenv(\"B\")\n\treturn u\n}\n"
			}
			dir := writeFiles(t, files)
			_, rep := runJSON(t, append([]string{"--max-connections", "100"}, append(tc.args, dir)...)...)
			if codes(rep) != tc.code {
				t.Fatalf("got %q want %q", codes(rep), tc.code)
			}
			if tc.per > 0 && rep.Budget.Rows[0].PerPod != tc.per {
				t.Fatalf("%+v", rep.Budget.Rows[0])
			}
		})
	}
}

func TestPgxDefaultBreakPoint(t *testing.T) {
	// 10 pods + 3 surge = 13 pools on the default. 13 x 7 = 91 fits 97,
	// 13 x 8 = 104 doesn't.
	dir := writeFiles(t, map[string]string{"go.mod": goMod, "cmd/api/main.go": pgxMain(`pool, _ := pgxpool.New(ctx, ""); _ = pool`), "deploy.yaml": deployYAML("api", 10, "")})
	_, rep := runJSON(t, "--max-connections", "100", dir)
	if len(rep.Findings) != 1 || !strings.Contains(rep.Findings[0].Message, "8 or more CPUs") {
		t.Fatalf("%+v", rep.Findings)
	}
}

func TestPgxDSNFromEnv(t *testing.T) {
	body := `pool, _ := pgxpool.New(ctx, os.Getenv("DATABASE_URL")); _ = pool`
	set := "          env:\n            - name: DATABASE_URL\n              value: postgres://db/app?pool_max_conns=3\n"
	dir := writeFiles(t, map[string]string{"go.mod": goMod, "cmd/api/main.go": pgxMain(body), "deploy.yaml": deployYAML("api", 1, set)})
	_, rep := runJSON(t, "--max-connections", "100", dir)
	if rep.Budget.Rows[0].PerPod != 3 || codes(rep) != "" {
		t.Fatalf("%s %+v", codes(rep), rep.Budget.Rows[0])
	}
	secret := "          env:\n            - name: DATABASE_URL\n              valueFrom:\n                secretKeyRef:\n                  name: db\n                  key: uri\n"
	dir = writeFiles(t, map[string]string{"go.mod": goMod, "cmd/api/main.go": pgxMain(body), "deploy.yaml": deployYAML("api", 1, secret)})
	_, rep = runJSON(t, "--max-connections", "100", dir)
	if codes(rep) != "TC901:warn" {
		t.Fatal(codes(rep))
	}
}

func TestHelperCalledTwiceAndConstants(t *testing.T) {
	db := `package db

import "database/sql"

const MaxOpen = 2 * Base

const Base = 3

func Open(dsn string) *sql.DB {
	d, _ := sql.Open("pgx", dsn)
	d.SetMaxOpenConns(MaxOpen)
	return d
}
`
	main := `package main

import "example.com/app/internal/db"

func main() {
	primary := db.Open("primary")
	replica := db.Open("replica")
	_, _ = primary, replica
}
`
	other := "package main\n\nfunc main() {}\n"
	dir := writeFiles(t, map[string]string{"go.mod": goMod, "internal/db/db.go": db, "cmd/api/main.go": main, "cmd/tool/main.go": other, "deploy.yaml": deployYAML("api", 1, "") + "---\n" + deployYAML("tool", 1, "")})
	_, rep := runJSON(t, "--max-connections", "100", dir)
	if len(rep.Budget.Rows) != 1 || rep.Budget.Rows[0].PerPod != 12 {
		t.Fatalf("%+v", rep.Budget.Rows)
	}
}

func TestParamFromCaller(t *testing.T) {
	db := "package db\n\nimport \"database/sql\"\n\nfunc Open(n int) *sql.DB {\n\td, _ := sql.Open(\"pgx\", \"\")\n\td.SetMaxOpenConns(n)\n\treturn d\n}\n"
	main := "package main\n\nimport \"example.com/app/internal/db\"\n\nfunc main() { db.Open(17) }\n"
	dir := writeFiles(t, map[string]string{"go.mod": goMod, "internal/db/db.go": db, "cmd/api/main.go": main, "deploy.yaml": deployYAML("api", 1, "")})
	_, rep := runJSON(t, "--max-connections", "100", dir)
	if rep.Budget.Rows[0].PerPod != 17 {
		t.Fatalf("%+v", rep.Budget.Rows)
	}
}

func TestOtherDatabasesSkipped(t *testing.T) {
	src := "package main\n\nimport \"database/sql\"\n\nfunc main() {\n\tdb, _ := sql.Open(\"mysql\", \"\")\n\t_ = db\n}\n"
	dir := writeFiles(t, map[string]string{"go.mod": goMod, "cmd/api/main.go": src, "deploy.yaml": deployYAML("api", 1, "")})
	code, _, errs := runTool(t, dir)
	if code != 3 || !strings.Contains(errs, "nothing to count") {
		t.Fatal(code, errs)
	}
}

func TestUnmatchedAndOverride(t *testing.T) {
	files := map[string]string{"go.mod": goMod, "cmd/api/main.go": sqlMain("db.SetMaxOpenConns(5)"), "deploy.yaml": deployYAML("web", 2, "")}
	dir := writeFiles(t, files)
	code, rep := runJSON(t, "--max-connections", "100", dir)
	if code != 1 || codes(rep) != "TC902:warn" {
		t.Fatal(codes(rep))
	}
	code, rep = runJSON(t, "--max-connections", "100", "--workload", "api=web", dir)
	if code != 0 || rep.Budget.Steady != 10 {
		t.Fatalf("%d %s %+v", code, codes(rep), rep.Budget)
	}
}

func TestUncountedClientWorkload(t *testing.T) {
	py := strings.Replace(deployYAML("reports", 1, "          env:\n            - name: DATABASE_URL\n              value: postgres://db/app\n"), "registry.example.com/reports:1.0", "python:3.12", 1)
	dir := writeFiles(t, map[string]string{"go.mod": goMod, "cmd/api/main.go": sqlMain("db.SetMaxOpenConns(5)"), "deploy.yaml": deployYAML("api", 1, "") + "---\n" + py})
	code, rep := runJSON(t, "--max-connections", "100", dir)
	if code != 1 || codes(rep) != "TC903:warn" {
		t.Fatal(codes(rep))
	}
	code, rep = runJSON(t, "--max-connections", "100", "--reserve", "20", dir)
	if code != 0 || codes(rep) != "TC903:note" || rep.Budget.Available != 77 {
		t.Fatal(code, codes(rep), rep.Budget.Available)
	}
}

func TestDaemonSetNeedsNodes(t *testing.T) {
	ds := strings.Replace(deployYAML("api", -1, ""), "kind: Deployment", "kind: DaemonSet", 1)
	dir := writeFiles(t, map[string]string{"go.mod": goMod, "cmd/api/main.go": sqlMain("db.SetMaxOpenConns(5)"), "deploy.yaml": ds})
	code, rep := runJSON(t, "--max-connections", "100", dir)
	if code != 1 || codes(rep) != "TC904:warn" {
		t.Fatal(codes(rep))
	}
	code, rep = runJSON(t, "--max-connections", "100", "--nodes", "30", dir)
	if code != 1 || codes(rep) != "TC101:error" || rep.Budget.Steady != 150 {
		t.Fatal(codes(rep), rep.Budget.Steady)
	}
}

func TestLimitSources(t *testing.T) {
	base := map[string]string{"go.mod": goMod, "cmd/api/main.go": sqlMain("db.SetMaxOpenConns(10)"), "deploy.yaml": deployYAML("api", 1, "")}
	dir := writeFiles(t, base)
	code, rep := runJSON(t, dir)
	if code != 1 || codes(rep) != "TC905:warn" || rep.Budget.Available != 97 {
		t.Fatal("assumed limit must not be clean:", codes(rep))
	}
	conf := writeFiles(t, map[string]string{"postgresql.conf": "# comment\nlisten_addresses = '*'\nmax_connections = 50\t\t# note\nsuperuser_reserved_connections = 5\nreserved_connections = 2\nshared_buffers 128MB\n"})
	code, rep = runJSON(t, "--pg-config", filepath.Join(conf, "postgresql.conf"), dir)
	if code != 0 || rep.Budget.Available != 43 {
		t.Fatal(code, rep.Budget.Available)
	}
	code, rep = runJSON(t, "--max-connections", "20", "--superuser-reserved", "0", dir)
	if rep.Budget.Available != 20 {
		t.Fatal(rep.Budget.Available)
	}
	cnpg := "apiVersion: postgresql.cnpg.io/v1\nkind: Cluster\nmetadata:\n  name: db\nspec:\n  instances: 1\n  postgresql:\n    parameters:\n      max_connections: \"30\"\n"
	withServer := map[string]string{"db.yaml": cnpg}
	for k, v := range base {
		withServer[k] = v
	}
	dir = writeFiles(t, withServer)
	code, rep = runJSON(t, dir)
	if code != 0 || rep.Budget.Available != 27 {
		t.Fatal(code, codes(rep), rep.Budget.Available)
	}
	withServer["db2.yaml"] = strings.Replace(cnpg, "name: db", "name: db2", 1)
	dir = writeFiles(t, withServer)
	if code, _, errs := runTool(t, dir); code != 3 || !strings.Contains(errs, "2 Postgres servers") {
		t.Fatal(code, errs)
	}
}

func TestBadPgConfig(t *testing.T) {
	dir := writeFiles(t, map[string]string{"go.mod": goMod, "cmd/api/main.go": sqlMain("db.SetMaxOpenConns(10)"), "deploy.yaml": deployYAML("api", 1, "")})
	for name, body := range map[string]string{
		"empty":     "",
		"noMax":     "shared_buffers = 128MB\n",
		"garbage":   "\x00\x01\x02binary",
		"cut":       "max_connections = 10",
		"psqlError": "psql: error: connection to server failed\n",
		"notNumber": "max_connections = many\n",
		"panic":     "panic: runtime error\n",
	} {
		conf := writeFiles(t, map[string]string{"postgresql.conf": body})
		for _, fail := range []string{"warn", "never"} {
			code, out, _ := runTool(t, "--fail-on", fail, "--pg-config", filepath.Join(conf, "postgresql.conf"), dir)
			if code != 3 || out != "" {
				t.Errorf("%s (--fail-on %s): code %d, out %q", name, fail, code, out)
			}
		}
	}
}

func TestRefusedInput(t *testing.T) {
	good := map[string]string{"go.mod": goMod, "cmd/api/main.go": sqlMain("db.SetMaxOpenConns(10)"), "deploy.yaml": deployYAML("api", 1, "")}
	cases := map[string]map[string]string{
		"helm template":   {"deploy.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: {{ .Release.Name }}\n"},
		"helm error":      {"deploy.yaml": "Error: INSTALLATION FAILED: template: x\n"},
		"no containers":   {"deploy.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\nspec:\n  replicas: 2\n"},
		"empty replicas":  {"deploy.yaml": strings.Replace(deployYAML("api", 1, ""), "replicas: 1", "replicas:", 1)},
		"hpa no max":      {"deploy.yaml": strings.Replace(hpa("api", 3), "  maxReplicas: 3\n", "", 1) + "---\n" + deployYAML("api", 1, "")},
		"mixed docs":      {"deploy.yaml": deployYAML("api", 1, "") + "---\nfoo: bar\n"},
		"plain text yaml": {"deploy.yaml": "just some words\n"},
		"go build output": {"cmd/api/main.go": "# example.com/app/cmd/api\ncmd/api/main.go:3:2: undefined: x\n"},
		"go cut":          {"cmd/api/main.go": "package main\n\nfunc main() {\n"},
		"no newline":      {"cmd/api/main.go": strings.TrimSuffix(sqlMain("db.SetMaxOpenConns(10)"), "\n")},
		"binary go":       {"cmd/api/main.go": "\x7fELF\x00\x00"},
		"empty go":        {"cmd/api/main.go": ""},
		"no main":         {"cmd/api/main.go": strings.Replace(sqlMain("db.SetMaxOpenConns(10)"), "package main", "package lib", 1)},
		"no pool":         {"cmd/api/main.go": "package main\n\nfunc main() {}\n"},
		"no workloads":    {"deploy.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"},
		"no manifests":    {"deploy.yaml": ""},
	}
	for name, change := range cases {
		files := map[string]string{}
		for k, v := range good {
			files[k] = v
		}
		for k, v := range change {
			if v == "" && name == "no manifests" {
				delete(files, k)
				continue
			}
			files[k] = v
		}
		dir := writeFiles(t, files)
		for _, fail := range []string{"warn", "never"} {
			code, out, errs := runTool(t, "--fail-on", fail, "--max-connections", "100", dir)
			if code != 3 || out != "" || !strings.Contains(errs, "no result") {
				t.Errorf("%s (--fail-on %s): code %d, out %q, err %q", name, fail, code, out, errs)
			}
		}
	}
}

func TestNonManifestYAMLIgnoredInFolders(t *testing.T) {
	files := map[string]string{"go.mod": goMod, "cmd/api/main.go": sqlMain("db.SetMaxOpenConns(10)"), "deploy.yaml": deployYAML("api", 1, ""),
		"docker-compose.yml": "services:\n  db:\n    image: postgres", "chart/Chart.yaml": "name: x\n", "chart/templates/d.yaml": "kind: {{ x }}\n"}
	dir := writeFiles(t, files)
	code, rep := runJSON(t, "--max-connections", "100", dir)
	if code != 0 || len(rep.Budget.Rows) != 1 {
		t.Fatal(code, codes(rep))
	}
	// Named on the command line, a file without objects is refused.
	if code, _, _ := runTool(t, "--max-connections", "100", dir, filepath.Join(dir, "docker-compose.yml")); code != 3 {
		t.Fatal(code)
	}
}

func TestOptions(t *testing.T) {
	for _, args := range [][]string{{"--format", "xml", "."}, {"--fail-on", "sometimes", "."}, {}, {"--max-connections", "-1", "."}, {"--pg-config", "x", "--max-connections", "5", "."}, {"--workload", "api", "."}, {"--nope"}} {
		if code, _, _ := runTool(t, args...); code != 2 {
			t.Errorf("%v: %d", args, code)
		}
	}
	if code, out, _ := runTool(t, "--version"); code != 0 || !strings.Contains(out, version) {
		t.Error(out)
	}
	if code, _, errs := runTool(t, "/does/not/exist"); code != 3 || !strings.Contains(errs, "no result") {
		t.Error(errs)
	}
	dir := writeFiles(t, map[string]string{"build.log": "FAIL\n"})
	if code, _, _ := runTool(t, filepath.Join(dir, "build.log")); code != 3 {
		t.Error("a .log file must be refused")
	}
}

func TestFailOnNever(t *testing.T) {
	dir := writeFiles(t, map[string]string{"go.mod": goMod, "cmd/api/main.go": sqlMain(""), "deploy.yaml": deployYAML("api", 2, "")})
	code, out, _ := runTool(t, "--fail-on", "never", "--max-connections", "100", dir)
	if code != 0 || !strings.Contains(out, "TC103") || strings.Contains(out, "Result: at most") {
		t.Fatal(code, out)
	}
}
