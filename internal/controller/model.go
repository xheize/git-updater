// Package controller interprets immutable Git snapshots. It has no Git, HTTP,
// credential or cluster access; the same model feeds planning and visualization.
package controller

import (
	"bytes"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/distribution/reference"
	"gopkg.in/yaml.v3"
)

type Location struct {
	File     string `json:"file"`
	Document int    `json:"document"`
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
	Value    string `json:"value"`
	TagOnly  bool   `json:"tagOnly"`
}

func (l Location) Key() string { return fmt.Sprintf("%s#%d:%s", l.File, l.Document, l.Path) }

type Resource struct {
	ID        string `json:"id"`
	File      string `json:"file"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}
type Use struct {
	Resource       string     `json:"resource"`
	Container      string     `json:"container"`
	Environment    string     `json:"environment"`
	SourceImage    string     `json:"sourceImage"`
	EffectiveImage string     `json:"effectiveImage"`
	Source         Location   `json:"source"`
	Target         Location   `json:"target"`
	Overrides      []Location `json:"overrides"`
}
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Kind string `json:"kind"`
}
type Diagnostic struct {
	File    string `json:"file"`
	Code    string `json:"code"`
	Message string `json:"message"`
}
type Model struct {
	Revision     string       `json:"revision"`
	Files        []string     `json:"files"`
	Resources    []Resource   `json:"resources"`
	Uses         []Use        `json:"uses"`
	Dependencies []Edge       `json:"dependencies"`
	Diagnostics  []Diagnostic `json:"diagnostics"`
}
type kustomization struct {
	file string
	root *yaml.Node
}
type parser struct {
	model      Model
	files      map[string]string
	plain      map[string][]Use
	ks         map[string]kustomization
	referenced map[string]bool
}

// Parse supports built-in pod-spec locations and local resources/images-only
// Kustomizations. Unknown transforms are diagnostics, never silently flattened.
func Parse(revision string, files map[string]string) Model {
	p := &parser{model: Model{Revision: revision, Files: []string{}, Resources: []Resource{}, Uses: []Use{}, Dependencies: []Edge{}, Diagnostics: []Diagnostic{}}, files: files, plain: map[string][]Use{}, ks: map[string]kustomization{}, referenced: map[string]bool{}}
	for file := range files {
		p.model.Files = append(p.model.Files, file)
	}
	sort.Strings(p.model.Files)
	for _, file := range p.model.Files {
		base := path.Base(file)
		if strings.EqualFold(base, "Chart.yaml") {
			p.problem(file, "unsupported_helm", "Helm rendering and reverse bindings are not supported")
			continue
		}
		if strings.Contains(files[file], "{{") {
			p.problem(file, "unsupported_template", "Templated YAML is not supported")
			continue
		}
		dec := yaml.NewDecoder(strings.NewReader(files[file]))
		for doc := 0; ; doc++ {
			var n yaml.Node
			err := dec.Decode(&n)
			if err == io.EOF {
				break
			}
			if err != nil {
				p.problem(file, "invalid_yaml", err.Error())
				break
			}
			if len(n.Content) == 0 {
				continue
			}
			root := n.Content[0]
			if err := checkNode(root); err != nil {
				p.problem(file, "unsupported_yaml", err.Error())
				continue
			}
			if base == "kustomization.yaml" || base == "kustomization.yml" || base == "Kustomization" {
				if doc != 0 {
					p.problem(file, "invalid_kustomization", "Kustomization must contain one document")
					continue
				}
				allowed := map[string]bool{"apiVersion": true, "kind": true, "resources": true, "bases": true, "images": true}
				if root.Kind != yaml.MappingNode {
					p.problem(file, "invalid_kustomization", "Expected a mapping")
					continue
				}
				for i := 0; i < len(root.Content); i += 2 {
					if !allowed[root.Content[i].Value] {
						p.problem(file, "unsupported_transform", "Unsupported Kustomize field: "+root.Content[i].Value)
					}
				}
				dir := path.Dir(file)
				if _, ok := p.ks[dir]; ok {
					p.problem(file, "ambiguous_kustomization", "Multiple Kustomizations in one directory")
				}
				p.ks[dir] = kustomization{file, root}
			} else {
				p.resource(file, doc, root, "")
			}
		}
	}
	// Resolve dependencies first so only graph roots become environment profiles.
	for dir, k := range p.ks {
		for _, field := range []string{"resources", "bases"} {
			seq := fieldNode(k.root, field)
			if seq == nil {
				continue
			}
			if seq.Kind != yaml.SequenceNode {
				p.problem(k.file, "invalid_resources", field+" must be a sequence")
				continue
			}
			for _, v := range seq.Content {
				name, err := localPath(dir, v.Value)
				if err != nil {
					p.problem(k.file, "unsafe_dependency", err.Error())
					continue
				}
				p.referenced[name] = true
				p.model.Dependencies = append(p.model.Dependencies, Edge{k.file, name, "includes"})
			}
		}
	}
	dirs := []string{}
	for dir := range p.ks {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		if !p.referenced[dir] {
			uses := p.resolve(dir, map[string]bool{})
			for _, u := range uses {
				u.Environment = dir
				p.model.Uses = append(p.model.Uses, u)
			}
		}
	}
	// Resolve every node as well: a disconnected cycle must not disappear.
	for _, dir := range dirs {
		p.resolve(dir, map[string]bool{})
	}
	for _, file := range p.model.Files {
		if !p.referenced[file] {
			for _, u := range p.plain[file] {
				u.Environment = "plain:" + path.Dir(file)
				p.model.Uses = append(p.model.Uses, u)
			}
		}
	}
	sort.Slice(p.model.Dependencies, func(i, j int) bool {
		a, b := p.model.Dependencies[i], p.model.Dependencies[j]
		return a.From+a.To < b.From+b.To
	})
	sort.Slice(p.model.Diagnostics, func(i, j int) bool {
		a, b := p.model.Diagnostics[i], p.model.Diagnostics[j]
		return a.File+a.Code+a.Message < b.File+b.Code+b.Message
	})
	return p.model
}
func (p *parser) problem(file, code, msg string) {
	d := Diagnostic{file, code, msg}
	for _, old := range p.model.Diagnostics {
		if old == d {
			return
		}
	}
	p.model.Diagnostics = append(p.model.Diagnostics, d)
}
func fieldNode(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}
func value(n *yaml.Node, key string) string {
	v := fieldNode(n, key)
	if v == nil {
		return ""
	}
	return v.Value
}
func at(n *yaml.Node, keys ...string) *yaml.Node {
	for _, k := range keys {
		n = fieldNode(n, k)
	}
	return n
}
func checkNode(n *yaml.Node) error {
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return fmt.Errorf("anchors and aliases require an explicit resolver")
	}
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode || k.Value == "<<" || seen[k.Value] {
				return fmt.Errorf("duplicate, merge or complex mapping key")
			}
			seen[k.Value] = true
		}
	}
	for _, c := range n.Content {
		if err := checkNode(c); err != nil {
			return err
		}
	}
	return nil
}
func (p *parser) resource(file string, doc int, n *yaml.Node, prefix string) {
	kind := value(n, "kind")
	api := value(n, "apiVersion")
	if kind == "List" && api == "v1" {
		items := fieldNode(n, "items")
		if items != nil {
			for i, item := range items.Content {
				p.resource(file, doc, item, fmt.Sprintf("%s/items/%d", prefix, i))
			}
		}
		return
	}
	if kind == "" {
		return
	}
	id := fmt.Sprintf("%s#%d%s", file, doc, prefix)
	p.model.Resources = append(p.model.Resources, Resource{id, file, kind, value(at(n, "metadata"), "name"), value(at(n, "metadata"), "namespace")})
	var keys []string
	switch {
	case kind == "Pod" && api == "v1":
		keys = []string{"spec"}
	case (kind == "Deployment" || kind == "StatefulSet" || kind == "DaemonSet" || kind == "ReplicaSet") && api == "apps/v1":
		keys = []string{"spec", "template", "spec"}
	case kind == "ReplicationController" && api == "v1":
		keys = []string{"spec", "template", "spec"}
	case kind == "Job" && api == "batch/v1":
		keys = []string{"spec", "template", "spec"}
	case kind == "CronJob" && api == "batch/v1":
		keys = []string{"spec", "jobTemplate", "spec", "template", "spec"}
	default:
		return
	}
	spec := at(n, keys...)
	for _, group := range []string{"containers", "initContainers", "ephemeralContainers"} {
		seq := fieldNode(spec, group)
		if seq == nil {
			continue
		}
		if seq.Kind != yaml.SequenceNode {
			p.problem(file, "invalid_workload", group+" must be a sequence")
			continue
		}
		for i, c := range seq.Content {
			image := fieldNode(c, "image")
			if image == nil {
				p.problem(file, "invalid_image", "Missing container image")
				continue
			}
			if _, err := reference.ParseNormalizedNamed(image.Value); err != nil {
				p.problem(file, "invalid_image", "Invalid image at "+id)
				continue
			}
			loc := Location{file, doc, prefix + "/" + strings.Join(keys, "/") + fmt.Sprintf("/%s/%d/image", group, i), image.Line, image.Column, image.Value, false}
			p.plain[file] = append(p.plain[file], Use{Resource: id, Container: value(c, "name"), SourceImage: image.Value, EffectiveImage: image.Value, Source: loc, Target: loc, Overrides: []Location{}})
		}
	}
}
func localPath(dir, raw string) (string, error) {
	if raw == "" || strings.ContainsAny(raw, "\\:\x00?#") || path.IsAbs(raw) {
		return "", fmt.Errorf("only local repository dependencies are supported")
	}
	v := path.Clean(path.Join(dir, raw))
	if v == ".." || strings.HasPrefix(v, "../") {
		return "", fmt.Errorf("dependency escapes repository")
	}
	return v, nil
}
func ImageName(raw string) string {
	n, err := reference.ParseNormalizedNamed(raw)
	if err != nil {
		return ""
	}
	return n.Name()
}
func ImageTag(raw string) string {
	n, err := reference.ParseNormalizedNamed(raw)
	if err != nil {
		return ""
	}
	if t, ok := n.(reference.Tagged); ok {
		return t.Tag()
	}
	return ""
}
func withTag(raw, tag string) string {
	s := strings.SplitN(raw, "@", 2)[0]
	idx := strings.LastIndex(s, ":")
	if idx > strings.LastIndex(s, "/") {
		s = s[:idx]
	}
	return s + ":" + tag
}
func (p *parser) resolve(dir string, stack map[string]bool) []Use {
	k, ok := p.ks[dir]
	if !ok {
		return nil
	}
	if stack[dir] {
		p.problem(k.file, "dependency_cycle", "Kustomize dependency cycle")
		return nil
	}
	stack[dir] = true
	defer delete(stack, dir)
	uses := []Use{}
	for _, field := range []string{"resources", "bases"} {
		seq := fieldNode(k.root, field)
		if seq == nil || seq.Kind != yaml.SequenceNode {
			continue
		}
		for _, v := range seq.Content {
			name, err := localPath(dir, v.Value)
			if err != nil {
				continue
			}
			if _, ok := p.ks[name]; ok {
				uses = append(uses, p.resolve(name, stack)...)
			} else if _, ok := p.files[name]; ok {
				uses = append(uses, p.plain[name]...)
			} else {
				p.problem(k.file, "missing_dependency", "Missing resource or Kustomization: "+name)
			}
		}
	}
	seen := map[string]bool{}
	for _, u := range uses {
		key := u.Resource + u.Container
		if seen[key] {
			p.problem(k.file, "duplicate_resource", "Resource included more than once: "+u.Resource)
		}
		seen[key] = true
	}
	images := fieldNode(k.root, "images")
	if images == nil {
		return uses
	}
	if images.Kind != yaml.SequenceNode {
		p.problem(k.file, "invalid_images", "images must be a sequence")
		return uses
	}
	names := map[string]bool{}
	for i, ov := range images.Content {
		if ov.Kind != yaml.MappingNode {
			p.problem(k.file, "unsupported_images", "Use mapping-form images overrides")
			continue
		}
		name := value(ov, "name")
		normalized := ImageName(name)
		if normalized == "" || names[normalized] {
			p.problem(k.file, "ambiguous_override", "Invalid or duplicate images.name")
			continue
		}
		names[normalized] = true
		for j := 0; j < len(ov.Content); j += 2 {
			switch ov.Content[j].Value {
			case "name", "newName", "newTag":
			default:
				p.problem(k.file, "unsupported_override", "Unsupported images field: "+ov.Content[j].Value)
			}
		}
		for j := range uses {
			u := &uses[j]
			if ImageName(u.EffectiveImage) != normalized {
				continue
			}
			if nn := value(ov, "newName"); nn != "" {
				r, e := reference.ParseNormalizedNamed(nn)
				if e != nil || !reference.IsNameOnly(r) {
					p.problem(k.file, "invalid_override", "newName must be an image name")
					continue
				}
				tag := ImageTag(u.EffectiveImage)
				u.EffectiveImage = nn
				if tag != "" {
					u.EffectiveImage = withTag(nn, tag)
				}
			}
			if tag := fieldNode(ov, "newTag"); tag != nil {
				u.EffectiveImage = withTag(u.EffectiveImage, tag.Value)
				loc := Location{k.file, 0, fmt.Sprintf("/images/%d/newTag", i), tag.Line, tag.Column, tag.Value, true}
				u.Target = loc
				u.Overrides = append(append([]Location{}, u.Overrides...), loc)
			}
			if _, e := reference.ParseNormalizedNamed(u.EffectiveImage); e != nil {
				p.problem(k.file, "invalid_override", "Override produces an invalid image")
			}
		}
	}
	return uses
}

// scalarSpan is deliberately conservative: single-line plain/quoted scalars
// only, with a round-trip decode of the token. Comments and all other bytes stay.
func scalarSpan(data string, l Location) (int, int, error) {
	lines := strings.SplitAfter(data, "\n")
	if l.Line < 1 || l.Line > len(lines) {
		return 0, 0, fmt.Errorf("invalid source line")
	}
	line := lines[l.Line-1]
	runes := []rune(line)
	if l.Column < 1 || l.Column > len(runes) {
		return 0, 0, fmt.Errorf("invalid source column")
	}
	start := 0
	for _, s := range lines[:l.Line-1] {
		start += len(s)
	}
	start += len(string(runes[:l.Column-1]))
	tail := data[start:]
	end := 0
	if tail[0] == '\'' || tail[0] == '"' {
		q := tail[0]
		end = 1
		for end < len(tail) {
			if tail[end] == '\n' || tail[end] == '\r' {
				return 0, 0, fmt.Errorf("multiline scalar unsupported")
			}
			if q == '"' && tail[end] == '\\' {
				end += 2
				continue
			}
			if tail[end] == q {
				if q == '\'' && end+1 < len(tail) && tail[end+1] == q {
					end += 2
					continue
				}
				end++
				break
			}
			end++
		}
	} else {
		for end < len(tail) && !strings.ContainsRune("\r\n,]}\t ", rune(tail[end])) {
			end++
		}
	}
	if end > len(tail) || end == 0 {
		return 0, 0, fmt.Errorf("unsupported scalar")
	}
	var n yaml.Node
	if err := yaml.NewDecoder(bytes.NewBufferString(tail[:end])).Decode(&n); err != nil || len(n.Content) != 1 || n.Content[0].Value != l.Value {
		return 0, 0, fmt.Errorf("scalar token does not match parsed value")
	}
	return start, start + end, nil
}
