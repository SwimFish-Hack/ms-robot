package deviceupload

import (
	"bytes"
	"crypto/md5"
	"embed"
	"encoding/hex"
	"fmt"
	"github.com/ms-robots/ms-robot/internal/logutil"
	"io"
	"os"
	"strings"
	"time"

	adb "github.com/ms-robots/ms-robot/third_party/adb"
)

// Device 设备侧执行 shell 与 push 的抽象
type Device interface {
	RunShellCommand(cmd string, args ...string) (string, error)
	Push(reader io.Reader, remotePath string, modTime time.Time, mode ...os.FileMode) error
}

// AdbDeviceAdapter 将 *adb.Device 适配为 deviceupload.Device（Push 通过临时文件）
type AdbDeviceAdapter struct{ *adb.Device }

func (a *AdbDeviceAdapter) RunShellCommand(cmd string, args ...string) (string, error) {
	return a.RunCommand(cmd, args...)
}

func (a *AdbDeviceAdapter) Push(reader io.Reader, remotePath string, modTime time.Time, mode ...os.FileMode) error {
	f, err := os.CreateTemp("", "ms-robot-push-*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpPath := f.Name()
	defer os.Remove(tmpPath)
	if _, err := io.Copy(f, reader); err != nil {
		f.Close()
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if a.Device.Push(tmpPath, remotePath) != 0 {
		return fmt.Errorf("adb push 失败")
	}
	return nil
}

// Cache 用于缓存已上传文件的 MD5，避免重复读文件计算（APK/jar 等通用）
type Cache interface {
	Get(embedPath string) string
	Set(embedPath, md5 string)
}

func deviceMD5Matches(md5Output string, err error, expected string) bool {
	if err != nil || expected == "" {
		return false
	}
	fields := strings.Fields(strings.TrimSpace(md5Output))
	return len(fields) >= 1 && len(fields[0]) == 32 && fields[0] == expected
}

// Upload 读取文件并推送到设备；有 MD5 缓存且设备端一致则跳过上传。logPrefix 用于日志前缀。
// 数据源为 embed.FS，适用于 APK、jar 等任意资源。
func Upload(assetsFS embed.FS, embedPath, remotePath string, device Device, cache Cache, logPrefix string) error {
	deviceOutput, deviceErr := device.RunShellCommand("md5sum", remotePath)
	if deviceMD5Matches(deviceOutput, deviceErr, cache.Get(embedPath)) {
		logutil.Debugf("%s %s 已存在且校验和一致，跳过上传", logPrefix, remotePath)
		_, _ = device.RunShellCommand("chmod", "755", remotePath)
		return nil
	}

	file, err := assetsFS.Open(embedPath)
	if err != nil {
		return fmt.Errorf("打开嵌入文件失败: %v", err)
	}
	defer file.Close()
	fileInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("获取文件信息失败: %v", err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("读取嵌入文件失败: %v", err)
	}
	sum := md5.Sum(data)
	localMD5 := hex.EncodeToString(sum[:])

	if deviceMD5Matches(deviceOutput, deviceErr, localMD5) {
		logutil.Debugf("%s %s 已存在且校验和一致，跳过上传", logPrefix, remotePath)
		cache.Set(embedPath, localMD5)
		_, _ = device.RunShellCommand("chmod", "755", remotePath)
		return nil
	}
	if err := device.Push(bytes.NewReader(data), remotePath, fileInfo.ModTime()); err != nil {
		return fmt.Errorf("推送文件失败: %v", err)
	}
	cache.Set(embedPath, localMD5)
	if _, err := device.RunShellCommand("chmod", "755", remotePath); err != nil {
		return fmt.Errorf("设置权限失败: %v", err)
	}
	logutil.Debugf("%s 从嵌入文件系统上传 %s 到 %s 成功", logPrefix, embedPath, remotePath)
	return nil
}
