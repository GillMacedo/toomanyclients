package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// dbLimit is how many connections Postgres accepts and where that number
// comes from.
type dbLimit struct {
	max       int
	superuser int
	reserved  int
	extra     int // --reserve: kept for clients outside these files
	source    string
	assumed   bool
}

func (l dbLimit) available() int { return l.max - l.superuser - l.reserved - l.extra }

func (l dbLimit) describe() string {
	s := fmt.Sprintf("max_connections %d - superuser_reserved_connections %d", l.max, l.superuser)
	if l.reserved > 0 {
		s += fmt.Sprintf(" - reserved_connections %d", l.reserved)
	}
	if l.extra > 0 {
		s += fmt.Sprintf(" - --reserve %d", l.extra)
	}
	return s + fmt.Sprintf(" = %d", l.available())
}

// PostgreSQL's defaults, the same in versions 12 to 18.
const (
	defaultMaxConnections = 100
	defaultSuperuser      = 3
)

var confLine = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_.]*)\s*(=\s*)?(.*?)\s*$`)

// readPgConf reads max_connections and the reserved slots from a
// postgresql.conf file. Lines that aren't settings or comments are
// refused, and so is a file without max_connections, since that is the
// value the file was passed for.
func readPgConf(path string) (dbLimit, error) {
	data, err := readChecked(path, "a postgresql.conf file")
	if err != nil {
		return dbLimit{}, err
	}
	l := dbLimit{max: -1, superuser: defaultSuperuser, source: path}
	var includes []string
	for i, line := range strings.Split(string(data), "\n") {
		if c := strings.Index(line, "#"); c >= 0 && !strings.Contains(line[:c], "'") {
			line = line[:c]
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		m := confLine.FindStringSubmatch(line)
		if m == nil || (m[2] == "" && m[3] == "") {
			return dbLimit{}, refuse("%s:%d: not a postgresql.conf setting: %q", path, i+1, truncate(strings.TrimSpace(line), 80))
		}
		name, val := strings.ToLower(m[1]), strings.Trim(m[3], `'`)
		num := func() (int, error) {
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 {
				return 0, refuse("%s:%d: %s = %q isn't a whole number", path, i+1, name, val)
			}
			return n, nil
		}
		switch name {
		case "max_connections":
			if l.max, err = num(); err != nil {
				return dbLimit{}, err
			}
		case "superuser_reserved_connections":
			if l.superuser, err = num(); err != nil {
				return dbLimit{}, err
			}
		case "reserved_connections":
			if l.reserved, err = num(); err != nil {
				return dbLimit{}, err
			}
		case "include", "include_if_exists", "include_dir":
			includes = append(includes, val)
		}
	}
	if l.max < 0 {
		msg := fmt.Sprintf("%s has no max_connections setting", path)
		if len(includes) > 0 {
			msg += " (it includes " + strings.Join(includes, ", ") + ", which isn't read)"
		}
		return dbLimit{}, refuse("%s. Pass the file that sets it, or --max-connections", msg)
	}
	return l, nil
}

// limitFromServer reads the limit from a CloudNativePG Cluster or a
// Zalando postgresql object.
func limitFromServer(s *dbServer) (dbLimit, error) {
	l := dbLimit{max: defaultMaxConnections, superuser: defaultSuperuser, source: fmt.Sprintf("%s (%s) at %s", s.obj.ref(), s.operator, s.obj.where())}
	set := false
	for name, dst := range map[string]*int{"max_connections": &l.max, "superuser_reserved_connections": &l.superuser, "reserved_connections": &l.reserved} {
		v, ok := s.params[name]
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < 0 {
			return dbLimit{}, refuse("%s: %s %q isn't a whole number", s.obj.where(), name, v)
		}
		*dst = n
		if name == "max_connections" {
			set = true
		}
	}
	if !set {
		l.assumed = true
		l.source += ", which doesn't set max_connections, so PostgreSQL's default of 100 is assumed"
	}
	return l, nil
}
