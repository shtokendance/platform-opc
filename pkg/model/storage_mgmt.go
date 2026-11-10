package model

// AppStorageMountDetail 包含持久化挂载点的运行时统计信息（容量、文件数、种子保护状态）
type AppStorageMountDetail struct {
	AppStorageMount
	SizeBytes     int64    `json:"size_bytes"`
	SizeHuman     string   `json:"size_human"`
	FileCount     int      `json:"file_count"`
	SampleFiles   []string `json:"sample_files,omitempty"`
	Status        string   `json:"status"` // healthy | missing | empty
	SeedProtected bool     `json:"seed_protected"`
}

// StorageServerDiskInfo 服务器底层磁盘水位看板信息
type StorageServerDiskInfo struct {
	Path        string  `json:"path"`
	TotalBytes  uint64  `json:"total_bytes"`
	UsedBytes   uint64  `json:"used_bytes"`
	FreeBytes   uint64  `json:"free_bytes"`
	TotalHuman  string  `json:"total_human"`
	UsedHuman   string  `json:"used_human"`
	FreeHuman   string  `json:"free_human"`
	UsageRatio  float64 `json:"usage_ratio"`  // 百分比，如 41.2
	IsLowSpace  bool    `json:"is_low_space"` // 剩余空间少于 10%
}

// GlobalStorageSummary 全局存储管理大盘摘要响应
type GlobalStorageSummary struct {
	ServerDisk   StorageServerDiskInfo   `json:"server_disk"`
	TotalVolumes int                     `json:"total_volumes"`
	TotalBytes   int64                   `json:"total_bytes"`
	TotalHuman   string                  `json:"total_human"`
	AppsStorage  []AppStorageOverview    `json:"apps_storage"`
}

// AppStorageOverview 单个应用的存储卷占用总览
type AppStorageOverview struct {
	AppSlug       string                  `json:"app_slug"`
	AppName       string                  `json:"app_name"`
	AppStatus     string                  `json:"app_status"`
	MountsCount   int                     `json:"mounts_count"`
	TotalBytes    int64                   `json:"total_bytes"`
	TotalHuman    string                  `json:"total_human"`
	TotalFiles    int                     `json:"total_files"`
	Mounts        []AppStorageMountDetail `json:"mounts"`
}
