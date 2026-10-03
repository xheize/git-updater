package controller

import (
	"strings"
	"testing"
)

func deployment(image string) string {
	return "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\nspec:\n  template:\n    spec:\n      containers:\n        - name: api\n          image: " + image + " # keep this\n"
}

const kyvernoPolicy = `apiVersion: kyverno.io/v1
kind: ClusterPolicy
metadata:
  name: inject-registry-creds
spec:
  background: false
  rules:
  - name: inject-mounts
    mutate:
      foreach:
      - list: "request.object.spec.containers"
        patchStrategicMerge:
          spec:
            containers:
            - name: "{{ element.name }}"
              image: "{{ request.object.metadata.labels.image }}"
              volumeMounts:
              - name: registry-creds # preserve this comment
                mountPath: /docker
`

func yamlList(items ...string) string {
	s := "apiVersion: v1\nkind: List\nitems:\n"
	for _, item := range items {
		s += "- " + strings.ReplaceAll(strings.TrimSuffix(item, "\n"), "\n", "\n  ") + "\n"
	}
	return s
}

func TestKyvernoRuntimeExpressionsPreservePolicy(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"separate":       {"policy.yaml": kyvernoPolicy, "app.yaml": deployment("nginx:v1")},
		"multi-document": {"all.yaml": kyvernoPolicy + "---\n" + deployment("nginx:v1")},
		"list":           {"all.yaml": yamlList(kyvernoPolicy, deployment("nginx:v1"))},
		"namespaced":     {"policy.yaml": strings.Replace(kyvernoPolicy, "kind: ClusterPolicy", "kind: Policy", 1), "app.yaml": deployment("nginx:v1")},
		"kustomize":      {"policy.yaml": kyvernoPolicy, "app.yaml": deployment("nginx:v1"), "kustomization.yaml": "resources: [policy.yaml, app.yaml]\n"},
		"comment":        {"app.yaml": "# {{ harmless comment }}\n" + deployment("nginx:v1")},
	} {
		t.Run(name, func(t *testing.T) {
			m := Parse("abc", files)
			if len(m.Diagnostics) != 0 || len(m.Uses) != 1 {
				t.Fatalf("unexpected model: %+v", m)
			}
			p, err := BuildPlan(m, files, Intent{"release", []Change{{Image: "nginx", Tag: "v2"}}})
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Mutations) != 1 {
				t.Fatalf("unexpected mutations: %+v", p.Mutations)
			}
			after, err := Apply(files, p.Mutations)
			if err != nil {
				t.Fatal(err)
			}
			for file, before := range files {
				if after[file] != strings.Replace(before, "nginx:v1", "nginx:v2", 1) {
					t.Fatalf("unrelated bytes changed in %s", file)
				}
			}
		})
	}
}

func TestKyvernoExceptionDoesNotBypassValidation(t *testing.T) {
	for name, extra := range map[string]map[string]string{
		"image":          {"bad.yaml": deployment("'nginx:{{ tag }}'")},
		"multi-document": {"bad.yaml": kyvernoPolicy + "---\n" + deployment("'nginx:{{ tag }}'")},
		"list":           {"bad.yaml": yamlList(kyvernoPolicy, deployment("'nginx:{{ tag }}'"))},
		"identity":       {"bad.yaml": strings.Replace(kyvernoPolicy, "name: inject-registry-creds", "name: '{{ policy }}'", 1)},
		"unknown-api":    {"bad.yaml": strings.Replace(kyvernoPolicy, "kyverno.io/v1", "other.io/v1", 1)},
		"unknown-kind":   {"bad.yaml": strings.Replace(kyvernoPolicy, "ClusterPolicy", "OtherPolicy", 1)},
		"duplicate":      {"bad.yaml": kyvernoPolicy + "spec: {}\n"},
		"alias":          {"bad.yaml": strings.Replace(kyvernoPolicy, "background: false", "background: &value false\n  copy: *value", 1)},
		"nested-policy":  {"bad.yaml": "kind: ConfigMap\napiVersion: v1\ndata:\n  embedded:\n    " + strings.ReplaceAll(kyvernoPolicy, "\n", "\n    ")},
		"dependency":     {"kustomization.yaml": "resources: ['{{ base }}']\n"},
		"override":       {"kustomization.yaml": "resources: [app.yaml]\nimages:\n- name: nginx\n  newTag: '{{ tag }}'\n"},
		"helm":           {"chart/Chart.yaml": "apiVersion: v2\nname: example\n", "chart/templates/policy.yaml": kyvernoPolicy},
	} {
		t.Run(name, func(t *testing.T) {
			files := map[string]string{"app.yaml": deployment("nginx:v1")}
			for k, v := range extra {
				files[k] = v
			}
			m := Parse("abc", files)
			if len(m.Diagnostics) == 0 {
				t.Fatal("missing diagnostics")
			}
			if _, err := BuildPlan(m, files, Intent{"release", []Change{{Image: "nginx", Tag: "v2"}}}); err == nil {
				t.Fatal("unsafe plan accepted")
			}
		})
	}
}
func TestKustomizeImpactAndMinimalMutation(t *testing.T) {
	files := map[string]string{
		"base/deployment.yaml":             deployment("'ghcr.io/foo/api:v1'"),
		"base/kustomization.yaml":          "resources: [deployment.yaml]\n",
		"overlays/dev/kustomization.yaml":  "resources: [../../base]\nimages:\n- name: ghcr.io/foo/api\n  newTag: 'v2' # dev\n",
		"overlays/prod/kustomization.yaml": "resources: [../../base]\nimages:\n- name: ghcr.io/foo/api\n  newTag: v3\n",
		"config.yaml":                      "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: example}\ndata:\n  image: ghcr.io/foo/api:v0\n",
	}
	m := Parse("abc", files)
	if len(m.Diagnostics) != 0 || len(m.Uses) != 2 {
		t.Fatalf("%+v", m)
	}
	if m.Uses[0].SourceImage != "ghcr.io/foo/api:v1" || m.Uses[0].EffectiveImage != "ghcr.io/foo/api:v2" {
		t.Fatalf("%+v", m.Uses)
	}
	p, err := BuildPlan(m, files, Intent{"release", []Change{{Image: "ghcr.io/foo/api", Tag: "v4", Environments: []string{"overlays/dev"}}}})
	if err != nil {
		t.Fatal(err)
	}
	after, err := Apply(files, p.Mutations)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Mutations) != 1 || len(p.Impact) != 1 || after["overlays/dev/kustomization.yaml"] != strings.Replace(files["overlays/dev/kustomization.yaml"], "'v2'", "'v4'", 1) {
		t.Fatalf("%+v", p)
	}
	if after["base/deployment.yaml"] != files["base/deployment.yaml"] || after["config.yaml"] != files["config.yaml"] {
		t.Fatal("unrelated bytes changed")
	}
}
func TestSharedBaseCannotLeakAcrossEnvironments(t *testing.T) {
	files := map[string]string{"base/a.yaml": deployment("nginx:v1"), "base/kustomization.yaml": "resources: [a.yaml]\n", "dev/kustomization.yaml": "resources: [../base]\n", "prod/kustomization.yaml": "resources: [../base]\n"}
	_, err := BuildPlan(Parse("abc", files), files, Intent{"release", []Change{{Image: "nginx", Tag: "v2", Environments: []string{"dev"}}}})
	if err == nil || !strings.Contains(err.Error(), "unselected") {
		t.Fatalf("%v", err)
	}
}
func TestAtomicValidationAndUnsupportedInputs(t *testing.T) {
	for name, extra := range map[string]map[string]string{
		"helm":      {"chart/Chart.yaml": "apiVersion: v2\nname: test\n"},
		"patch":     {"kustomization.yaml": "resources: [a.yaml]\npatches: []\n"},
		"cycle":     {"a/kustomization.yaml": "resources: [../b]\n", "b/kustomization.yaml": "resources: [../a]\n"},
		"missing":   {"kustomization.yaml": "resources: [missing]\n"},
		"duplicate": {"bad.yaml": "kind: Pod\nkind: Deployment\n"},
		"anchor":    {"anchor.yaml": "x: &a nginx\nimage: *a\n"},
	} {
		t.Run(name, func(t *testing.T) {
			files := map[string]string{"a.yaml": deployment("nginx:v1")}
			for k, v := range extra {
				files[k] = v
			}
			m := Parse("abc", files)
			if len(m.Diagnostics) == 0 {
				t.Fatal("missing diagnostics")
			}
			if _, err := BuildPlan(m, files, Intent{"x", []Change{{Image: "nginx", Tag: "v2"}}}); err == nil {
				t.Fatal("unsafe plan accepted")
			}
		})
	}
	files := map[string]string{"a.yaml": deployment("nginx:v1")}
	m := Parse("abc", files)
	for _, changes := range [][]Change{{{Image: "nginx", Tag: "v2"}, {Image: "absent", Tag: "v2"}}, {{Image: "nginx", Tag: "v2"}, {Image: "nginx", Tag: "v3"}}} {
		if _, err := BuildPlan(m, files, Intent{"x", changes}); err == nil {
			t.Fatal("partial/conflicting plan accepted")
		}
	}
}
func TestCRLFUnicodeQuotesAndDigest(t *testing.T) {
	raw := strings.ReplaceAll("# 한글\n"+deployment("\"nginx:v1\""), "\n", "\r\n")
	files := map[string]string{"a.yaml": raw}
	p, err := BuildPlan(Parse("abc", files), files, Intent{"x", []Change{{Image: "nginx", Tag: "v2"}}})
	if err != nil {
		t.Fatal(err)
	}
	after, err := Apply(files, p.Mutations)
	if err != nil {
		t.Fatal(err)
	}
	if after["a.yaml"] != strings.Replace(raw, "nginx:v1", "nginx:v2", 1) {
		t.Fatal("formatting lost")
	}
	files["a.yaml"] = deployment("nginx:v1@sha256:" + strings.Repeat("a", 64))
	if _, err := BuildPlan(Parse("abc", files), files, Intent{"x", []Change{{Image: "nginx", Tag: "v2"}}}); err == nil {
		t.Fatal("digest silently dropped")
	}
}

func TestRenamedImageValidatesEffectiveArtifact(t *testing.T) {
	files := map[string]string{"a.yaml": deployment("nginx:v1"), "kustomization.yaml": "resources: [a.yaml]\nimages:\n- name: nginx\n  newName: mirror.test/team/api\n  newTag: v2\n"}
	m := Parse("abc", files)
	p, err := BuildPlan(m, files, Intent{"rename", []Change{{Image: "nginx", Tag: "v3"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Artifacts) != 1 || p.Artifacts[0].Image != "mirror.test/team/api" || p.Artifacts[0].Tag != "v3" || len(m.Uses[0].Overrides) != 2 {
		t.Fatalf("%+v %+v", p, m.Uses)
	}
}

func TestNumericTagRemainsYAMLString(t *testing.T) {
	files := map[string]string{"a.yaml": deployment("nginx:v1"), "kustomization.yaml": "resources: [a.yaml]\nimages:\n- name: nginx\n  newTag: v2 # keep\n"}
	p, err := BuildPlan(Parse("abc", files), files, Intent{"numeric", []Change{{Image: "nginx", Tag: "20260927"}}})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := Apply(files, p.Mutations)
	if !strings.Contains(after["kustomization.yaml"], `newTag: "20260927" # keep`) {
		t.Fatal(after)
	}
}

func TestListCronJobAndInitContainers(t *testing.T) {
	files := map[string]string{"jobs.yaml": "apiVersion: v1\nkind: List\nitems:\n- apiVersion: batch/v1\n  kind: CronJob\n  metadata: {name: tick}\n  spec:\n    jobTemplate:\n      spec:\n        template:\n          spec:\n            containers: [{name: job, image: 'busybox:v1'}]\n            initContainers: [{name: init, image: 'alpine:v1'}]\n---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: literal}\ndata: {image: 'busybox:v1'}\n"}
	m := Parse("abc", files)
	p, err := BuildPlan(m, files, Intent{"multi", []Change{{Image: "busybox", Tag: "v2"}, {Image: "alpine", Tag: "v3"}}})
	if err != nil {
		t.Fatal(err)
	}
	after, err := Apply(files, p.Mutations)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Mutations) != 2 || !strings.Contains(after["jobs.yaml"], "data: {image: 'busybox:v1'}") {
		t.Fatal("container locations or ConfigMap exclusion failed")
	}
}
