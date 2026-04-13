package files

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/ms-robots/ms-robot/internal/logutil"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	// StorageDir 文件存储目录
	StorageDir = "storage/files"
	// ManifestSuffix manifest文件后缀
	ManifestSuffix = ".manifest.json"
)

// Manager 文件管理器
type Manager struct {
	md5Set map[string]struct{} // md5集合（用于CheckMD5快速查询）
	mu     sync.RWMutex
}

// NewManager 创建文件管理器
func NewManager() (*Manager, error) {
	m := &Manager{
		md5Set: make(map[string]struct{}),
	}

	// 加载元数据（如果目录存在）
	if err := m.loadMetadata(); err != nil {
		logutil.Warnf("[FileManager] 加载元数据失败: %v，将创建新的元数据文件", err)
	}

	return m, nil
}

// loadMetadata 加载元数据到内存（用于CheckMD5快速查询）
// 只加载MD5集合，不加载完整FileInfo（因为ListFiles已经直接扫描文件系统）
func (m *Manager) loadMetadata() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 递归扫描所有目录，只建立MD5集合
	err := filepath.WalkDir(StorageDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 忽略错误，继续扫描
		}

		// 只处理manifest文件
		if d.IsDir() || !strings.HasSuffix(d.Name(), ManifestSuffix) {
			return nil
		}

		// 从目录路径提取MD5（目录结构：file/哈希前2位/哈希[2:4]/完整哈希/）
		dir := filepath.Dir(path)
		md5Hash := filepath.Base(dir) // 目录名就是完整MD5
		if len(md5Hash) != 32 {
			// 不是标准MD5目录结构，跳过
			return nil
		}

		// 验证文件是否存在
		filePath := getFilePath(md5Hash)
		if _, err := os.Stat(filePath); err != nil {
			// 文件不存在，删除manifest
			os.Remove(path)
			return nil
		}

		// 只建立MD5集合（用于CheckMD5快速查询）
		m.md5Set[md5Hash] = struct{}{}

		return nil
	})

	if err != nil && !os.IsNotExist(err) {
		return err
	}

	return nil
}

// ManifestData manifest文件内容（只保留必要字段）
// 文件名在manifest文件名中，MD5可以从目录路径提取（目录名就是完整MD5）
// ID可以从md5+文件名推导（md5_filename格式）
// size可以从文件系统获取，extension/type可以从文件名推导
type ManifestData struct {
	UploadTime int64 `json:"upload_time"` // 上传时间（Unix 秒时间戳）
}

// saveFileMetadata 保存单个文件的元数据
func (m *Manager) saveFileMetadata(fileInfo *FileInfo) error {
	// 确保目录存在
	fileDir := getFileDir(fileInfo.MD5)
	if err := os.MkdirAll(fileDir, 0755); err != nil {
		return fmt.Errorf("创建目录失败: %v", err)
	}
	// 使用最新文件名作为manifest文件名
	manifestPath := getManifestPath(fileInfo.MD5, fileInfo.Name)
	// 只保存上传时间（MD5可以从目录路径提取，ID可以从md5+文件名推导，size可以从文件系统获取）
	manifestData := ManifestData{
		UploadTime: fileInfo.UploadTime,
	}
	data, err := json.MarshalIndent(manifestData, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath, data, 0644)
}

// CheckMD5 检查MD5是否存在
func (m *Manager) CheckMD5(md5Hash string) (bool, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// 先检查内存中的集合
	if _, exists := m.md5Set[md5Hash]; exists {
		return true, md5Hash
	}

	// 如果内存中没有，直接检查文件系统
	filePath := getFilePath(md5Hash)
	if _, err := os.Stat(filePath); err == nil {
		// 文件存在，更新内存集合
		m.md5Set[md5Hash] = struct{}{}
		return true, md5Hash
	}

	return false, ""
}

// SaveFile 保存文件（流式处理，前端已检查MD5不存在，直接保存）
func (m *Manager) SaveFile(reader io.Reader, filename string, size int64, expectedMD5 string) (*FileInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 再次检查MD5（防止并发上传相同文件）
	if expectedMD5 != "" {
		if _, exists := m.md5Set[expectedMD5]; exists {
			// 文件本体已存在，检查是否已有相同文件名的manifest
			manifestPath := getManifestPath(expectedMD5, filename)
			if _, err := os.Stat(manifestPath); err == nil {
				// manifest已存在，从文件系统读取并返回
				return m.getFileInfoFromManifest(expectedMD5, filename)
			}
			// 创建新的manifest入口（使用新文件名）
			ext := filepath.Ext(filename)
			filePath := getFilePath(expectedMD5)
			fileStat, _ := os.Stat(filePath)
			var fileSize int64
			if fileStat != nil {
				fileSize = fileStat.Size()
			}
			newFileInfo := &FileInfo{
				ID:         expectedMD5 + "_" + filename, // 使用md5+文件名作为唯一ID
				Name:       filename,
				Size:       fileSize,
				Type:       getMimeType(ext),
				MD5:        expectedMD5,
				UploadTime: time.Now().Unix(),
				Extension:  ext,
			}
			if err := m.saveFileMetadata(newFileInfo); err != nil {
				logutil.Errorf("[FileManager] 创建新manifest入口失败: %v", err)
			}
			return newFileInfo, nil
		}
	}

	// 使用MD5作为文件名（先写入临时文件，计算MD5后重命名）
	ext := filepath.Ext(filename)
	// 确保StorageDir存在（用于存放临时文件）
	if err := os.MkdirAll(StorageDir, 0755); err != nil {
		return nil, fmt.Errorf("创建存储目录失败: %v", err)
	}
	tempFilePath := filepath.Join(StorageDir, "temp_"+uuid.New().String())

	// 创建临时文件
	file, err := os.Create(tempFilePath)
	if err != nil {
		return nil, fmt.Errorf("创建文件失败: %v", err)
	}
	defer file.Close()

	// 使用流式处理：同时写入文件和计算MD5（用于验证）
	hash := md5.New()
	multiWriter := io.MultiWriter(file, hash)

	// 限制读取大小，防止超出预期大小
	limitedReader := io.LimitReader(reader, size)

	// 流式复制：分块读取和写入，避免一次性加载到内存
	buf := make([]byte, 32*1024) // 32KB缓冲区
	var totalWritten int64
	for {
		nr, err := limitedReader.Read(buf)
		if nr > 0 {
			nw, writeErr := multiWriter.Write(buf[:nr])
			if writeErr != nil {
				os.Remove(tempFilePath) // 清理临时文件
				return nil, fmt.Errorf("写入文件失败: %v", writeErr)
			}
			if nw != nr {
				os.Remove(tempFilePath)
				return nil, fmt.Errorf("写入字节数不匹配: 期望 %d，实际 %d", nr, nw)
			}
			totalWritten += int64(nw)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			os.Remove(tempFilePath)
			return nil, fmt.Errorf("读取文件失败: %v", err)
		}
	}

	// 确保文件大小匹配
	if totalWritten != size {
		os.Remove(tempFilePath)
		return nil, fmt.Errorf("文件大小不匹配: 期望 %d，实际 %d", size, totalWritten)
	}

	// 关闭文件
	if err := file.Close(); err != nil {
		os.Remove(tempFilePath)
		return nil, fmt.Errorf("关闭文件失败: %v", err)
	}

	// 计算MD5用于验证
	md5Hash := hex.EncodeToString(hash.Sum(nil))

	// 如果提供了预期MD5，验证是否匹配
	if expectedMD5 != "" && md5Hash != expectedMD5 {
		os.Remove(tempFilePath)
		return nil, fmt.Errorf("MD5校验失败: 期望 %s，实际 %s", expectedMD5, md5Hash)
	}

	// 再次检查MD5（防止并发问题）
	if _, exists := m.md5Set[md5Hash]; exists {
		// 文件本体已存在（可能是并发上传），删除刚创建的临时文件
		os.Remove(tempFilePath)
		// 检查是否已有相同文件名的manifest
		manifestPath := getManifestPath(md5Hash, filename)
		if _, err := os.Stat(manifestPath); err == nil {
			// manifest已存在，从文件系统读取并返回
			return m.getFileInfoFromManifest(md5Hash, filename)
		}
		// 创建新的manifest入口（使用新文件名）
		newFileInfo := &FileInfo{
			ID:         md5Hash + "_" + filename, // 使用md5+文件名作为唯一ID
			Name:       filename,
			Size:       size,
			Type:       getMimeType(ext),
			MD5:        md5Hash,
			UploadTime: time.Now().Unix(),
			Extension:  ext,
		}
		if err := m.saveFileMetadata(newFileInfo); err != nil {
			logutil.Errorf("[FileManager] 创建新manifest入口失败: %v", err)
		}
		return newFileInfo, nil
	}

	// 确保目录存在（在移动文件之前）
	fileDir := getFileDir(md5Hash)
	if err := os.MkdirAll(fileDir, 0755); err != nil {
		os.Remove(tempFilePath)
		return nil, fmt.Errorf("创建目录失败: %v", err)
	}

	// 将临时文件移动到新目录（文件名为MD5）
	finalFilePath := getFilePath(md5Hash)
	if err := os.Rename(tempFilePath, finalFilePath); err != nil {
		os.Remove(tempFilePath)
		return nil, fmt.Errorf("重命名文件失败: %v", err)
	}

	// 创建文件信息（ID使用md5_filename格式）
	fileInfo := &FileInfo{
		ID:         md5Hash + "_" + filename, // 使用md5+文件名作为唯一ID
		Name:       filename,
		Size:       size,
		Type:       getMimeType(ext),
		MD5:        md5Hash,
		UploadTime: time.Now().Unix(),
		Extension:  ext,
	}

	// 保存MD5到集合（用于CheckMD5快速查询）
	m.md5Set[md5Hash] = struct{}{}

	// 保存manifest文件
	if err := m.saveFileMetadata(fileInfo); err != nil {
		logutil.Errorf("[FileManager] 保存manifest文件失败: %v", err)
		// 如果保存manifest失败，删除文件
		os.Remove(finalFilePath)
		return nil, fmt.Errorf("保存manifest文件失败: %v", err)
	}

	return fileInfo, nil
}

// getFileInfoFromManifest 从manifest文件读取FileInfo
func (m *Manager) getFileInfoFromManifest(md5Hash, fileName string) (*FileInfo, error) {
	manifestPath := getManifestPath(md5Hash, fileName)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}

	var manifestData ManifestData
	if err := json.Unmarshal(data, &manifestData); err != nil {
		return nil, err
	}

	// 动态计算字段
	ext := filepath.Ext(fileName)
	filePath := getFilePath(md5Hash)
	fileStat, _ := os.Stat(filePath)
	var fileSize int64
	if fileStat != nil {
		fileSize = fileStat.Size()
	}

	return &FileInfo{
		ID:         md5Hash + "_" + fileName, // 从md5和文件名推导
		Name:       fileName,
		Size:       fileSize,
		Type:       getMimeType(ext),
		MD5:        md5Hash, // 从目录路径提取
		UploadTime: manifestData.UploadTime,
		Extension:  ext,
	}, nil
}

// GetFile 获取文件信息
func (m *Manager) GetFile(fileID string) (*FileInfo, bool) {
	// 解析ID：可能是纯MD5（旧格式）或 md5_filename（新格式）
	parts := strings.SplitN(fileID, "_", 2)
	if len(parts) == 2 {
		// 新格式：md5_filename
		md5Hash := parts[0]
		fileName := parts[1]
		// 验证MD5格式（32个十六进制字符）和文件名不为空
		if len(md5Hash) == 32 && fileName != "" {
			fileInfo, err := m.getFileInfoFromManifest(md5Hash, fileName)
			if err == nil {
				return fileInfo, true
			}
		}
	} else {
		// 旧格式：纯MD5，查找该MD5目录下的第一个manifest
		md5Hash := fileID
		fileDir := getFileDir(md5Hash)
		entries, err := os.ReadDir(fileDir)
		if err == nil {
			for _, entry := range entries {
				if !entry.IsDir() && strings.HasSuffix(entry.Name(), ManifestSuffix) {
					fileName := strings.TrimSuffix(entry.Name(), ManifestSuffix)
					fileInfo, err := m.getFileInfoFromManifest(md5Hash, fileName)
					if err == nil {
						return fileInfo, true
					}
					break
				}
			}
		}
	}

	return nil, false
}

// GetFilePath 获取文件路径
func (m *Manager) GetFilePath(fileID string) (string, error) {
	// 解析ID：可能是纯MD5（旧格式）或 md5_filename（新格式）
	parts := strings.SplitN(fileID, "_", 2)
	var md5Hash string
	if len(parts) == 2 {
		md5Hash = parts[0]
	} else {
		md5Hash = fileID // 旧格式：纯MD5
	}

	// 使用新的目录结构
	filePath := getFilePath(md5Hash)
	if _, err := os.Stat(filePath); err != nil {
		return "", fmt.Errorf("文件不存在: %s", filePath)
	}

	return filePath, nil
}

// ListFiles 列出所有文件（返回所有manifest入口）
func (m *Manager) ListFiles() []*FileInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	files := make([]*FileInfo, 0)

	// 重新扫描所有manifest文件，返回所有入口（不按MD5去重）
	err := filepath.WalkDir(StorageDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 忽略错误，继续扫描
		}

		// 只处理manifest文件
		if d.IsDir() || !strings.HasSuffix(d.Name(), ManifestSuffix) {
			return nil
		}

		// 从目录路径提取MD5（目录结构：file/哈希前2位/哈希[2:4]/完整哈希/）
		dir := filepath.Dir(path)
		md5Hash := filepath.Base(dir) // 目录名就是完整MD5
		if len(md5Hash) != 32 {
			// 不是标准MD5目录结构，跳过
			return nil
		}

		// 验证文件是否存在
		filePath := getFilePath(md5Hash)
		if _, err := os.Stat(filePath); err != nil {
			return nil
		}

		// 读取manifest文件获取上传时间
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		var manifestData ManifestData
		if err := json.Unmarshal(data, &manifestData); err != nil {
			return nil
		}

		// 从manifest文件名提取文件名
		manifestFileName := d.Name()
		fileName := strings.TrimSuffix(manifestFileName, ManifestSuffix)

		// 动态计算字段
		ext := filepath.Ext(fileName)
		fileStat, _ := os.Stat(filePath)
		var fileSize int64
		if fileStat != nil {
			fileSize = fileStat.Size()
		}

		// 构建FileInfo（每个manifest都是独立入口）
		// ID从md5+文件名推导（md5_filename格式）
		fileInfo := &FileInfo{
			ID:         md5Hash + "_" + fileName, // 从md5和文件名推导
			Name:       fileName,
			Size:       fileSize,         // 从文件系统获取
			Type:       getMimeType(ext), // 从扩展名推导
			MD5:        md5Hash,          // 从目录路径提取
			UploadTime: manifestData.UploadTime,
			Extension:  ext, // 从文件名提取
		}

		files = append(files, fileInfo)
		return nil
	})

	if err != nil {
		logutil.Errorf("[FileManager] 扫描manifest文件失败: %v", err)
	}

	return files
}

// DeleteFile 删除文件
func (m *Manager) DeleteFile(fileID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 解析ID：可能是纯MD5（旧格式）或 md5_filename（新格式）
	var md5Hash, fileName string
	parts := strings.SplitN(fileID, "_", 2)
	if len(parts) == 2 {
		// 新格式：md5_filename
		md5Hash = parts[0]
		fileName = parts[1]
	} else {
		// 旧格式：纯MD5，需要扫描该MD5目录下的所有manifest文件
		md5Hash = fileID
		fileDir := getFileDir(md5Hash)
		entries, err := os.ReadDir(fileDir)
		if err != nil {
			return fmt.Errorf("文件不存在: %s", fileID)
		}
		// 找到第一个manifest文件作为要删除的目标
		found := false
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ManifestSuffix) {
				fileName = strings.TrimSuffix(entry.Name(), ManifestSuffix)
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("文件不存在: %s", fileID)
		}
	}

	fileDir := getFileDir(md5Hash)

	// 先验证要删除的manifest文件是否存在
	manifestPath := getManifestPath(md5Hash, fileName)
	if _, err := os.Stat(manifestPath); err != nil {
		return fmt.Errorf("manifest文件不存在: %s", manifestPath)
	}

	// 删除指定的manifest文件
	if err := os.Remove(manifestPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除manifest文件失败: %v", err)
	}

	// 重新读取目录，检查该目录下是否还有其他manifest文件
	// 注意：需要重新读取，因为可能有并发删除
	entries, err := os.ReadDir(fileDir)
	hasOtherManifests := false
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ManifestSuffix) {
				hasOtherManifests = true
				break
			}
		}
	}

	// 如果没有其他manifest文件，删除文件本体和目录
	if !hasOtherManifests {
		filePath := getFilePath(md5Hash)
		if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除文件失败: %v", err)
		}

		// 检查目录是否为空，如果为空则删除整个目录树
		remainingEntries, err := os.ReadDir(fileDir)
		if err == nil {
			// 检查是否还有文件（忽略.和..）
			hasFiles := false
			for _, entry := range remainingEntries {
				if !entry.IsDir() {
					hasFiles = true
					break
				}
			}
			// 如果目录为空，删除整个目录树
			if !hasFiles {
				// 从最深层目录开始向上删除
				currentDir := fileDir
				for i := 0; i < 3; i++ { // 最多3层：完整哈希/哈希[2:4]/哈希前2位
					if currentDir == StorageDir {
						break
					}
					// 检查目录是否为空
					dirEntries, err := os.ReadDir(currentDir)
					if err != nil {
						break
					}
					empty := true
					for _, entry := range dirEntries {
						if entry.Name() != "." && entry.Name() != ".." {
							empty = false
							break
						}
					}
					if empty {
						os.Remove(currentDir)
						currentDir = filepath.Dir(currentDir)
					} else {
						break
					}
				}
			}
		}
	}

	// 如果该MD5没有其他manifest了，从md5Set中删除
	if !hasOtherManifests {
		delete(m.md5Set, md5Hash)
	}

	return nil
}

// DeleteFileFailed 删除失败项（用于 DeleteFilesBefore 返回）
type DeleteFileFailed struct {
	ID  string
	Err string
}

// DeleteFilesBefore 删除 upload_time < beforeTs 的所有文件，返回已删 id 列表与失败列表
func (m *Manager) DeleteFilesBefore(beforeTs int64) (deleted []string, failed []DeleteFileFailed) {
	files := m.ListFiles()
	for _, f := range files {
		if f.UploadTime >= beforeTs {
			continue
		}
		if err := m.DeleteFile(f.ID); err != nil {
			failed = append(failed, DeleteFileFailed{ID: f.ID, Err: err.Error()})
		} else {
			deleted = append(deleted, f.ID)
		}
	}
	return deleted, failed
}

// sanitizeFileName 清理文件名，使其可以作为文件系统文件名
// 移除或替换不允许的字符
func sanitizeFileName(filename string) string {
	// 替换Windows/Linux不允许的字符
	invalidChars := []string{"<", ">", ":", "\"", "/", "\\", "|", "?", "*"}
	result := filename
	for _, char := range invalidChars {
		result = strings.ReplaceAll(result, char, "_")
	}
	// 移除控制字符
	var builder strings.Builder
	for _, r := range result {
		if r >= 32 && r != 127 {
			builder.WriteRune(r)
		}
	}
	result = builder.String()
	// 移除首尾空格和点
	result = strings.Trim(result, " .")
	// 如果为空，使用默认名
	if result == "" {
		result = "unnamed"
	}
	return result
}

// getFileDir 根据MD5获取文件目录路径
// 格式：file/哈希前两位/哈希[2:4]/完整哈希/
func getFileDir(md5Hash string) string {
	if len(md5Hash) < 4 {
		return filepath.Join(StorageDir, md5Hash)
	}
	return filepath.Join(StorageDir, md5Hash[0:2], md5Hash[2:4], md5Hash)
}

// getFilePath 获取文件本体路径（使用固定文件名，因为目录名已经是MD5）
func getFilePath(md5Hash string) string {
	return filepath.Join(getFileDir(md5Hash), "file")
}

// getManifestPath 获取manifest文件路径
// 格式：file/哈希前两位/哈希[2:4]/完整哈希/文件名.manifest.json
func getManifestPath(md5Hash, filename string) string {
	sanitized := sanitizeFileName(filename)
	return filepath.Join(getFileDir(md5Hash), sanitized+ManifestSuffix)
}

// getMimeType 根据扩展名获取MIME类型
func getMimeType(ext string) string {
	mimeTypes := map[string]string{
		".apk":  "application/vnd.android.package-archive",
		".jpg":  "image/jpeg",
		".jpeg": "image/jpeg",
		".png":  "image/png",
		".gif":  "image/gif",
		".pdf":  "application/pdf",
		".txt":  "text/plain",
		".json": "application/json",
		".zip":  "application/zip",
		".mp4":  "video/mp4",
		".mp3":  "audio/mpeg",
	}

	if mime, ok := mimeTypes[ext]; ok {
		return mime
	}
	return "application/octet-stream"
}
