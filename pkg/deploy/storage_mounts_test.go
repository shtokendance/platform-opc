package deploy

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"weknora-tokendance/pkg/model"
)

func buildTestTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		hdr := &tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%s): %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("Write(%s): %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("Close tar: %v", err)
	}
	return buf.Bytes()
}

func TestNormalizeAndBuildServiceUpdateOverrideJSON(t *testing.T) {
	// 无挂载时保持默认 dnsrr
	emptyOverride := BuildServiceUpdateOverrideJSON("supplier", nil)
	if emptyOverride != `{"EndpointSpec":{"Mode":"dnsrr"},"TaskTemplate":{"ContainerSpec":{"Healthcheck":{"Test":["NONE"]}}}}` {
		t.Fatalf("unexpected empty override: %s", emptyOverride)
	}

	mounts := []model.AppStorageMount{
		{Name: "data", ContainerPath: "/app/data", RelativePath: "data"},
		{Name: "uploads", ContainerPath: "/app/uploads", RelativePath: "uploads"},
		// 禁止直接挂 /app 或 /etc
		{Name: "bad-root", ContainerPath: "/app"},
		{Name: "bad-etc", ContainerPath: "/etc/passwd"},
	}
	normalized := NormalizeStorageMounts("supplier", mounts)
	if len(normalized) != 2 {
		t.Fatalf("expected 2 valid mounts, got %d", len(normalized))
	}
	if normalized[0].VolumeName != "opc-vol-supplier-data" {
		t.Fatalf("expected opc-vol-supplier-data, got %s", normalized[0].VolumeName)
	}
	if normalized[1].VolumeName != "opc-vol-supplier-uploads" {
		t.Fatalf("expected opc-vol-supplier-uploads, got %s", normalized[1].VolumeName)
	}

	overrideJSON := BuildServiceUpdateOverrideJSON("supplier", normalized)
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(overrideJSON), &parsed); err != nil {
		t.Fatalf("unmarshal overrideJSON: %v", err)
	}
	updCfg, _ := parsed["UpdateConfig"].(map[string]interface{})
	if updCfg["Order"] != "stop-first" {
		t.Fatalf("expected stop-first update order, got %v", updCfg["Order"])
	}
	taskTpl, _ := parsed["TaskTemplate"].(map[string]interface{})
	contSpec, _ := taskTpl["ContainerSpec"].(map[string]interface{})
	mountArr, _ := contSpec["Mounts"].([]interface{})
	if len(mountArr) != 2 {
		t.Fatalf("expected 2 swarm mounts, got %d", len(mountArr))
	}
}

func TestInferStorageMountsAndSeedProtection(t *testing.T) {
	df := "FROM node:20-alpine\nWORKDIR /app\nCOPY . .\nEXPOSE 8080\nCMD [\"node\", \"container-entry.mjs\"]\n"
	serverMjs := `
import { join } from "node:path";
const root = process.cwd();
const dataDir = join(root, "data");
const uploadDir = join(root, "uploads");
const reportDir = join(root, "reports");
`
	entryMjs := `import "./server.mjs";`
	tarBytes := buildTestTar(t, map[string]string{
		"Dockerfile":          df,
		"server.mjs":          serverMjs,
		"container-entry.mjs": entryMjs,
		"data/state.json":     `{"suppliers":[]}`,
		"src/data/seed.js":    `export const seed = [];`,
	})

	inferred := InferStorageMountsFromTar("supplier", df, tarBytes)
	if len(inferred) != 3 {
		t.Fatalf("expected 3 inferred mounts (data, reports, uploads), got %d: %+v", len(inferred), inferred)
	}

	protectedTar := EnsureTarSeedProtection("supplier", tarBytes, inferred)
	tr := tar.NewReader(bytes.NewReader(protectedTar))
	foundDockerfileSeed := false
	foundEntrySeed := false
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(tr)
		text := buf.String()
		if hdr.Name == "Dockerfile" && strings.Contains(text, "/app/.opc-seed") {
			foundDockerfileSeed = true
		}
		if hdr.Name == "container-entry.mjs" && strings.Contains(text, "_opcCopySeedDir") {
			foundEntrySeed = true
		}
	}
	if !foundDockerfileSeed {
		t.Fatalf("expected Dockerfile in protected tar to include /app/.opc-seed backup")
	}
	if !foundEntrySeed {
		t.Fatalf("expected container-entry.mjs in protected tar to include _opcCopySeedDir hydration")
	}
}
