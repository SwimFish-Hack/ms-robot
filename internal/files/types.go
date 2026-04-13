package files

import (
	"context"
	"time"
)

// FileInfo 文件信息
type FileInfo struct {
	ID         string `json:"id"`          // 文件ID（UUID）
	Name       string `json:"name"`        // 文件名（从manifest文件名获取）
	Size       int64  `json:"size"`        // 文件大小（字节）
	Type       string `json:"type"`        // MIME类型
	MD5        string `json:"md5"`         // MD5哈希值
	UploadTime int64  `json:"upload_time"` // 上传时间（Unix 秒时间戳）
	Extension  string `json:"extension"`   // 文件扩展名
}

// IsAPK 判断是否为APK文件
func (f *FileInfo) IsAPK() bool {
	return f.Extension == ".apk" || f.Type == "application/vnd.android.package-archive"
}

// CheckRequest MD5检查请求
type CheckRequest struct {
	MD5 string `json:"md5" binding:"required"`
}

// CheckResponse MD5检查响应
type CheckResponse struct {
	Exists bool   `json:"exists"`
	FileID string `json:"file_id,omitempty"`
}

// InstallResult 安装结果
type InstallResult struct {
	UDID    string `json:"udid"`
	FileID  string `json:"file_id"`
	Success bool   `json:"success"`
	Message string `json:"message"`
	// Output 设备上 pm install 的原始标准输出（与 POST .../adb/shell 的 results[].output 一致）
	Output string `json:"output,omitempty"`
}

// PushResult 推送结果
type PushResult struct {
	UDID      string `json:"udid"`
	FileID    string `json:"file_id"`
	TargetDir string `json:"target_dir"`
	Success   bool   `json:"success"`
	Message   string `json:"message"`
}

// DevicesAdbPushBody 多设备多文件推送的请求体（路径配置）
type DevicesAdbPushBody struct {
	TargetDir        string            `json:"target_dir"`         // 默认路径，未指定时用 /sdcard/Download
	FileTargetDirs   map[string]string `json:"file_target_dirs"`   // file_id -> 目标路径
	DeviceTargetDirs map[string]string `json:"device_target_dirs"` // udid -> 目标路径
	// 解析顺序：device_target_dirs[udid] > file_target_dirs[file_id] > target_dir
}

// TaskType 任务类型
type TaskType string

const (
	TaskTypeInstall TaskType = "install"
	TaskTypePush    TaskType = "push"
)

// Task 任务
type Task struct {
	ID          string             `json:"id"`
	Type        TaskType           `json:"type"`
	UDID        string             `json:"udid"`
	FileID      string             `json:"file_id"`
	TargetDir   string             `json:"target_dir,omitempty"` // 仅推送任务需要
	CreatedAt   time.Time          `json:"created_at"`
	CompletedAt *time.Time         `json:"completed_at,omitempty"`
	Result      *TaskResult        `json:"result,omitempty"`
	Ctx         context.Context    `json:"-"` // 上下文，用于取消任务
	Cancel      context.CancelFunc `json:"-"` // 取消函数
}

// TaskResult 任务结果
type TaskResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Output  string `json:"output,omitempty"` // 安装任务：pm install 原始回显
}

// TaskStatusResponse 任务状态响应（内部轮询用）
type TaskStatusResponse struct {
	TaskID      string      `json:"task_id"`
	Status      string      `json:"status"`             // "pending", "running", "completed", "failed"
	Progress    float64     `json:"progress,omitempty"` // 0-100
	Result      *TaskResult `json:"result,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
	CompletedAt *time.Time  `json:"completed_at,omitempty"`
}
