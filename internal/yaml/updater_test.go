package yaml

import (
	"bytes"
	"testing"

	yaml3 "gopkg.in/yaml.v3"
)

func TestProcessYAMLUpdates(t *testing.T) {
	yamlData := []byte(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-deploy
spec:
  replicas: 3
  template:
    spec:
      containers:
        - name: web
          image: nginx:1.14.2
        - name: helper
          image: busybox:latest
`)

	updates := map[string]string{
		"spec.template.spec.containers[0].image": "nginx:1.21.0",
		"spec.replicas":                          "5",
	}

	updated, err := ProcessYAMLUpdates(yamlData, updates)
	if err != nil {
		t.Fatalf("failed to update YAML: %v", err)
	}

	// Simple check if strings exist in the updated output
	if !bytes.Contains(updated, []byte("image: nginx:1.21.0")) {
		t.Errorf("expected updated image not found in output")
	}
	if !bytes.Contains(updated, []byte("replicas: 5")) {
		t.Errorf("expected updated replicas not found in output")
	}
}

func TestProcessYAMLImageUpdate(t *testing.T) {
	yamlData := []byte(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: test-deploy
spec:
  template:
    spec:
      containers:
        - name: web1
          image: nginx:1.14.2
        - name: web2
          image: nginx
        - name: helper
          image: registry.xheize.cc:5000/app:1.20
`)

	// Test 1: Update simple image (nginx)
	updated, ok, err := ProcessYAMLImageUpdate(yamlData, "nginx", "1.21.0")
	if err != nil {
		t.Fatalf("failed to update YAML: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok to be true, got false")
	}
	if !bytes.Contains(updated, []byte("image: nginx:1.21.0")) {
		t.Errorf("expected updated image not found in output")
	}

	// Test 2: Update image with port (registry.xheize.cc:5000/app)
	updated2, ok2, err2 := ProcessYAMLImageUpdate(yamlData, "registry.xheize.cc:5000/app", "1.25")
	if err2 != nil {
		t.Fatalf("failed to update YAML with port: %v", err2)
	}
	if !ok2 {
		t.Fatalf("expected ok2 to be true, got false")
	}
	if !bytes.Contains(updated2, []byte("image: registry.xheize.cc:5000/app:1.25")) {
		t.Errorf("expected updated image with port not found in output")
	}
}

func TestIsArgoCDApplication(t *testing.T) {
	argoYAML := []byte(`
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: guestbook
spec:
  project: default
`)

	deployYAML := []byte(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: guestbook
`)

	// Test 1: ArgoCD App
	dec1 := yaml3.NewDecoder(bytes.NewReader(argoYAML))
	var doc1 yaml3.Node
	if err := dec1.Decode(&doc1); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if !IsArgoCDApplication(&doc1) {
		t.Errorf("expected IsArgoCDApplication to be true, got false")
	}

	// Test 2: Deployment
	dec2 := yaml3.NewDecoder(bytes.NewReader(deployYAML))
	var doc2 yaml3.Node
	if err := dec2.Decode(&doc2); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if IsArgoCDApplication(&doc2) {
		t.Errorf("expected IsArgoCDApplication to be false, got true")
	}
}
