package deploy

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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

func TestUniversalSeedProtectionPython(t *testing.T) {
	df := "FROM python:3.11-slim\nWORKDIR /app\nCOPY . .\nEXPOSE 5000\nCMD [\"python\", \"app.py\"]\n"
	appPy := `import os; print("hello")`
	tarBytes := buildTestTar(t, map[string]string{
		"Dockerfile":  df,
		"app.py":      appPy,
		"data/app.db": "sqlite database dummy",
	})

	mounts := []model.AppStorageMount{
		{Name: "data", ContainerPath: "/app/data", RelativePath: "data"},
	}

	protectedTar := EnsureTarSeedProtection("python-app", tarBytes, mounts)
	tr := tar.NewReader(bytes.NewReader(protectedTar))
	foundDockerfileSeed := false
	foundEntrypointSh := false
	foundEntrypointInDF := false
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(tr)
		text := buf.String()
		if hdr.Name == ".opc-seed-entrypoint.sh" {
			foundEntrypointSh = true
			if !strings.Contains(text, "_opc_copy_seed_dir") || !strings.Contains(text, `_opc_copy_seed_dir "/app/.opc-seed/data" "/app/data"`) {
				t.Fatalf("unexpected entrypoint.sh content: %s", text)
			}
		}
		if hdr.Name == "Dockerfile" {
			if strings.Contains(text, "/app/.opc-seed") {
				foundDockerfileSeed = true
			}
			if strings.Contains(text, `ENTRYPOINT ["/app/.opc-seed-entrypoint.sh"]`) {
				foundEntrypointInDF = true
			}
		}
	}
	if !foundDockerfileSeed {
		t.Fatalf("expected Dockerfile in protected tar to include /app/.opc-seed backup")
	}
	if !foundEntrypointSh {
		t.Fatalf("expected .opc-seed-entrypoint.sh in protected tar")
	}
	if !foundEntrypointInDF {
		t.Fatalf("expected Dockerfile to contain ENTRYPOINT for .opc-seed-entrypoint.sh")
	}
}

func TestSeedSafetyAndStableMountIdentity(t *testing.T) {
	if ValidateSeedProtection("FROM scratch\n", []model.AppStorageMount{{ContainerPath: "/app/data"}}) == nil {
		t.Fatal("scratch must not receive shell entrypoint")
	}
	if ValidateSeedProtection("FROM golang:1.22 AS builder\nRUN go build -o /out/app\nFROM gcr.io/distroless/base-debian12\nCOPY --from=builder /out/app /app/app\n", []model.AppStorageMount{{ContainerPath: "/app/data"}}) == nil {
		t.Fatal("multistage distroless final image must be rejected")
	}
	multistageAlpine := "FROM golang:1.22 AS builder\nENTRYPOINT [\"/bin/true\"]\nFROM alpine:3.19\nWORKDIR /app\nCOPY --from=builder /out/app /app/server\nUSER appuser\nENTRYPOINT [\"/app/server\"]\nCMD [\"--port=8080\"]\n"
	if err := ValidateSeedProtection(multistageAlpine, []model.AppStorageMount{{ContainerPath: "/app/data"}}); err != nil {
		t.Fatalf("multistage with shell-compatible final stage should be supported: %v", err)
	}
	old := NormalizeStorageMounts("demo", []model.AppStorageMount{{Name: "data", ContainerPath: "/app/data"}})
	got, err := ReconcileStorageMounts("demo", old, []model.AppStorageMount{{Name: "database", ContainerPath: "/app/data", VolumeName: "opc-vol-other-hijack", HostPath: "/etc"}})
	if err != nil || got[0].VolumeName != old[0].VolumeName || got[0].HostPath != "" {
		t.Fatalf("lost volume or trusted spoofed volume/hostPath: %v %v", got, err)
	}
	if _, err := ReconcileStorageMounts("demo", old, nil); err == nil {
		t.Fatal("must not silently remove data mount")
	}
	data := EnsureTarSeedProtection("demo", buildTestTar(t, map[string]string{
		"Dockerfile":    multistageAlpine,
		".dockerignore": "*\n!main.py\n",
	}), old)
	tr := tar.NewReader(bytes.NewReader(data))
	checkedDF := false
	checkedDI := false
	for {
		h, e := tr.Next()
		if e != nil {
			break
		}
		b, _ := io.ReadAll(tr)
		if h.Name == "Dockerfile" {
			checkedDF = true
			dfStr := string(b)
			if !strings.Contains(dfStr, "COPY --chmod=755 .opc-seed-entrypoint.sh /app/.opc-seed-entrypoint.sh") {
				t.Fatalf("missing explicit script copy in Dockerfile:\n%s", dfStr)
			}
			if !strings.Contains(dfStr, `ENTRYPOINT ["/bin/true"]`) {
				t.Fatalf("builder stage ENTRYPOINT should not be modified:\n%s", dfStr)
			}
			if !strings.Contains(dfStr, `ENTRYPOINT ["/app/.opc-seed-entrypoint.sh", "/app/server"]`) {
				t.Fatalf("final stage ENTRYPOINT should be wrapped:\n%s", dfStr)
			}
			if strings.Index(dfStr, "/app/.opc-seed") > strings.Index(dfStr, "USER appuser") {
				t.Fatalf("seed backup should run before USER directive:\n%s", dfStr)
			}
		}
		if h.Name == ".dockerignore" {
			checkedDI = true
			if !strings.Contains(string(b), "!.opc-seed-entrypoint.sh") {
				t.Fatalf("expected .dockerignore to allow .opc-seed-entrypoint.sh:\n%s", string(b))
			}
		}
	}
	if !checkedDF || !checkedDI {
		t.Fatalf("expected both Dockerfile and .dockerignore in rewritten tar")
	}
}

func TestUpgradeSeedProtectionPositionExpansionAndSQLiteSync(t *testing.T) {
	// 1. EXPOSE before COPY must not pull RUN cp -a ahead of COPY, and psycopg must not hide SQLite
	dfEarlyExpose := "FROM python:3.11-slim\nWORKDIR /app\nEXPOSE 8080\nCOPY . .\nCMD [\"python\", \"app.py\"]\n"
	tarWithPsycopg := buildTestTar(t, map[string]string{
		"Dockerfile":  dfEarlyExpose + "# psycopg",
		"data/app.db": "sqlite database dummy",
	})
	inferred := InferStorageMountsFromTar("sqlite-app", dfEarlyExpose+"# psycopg", tarWithPsycopg)
	if len(inferred) != 1 || inferred[0].ContainerPath != "/app/data" {
		t.Fatalf("expected /app/data inferred even with psycopg present, got %+v", inferred)
	}
	protected := EnsureTarSeedProtection("sqlite-app", tarWithPsycopg, inferred)
	tr := tar.NewReader(bytes.NewReader(protected))
	var dfAfter string
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(tr)
		if h.Name == "Dockerfile" {
			dfAfter = string(b)
		}
	}
	if strings.Index(dfAfter, "COPY . .") > strings.Index(dfAfter, "/app/.opc-seed") {
		t.Fatalf("seed backup RUN must appear after COPY . .:\n%s", dfAfter)
	}

	// 2. Adding a second mount (/app/uploads) when Dockerfile already has .opc-seed from v1 must regenerate seed block
	v1EntrySh := "#!/bin/sh\necho old\n"
	dfV1WithSeed := "FROM python:3.11-slim\nWORKDIR /app\nCOPY . .\nRUN mkdir -p /app/.opc-seed && if [ -d \"/app/data\" ]; then mkdir -p \"/app/.opc-seed/data\"; fi\nCOPY --chmod=755 .opc-seed-entrypoint.sh /app/.opc-seed-entrypoint.sh\nENTRYPOINT [\"/app/.opc-seed-entrypoint.sh\"]\nCMD [\"python\", \"app.py\"]\n"
	v2Tar := buildTestTar(t, map[string]string{
		"Dockerfile":              dfV1WithSeed,
		".opc-seed-entrypoint.sh": v1EntrySh,
		"data/app.db":             "dummy",
		"uploads/sample.txt":      "hello",
	})
	v2Mounts := NormalizeStorageMounts("sqlite-app", []model.AppStorageMount{
		{Name: "data", ContainerPath: "/app/data"},
		{Name: "uploads", ContainerPath: "/app/uploads"},
	})
	v2Protected := EnsureTarSeedProtection("sqlite-app", v2Tar, v2Mounts)
	tr2 := tar.NewReader(bytes.NewReader(v2Protected))
	var v2DF, v2Entry string
	for {
		h, err := tr2.Next()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(tr2)
		if h.Name == "Dockerfile" {
			v2DF = string(b)
		} else if h.Name == ".opc-seed-entrypoint.sh" {
			v2Entry = string(b)
		}
	}
	if !strings.Contains(v2DF, "/app/uploads") || !strings.Contains(v2Entry, "/app/uploads") {
		t.Fatalf("expected regenerated seed protection to include /app/uploads:\ndf=%s\nentry=%s", v2DF, v2Entry)
	}

	// 3. Test additiveSQLiteSyncPython with real python3 + sqlite3 (non-destructive additive schema sync)
	if _, err := exec.LookPath("python3"); err == nil {
		tmp := t.TempDir()
		seedDir := filepath.Join(tmp, "seed")
		dstDir := filepath.Join(tmp, "dst")
		_ = os.MkdirAll(seedDir, 0o755)
		_ = os.MkdirAll(dstDir, 0o755)
		seedDB := filepath.Join(seedDir, "app.db")
		dstDB := filepath.Join(dstDir, "app.db")

		setupPy := `
import sqlite3, sys
seed_p, dst_p = sys.argv[1], sys.argv[2]
dc = sqlite3.connect(dst_p)
dc.execute("CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT)")
dc.execute("INSERT INTO items (id, name) VALUES (1, 'prod-row')")
dc.commit(); dc.close()

sc = sqlite3.connect(seed_p)
sc.execute("CREATE TABLE items (id INTEGER PRIMARY KEY, name TEXT, category TEXT NOT NULL DEFAULT 'general', qty INTEGER NOT NULL)")
sc.execute("INSERT INTO items (id, name, category, qty) VALUES (1, 'seed-overwrite-must-not-happen', 'x', 9)")
sc.execute("CREATE TABLE audit_logs (id INTEGER PRIMARY KEY, note TEXT)")
sc.execute("INSERT INTO audit_logs (id, note) VALUES (1, 'init')")
sc.commit(); sc.close()
`
		if out, err := exec.Command("python3", "-c", setupPy, seedDB, dstDB).CombinedOutput(); err != nil {
			t.Fatalf("setup sqlite test dbs: %v (%s)", err, out)
		}
		if out, err := exec.Command("python3", "-c", additiveSQLiteSyncPython, seedDir, dstDir).CombinedOutput(); err != nil {
			t.Fatalf("run additiveSQLiteSyncPython: %v (%s)", err, out)
		}
		verifyPy := `
import sqlite3, sys
dc = sqlite3.connect(sys.argv[1])
rows = dc.execute("SELECT id, name, category, qty FROM items").fetchall()
assert rows == [(1, 'prod-row', 'general', 0)], f"unexpected items rows: {rows}"
logs = dc.execute("SELECT id, note FROM audit_logs").fetchall()
assert logs == [(1, 'init')], f"unexpected audit_logs rows: {logs}"
dc.close()
`
		if out, err := exec.Command("python3", "-c", verifyPy, dstDB).CombinedOutput(); err != nil {
			t.Fatalf("verify additive schema sync: %v (%s)", err, out)
		}
	}
}
