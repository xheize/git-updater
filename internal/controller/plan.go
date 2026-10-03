package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/distribution/reference"
	"gopkg.in/yaml.v3"
)

var ErrInvalid = errors.New("invalid change")
var ErrConflict = errors.New("plan conflict; create and review a new intent")
var ErrNotFound = errors.New("not found")
var ErrNoMatch = errors.New("no matching workload image")

type Change struct {
	Image        string   `json:"image"`
	Tag          string   `json:"tag"`
	File         string   `json:"file,omitempty"`
	Environments []string `json:"environments,omitempty"`
}
type Intent struct {
	ID      string   `json:"id"`
	Changes []Change `json:"changes"`
}
type Mutation struct {
	Location Location `json:"location"`
	NewValue string   `json:"newValue"`
}
type Impact struct {
	Resource    string   `json:"resource"`
	Container   string   `json:"container"`
	Environment string   `json:"environment"`
	Current     string   `json:"current"`
	Proposed    string   `json:"proposed"`
	Target      Location `json:"target"`
}
type FileDiff struct {
	File string `json:"file"`
	Diff string `json:"diff"`
}
type Identity struct {
	VerificationMethod string `json:"verificationMethod,omitempty"`
	ProtectionKnown    bool   `json:"protectionKnown"`
	CheckedAt          string `json:"checkedAt,omitempty"`
	Provider           string `json:"provider"`
	RepositoryID       string `json:"repositoryId"`
	Name               string `json:"name"`
	DefaultBranch      string `json:"defaultBranch"`
	Verified           bool   `json:"verified"`
	Writable           bool   `json:"writable"`
	Protected          bool   `json:"protected"`
	Detail             string `json:"detail"`
}
type Artifact struct {
	Image  string `json:"image"`
	Tag    string `json:"tag"`
	Digest string `json:"digest"`
}
type Plan struct {
	ID           string     `json:"id"`
	Intent       Intent     `json:"intent"`
	Identity     Identity   `json:"identity"`
	BaseRevision string     `json:"baseRevision"`
	Branch       string     `json:"branch"`
	Scope        string     `json:"scope"`
	Atomic       bool       `json:"atomic"`
	Mutations    []Mutation `json:"mutations"`
	Impact       []Impact   `json:"impact"`
	Diffs        []FileDiff `json:"diffs"`
	Artifacts    []Artifact `json:"artifacts"`
	State        string     `json:"state"`
	Commit       string     `json:"commit,omitempty"`
	Error        string     `json:"error,omitempty"`
	CreatedAt    time.Time  `json:"createdAt"`
}

func ValidateIntent(in Intent) error {
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,159}$`).MatchString(in.ID) {
		return fmt.Errorf("%w: id must contain 1-160 letters, digits, dots, underscores, colons or hyphens", ErrInvalid)
	}
	if len(in.Changes) < 1 || len(in.Changes) > 100 {
		return fmt.Errorf("%w: provide 1-100 image changes", ErrInvalid)
	}
	for _, c := range in.Changes {
		n, e := reference.ParseNormalizedNamed(c.Image)
		if e != nil || !reference.IsNameOnly(n) {
			return fmt.Errorf("%w: image must be a name without tag or digest", ErrInvalid)
		}
		if _, e := reference.WithTag(n, c.Tag); e != nil {
			return fmt.Errorf("%w: invalid tag", ErrInvalid)
		}
		if len(c.Environments) > 100 {
			return fmt.Errorf("%w: too many environments", ErrInvalid)
		}
	}
	return nil
}
func SameIntent(a, b Intent) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
func selected(u Use, c Change) bool {
	return (ImageName(u.SourceImage) == ImageName(c.Image) || ImageName(u.EffectiveImage) == ImageName(c.Image)) && (c.File == "" || u.Source.File == c.File || u.Target.File == c.File) && (len(c.Environments) == 0 || contains(c.Environments, u.Environment))
}
func useKey(u Use) string {
	return u.Resource + "|" + u.Container + "|" + u.Environment + "|" + u.Source.Path
}

func BuildPlan(model Model, files map[string]string, in Intent) (Plan, error) {
	p := Plan{ID: in.ID, Intent: in, BaseRevision: model.Revision, Atomic: true, State: "planned", CreatedAt: time.Now().UTC(), Mutations: []Mutation{}, Impact: []Impact{}, Diffs: []FileDiff{}, Artifacts: []Artifact{}}
	if err := ValidateIntent(in); err != nil {
		return p, err
	}
	if len(model.Diagnostics) > 0 {
		return p, fmt.Errorf("%w: repository has %d parser diagnostics; inspect repository model", ErrInvalid, len(model.Diagnostics))
	}
	targets := map[string]Mutation{}
	expected := map[string]string{}
	for _, c := range in.Changes {
		found := false
		envFound := map[string]bool{}
		for _, u := range model.Uses {
			if !selected(u, c) {
				continue
			}
			found = true
			envFound[u.Environment] = true
			// Dropping an existing digest would change pinning semantics.
			if strings.Contains(u.SourceImage, "@") || strings.Contains(u.EffectiveImage, "@") {
				return p, fmt.Errorf("%w: digest-pinned images require a digest intent", ErrInvalid)
			}
			next := withTag(u.EffectiveImage, c.Tag)
			key := useKey(u)
			if old, ok := expected[key]; ok && old != next {
				return p, fmt.Errorf("%w: conflicting image intents", ErrInvalid)
			}
			expected[key] = next
			raw := withTag(u.Target.Value, c.Tag)
			if u.Target.TagOnly {
				raw = c.Tag
			}
			mutation := Mutation{u.Target, raw}
			tk := u.Target.Key()
			if old, ok := targets[tk]; ok && old.NewValue != raw {
				return p, fmt.Errorf("%w: conflicting scalar edits", ErrInvalid)
			}
			targets[tk] = mutation
		}
		if !found {
			return p, fmt.Errorf("%w: %s", ErrNoMatch, c.Image)
		}
		for _, env := range c.Environments {
			if !envFound[env] {
				return p, fmt.Errorf("%w: image not found in environment %s", ErrInvalid, env)
			}
		}
	}
	for _, u := range model.Uses {
		if m, ok := targets[u.Target.Key()]; ok && m.NewValue != m.Location.Value {
			if _, ok := expected[useKey(u)]; !ok {
				return p, fmt.Errorf("%w: shared source also affects unselected environment %s; add a dedicated override", ErrInvalid, u.Environment)
			}
		}
	}
	for _, m := range targets {
		if m.NewValue != m.Location.Value {
			p.Mutations = append(p.Mutations, m)
		}
	}
	sort.Slice(p.Mutations, func(i, j int) bool { return p.Mutations[i].Location.Key() < p.Mutations[j].Location.Key() })
	changed, err := Apply(files, p.Mutations)
	if err != nil {
		return p, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	after := Parse(model.Revision, changed)
	if len(after.Diagnostics) > 0 {
		return p, fmt.Errorf("%w: proposed repository failed parsing", ErrInvalid)
	}
	afterUses := map[string]Use{}
	artifacts := map[string]Artifact{}
	for _, u := range after.Uses {
		afterUses[useKey(u)] = u
	}
	for _, before := range model.Uses {
		u, ok := afterUses[useKey(before)]
		if !ok {
			return p, fmt.Errorf("%w: changed resource identity", ErrInvalid)
		}
		want, selected := expected[useKey(before)]
		if !selected {
			want = before.EffectiveImage
		}
		if u.EffectiveImage != want {
			return p, fmt.Errorf("%w: unexpected rendered impact in %s", ErrInvalid, before.Environment)
		}
		if selected {
			a := Artifact{Image: ImageName(u.EffectiveImage), Tag: ImageTag(u.EffectiveImage)}
			artifacts[a.Image+":"+a.Tag] = a
		}
		if u.EffectiveImage != before.EffectiveImage {
			p.Impact = append(p.Impact, Impact{before.Resource, before.Container, before.Environment, before.EffectiveImage, u.EffectiveImage, before.Target})
		}
	}
	for _, a := range artifacts {
		p.Artifacts = append(p.Artifacts, a)
	}
	sort.Slice(p.Artifacts, func(i, j int) bool {
		return p.Artifacts[i].Image+":"+p.Artifacts[i].Tag < p.Artifacts[j].Image+":"+p.Artifacts[j].Tag
	})
	for _, f := range model.Files {
		if files[f] != changed[f] {
			p.Diffs = append(p.Diffs, FileDiff{f, unified(f, files[f], changed[f])})
		}
	}
	return p, nil
}

func Apply(files map[string]string, mutations []Mutation) (map[string]string, error) {
	result := map[string]string{}
	for k, v := range files {
		result[k] = v
	}
	type edit struct {
		start, end int
		text       string
	}
	edits := map[string][]edit{}
	for _, m := range mutations {
		data, ok := files[m.Location.File]
		if !ok {
			return nil, fmt.Errorf("source file missing")
		}
		start, end, err := scalarSpan(data, m.Location)
		if err != nil {
			return nil, err
		}
		text := m.NewValue
		switch data[start] {
		case '\'':
			text = "'" + strings.ReplaceAll(text, "'", "''") + "'"
		case '"':
			b, _ := json.Marshal(text)
			text = string(b)
		default:
			var scalar yaml.Node
			if err := yaml.Unmarshal([]byte(text), &scalar); err != nil || len(scalar.Content) != 1 || scalar.Content[0].Kind != yaml.ScalarNode || scalar.Content[0].Tag != "!!str" || scalar.Content[0].Value != text {
				b, _ := json.Marshal(text)
				text = string(b)
			}
		}
		edits[m.Location.File] = append(edits[m.Location.File], edit{start, end, text})
	}
	for file, list := range edits {
		sort.Slice(list, func(i, j int) bool { return list[i].start > list[j].start })
		last := len(result[file])
		for _, e := range list {
			if e.end > last {
				return nil, fmt.Errorf("overlapping edits")
			}
			result[file] = result[file][:e.start] + e.text + result[file][e.end:]
			last = e.start
		}
	}
	return result, nil
}
func unified(file, before, after string) string {
	// Full-file hunks are valid unified diffs, including a missing final newline.
	lines := func(s string) []string {
		if s == "" {
			return nil
		}
		return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	}
	a, b := lines(before), lines(after)
	var out strings.Builder
	fmt.Fprintf(&out, "--- a/%s\n+++ b/%s\n@@ -1,%d +1,%d @@\n", file, file, len(a), len(b))
	for _, pair := range []struct {
		prefix string
		rows   []string
		raw    string
	}{{"-", a, before}, {"+", b, after}} {
		for _, line := range pair.rows {
			out.WriteString(pair.prefix + line + "\n")
		}
		if !strings.HasSuffix(pair.raw, "\n") {
			out.WriteString("\\ No newline at end of file\n")
		}
	}
	return out.String()
}
