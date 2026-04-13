package files

import (
	"context"
	"fmt"
	"github.com/ms-robots/ms-robot/internal/device"
	"github.com/ms-robots/ms-robot/internal/deviceupload"
	"github.com/ms-robots/ms-robot/internal/logutil"
	"github.com/ms-robots/ms-robot/third_party/adb"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// sanitizeDeviceFileName 用于设备端路径：空格、/ \ 等替换为 _，避免 shell/pm install 解析问题。空则返回空。
func sanitizeDeviceFileName(name string) string {
	s := strings.TrimSpace(name)
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	return s
}

// Handler 文件管理处理器
type Handler struct {
	fileManager          *Manager
	unifiedDeviceManager *device.UnifiedDeviceManager

	taskQueue   chan *Task
	workerCount int
	tasks       map[string]*Task
	taskMutex   sync.RWMutex
	maxWorkers  int
}

// NewHandler 创建文件管理处理器
func NewHandler(fileManager *Manager, unifiedDeviceManager *device.UnifiedDeviceManager) *Handler {
	handler := &Handler{
		fileManager:          fileManager,
		unifiedDeviceManager: unifiedDeviceManager,
		taskQueue:            make(chan *Task, 1000),
		workerCount:          0,
		tasks:                make(map[string]*Task),
		maxWorkers:           3,
	}
	handler.startTaskProcessor()
	return handler
}

// getADBDevice 按 UDID 解析 ADB 设备
func (h *Handler) getADBDevice(udid string) (*adb.Device, error) {
	dev, _, err := h.unifiedDeviceManager.GetADBDevice(udid)
	return dev, err
}

// HandleCheckMD5 检查MD5是否存在
func (h *Handler) HandleCheckMD5(c *gin.Context) {
	var req CheckRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "请求参数错误: " + err.Error(), "code": 1001})
		return
	}

	exists, fileID := h.fileManager.CheckMD5(req.MD5)
	c.JSON(200, gin.H{
		"success": true,
		"data": CheckResponse{
			Exists: exists,
			FileID: fileID,
		},
	})
}

// HandleUpload 上传文件
func (h *Handler) HandleUpload(c *gin.Context) {
	// 获取文件
	file, err := c.FormFile("file")
	if err != nil {
		c.JSON(400, gin.H{"error": "获取文件失败: " + err.Error(), "code": 1002})
		return
	}

	// 获取MD5（如果前端提供了）
	expectedMD5 := c.PostForm("md5")

	// 打开文件
	src, err := file.Open()
	if err != nil {
		c.JSON(500, gin.H{"error": "打开文件失败: " + err.Error(), "code": 1003})
		return
	}
	defer src.Close()

	// 保存文件（流式处理）
	// 即使文件已存在，SaveFile 也会更新文件名信息（如果不同）
	fileInfo, err := h.fileManager.SaveFile(src, file.Filename, file.Size, expectedMD5)
	if err != nil {
		c.JSON(500, gin.H{"error": "保存文件失败: " + err.Error(), "code": 1004})
		return
	}

	// 检查是否是已存在的文件（通过比较MD5）
	isExisting := false
	if expectedMD5 != "" {
		existingFile, _ := h.fileManager.GetFile(fileInfo.ID)
		if existingFile != nil && existingFile.MD5 == expectedMD5 {
			// 检查上传时间，如果很早就说明是已存在的文件
			if existingFile.UploadTime < time.Now().Add(-time.Second).Unix() {
				isExisting = true
			}
		}
	}

	c.JSON(200, gin.H{
		"success": true,
		"data":    fileInfo,
		"exists":  isExisting,
	})
}

// HandleListFiles 列出所有文件
func (h *Handler) HandleListFiles(c *gin.Context) {
	files := h.fileManager.ListFiles()
	c.JSON(200, gin.H{
		"success": true,
		"data":    files,
	})
}

// HandleDeleteFiles 批量删除文件（:ids 逗号分隔）
func (h *Handler) HandleDeleteFiles(c *gin.Context) {
	idsParam := strings.TrimSpace(c.Param("ids"))
	if idsParam == "" {
		c.JSON(400, gin.H{"error": "ids 不能为空"})
		return
	}
	ids := strings.Split(idsParam, ",")
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
	}
	var validIDs []string
	for _, id := range ids {
		if id != "" {
			validIDs = append(validIDs, id)
		}
	}
	if len(validIDs) == 0 {
		c.JSON(400, gin.H{"error": "没有有效的 id"})
		return
	}

	var deleted []string
	var failed []gin.H
	for _, id := range validIDs {
		if err := h.fileManager.DeleteFile(id); err != nil {
			failed = append(failed, gin.H{"id": id, "error": err.Error()})
		} else {
			deleted = append(deleted, id)
		}
	}
	c.JSON(200, gin.H{"success": true, "data": gin.H{"deleted": deleted, "failed": failed}})
}

// HandleCleanupFiles 条件删除（query: upload_time_at 或 upload_time_before，Unix 秒时间戳，删除 upload_time < 阈值的文件）
func (h *Handler) HandleCleanupFiles(c *gin.Context) {
	tsStr := c.Query("upload_time_before")
	if tsStr == "" {
		tsStr = c.Query("upload_time_at")
	}
	if tsStr == "" {
		c.JSON(400, gin.H{"error": "请传 upload_time_at 或 upload_time_before（整数时间戳秒）"})
		return
	}
	t, err := parseInt(tsStr)
	if err != nil {
		c.JSON(400, gin.H{"error": "upload_time_at / upload_time_before 须为整数时间戳（秒）"})
		return
	}
	beforeTs := int64(t)

	deleted, failed := h.fileManager.DeleteFilesBefore(beforeTs)
	failedResp := make([]gin.H, len(failed))
	for i, f := range failed {
		failedResp[i] = gin.H{"id": f.ID, "error": f.Err}
	}
	c.JSON(200, gin.H{"success": true, "data": gin.H{"deleted": deleted, "failed": failedResp}})
}

func parseInt(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n)
	return n, err
}

// installAPK 安装APK到设备
func (h *Handler) installAPK(udid, fileID, filePath string) InstallResult {
	fileInfo, fileExists := h.fileManager.GetFile(fileID)
	if fileExists {
		logutil.Infof("[安装] 开始 设备=%s 文件=%s 名称=%s 大小≈%dMB", udid, fileID, fileInfo.Name, fileInfo.Size/(1024*1024))
	}
	adbDevice, err := h.getADBDevice(udid)
	if err != nil {
		logutil.Errorf("[安装] 失败 设备=%s 文件=%s 原因=获取设备失败 %v", udid, fileID, err)
		return InstallResult{
			UDID:    udid,
			FileID:  fileID,
			Success: false,
			Message: "失败\n" + err.Error(),
		}
	}

	// 获取文件信息（用于获取原始文件名）
	if !fileExists {
		logutil.Errorf("[安装] 失败 设备=%s 文件=%s 原因=文件信息不存在", udid, fileID)
		return InstallResult{
			UDID:    udid,
			FileID:  fileID,
			Success: false,
			Message: "失败\n文件信息不存在",
		}
	}

	// 打开APK文件
	file, err := os.Open(filePath)
	if err != nil {
		return InstallResult{
			UDID:    udid,
			FileID:  fileID,
			Success: false,
			Message: "失败\n" + err.Error(),
		}
	}
	defer file.Close()

	fileStat, err := file.Stat()
	if err != nil {
		return InstallResult{
			UDID:    udid,
			FileID:  fileID,
			Success: false,
			Message: "失败\n" + err.Error(),
		}
	}

	// 推送到设备 tmp/apks 目录，安装后不删（部分机型如 OPPO 上 pm install 可能异步读文件）
	tempDir := "/data/local/tmp/apks"
	baseName := sanitizeDeviceFileName(fileInfo.Name)
	if baseName == "" {
		baseName = fileInfo.MD5 + ".apk"
	} else if !strings.HasSuffix(strings.ToLower(baseName), ".apk") {
		baseName = baseName + ".apk"
	}
	tempPath := tempDir + "/" + baseName

	// 先创建临时目录（如果不存在）
	_, err = adbDevice.RunCommand("mkdir", "-p", tempDir)
	if err != nil {
		return InstallResult{
			UDID:    udid,
			FileID:  fileID,
			Success: false,
			Message: "失败\n" + err.Error(),
		}
	}
	// 删除 apks 下超过 30 分钟的文件，避免堆积
	_, _ = adbDevice.RunCommand("sh", "-c", "find "+tempDir+" -type f -mmin +30 -delete 2>/dev/null")

	// 推送APK文件到设备临时目录
	adapter := &deviceupload.AdbDeviceAdapter{Device: adbDevice}
	if err := adapter.Push(file, tempPath, fileStat.ModTime(), 0644); err != nil {
		logutil.Errorf("[安装] 失败 设备=%s 文件=%s 原因=推送失败 %v", udid, fileID, err)
		return InstallResult{
			UDID:    udid,
			FileID:  fileID,
			Success: false,
			Message: "失败\n" + err.Error(),
		}
	}
	logutil.Infof("[安装] 推送完成 设备=%s 文件=%s 开始执行 pm install", udid, fileID)

	// 使用pm install命令安装APK（-r表示替换已存在的应用）；安装后不删 apks 下文件，避免部分机型异步读导致解析失败
	output, err := adbDevice.RunCommand("pm", "install", "-r", tempPath)
	outTrim := strings.TrimSpace(output)
	if err != nil {
		msg := "失败"
		if outTrim == "" {
			msg = "失败\n" + err.Error()
		}
		return InstallResult{
			UDID:    udid,
			FileID:  fileID,
			Success: false,
			Message: msg,
			Output:  output,
		}
	}

	outputLower := strings.ToLower(output)
	if strings.Contains(outputLower, "success") {
		logutil.Infof("[安装] 成功 设备=%s 文件=%s", udid, fileID)
		return InstallResult{
			UDID:    udid,
			FileID:  fileID,
			Success: true,
			Message: "成功",
			Output:  output,
		}
	}
	logutil.Errorf("[安装] 失败 设备=%s 文件=%s 原因=pm install 输出=%s", udid, fileID, outTrim)
	return InstallResult{
		UDID:    udid,
		FileID:  fileID,
		Success: false,
		Message: "失败",
		Output:  output,
	}
}

// pushFile 推送文件到设备
func (h *Handler) pushFile(udid, fileID, filePath, targetDir string) PushResult {
	logutil.Infof("[推送] 开始 设备=%s 文件=%s 目标=%s", udid, fileID, targetDir)
	adbDevice, err := h.getADBDevice(udid)
	if err != nil {
		logutil.Errorf("[推送] 失败 设备=%s 文件=%s 原因=获取设备失败 %v", udid, fileID, err)
		return PushResult{
			UDID:      udid,
			FileID:    fileID,
			TargetDir: targetDir,
			Success:   false,
			Message:   "失败\n" + err.Error(),
		}
	}

	// 获取文件信息（用于获取原始文件名）
	fileInfo, fileExists := h.fileManager.GetFile(fileID)
	if !fileExists {
		return PushResult{
			UDID:      udid,
			FileID:    fileID,
			TargetDir: targetDir,
			Success:   false,
			Message:   "失败\n文件信息不存在",
		}
	}

	// 设备上使用原始文件名（安全化后）；空名时回退为 MD5+扩展名
	targetDirLinux := strings.ReplaceAll(targetDir, "\\", "/")
	baseName := sanitizeDeviceFileName(fileInfo.Name)
	if baseName == "" {
		ext := fileInfo.Extension
		if ext == "" && len(fileInfo.Name) > 0 {
			ext = filepath.Ext(fileInfo.Name)
		}
		baseName = fileInfo.MD5 + ext
	}
	targetPath := targetDirLinux + "/" + baseName

	file, err := os.Open(filePath)
	if err != nil {
		return PushResult{
			UDID:      udid,
			FileID:    fileID,
			TargetDir: targetDir,
			Success:   false,
			Message:   "失败\n" + err.Error(),
		}
	}
	defer file.Close()

	fileStat, err := file.Stat()
	if err != nil {
		return PushResult{
			UDID:      udid,
			FileID:    fileID,
			TargetDir: targetDir,
			Success:   false,
			Message:   "失败\n" + err.Error(),
		}
	}

	// 先创建目录（如果不存在）
	_, err = adbDevice.RunCommand("mkdir", "-p", targetDirLinux)
	if err != nil {
		return PushResult{
			UDID:      udid,
			FileID:    fileID,
			TargetDir: targetDir,
			Success:   false,
			Message:   "失败\n" + err.Error(),
		}
	}

	// 推送文件
	adapter := &deviceupload.AdbDeviceAdapter{Device: adbDevice}
	if err := adapter.Push(file, targetPath, fileStat.ModTime(), 0644); err != nil {
		logutil.Errorf("[推送] 失败 设备=%s 文件=%s 原因=%v", udid, fileID, err)
		return PushResult{
			UDID:      udid,
			FileID:    fileID,
			TargetDir: targetDir,
			Success:   false,
			Message:   "失败\n" + err.Error(),
		}
	}
	logutil.Infof("[推送] 成功 设备=%s 文件=%s", udid, fileID)
	return PushResult{
		UDID:      udid,
		FileID:    fileID,
		TargetDir: targetDir,
		Success:   true,
		Message:   "成功\n" + targetPath,
	}
}

// startTaskProcessor 启动任务处理器
func (h *Handler) startTaskProcessor() {
	for i := 0; i < h.maxWorkers; i++ {
		go h.taskWorker()
	}
}

// taskWorker 任务工作器
func (h *Handler) taskWorker() {
	for task := range h.taskQueue {
		h.processTask(task)
	}
}

// processTask 处理单个任务
func (h *Handler) processTask(task *Task) {
	// 更新任务状态为运行中
	h.taskMutex.Lock()
	task.Result = nil // 清空之前的结果
	h.taskMutex.Unlock()

	var result TaskResult

	// 获取文件路径
	filePath, err := h.fileManager.GetFilePath(task.FileID)
	if err != nil {
		result.Success = false
		result.Message = "获取文件路径失败: " + err.Error()
	} else {
		switch task.Type {
		case TaskTypeInstall:
			installResult := h.installAPK(task.UDID, task.FileID, filePath)
			result.Success = installResult.Success
			result.Message = installResult.Message
			result.Output = installResult.Output

		case TaskTypePush:
			pushResult := h.pushFile(task.UDID, task.FileID, filePath, task.TargetDir)
			result.Success = pushResult.Success
			result.Message = pushResult.Message

		default:
			result.Success = false
			result.Message = "未知任务类型"
		}
	}

	// 更新任务完成状态
	h.taskMutex.Lock()
	task.CompletedAt = &time.Time{}
	*task.CompletedAt = time.Now()
	task.Result = &result
	h.taskMutex.Unlock()
}

// AddTask 添加任务到队列
func (h *Handler) AddTask(taskType TaskType, udid, fileID, targetDir string) (string, error) {
	taskID := uuid.New().String()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute) // 5分钟超时

	task := &Task{
		ID:        taskID,
		Type:      taskType,
		UDID:      udid,
		FileID:    fileID,
		TargetDir: targetDir,
		CreatedAt: time.Now(),
		Ctx:       ctx,
		Cancel:    cancel,
	}

	h.taskMutex.Lock()
	h.tasks[taskID] = task
	h.taskMutex.Unlock()

	// 尝试将任务加入队列（非阻塞）
	select {
	case h.taskQueue <- task:
		return taskID, nil
	default:
		// 队列已满，取消任务
		h.taskMutex.Lock()
		delete(h.tasks, taskID)
		h.taskMutex.Unlock()
		cancel()
		return "", fmt.Errorf("任务队列已满，请稍后重试")
	}
}

// GetTaskStatus 获取任务状态
func (h *Handler) GetTaskStatus(taskID string) *TaskStatusResponse {
	h.taskMutex.RLock()
	task, exists := h.tasks[taskID]
	h.taskMutex.RUnlock()

	if !exists {
		return nil
	}

	status := "pending"
	if task.CompletedAt != nil {
		if task.Result != nil && task.Result.Success {
			status = "completed"
		} else {
			status = "failed"
		}
	} else {
		// 检查任务是否在队列中（简单判断）
		status = "running"
	}

	return &TaskStatusResponse{
		TaskID:      task.ID,
		Status:      status,
		Result:      task.Result,
		CreatedAt:   task.CreatedAt,
		CompletedAt: task.CompletedAt,
	}
}

// waitForTaskResults 轮询任务直到全部完成，返回与 taskIDs 同序的 TaskResult 列表
func (h *Handler) waitForTaskResults(taskIDs []string, pollInterval, timeout time.Duration) []*TaskResult {
	results := make([]*TaskResult, len(taskIDs))
	completed := make(map[int]bool)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allDone := true
		for i, taskID := range taskIDs {
			if completed[i] {
				continue
			}
			allDone = false
			status := h.GetTaskStatus(taskID)
			if status == nil {
				continue
			}
			if status.Status == "completed" || status.Status == "failed" {
				completed[i] = true
				results[i] = status.Result
			}
		}
		if allDone {
			break
		}
		time.Sleep(pollInterval)
	}
	for i := range results {
		if results[i] == nil {
			results[i] = &TaskResult{Success: false, Message: "超时或未完成"}
		}
	}
	return results
}

// resolveTargetDir 按 body 规则解析 (udid, fileID) 的目标路径
func resolveTargetDir(body *DevicesAdbPushBody, udid, fileID string) string {
	defaultPath := "/sdcard/Download"
	if body != nil && body.TargetDir != "" {
		defaultPath = body.TargetDir
	}
	if body != nil && body.DeviceTargetDirs != nil {
		if p := body.DeviceTargetDirs[udid]; p != "" {
			return p
		}
	}
	if body != nil && body.FileTargetDirs != nil {
		if p := body.FileTargetDirs[fileID]; p != "" {
			return p
		}
	}
	return defaultPath
}

// batchItemSetOutput 批量安装/推送 API：有设备原始回显时写入 output（前端优先展示）
func batchItemSetOutput(item gin.H, r *TaskResult) {
	if r != nil && r.Output != "" {
		item["output"] = r.Output
	}
}

// HandleDevicesAdbPush 多设备多文件推送（deviceKeys 由调用方从 context 解析后传入）
// POST /api/devices/:udids/adb/push/:files  body: { "target_dir", "file_target_dirs", "device_target_dirs" }
func (h *Handler) HandleDevicesAdbPush(c *gin.Context, deviceKeys []string) {
	if len(deviceKeys) == 0 {
		c.JSON(400, gin.H{"error": "缺少设备解析结果"})
		return
	}
	filesParam := strings.TrimSpace(c.Param("files"))
	if filesParam == "" {
		c.JSON(400, gin.H{"error": "files 不能为空"})
		return
	}
	fileIDList := strings.Split(filesParam, ",")
	for i := range fileIDList {
		fileIDList[i] = strings.TrimSpace(fileIDList[i])
	}

	var body DevicesAdbPushBody
	_ = c.ShouldBindJSON(&body)

	validUdids := deviceKeys
	var validFileIDs []string
	for _, f := range fileIDList {
		if f == "" {
			continue
		}
		_, exists := h.fileManager.GetFile(f)
		if !exists {
			c.JSON(404, gin.H{"error": "文件不存在: " + f})
			return
		}
		validFileIDs = append(validFileIDs, f)
	}
	if len(validUdids) == 0 {
		c.JSON(400, gin.H{"error": "没有有效的 udid"})
		return
	}
	if len(validFileIDs) == 0 {
		c.JSON(400, gin.H{"error": "没有有效的 file_id"})
		return
	}

	var taskIDs []string
	var pairs []struct{ udid, fileID, targetDir string }
	for _, udid := range validUdids {
		for _, fileID := range validFileIDs {
			targetDir := resolveTargetDir(&body, udid, fileID)
			taskID, err := h.AddTask(TaskTypePush, udid, fileID, targetDir)
			if err != nil {
				c.JSON(500, gin.H{"error": err.Error()})
				return
			}
			taskIDs = append(taskIDs, taskID)
			pairs = append(pairs, struct{ udid, fileID, targetDir string }{udid, fileID, targetDir})
		}
	}

	logutil.Infof("[推送] 请求 设备数=%d 文件数=%d 任务数=%d", len(validUdids), len(validFileIDs), len(taskIDs))
	results := h.waitForTaskResults(taskIDs, 500*time.Millisecond, 5*time.Minute)
	succeeded := make([]gin.H, 0)
	failed := make([]gin.H, 0)
	ok := 0
	for i, r := range results {
		item := gin.H{"udid": pairs[i].udid, "file_id": pairs[i].fileID, "target_dir": pairs[i].targetDir}
		if r.Success {
			item["message"] = r.Message
			batchItemSetOutput(item, r)
			succeeded = append(succeeded, item)
			ok++
		} else {
			item["error"] = r.Message
			batchItemSetOutput(item, r)
			failed = append(failed, item)
		}
	}
	logutil.Infof("[推送] 完成 成功=%d 失败=%d", ok, len(results)-ok)
	c.JSON(200, gin.H{"success": true, "data": gin.H{"succeeded": succeeded, "failed": failed}})
}

// HandleDevicesAdbInstall 多设备多文件安装 APK（deviceKeys 由调用方从 context 解析后传入）
// POST /api/devices/:udids/adb/install/:files  安装成功后删除该文件（仅当该 file_id 在所有目标设备上均安装成功时删）
func (h *Handler) HandleDevicesAdbInstall(c *gin.Context, deviceKeys []string) {
	if len(deviceKeys) == 0 {
		c.JSON(400, gin.H{"error": "缺少设备解析结果"})
		return
	}
	filesParam := strings.TrimSpace(c.Param("files"))
	if filesParam == "" {
		c.JSON(400, gin.H{"error": "files 不能为空"})
		return
	}
	fileIDList := strings.Split(filesParam, ",")
	for i := range fileIDList {
		fileIDList[i] = strings.TrimSpace(fileIDList[i])
	}

	validUdids := deviceKeys
	var validFileIDs []string
	for _, f := range fileIDList {
		if f == "" {
			continue
		}
		fileInfo, exists := h.fileManager.GetFile(f)
		if !exists {
			c.JSON(404, gin.H{"error": "文件不存在: " + f})
			return
		}
		if !fileInfo.IsAPK() {
			c.JSON(400, gin.H{"error": "文件不是 APK 格式: " + fileInfo.Name})
			return
		}
		validFileIDs = append(validFileIDs, f)
	}
	if len(validUdids) == 0 {
		c.JSON(400, gin.H{"error": "没有有效的 udid"})
		return
	}
	if len(validFileIDs) == 0 {
		c.JSON(400, gin.H{"error": "没有有效的 file_id"})
		return
	}

	var taskIDs []string
	var pairs []struct{ udid, fileID string }
	for _, udid := range validUdids {
		for _, fileID := range validFileIDs {
			taskID, err := h.AddTask(TaskTypeInstall, udid, fileID, "")
			if err != nil {
				c.JSON(500, gin.H{"error": err.Error()})
				return
			}
			taskIDs = append(taskIDs, taskID)
			pairs = append(pairs, struct{ udid, fileID string }{udid, fileID})
		}
	}

	logutil.Infof("[安装] 请求 设备数=%d 文件数=%d 任务数=%d", len(validUdids), len(validFileIDs), len(taskIDs))
	results := h.waitForTaskResults(taskIDs, 500*time.Millisecond, 5*time.Minute)

	// 安装成功后删除文件：仅当该 file_id 在所有目标设备上均安装成功时才删
	for _, fileID := range validFileIDs {
		allSuccess := true
		for i := range pairs {
			if pairs[i].fileID != fileID {
				continue
			}
			if results[i] == nil || !results[i].Success {
				allSuccess = false
				break
			}
		}
		if allSuccess {
			if err := h.fileManager.DeleteFile(fileID); err != nil {
				logutil.Warnf("[安装] 安装后删除文件 %s 失败: %v", fileID, err)
			} else {
				logutil.Infof("[安装] 已删除文件 %s（安装成功后）", fileID)
			}
		}
	}

	succeeded := make([]gin.H, 0)
	failed := make([]gin.H, 0)
	for i, r := range results {
		item := gin.H{"udid": pairs[i].udid, "file_id": pairs[i].fileID}
		if r.Success {
			item["message"] = r.Message
			batchItemSetOutput(item, r)
			succeeded = append(succeeded, item)
		} else {
			errMsg := r.Message
			if errMsg == "" {
				errMsg = "安装失败"
			}
			item["error"] = errMsg
			batchItemSetOutput(item, r)
			failed = append(failed, item)
		}
	}
	logutil.Infof("[安装] 完成 成功=%d 失败=%d", len(succeeded), len(failed))
	c.JSON(200, gin.H{"success": true, "data": gin.H{"succeeded": succeeded, "failed": failed}})
}
