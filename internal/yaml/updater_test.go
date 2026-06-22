package yaml

import (
	"bytes"
	"testing"
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
