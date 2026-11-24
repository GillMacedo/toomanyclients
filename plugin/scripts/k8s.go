package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

// obj is one Kubernetes object read from a manifest file.
type obj struct {
	file             string
	line             int
	apiVersion, kind string
	name, ns         string
	node             *Node
}

func (o *obj) ref() string { return o.kind + " " + o.name }

func (o *obj) where() string { return fmt.Sprintf("%s:%d", o.file, o.line) }

// get walks mapping keys from n and returns the node, or nil.
func get(n *Node, path ...string) *Node {
	for _, k := range path {
		if n == nil || n.Kind != MappingNode {
			return nil
		}
		var next *Node
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == k {
				next = n.Content[i+1]
			}
		}
		n = next
	}
	return n
}

func str(n *Node, path ...string) string {
	n = get(n, path...)
	if n == nil || n.Kind != ScalarNode || n.Tag == "!!null" {
		return ""
	}
	return n.Value
}

func items(n *Node) []*Node {
	if n == nil || n.Kind != SequenceNode {
		return nil
	}
	return n.Content
}

// intField reads an optional integer. ok is false when the field is there
// but isn't an integer, such as an empty value left by a cut.
func intField(n *Node, path ...string) (v int, present, ok bool) {
	x := get(n, path...)
	if x == nil {
		return 0, false, true
	}
	if x.Kind != ScalarNode || x.Tag == "!!null" {
		return 0, true, false
	}
	i, err := strconv.Atoi(x.Value)
	if err != nil {
		return 0, true, false
	}
	return i, true, true
}

// loadManifests reads every object in the files. Files with no Kubernetes
// object at all, such as a docker-compose file, are skipped when they were
// found in a folder and refused when named on the command line.
func loadManifests(files []string, named map[string]bool) ([]*obj, []string, error) {
	var objs []*obj
	var ignored []string
	for _, f := range files {
		if !named[f] && !looksLikeManifest(f) {
			// A folder's YAML files that never mention apiVersion and
			// kind, such as CI or app config, are skipped when they are
			// valid YAML, even without a final newline. Broken ones are
			// still refused: one may be a manifest overwritten by an error.
			if err := checkOtherYAML(f); err != nil {
				return nil, nil, refuse("%s. If it should hold manifests, run the command that writes it again. If it isn't part of the deployment, pass only the folders with the code and the rendered manifests", err.Error())
			}
			ignored = append(ignored, f)
			continue
		}
		src, err := readChecked(f, "Kubernetes manifests")
		if err != nil {
			return nil, nil, err
		}
		if bytes.Contains(src, []byte("{{")) && bytes.Contains(src, []byte("}}")) {
			return nil, nil, refuse("%s: has template braces, so it looks like an unrendered Helm template. Render the chart with helm template and pass the output", f)
		}
		dec := NewDecoder(bytes.NewReader(src))
		var got []*obj
		other := 0
		for doc := 1; ; doc++ {
			var root Node
			err := dec.Decode(&root)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, nil, refuse("%s isn't valid YAML, so it may be cut off: %s", f, firstLine(err.Error()))
			}
			body := &root
			if body.Kind == DocumentNode && len(body.Content) == 1 {
				body = body.Content[0]
			}
			if body.Kind == ScalarNode && body.Tag == "!!null" {
				continue
			}
			if body.Kind == ScalarNode {
				return nil, nil, refuse("%s, document %d (line %d), is plain text, not YAML objects: %q", f, doc, body.Line, truncate(firstLine(body.Value), 80))
			}
			if body.Kind != MappingNode || str(body, "kind") == "" || str(body, "apiVersion") == "" {
				other++
				continue
			}
			o, err := objectsIn(f, doc, body)
			if err != nil {
				return nil, nil, err
			}
			got = append(got, o...)
		}
		if len(got) > 0 && other > 0 {
			return nil, nil, refuse("%s mixes Kubernetes objects with %d %s that have no kind or apiVersion, so part of it may be cut off", f, other, plural(other, "document", "documents"))
		}
		if len(got) == 0 {
			if named[f] {
				return nil, nil, refuse("%s has no Kubernetes objects (nothing with apiVersion and kind)", f)
			}
			ignored = append(ignored, f)
			continue
		}
		objs = append(objs, got...)
	}
	return objs, ignored, nil
}

func checkOtherYAML(f string) error {
	src, err := readText(f, "YAML")
	if err != nil {
		return err
	}
	dec := NewDecoder(bytes.NewReader(src))
	for doc := 1; ; doc++ {
		var root Node
		err := dec.Decode(&root)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return refuse("%s isn't valid YAML: %s", f, firstLine(err.Error()))
		}
		body := &root
		if body.Kind == DocumentNode && len(body.Content) == 1 {
			body = body.Content[0]
		}
		if body.Kind == ScalarNode && body.Tag != "!!null" {
			return refuse("%s, document %d (line %d), is plain text, not YAML: %q", f, doc, body.Line, truncate(firstLine(body.Value), 80))
		}
	}
}

// looksLikeManifest reports whether a file mentions both apiVersion and
// kind at the start of a line, as every Kubernetes object does.
func looksLikeManifest(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return true // let readChecked report it
	}
	text := "\n" + string(data)
	return strings.Contains(text, "\napiVersion:") || strings.Contains(text, "\nkind:") || strings.Contains(text, "  kind:")
}

func objectsIn(file string, doc int, body *Node) ([]*obj, error) {
	kind, api := str(body, "kind"), str(body, "apiVersion")
	if strings.HasSuffix(kind, "List") && get(body, "items") != nil {
		var out []*obj
		for _, it := range items(get(body, "items")) {
			if it.Kind != MappingNode || str(it, "kind") == "" {
				return nil, refuse("%s:%d: an item of %s has no kind, so it may be cut off", file, it.Line, kind)
			}
			got, err := objectsIn(file, doc, it)
			if err != nil {
				return nil, err
			}
			out = append(out, got...)
		}
		return out, nil
	}
	o := &obj{file: file, line: body.Line, apiVersion: api, kind: kind, node: body,
		name: str(body, "metadata", "name"), ns: str(body, "metadata", "namespace")}
	if o.name == "" && str(body, "metadata", "generateName") == "" {
		return nil, refuse("%s:%d: %s has no metadata.name, so it may be cut off", file, body.Line, kind)
	}
	if o.name == "" {
		o.name = str(body, "metadata", "generateName") + "*"
	}
	return []*obj{o}, nil
}

// container is one container of a pod template.
type container struct {
	name, image string
	command     []string
	args        []string
	env         []envVar
	envFrom     []envFrom
	line        int
}

type envVar struct {
	name     string
	value    string
	hasValue bool
	cmName   string // configMapKeyRef
	cmKey    string
	secret   string // secretKeyRef name
	other    string // fieldRef or resourceFieldRef
}

type envFrom struct {
	prefix string
	cm     string
	secret string
}

// workload is an object that runs pods: its pod count and containers.
type workload struct {
	obj        *obj
	containers []*container
	replicas   int
	hasReplica bool
	// surge is maxSurge as written, for Deployments with RollingUpdate.
	surge    string
	recreate bool
	daemon   bool
}

// scaler is a HorizontalPodAutoscaler or a KEDA ScaledObject.
type scaler struct {
	obj        *obj
	targetKind string
	targetName string
	max        int
}

// configMap is a ConfigMap's data.
type configMap struct {
	obj  *obj
	data map[string]string
}

// dbServer is a Postgres server defined in the manifests by an operator.
type dbServer struct {
	obj      *obj
	params   map[string]string
	operator string
}

type manifests struct {
	workloads  []*workload
	scalers    []*scaler
	configMaps []*configMap
	servers    []*dbServer
}

var workloadKinds = map[string]bool{"Deployment": true, "StatefulSet": true, "DaemonSet": true, "Job": true, "CronJob": true, "ReplicaSet": true, "Pod": true}

// readObjects turns objects into workloads, scalers, ConfigMaps and
// Postgres servers, refusing workloads that are missing what every real
// one has.
func readObjects(objs []*obj) (*manifests, error) {
	m := &manifests{}
	for _, o := range objs {
		switch {
		case workloadKinds[o.kind] && !strings.Contains(o.apiVersion, "."):
			w, err := readWorkload(o)
			if err != nil {
				return nil, err
			}
			m.workloads = append(m.workloads, w)
		case o.kind == "HorizontalPodAutoscaler":
			max, present, ok := intField(o.node, "spec", "maxReplicas")
			name := str(o.node, "spec", "scaleTargetRef", "name")
			if !present || !ok || name == "" {
				return nil, refuse("%s: %s has no spec.maxReplicas or spec.scaleTargetRef.name, so it may be cut off", o.where(), o.ref())
			}
			m.scalers = append(m.scalers, &scaler{obj: o, targetKind: str(o.node, "spec", "scaleTargetRef", "kind"), targetName: name, max: max})
		case o.kind == "ScaledObject" && strings.HasPrefix(o.apiVersion, "keda.sh/"):
			name := str(o.node, "spec", "scaleTargetRef", "name")
			max, present, ok := intField(o.node, "spec", "maxReplicaCount")
			if name == "" || !ok {
				return nil, refuse("%s: %s has no spec.scaleTargetRef.name or an unreadable maxReplicaCount, so it may be cut off", o.where(), o.ref())
			}
			if !present {
				max = 100 // KEDA's default maxReplicaCount
			}
			kind := str(o.node, "spec", "scaleTargetRef", "kind")
			if kind == "" {
				kind = "Deployment"
			}
			m.scalers = append(m.scalers, &scaler{obj: o, targetKind: kind, targetName: name, max: max})
		case o.kind == "ConfigMap":
			cm := &configMap{obj: o, data: map[string]string{}}
			d := get(o.node, "data")
			if d != nil && d.Kind == MappingNode {
				for i := 0; i+1 < len(d.Content); i += 2 {
					cm.data[d.Content[i].Value] = d.Content[i+1].Value
				}
			}
			m.configMaps = append(m.configMaps, cm)
		case o.kind == "Cluster" && strings.HasPrefix(o.apiVersion, "postgresql.cnpg.io/"):
			m.servers = append(m.servers, &dbServer{obj: o, params: scalarMap(get(o.node, "spec", "postgresql", "parameters")), operator: "CloudNativePG"})
		case o.kind == "postgresql" && strings.HasPrefix(o.apiVersion, "acid.zalan.do/"):
			m.servers = append(m.servers, &dbServer{obj: o, params: scalarMap(get(o.node, "spec", "postgresql", "parameters")), operator: "Zalando postgres-operator"})
		}
	}
	return m, nil
}

func scalarMap(n *Node) map[string]string {
	out := map[string]string{}
	if n == nil || n.Kind != MappingNode {
		return out
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		out[n.Content[i].Value] = n.Content[i+1].Value
	}
	return out
}

func readWorkload(o *obj) (*workload, error) {
	w := &workload{obj: o}
	spec := get(o.node, "spec")
	podSpec := get(spec, "template", "spec")
	switch o.kind {
	case "CronJob":
		podSpec = get(spec, "jobTemplate", "spec", "template", "spec")
		w.replicas, w.hasReplica = 1, true
		if p, present, ok := intField(spec, "jobTemplate", "spec", "parallelism"); !ok {
			return nil, refuse("%s: %s has an unreadable parallelism", o.where(), o.ref())
		} else if present {
			w.replicas = p
		}
	case "Job":
		w.replicas, w.hasReplica = 1, true
		if p, present, ok := intField(spec, "parallelism"); !ok {
			return nil, refuse("%s: %s has an unreadable parallelism", o.where(), o.ref())
		} else if present {
			w.replicas = p
		}
	case "Pod":
		podSpec = spec
		w.replicas, w.hasReplica = 1, true
	case "DaemonSet":
		w.daemon = true
	default:
		r, present, ok := intField(spec, "replicas")
		if !ok {
			return nil, refuse("%s: %s has a spec.replicas that isn't a number, so it may be cut off", o.where(), o.ref())
		}
		w.replicas, w.hasReplica = 1, present
		if present {
			w.replicas = r
		}
	}
	if o.kind == "Deployment" {
		switch str(spec, "strategy", "type") {
		case "Recreate":
			w.recreate = true
		default:
			w.surge = "25%"
			if s := str(spec, "strategy", "rollingUpdate", "maxSurge"); s != "" {
				w.surge = s
			}
		}
	}
	cs := items(get(podSpec, "containers"))
	if len(cs) == 0 {
		return nil, refuse("%s: %s has no containers, so it may be cut off", o.where(), o.ref())
	}
	for _, c := range cs {
		ct := &container{name: str(c, "name"), image: str(c, "image"), line: c.Line}
		if ct.name == "" || ct.image == "" {
			return nil, refuse("%s:%d: a container of %s has no name or image, so it may be cut off", o.file, c.Line, o.ref())
		}
		for _, x := range items(get(c, "command")) {
			ct.command = append(ct.command, x.Value)
		}
		for _, x := range items(get(c, "args")) {
			ct.args = append(ct.args, x.Value)
		}
		for _, e := range items(get(c, "env")) {
			ev := envVar{name: str(e, "name")}
			if ev.name == "" {
				return nil, refuse("%s:%d: an env entry of %s has no name, so it may be cut off", o.file, e.Line, o.ref())
			}
			if v := get(e, "value"); v != nil && v.Kind == ScalarNode {
				ev.value, ev.hasValue = v.Value, true
				if v.Tag == "!!null" {
					ev.value = ""
				}
			}
			switch {
			case get(e, "valueFrom", "configMapKeyRef") != nil:
				ev.cmName, ev.cmKey = str(e, "valueFrom", "configMapKeyRef", "name"), str(e, "valueFrom", "configMapKeyRef", "key")
			case get(e, "valueFrom", "secretKeyRef") != nil:
				ev.secret = str(e, "valueFrom", "secretKeyRef", "name")
			case get(e, "valueFrom") != nil:
				ev.other = "fieldRef or resourceFieldRef"
			case !ev.hasValue:
				ev.hasValue = true // name only: an empty value
			}
			ct.env = append(ct.env, ev)
		}
		for _, e := range items(get(c, "envFrom")) {
			ct.envFrom = append(ct.envFrom, envFrom{prefix: str(e, "prefix"), cm: str(e, "configMapRef", "name"), secret: str(e, "secretRef", "name")})
		}
		w.containers = append(w.containers, ct)
	}
	return w, nil
}

// surgePods is how many extra pods a rolling update adds on top of pods,
// the way the Deployment controller rounds maxSurge: percentages round up.
func (w *workload) surgePods(pods int) (int, error) {
	if w.recreate || w.surge == "" {
		return 0, nil
	}
	if strings.HasSuffix(w.surge, "%") {
		p, err := strconv.Atoi(strings.TrimSuffix(w.surge, "%"))
		if err != nil {
			return 0, err
		}
		return int(math.Ceil(float64(pods) * float64(p) / 100)), nil
	}
	return strconv.Atoi(w.surge)
}

// imageName is an image's last path element without tag or digest.
func imageName(image string) string {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	last := image[strings.LastIndex(image, "/")+1:]
	if i := strings.Index(last, ":"); i >= 0 {
		last = last[:i]
	}
	return last
}

func baseName(s string) string {
	return s[strings.LastIndex(s, "/")+1:]
}
