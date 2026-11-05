package deploy

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"weknora-tokendance/pkg/model"
)

var (
	mountNameSanitizer = regexp.MustCompile(`[^a-z0-9-]+`)
	workdirDirectiveRe = regexp.MustCompile(`(?im)^\s*WORKDIR\s+([^\s#]+)`)
	nodeJoinDirRe      = regexp.MustCompile(`join\(\s*(?:root|__dirname|process\.cwd\(\))\s*,\s*["'](data|uploads?|reports?|backups?|storage|files|attachments|media)["']\s*\)`)
)

var forbiddenMountTargets = map[string]bool{
	"/":        true,
	"/bin":     true,
	"/boot":    true,
	"/dev":     true,
	"/etc":     true,
	"/lib":     true,
	"/lib64":   true,
	"/proc":    true,
	"/root":    true,
	"/run":     true,
	"/sbin":    true,
	"/sys":     true,
	"/tmp":     true,
	"/usr":     true,
	"/var":     true,
	"/var/run": true,
}

var knownPersistentDirPurposes = map[string]struct {
	Kind    string
	Purpose string
}{
	"data":        {Kind: "state-data", Purpose: "持久化业务状态数据文件 (JSON / SQLite / 业务配置)"},
	"uploads":     {Kind: "uploads", Purpose: "持久化用户上传附件、图片与导入文件"},
	"upload":      {Kind: "uploads", Purpose: "持久化用户上传附件与导入文件"},
	"files":       {Kind: "uploads", Purpose: "持久化业务上传单据与附件文件"},
	"reports":     {Kind: "exports", Purpose: "持久化生成的业务报告与导出文档"},
	"backups":     {Kind: "backups", Purpose: "持久化业务数据本地自动备份文件"},
	"storage":     {Kind: "state-data", Purpose: "持久化应用本地存储目录"},
	"attachments": {Kind: "uploads", Purpose: "持久化业务附件文件"},
	"media":       {Kind: "uploads", Purpose: "持久化媒体与上传资源文件"},
}

func sanitizeMountToken(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, "/", "-")
	s = mountNameSanitizer.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 32 {
		s = strings.Trim(s[:32], "-")
	}
	return s
}

// ExtractWorkdirFromDockerfile 从 Dockerfile 内容解析最后一个 WORKDIR（默认 /app）
func ExtractWorkdirFromDockerfile(dockerfileContent string) string {
	matches := workdirDirectiveRe.FindAllStringSubmatch(dockerfileContent, -1)
	if len(matches) == 0 {
		return "/app"
	}
	last := strings.Trim(strings.TrimSpace(matches[len(matches)-1][1]), `"'`)
	if !strings.HasPrefix(last, "/") {
		last = path.Clean("/app/" + last)
	} else {
		last = path.Clean(last)
	}
	if last == "/" || last == "." {
		return "/app"
	}
	return last
}

// NormalizeStorageMounts 规范化并校验应用的持久化卷挂载列表，生成固定格式的 Docker Volume 名称或宿主机/NAS 路径
func NormalizeStorageMounts(slug string, mounts []model.AppStorageMount) []model.AppStorageMount {
	cleanSlug := sanitizeMountToken(slug)
	if cleanSlug == "" {
		cleanSlug = "app"
	}
	hostRoot := strings.TrimSpace(os.Getenv("OPC_VOLUME_HOST_ROOT"))
	s3Driver := strings.ToLower(strings.TrimSpace(os.Getenv("OPC_STORAGE_DRIVER")))

	seenTargets := make(map[string]bool)
	seenNames := make(map[string]bool)
	out := make([]model.AppStorageMount, 0, len(mounts))

	for _, m := range mounts {
		target := strings.TrimSpace(m.ContainerPath)
		rel := strings.Trim(strings.TrimSpace(m.RelativePath), "/")
		if target == "" && rel != "" {
			target = path.Clean("/app/" + rel)
		}
		if target == "" || !strings.HasPrefix(target, "/") {
			continue
		}
		target = path.Clean(target)
		if forbiddenMountTargets[target] || strings.HasPrefix(target, "/proc/") || strings.HasPrefix(target, "/sys/") || strings.HasPrefix(target, "/dev/") || strings.HasPrefix(target, "/etc/") {
			continue
		}
		// 不直接挂载整个 /app 代码根目录（会导致新版本代码更新被旧卷遮蔽），只允许挂载子目录如 /app/data, /app/uploads
		if target == "/app" || target == "/src" || target == "/workspace" {
			continue
		}
		if seenTargets[target] {
			continue
		}
		seenTargets[target] = true

		if rel == "" && strings.HasPrefix(target, "/app/") {
			rel = strings.TrimPrefix(target, "/app/")
		}

		name := sanitizeMountToken(m.Name)
		if name == "" {
			if rel != "" {
				name = sanitizeMountToken(rel)
			} else {
				name = sanitizeMountToken(path.Base(target))
			}
		}
		if name == "" {
			name = "data"
		}
		baseName := name
		for idx := 2; seenNames[name]; idx++ {
			name = fmt.Sprintf("%s-%d", baseName, idx)
		}
		seenNames[name] = true

		volName := fmt.Sprintf("opc-vol-%s-%s", cleanSlug, name)
		hostPath := strings.TrimSpace(m.HostPath)
		backend := strings.TrimSpace(m.StorageBackend)
		if hostPath == "" && hostRoot != "" {
			hostPath = filepath.Join(hostRoot, cleanSlug, name)
			_ = os.MkdirAll(hostPath, 0o777)
		}
		if hostPath != "" {
			backend = "host-bind"
		} else if s3Driver == "s3" || s3Driver == "oss" {
			backend = "docker-volume+s3-sync"
		} else if backend == "" {
			backend = "docker-volume"
		}

		kind := strings.TrimSpace(m.Kind)
		purpose := strings.TrimSpace(m.Purpose)
		if meta, ok := knownPersistentDirPurposes[path.Base(target)]; ok {
			if kind == "" {
				kind = meta.Kind
			}
			if purpose == "" {
				purpose = meta.Purpose
			}
		}
		if kind == "" {
			kind = "state-data"
		}
		if purpose == "" {
			purpose = fmt.Sprintf("持久化容器目录 %s", target)
		}

		out = append(out, model.AppStorageMount{
			Name:           name,
			ContainerPath:  target,
			RelativePath:   rel,
			Purpose:        purpose,
			Kind:           kind,
			VolumeName:     volName,
			HostPath:       hostPath,
			StorageBackend: backend,
			SeedFilesCount: m.SeedFilesCount,
		})
	}
	return out
}

// BuildServiceUpdateOverrideJSON 构造 CapRover serviceUpdateOverride JSON。
// 通过 Swarm TaskTemplate.ContainerSpec.Mounts 下发持久化卷并开启 stop-first 更新策略，
// 兼容 CapRover 中 hasPersistentData=false 的存量应用与新建应用。
func BuildServiceUpdateOverrideJSON(slug string, mounts []model.AppStorageMount) string {
	normalized := NormalizeStorageMounts(slug, mounts)
	if len(normalized) == 0 {
		return `{"EndpointSpec":{"Mode":"dnsrr"}}`
	}

	swarmMounts := make([]map[string]interface{}, 0, len(normalized))
	for _, m := range normalized {
		if m.HostPath != "" {
			swarmMounts = append(swarmMounts, map[string]interface{}{
				"Source":   m.HostPath,
				"Target":   m.ContainerPath,
				"Type":     "bind",
				"ReadOnly": false,
			})
		} else {
			swarmMounts = append(swarmMounts, map[string]interface{}{
				"Source":   m.VolumeName,
				"Target":   m.ContainerPath,
				"Type":     "volume",
				"ReadOnly": false,
			})
		}
	}

	override := map[string]interface{}{
		"EndpointSpec": map[string]interface{}{
			"Mode": "dnsrr",
		},
		"UpdateConfig": map[string]interface{}{
			"Order": "stop-first",
		},
		"TaskTemplate": map[string]interface{}{
			"ContainerSpec": map[string]interface{}{
				"Mounts": swarmMounts,
			},
		},
	}
	raw, err := json.Marshal(override)
	if err != nil {
		return `{"EndpointSpec":{"Mode":"dnsrr"}}`
	}
	return string(raw)
}

// InferStorageMountsFromTar 从上传的 app.tar 中自动探测本地数据目录、JSON 状态目录与上传目录
func InferStorageMountsFromTar(slug string, dockerfileContent string, tarBytes []byte) []model.AppStorageMount {
	if len(tarBytes) == 0 {
		return nil
	}
	workdir := ExtractWorkdirFromDockerfile(dockerfileContent)

	tr := tar.NewReader(bytes.NewReader(tarBytes))
	dirSeedCounts := make(map[string]int)
	dirHasCodeFiles := make(map[string]bool)
	codeReferencedDirs := make(map[string]bool)
	hasMigrations := false
	if strings.Contains(dockerfileContent, "psycopg") {
		hasMigrations = true
	}

	isCodeExt := func(ext string) bool {
		switch ext {
		case ".py", ".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx", ".go", ".java", ".cs", ".rb", ".php", ".vue", ".svelte":
			return true
		default:
			return false
		}
	}

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		cleanName := strings.TrimPrefix(strings.TrimPrefix(path.Clean(hdr.Name), "./"), "/")
		if cleanName == "" || cleanName == "." {
			continue
		}
		parts := strings.Split(cleanName, "/")
		top := parts[0]
		// 跳过前端源码组件目录（如 src/data/seedData.js 是代码模块而非运行时可变数据目录）
		if top == "src" || top == "node_modules" || top == ".git" || top == "test" || top == "tests" || top == "docs" || top == "miniprogram" || top == "web" || top == "public" || top == "static" {
			continue
		}

		// 检查顶层已知持久化目录
		if _, ok := knownPersistentDirPurposes[top]; ok {
			if hdr.Typeflag == tar.TypeDir {
				if _, exists := dirSeedCounts[top]; !exists {
					dirSeedCounts[top] = 0
				}
			} else if hdr.Typeflag == tar.TypeReg {
				ext := strings.ToLower(path.Ext(cleanName))
				if isCodeExt(ext) {
					dirHasCodeFiles[top] = true
					continue
				}
				// 若已迁移至 PostgreSQL 且目录下仅有 sqlite 文件，不强制作为本地文件卷
				if hasMigrations && (ext == ".sqlite" || ext == ".sqlite3" || ext == ".db") {
					continue
				}
				dirSeedCounts[top]++
			}
		}

		// 检查二级后端目录下的 data / uploads（例如 reimburse-web/server/data）
		if len(parts) >= 2 {
			lastDir := parts[len(parts)-1]
			if hdr.Typeflag == tar.TypeReg {
				lastDir = parts[len(parts)-2]
			}
			if _, ok := knownPersistentDirPurposes[lastDir]; ok {
				var relDir string
				if hdr.Typeflag == tar.TypeDir {
					relDir = cleanName
				} else {
					relDir = strings.Join(parts[:len(parts)-1], "/")
				}
				// 排除含 src/ 或静态资源目录
				if !strings.Contains("/"+relDir+"/", "/src/") && !strings.Contains("/"+relDir+"/", "/assets/") && len(strings.Split(relDir, "/")) <= 3 {
					if hdr.Typeflag == tar.TypeReg {
						ext := strings.ToLower(path.Ext(cleanName))
						if isCodeExt(ext) {
							dirHasCodeFiles[relDir] = true
						} else if !(hasMigrations && (ext == ".sqlite" || ext == ".sqlite3" || ext == ".db")) {
							dirSeedCounts[relDir]++
						}
					} else if _, exists := dirSeedCounts[relDir]; !exists {
						dirSeedCounts[relDir] = 0
					}
				}
			}
		}

		// 扫描入口源码（如 server.mjs / app.py / main.py）中通过代码动态创建的目录（如 uploads, reports, backups）
		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 && hdr.Size <= 2*1024*1024 {
			ext := strings.ToLower(path.Ext(cleanName))
			if ext == ".mjs" || ext == ".js" || ext == ".ts" || ext == ".py" || ext == ".go" {
				raw, readErr := io.ReadAll(io.LimitReader(tr, 2*1024*1024))
				if readErr == nil {
					content := string(raw)
					for _, m := range nodeJoinDirRe.FindAllStringSubmatch(content, -1) {
						if len(m) >= 2 {
							codeReferencedDirs[m[1]] = true
						}
					}
				}
			}
		}
	}

	for d := range codeReferencedDirs {
		if _, exists := dirSeedCounts[d]; !exists {
			dirSeedCounts[d] = 0
		}
	}

	// 如果存在子目录（如 server/data）且 Dockerfile WORKDIR 指向该子目录的上级（如 /app/reimburse-web/server），对齐容器内路径
	relDirs := make([]string, 0, len(dirSeedCounts))
	for relDir := range dirSeedCounts {
		if dirHasCodeFiles[relDir] {
			continue
		}
		relDirs = append(relDirs, relDir)
	}
	sort.Strings(relDirs)

	// 若同时识别到父目录和子目录（如 data 和 data/files），只挂载外层父目录 data，避免嵌套挂载冲突
	filteredDirs := make([]string, 0, len(relDirs))
	for _, d := range relDirs {
		nested := false
		for _, parent := range relDirs {
			if parent != d && strings.HasPrefix(d, parent+"/") {
				nested = true
				break
			}
		}
		if !nested {
			filteredDirs = append(filteredDirs, d)
		}
	}

	mounts := make([]model.AppStorageMount, 0, len(filteredDirs))
	for _, relDir := range filteredDirs {
		containerPath := path.Clean(workdir + "/" + relDir)
		// 若 Dockerfile 中 COPY 了子目录到 WORKDIR（例如 COPY reimburse-web/server/ /app/），自动映射为 /app/data
		baseDir := path.Base(relDir)
		if strings.Contains(relDir, "/") && strings.Contains(dockerfileContent, path.Dir(relDir)) {
			containerPath = path.Clean(workdir + "/" + baseDir)
		}
		meta := knownPersistentDirPurposes[baseDir]
		mounts = append(mounts, model.AppStorageMount{
			Name:           baseDir,
			ContainerPath:  containerPath,
			RelativePath:   relDir,
			Purpose:        meta.Purpose,
			Kind:           meta.Kind,
			SeedFilesCount: dirSeedCounts[relDir],
		})
	}

	return NormalizeStorageMounts(slug, mounts)
}

// EnsureTarSeedProtection 在向 CapRover 上传构建包前，确保包含持久化卷的应用具备：
// 1. 构建期将初始种子目录备份至 /app/.opc-seed/<name>；
// 2. 容器启动期自动执行无损种子补齐（仅当挂载卷内缺失对应文件时才从 .opc-seed 复制，绝不覆盖线上已有数据）。
func EnsureTarSeedProtection(slug string, tarBytes []byte, mounts []model.AppStorageMount) []byte {
	normalized := NormalizeStorageMounts(slug, mounts)
	if len(tarBytes) == 0 || len(normalized) == 0 {
		return tarBytes
	}

	type tarItem struct {
		header *tar.Header
		data   []byte
	}
	var items []tarItem
	tr := tar.NewReader(bytes.NewReader(tarBytes))
	dockerfileIdx := -1
	containerEntryIdx := -1

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return tarBytes
		}
		var body []byte
		if hdr.Size > 0 {
			body, err = io.ReadAll(tr)
			if err != nil {
				return tarBytes
			}
		}
		hdrCopy := *hdr
		cleanName := strings.TrimPrefix(strings.TrimPrefix(path.Clean(hdr.Name), "./"), "/")
		if cleanName == "Dockerfile" {
			dockerfileIdx = len(items)
		} else if cleanName == "container-entry.mjs" {
			containerEntryIdx = len(items)
		}
		items = append(items, tarItem{header: &hdrCopy, data: body})
	}

	if dockerfileIdx < 0 {
		return tarBytes
	}
	dfContent := string(items[dockerfileIdx].data)
	// 若已包含 .opc-seed 种子保护逻辑，则直接原样返回
	if strings.Contains(dfContent, ".opc-seed") {
		return tarBytes
	}

	// 1. 生成构建期备份命令：将挂载路径下的初始种子文件备份到 /app/.opc-seed/<name>
	var seedBackupCmds []string
	var hydratePairs []string
	for _, m := range normalized {
		cPath := m.ContainerPath
		name := m.Name
		seedBackupCmds = append(seedBackupCmds, fmt.Sprintf(`if [ -d %q ]; then mkdir -p "/app/.opc-seed/%s" && cp -a %q/. "/app/.opc-seed/%s/" 2>/dev/null || true; fi`, cPath, name, cPath, name))
		hydratePairs = append(hydratePairs, fmt.Sprintf("%s:%s", name, cPath))
	}
	seedRunLine := fmt.Sprintf("RUN mkdir -p /app/.opc-seed && %s\n", strings.Join(seedBackupCmds, " && "))

	// 插入到最后一个 COPY 指令之后（或 EXPOSE / CMD 之前）
	lines := strings.Split(dfContent, "\n")
	insertPos := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(strings.ToUpper(lines[i]))
		if strings.HasPrefix(trimmed, "CMD ") || strings.HasPrefix(trimmed, "CMD[") || strings.HasPrefix(trimmed, "ENTRYPOINT ") || strings.HasPrefix(trimmed, "EXPOSE ") {
			insertPos = i
		}
	}
	updatedLines := make([]string, 0, len(lines)+2)
	updatedLines = append(updatedLines, lines[:insertPos]...)
	updatedLines = append(updatedLines, strings.TrimRight(seedRunLine, "\n"))
	updatedLines = append(updatedLines, lines[insertPos:]...)
	items[dockerfileIdx].data = []byte(strings.Join(updatedLines, "\n"))
	items[dockerfileIdx].header.Size = int64(len(items[dockerfileIdx].data))

	// 2. 如果存在 container-entry.mjs，在启动前加入无损种子恢复逻辑
	if containerEntryIdx >= 0 {
		entryStr := string(items[containerEntryIdx].data)
		if !strings.Contains(entryStr, ".opc-seed") {
			pairsJSON, _ := json.Marshal(normalized)
			seedBootstrapJS := fmt.Sprintf(`
// --- OPC Persistent Volume Seed Hydration (Non-destructive) ---
import { existsSync as _opcExists, mkdirSync as _opcMkdir, readdirSync as _opcReaddir, statSync as _opcStat, copyFileSync as _opcCopyFile } from "node:fs";
import { join as _opcJoin } from "node:path";
function _opcCopySeedDir(srcDir, dstDir) {
  if (!_opcExists(srcDir)) return;
  _opcMkdir(dstDir, { recursive: true });
  for (const entry of _opcReaddir(srcDir)) {
    const s = _opcJoin(srcDir, entry);
    const d = _opcJoin(dstDir, entry);
    try {
      const st = _opcStat(s);
      if (st.isDirectory()) {
        _opcCopySeedDir(s, d);
      } else if (!_opcExists(d)) {
        _opcCopyFile(s, d);
      }
    } catch {}
  }
}
try {
  const _opcMounts = %s;
  for (const m of _opcMounts) {
    _opcMkdir(m.container_path, { recursive: true });
    _opcCopySeedDir("/app/.opc-seed/" + m.name, m.container_path);
  }
} catch {}
// --- End OPC Seed Hydration ---
`, string(pairsJSON))
			items[containerEntryIdx].data = []byte(seedBootstrapJS + "\n" + entryStr)
			items[containerEntryIdx].header.Size = int64(len(items[containerEntryIdx].data))
		}
	}

	var outBuf bytes.Buffer
	tw := tar.NewWriter(&outBuf)
	for _, it := range items {
		it.header.Size = int64(len(it.data))
		if err := tw.WriteHeader(it.header); err != nil {
			return tarBytes
		}
		if len(it.data) > 0 {
			if _, err := tw.Write(it.data); err != nil {
				return tarBytes
			}
		}
	}
	if err := tw.Close(); err != nil {
		return tarBytes
	}
	_ = hydratePairs
	return outBuf.Bytes()
}
