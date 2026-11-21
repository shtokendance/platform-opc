package handler

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	"weknora-tokendance/pkg/deploy"
	"weknora-tokendance/pkg/model"
)

func validateDeliveryPlan(data []byte, slug string, port, currentVersion int, archive []byte) error {
	var plan struct {
		Format      int    `json:"format"`
		Slug        string `json:"slug"`
		Summary     string `json:"summary"`
		BaseVersion int    `json:"base_version"`
		Contract    struct {
			Slug    string `json:"slug"`
			Runtime struct {
				Port int `json:"port"`
			} `json:"runtime"`
			StorageMounts []json.RawMessage `json:"storage_mounts"`
		} `json:"contract"`
		Files []struct {
			Path   string `json:"path"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	if len(data) > 2<<20 || json.Unmarshal(data, &plan) != nil || plan.Format != 1 ||
		plan.Slug != slug || plan.Contract.Slug != slug || plan.Contract.Runtime.Port != port ||
		strings.TrimSpace(plan.Summary) == "" || plan.Files == nil || plan.Contract.StorageMounts == nil {
		return fmt.Errorf("invalid delivery_plan format, application, port, file list or mount list")
	}
	if plan.BaseVersion != currentVersion {
		return fmt.Errorf("application active version changed to v%d, analyze against the current version", currentVersion)
	}
	expected := make(map[string]string, len(plan.Files))
	for _, file := range plan.Files {
		if !validDeliveryFilePath(file.Path) || expected[file.Path] != "" || len(file.SHA256) != 64 {
			return fmt.Errorf("invalid or duplicate delivery_plan file")
		}
		expected[file.Path] = file.SHA256
	}
	reader := tar.NewReader(bytes.NewReader(archive))
	seen := make(map[string]bool, len(expected))
	for {
		entry, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("invalid application tar")
		}
		name := path.Clean(entry.Name)
		digest, ok := expected[name]
		if !ok {
			continue
		}
		if seen[name] || entry.Typeflag != tar.TypeReg {
			return fmt.Errorf("invalid application tar file: %s", name)
		}
		sum := sha256.New()
		if _, err := io.Copy(sum, reader); err != nil || fmt.Sprintf("%x", sum.Sum(nil)) != digest {
			return fmt.Errorf("delivery_plan file differs from application tar: %s", name)
		}
		seen[name] = true
	}
	if len(seen) != len(expected) {
		return fmt.Errorf("application tar is missing files listed in delivery_plan")
	}
	return nil
}

func validDeliveryFilePath(name string) bool {
	return name != "" && name != "." && name == path.Clean(name) &&
		!strings.HasPrefix(name, "/") && name != ".." && !strings.HasPrefix(name, "../") &&
		!strings.ContainsRune(name, 92) && !strings.ContainsRune(name, 0)
}

func readDeliveryArchiveFile(archive []byte, name string) (string, error) {
	if !validDeliveryFilePath(name) {
		return "", fmt.Errorf("invalid application file path")
	}
	reader := tar.NewReader(bytes.NewReader(archive))
	var found []byte
	for {
		entry, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("invalid application tar")
		}
		if path.Clean(entry.Name) != name {
			continue
		}
		if found != nil || entry.Typeflag != tar.TypeReg || entry.Size > 256<<10 {
			return "", fmt.Errorf("application file cannot be read as text")
		}
		found, err = io.ReadAll(reader)
		if err != nil || !utf8.Valid(found) || bytes.IndexByte(found, 0) >= 0 {
			return "", fmt.Errorf("application file cannot be read as text")
		}
	}
	if found == nil {
		return "", fmt.Errorf("application file does not exist in this version")
	}
	return string(found), nil
}

func buildDeploymentArchive(version *model.AppDeploymentVersion, archive []byte, mounts []model.AppStorageMount) []byte {
	if version.DeliveryPlanSHA256 != "" {
		return archive
	}
	return deploy.EnsureTarSeedProtection(version.AppSlug, archive, mounts)
}
