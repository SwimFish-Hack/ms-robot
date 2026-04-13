// 文件管理功能

let fileList = [];
let selectedFiles = new Set();

// 全局推送路径
let globalPushPath = '/sdcard/Download';

// 初始化文件管理
document.addEventListener('DOMContentLoaded', () => {
    initFileManager();
});

function initFileManager() {
    const fileManagerBtn = document.getElementById('file-manager-btn');
    const closeBtn = document.getElementById('close-file-manager-btn');
    const fileManagerPanel = document.getElementById('file-manager-panel');
    const uploadArea = document.getElementById('file-upload-area');
    const fileInput = document.getElementById('file-input');
    const batchDeleteBtn = document.getElementById('batch-delete-btn');
    const batchInstallBtn = document.getElementById('batch-install-btn');
    const batchPushBtn = document.getElementById('batch-push-btn');

    // 切换文件管理面板
    if (fileManagerBtn) {
        fileManagerBtn.addEventListener('click', () => {
            fileManagerPanel.classList.toggle('hidden');
            
            if (!fileManagerPanel.classList.contains('hidden')) {
                loadFileList();
            }
        });
    }

    // 关闭文件管理面板
    if (closeBtn) {
        closeBtn.addEventListener('click', () => {
            fileManagerPanel.classList.add('hidden');
        });
    }

    // 文件上传区域点击
    if (uploadArea) {
        uploadArea.addEventListener('click', () => {
            fileInput.click();
        });
    }

    // 文件选择
    if (fileInput) {
        fileInput.addEventListener('change', (e) => {
            handleFiles(e.target.files);
        });
    }

    // 拖拽上传
    if (uploadArea) {
        uploadArea.addEventListener('dragover', (e) => {
            e.preventDefault();
            uploadArea.classList.add('dragover');
        });

        uploadArea.addEventListener('dragleave', () => {
            uploadArea.classList.remove('dragover');
        });

        uploadArea.addEventListener('drop', (e) => {
            e.preventDefault();
            uploadArea.classList.remove('dragover');
            handleFiles(e.dataTransfer.files);
        });
    }

    // 批量安装APK
    if (batchInstallBtn) {
        batchInstallBtn.addEventListener('click', () => {
            handleBatchInstall();
        });
    }

    // 删除/清理：有选中则删除选中，无选中且有文件则清理
    if (batchDeleteBtn) {
        batchDeleteBtn.addEventListener('click', () => {
            if (selectedFiles.size > 0) handleBatchDelete();
            else if (fileList.length > 0) openCleanupDialog();
        });
    }

    // 批量发送到手机
    if (batchPushBtn) {
        batchPushBtn.addEventListener('click', () => {
            handleBatchPush();
        });
    }

    // 加载文件列表
    loadFileList();
    
    // 全选/取消全选
    const selectAllCheckbox = document.getElementById('select-all-files');
    if (selectAllCheckbox) {
        selectAllCheckbox.addEventListener('change', (e) => {
            const checkboxes = document.querySelectorAll('.file-item-checkbox');
            checkboxes.forEach(checkbox => {
                checkbox.checked = e.target.checked;
                const fileId = checkbox.dataset.fileId;
                if (e.target.checked) {
                    selectedFiles.add(fileId);
                    checkbox.closest('.file-item')?.classList.add('selected');
                } else {
                    selectedFiles.delete(fileId);
                    checkbox.closest('.file-item')?.classList.remove('selected');
                }
            });
            updateBatchButtons();
        });
    }
}

// 处理文件上传
async function handleFiles(files) {
    for (const file of files) {
        await uploadFile(file);
    }
}

// 上传文件（带MD5检查和进度条）
async function uploadFile(file) {
    try {
        // 显示上传进度UI
        showUploadProgress(file.name, file.size);
        
        // 先计算MD5（显示进度）
        updateUploadStatus(file.name, '计算MD5中...', 0);
        const md5 = await calculateMD5(file);
        
        // 直接上传文件（即使MD5已存在，也会创建新的manifest入口）
        updateUploadStatus(file.name, '上传中...', 10);
        await uploadFileWithProgress(file, md5);
        
        hideUploadProgress(file.name);
        showNotification(`文件 "${file.name}" 上传成功`, 'success');
        loadFileList();
    } catch (error) {
        console.error('上传文件失败:', error);
        hideUploadProgress(file.name);
        showNotification(`上传文件失败: ${error.message}`, 'error');
    }
}

// 使用XMLHttpRequest上传文件（支持进度）
function uploadFileWithProgress(file, md5) {
    return new Promise((resolve, reject) => {
        const xhr = new XMLHttpRequest();
        const formData = new FormData();
        formData.append('file', file);
        formData.append('md5', md5);

        const startTime = Date.now();
        let lastLoaded = 0;
        let lastTime = startTime;

        xhr.upload.addEventListener('progress', (e) => {
            if (e.lengthComputable) {
                const percent = Math.round((e.loaded / e.total) * 100);
                const currentTime = Date.now();
                const timeDiff = (currentTime - lastTime) / 1000; // 秒
                const loadedDiff = e.loaded - lastLoaded;
                
                // 计算速度（字节/秒）
                let speed = 0;
                if (timeDiff > 0) {
                    speed = loadedDiff / timeDiff;
                }
                
                // 更新进度（20%到95%，留5%给服务器处理）
                const uploadPercent = 20 + Math.round((percent / 100) * 75);
                updateUploadProgress(file.name, uploadPercent, speed, e.loaded, e.total);
                
                lastLoaded = e.loaded;
                lastTime = currentTime;
            }
        });

        xhr.addEventListener('load', () => {
            if (xhr.status === 200) {
                try {
                    const data = JSON.parse(xhr.responseText);
                    if (data.success) {
                        updateUploadProgress(file.name, 100, 0, file.size, file.size);
                        resolve(data);
                    } else {
                        reject(new Error((typeof data.error === 'string' ? data.error : data.error?.message) || '上传失败'));
                    }
                } catch (e) {
                    reject(new Error('解析响应失败'));
                }
            } else {
                reject(new Error(`上传失败: HTTP ${xhr.status}`));
            }
        });

        xhr.addEventListener('error', () => {
            reject(new Error('网络错误'));
        });

        xhr.addEventListener('abort', () => {
            reject(new Error('上传已取消'));
        });

        xhr.open('POST', '/api/files/upload');
        xhr.send(formData);
    });
}

// 计算文件MD5（使用spark-md5库）
async function calculateMD5(file) {
    return new Promise((resolve, reject) => {
        // 检查spark-md5是否可用
        if (typeof SparkMD5 === 'undefined') {
            reject(new Error('spark-md5库未加载'));
            return;
        }

        const spark = new SparkMD5.ArrayBuffer();
        const fileReader = new FileReader();
        const chunkSize = 2 * 1024 * 1024; // 2MB chunks
        let currentChunk = 0;
        const chunks = Math.ceil(file.size / chunkSize);

        fileReader.onload = function(e) {
            spark.append(e.target.result);
            currentChunk++;

            if (currentChunk < chunks) {
                loadNextChunk();
            } else {
                resolve(spark.end());
            }
        };

        fileReader.onerror = function() {
            reject(new Error('读取文件失败'));
        };

        function loadNextChunk() {
            const start = currentChunk * chunkSize;
            const end = Math.min(start + chunkSize, file.size);
            fileReader.readAsArrayBuffer(file.slice(start, end));
        }

        loadNextChunk();
    });
}

// 加载文件列表
async function loadFileList() {
    try {
        const response = await fetch('/api/files/list');
        const data = await response.json();
        
        if (data.success) {
            fileList = data.data || [];
            renderFileList();
            updateBatchButtons();
        } else {
            showNotification('加载文件列表失败', 'error');
        }
    } catch (error) {
        console.error('加载文件列表失败:', error);
        showNotification('加载文件列表失败', 'error');
    }
}

// 渲染文件列表
function renderFileList() {
    const fileListEl = document.getElementById('file-list');
    if (!fileListEl) return;

    fileListEl.innerHTML = '';

    fileList.forEach(file => {
        const fileItem = createFileItem(file);
        fileListEl.appendChild(fileItem);
    });
    updateBatchButtons();
}

// 创建文件项
function createFileItem(file) {
    const div = document.createElement('div');
    div.className = 'file-item';
    div.dataset.fileId = file.id;

    const isAPK = file.extension === '.apk' || file.type === 'application/vnd.android.package-archive';
    const fileSize = formatFileSize(file.size);

    div.innerHTML = `
        <input type="checkbox" class="file-item-checkbox" data-file-id="${file.id}">
        <div class="file-item-icon">${isAPK ? '📱' : '📄'}</div>
        <div class="file-item-info">
            <div class="file-item-name" title="${file.name}">${file.name}</div>
            <div class="file-item-meta" title="${fileSize} • ${formatDate(file.upload_time)}">${fileSize} • ${formatDate(file.upload_time)}</div>
        </div>
    `;

    const checkbox = div.querySelector('.file-item-checkbox');
    const wasSelected = selectedFiles.has(file.id);
    checkbox.checked = wasSelected;
    if (wasSelected) div.classList.add('selected');
    checkbox.addEventListener('change', (e) => {
        const fileId = e.target.dataset.fileId;
        if (e.target.checked) {
            selectedFiles.add(fileId);
            div.classList.add('selected');
        } else {
            selectedFiles.delete(fileId);
            div.classList.remove('selected');
        }
        updateBatchButtons();
    });

    // 点击文件项切换选中状态
    div.addEventListener('click', (e) => {
        if (e.target.type !== 'checkbox') {
            checkbox.checked = !checkbox.checked;
            checkbox.dispatchEvent(new Event('change'));
        }
    });

    return div;
}

// 更新批量按钮状态
function updateBatchButtons() {
    const batchDeleteBtn = document.getElementById('batch-delete-btn');
    const batchInstallBtn = document.getElementById('batch-install-btn');
    const batchPushBtn = document.getElementById('batch-push-btn');
    const selectAllCheckbox = document.getElementById('select-all-files');

    const selectedFilesArray = Array.from(selectedFiles);
    const selectedAPKs = selectedFilesArray.filter(fileId => {
        const file = fileList.find(f => f.id === fileId);
        return file && (file.extension === '.apk' || file.type === 'application/vnd.android.package-archive');
    });

    // 删除/清理按钮：有选中显示「删除」；无选中但有文件显示「清理」；无文件禁用
    if (batchDeleteBtn) {
        if (selectedFiles.size > 0) {
            batchDeleteBtn.textContent = '删除';
            batchDeleteBtn.disabled = false;
        } else if (fileList.length > 0) {
            batchDeleteBtn.textContent = '清理';
            batchDeleteBtn.disabled = false;
        } else {
            batchDeleteBtn.textContent = '删除';
            batchDeleteBtn.disabled = true;
        }
    }

    // 安装按钮：只有当选中的文件全是APK时才可用
    if (batchInstallBtn) {
        batchInstallBtn.disabled = selectedFilesArray.length === 0 || selectedAPKs.length !== selectedFilesArray.length;
    }

    // 发送按钮：有选中文件就可用
    if (batchPushBtn) {
        batchPushBtn.disabled = selectedFiles.size === 0;
    }
    
    // 更新全选复选框状态
    if (selectAllCheckbox) {
        const allCheckboxes = document.querySelectorAll('.file-item-checkbox');
        const checkedCount = Array.from(allCheckboxes).filter(cb => cb.checked).length;
        const totalCount = allCheckboxes.length;

        // 更新复选框状态
        if (allCheckboxes.length === 0) {
            selectAllCheckbox.checked = false;
            selectAllCheckbox.indeterminate = false;
        } else if (checkedCount === allCheckboxes.length) {
            selectAllCheckbox.checked = true;
            selectAllCheckbox.indeterminate = false;
        } else if (checkedCount > 0) {
            selectAllCheckbox.checked = false;
            selectAllCheckbox.indeterminate = true;
        } else {
            selectAllCheckbox.checked = false;
            selectAllCheckbox.indeterminate = false;
        }

        // 更新标签显示选中数量
        const label = document.querySelector('label[for="select-all-files"]');
        if (label) {
            const baseText = '文件列表';
            if (totalCount > 0) {
                label.textContent = `${baseText} (${checkedCount}/${totalCount})`;
            } else {
                label.textContent = baseText;
            }
        }
    }
}

// 批量安装APK
async function handleBatchInstall() {
    const selectedAPKs = Array.from(selectedFiles).filter(fileId => {
        const file = fileList.find(f => f.id === fileId);
        return file && (file.extension === '.apk' || file.type === 'application/vnd.android.package-archive');
    });

    if (selectedAPKs.length === 0) {
        showNotification('请选择要安装的APK文件', 'error');
        return;
    }

    // 获取当前选中的Android设备（复用统一函数）
    const deviceResult = await getSelectedAndroidDevices('请先选择要安装的Android设备');
    if (!deviceResult) return;
    const { selectedDevices } = deviceResult;

    // 隐藏文件管理面板，避免模态框显示混乱
    const fileManagerPanel = document.getElementById('file-manager-panel');
    const wasPanelVisible = !fileManagerPanel.classList.contains('hidden');
    if (wasPanelVisible) {
        fileManagerPanel.classList.add('hidden');
    }

    // 显示安装确认模态框
    const confirmed = await showInstallConfirmModal(selectedAPKs, selectedDevices, () => {
        // 取消时恢复文件管理面板状态
        if (wasPanelVisible) {
            fileManagerPanel.classList.remove('hidden');
        }
    });

    // 恢复文件管理面板状态
    if (wasPanelVisible) {
        fileManagerPanel.classList.remove('hidden');
    }

    if (!confirmed) {
        return; // 用户取消了安装
    }

    const udids = selectedDevices.join(',');
    const files = selectedAPKs.join(',');
    showNotification(`🚀 正在安装 ${selectedAPKs.length} 个 APK 到 ${selectedDevices.length} 台设备...`, null, 2000, 'info');

    try {
        const res = await fetch(`/api/devices/${encodeURIComponent(udids)}/adb/install/${encodeURIComponent(files)}`, {
            method: 'POST'
        });
        const data = await res.json();
        if (data.success && data.data) {
            const items = normalizeBatchData(data.data);
            if (items.length) showBatchOperationResultDialog('安装结果', items);
        } else {
            showNotification(`安装失败: ${(typeof data.error === 'string' ? data.error : data.error?.message) || '未知错误'}`, 'error');
        }
        selectedFiles.clear();
        renderFileList();
    } catch (error) {
        console.error('安装失败:', error);
        showNotification(`安装失败: ${error.message}`, 'error');
    }
}

// 发送文件到手机
// 统一的文件发送函数（使用已选中的设备）
async function sendFilesToDevices(fileIds, title, selectedDeviceObjects) {
    if (selectedDeviceObjects.length === 0) {
        showNotification('没有可用的Android设备', 'error');
        return;
    }

    // 获取文件信息
    const filesToSend = fileIds.map(fileId => {
        const file = fileList.find(f => f.id === fileId);
        return {
            id: fileId,
            name: file ? file.name : `文件${fileId}`
        };
    });

    // 显示批量文件发送模态框
    showBatchFilePushModal(title, filesToSend, selectedDeviceObjects);
}

// 显示批量文件发送模态框（支持全局路径和每个设备单独设置路径）
function showBatchFilePushModal(title, filesToSend, selectedDeviceObjects) {
    // 移除已存在的模态框
    const existingModal = document.querySelector('.batch-file-push-modal');
    if (existingModal) {
        existingModal.remove();
    }

    const modal = document.createElement('div');
    modal.className = 'batch-file-push-modal';

    // 用于存储每个设备的路径信息
    const devicePaths = new Map();
    let globalPath = globalPushPath || '/sdcard/Download';

    // 初始化每个设备的路径
    selectedDeviceObjects.forEach(device => {
        devicePaths.set(device.udid, globalPath);
    });

    const bodyHTML = `
            <!-- 全局路径设置区域 -->
            <div class="global-path-section" style="padding: 15px; border-bottom: 1px solid #eee; margin-bottom: 15px;">
                <div style="display: flex; align-items: center; gap: 10px;">
                    <label style="font-weight: 500; color: #2c3e50; white-space: nowrap;">推送路径:</label>
                    <input type="text" id="modal-global-path" placeholder="/sdcard/Download"
                           value="${globalPath}"
                           style="flex: 1; padding: 8px 12px; border: 1px solid #ddd; border-radius: 4px; font-size: 14px;">
                    <button id="apply-global-path" class="btn-secondary" style="padding: 8px 16px; white-space: nowrap;">应用到所有</button>
                    <div style="font-size: 12px; color: #7f8c8d; font-style: italic;">文件夹将自动创建</div>
                </div>
            </div>

            <!-- 文件列表 -->
            <div style="padding: 15px; border-bottom: 1px solid #eee; margin-bottom: 15px;">
                <strong style="color: #2c3e50;">📄 要发送的文件 (${filesToSend.length})</strong>
                <div style="margin-top: 10px; display: flex; flex-wrap: wrap; gap: 8px;">
                    ${filesToSend.map(file => `
                        <span style="padding: 4px 12px; background: #e8f5e9; border-radius: 4px; font-size: 12px; color: #2e7d32;">
                            ${file.name}
                        </span>
                    `).join('')}
                </div>
            </div>

            <!-- 设备列表（每个设备可以单独设置路径） -->
            <div style="max-height: 400px; overflow-y: auto; margin-bottom: 15px;">
                <strong style="color: #2c3e50; display: block; margin-bottom: 10px;">📱 目标设备 (${selectedDeviceObjects.length})</strong>
                ${selectedDeviceObjects.map(device => {
                    const deviceName = device.name || device.udid;
                    const showUdid = device.name && device.name !== device.udid;
                    const modelText = device.model ? `${device.model} • ` : '';
                    return `
                        <div class="device-select-item" style="display: flex; align-items: center; padding: 12px; border: 1px solid #eee; border-radius: 6px; margin-bottom: 8px; background: #f9f9f9;">
                            <div class="device-info" style="flex: 1;">
                                <div class="device-name-row">
                                    <strong>${deviceName}</strong>
                                    ${showUdid ? `<span style="font-size: 12px; color: #7f8c8d; margin-left: 8px;">${device.udid}</span>` : ''}
                                </div>
                                <div style="font-size: 12px; color: #7f8c8d; margin-top: 4px;">
                                    ${modelText}Android
                                </div>
                            </div>
                            <div style="display: flex; flex-direction: column; margin-left: 10px; gap: 4px;">
                                <input type="text" class="device-path-input" data-udid="${device.udid}"
                                       placeholder="/sdcard/Download" value="${globalPath}"
                                       style="padding: 6px 10px; border: 1px solid #ddd; border-radius: 4px; font-size: 12px; width: 250px;">
                                <div style="font-size: 11px; color: #7f8c8d; font-style: italic;">该设备的路径</div>
                            </div>
                        </div>
                    `;
                }).join('')}
            </div>

            <div class="device-select-actions">
                <button class="btn-secondary" onclick="this.closest('.batch-file-push-modal').remove()">取消</button>
                <button class="btn-primary" id="confirm-file-push">确定</button>
            </div>
    `;
    const shell = createDeviceSelectModal({ maxWidth: '800px', title, bodyHTML });
    if (shell && shell.wrap) modal.appendChild(shell.wrap);

    document.body.appendChild(modal);

    // 绑定关闭按钮事件
    const closeBtnEl = (shell && shell.closeBtn) || modal.querySelector('.close-btn');
    if (closeBtnEl) closeBtnEl.addEventListener('click', () => modal.remove());

    // 绑定全局路径输入框和应用按钮事件
    const globalPathInput = modal.querySelector('#modal-global-path');
    const applyGlobalBtn = modal.querySelector('#apply-global-path');
    
    // 全局路径输入框改变时更新变量
    if (globalPathInput) {
        globalPathInput.addEventListener('input', (e) => {
            globalPath = e.target.value.trim();
        });
    }
    
    // 点击"应用到所有"按钮，将全局路径应用到所有设备
    if (applyGlobalBtn) {
        applyGlobalBtn.addEventListener('click', () => {
            const currentGlobalPath = globalPathInput.value.trim() || '/sdcard/Download';
            globalPath = currentGlobalPath;
            
            // 更新所有设备的路径输入框
            selectedDeviceObjects.forEach(device => {
                const pathInput = modal.querySelector(`.device-path-input[data-udid="${CSS.escape(device.udid)}"]`);
                if (pathInput) {
                    pathInput.value = currentGlobalPath;
                    devicePaths.set(device.udid, currentGlobalPath);
                }
            });
            
            showNotification('已应用到所有设备', 'success', 1500);
        });
    }

    // 绑定每个设备的路径输入框事件
    selectedDeviceObjects.forEach(device => {
        const pathInput = modal.querySelector(`.device-path-input[data-udid="${CSS.escape(device.udid)}"]`);
        if (pathInput) {
            pathInput.addEventListener('input', (e) => {
                devicePaths.set(device.udid, e.target.value.trim());
            });
        }
    });

    // 确认按钮
    const confirmBtn = modal.querySelector('#confirm-file-push');
    confirmBtn.addEventListener('click', async () => {
        const fileCount = filesToSend.length;
        const deviceCount = selectedDeviceObjects.length;
        showNotification(`正在发送 ${fileCount} 个文件到 ${deviceCount} 个设备...`, 'info');

        const udids = selectedDeviceObjects.map(d => d.udid).join(',');
        const files = filesToSend.map(f => f.id).join(',');
        const targetDir = globalPath || '/sdcard/Download';
        const deviceTargetDirs = {};
        selectedDeviceObjects.forEach(d => {
            const p = devicePaths.get(d.udid) || targetDir;
            if (p !== targetDir) deviceTargetDirs[d.udid] = p;
        });
        const body = { target_dir: targetDir };
        if (Object.keys(deviceTargetDirs).length) body.device_target_dirs = deviceTargetDirs;

        // 关闭模态框
        modal.remove();

        try {
            const res = await fetch(`/api/devices/${encodeURIComponent(udids)}/adb/push/${encodeURIComponent(files)}`, {
                method: 'POST',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify(body)
            });
            const data = await res.json();
            if (data.success && data.data) {
                const items = normalizeBatchData(data.data);
                if (items.length) showBatchOperationResultDialog('发送结果', items);
                selectedFiles.clear();
                renderFileList();
            } else {
                showNotification(`发送失败: ${(typeof data.error === 'string' ? data.error : data.error?.message) || '未知错误'}`, 'error');
            }
        } catch (error) {
            console.error('发送失败:', error);
            showNotification(`发送失败: ${error.message}`, 'error');
        }
    });

    // 点击背景关闭
    modal.addEventListener('click', (e) => {
        if (e.target === modal) {
            modal.remove();
        }
    });
}

// 批量发送文件到手机
async function handleBatchPush() {
    if (selectedFiles.size === 0) {
        showNotification('请选择要发送的文件', 'error');
        return;
    }

    // 获取当前选中的Android设备（复用统一函数）
    const deviceResult = await getSelectedAndroidDevices('请先选择要发送的目标设备');
    if (!deviceResult) return;
    const { selectedDeviceObjects } = deviceResult;

    // 使用统一的发送函数
    const fileIds = Array.from(selectedFiles);
    await sendFilesToDevices(fileIds, '发送文件到手机', selectedDeviceObjects);

    // 清空选择
    selectedFiles.clear();
    renderFileList();
}

// 与 app.js getDeviceId 一致：须在覆盖 d.udid 之前用接口里的「序列号」字段计算（与卡片 data-device-id 对齐）
function deviceSlotIdFromListItem(d) {
    const serial = (d && d.udid) || (d && d.serial) || '';
    const eid = (d && d.endpoint_id) || '';
    return serial + (eid ? '@' + eid : '');
}

// 与 app.js 一致：多端点时 API 路径用 serial[:transport_id][@endpoint_id]
function toApiUdid(d) {
    const s = d.udid || d.serial || '';
    const t = d.transport_id;
    const e = d.endpoint_id;
    return s + (t ? ':' + t : '') + (e ? '@' + e : '');
}

// 获取Android设备列表
async function getAndroidDevices() {
    try {
        const response = await fetch('/api/devices');
        const data = await response.json();
        return (data.devices || [])
            .filter(d => d.status === 'online')
            .map(d => {
                const slotId = deviceSlotIdFromListItem(d);
                return { ...d, slotId, udid: toApiUdid(d) };
            });
    } catch (error) {
        console.error('获取设备列表失败:', error);
        return [];
    }
}

// 统一的设备信息获取函数（复用批量安装的逻辑）
function getDeviceInfos(deviceUdids) {
    return deviceUdids.map(udid => {
        // 优先查找 device-video-card，因为它有完整的设备信息（包括model）
        let deviceElement = document.querySelector(`.device-video-card[data-udid="${CSS.escape(udid)}"]`);
        if (!deviceElement) {
            deviceElement = document.querySelector(`[data-udid="${CSS.escape(udid)}"]`);
        }
        if (deviceElement) {
            const dataset = deviceElement.dataset;
            return {
                udid: udid,
                name: dataset.deviceName || udid,
                model: dataset.model || '',
                platform: dataset.platform || 'android'
            };
        }
        return {
            udid: udid,
            name: udid,
            model: '',
            platform: 'android'
        };
    });
}

// 获取选中的Android设备（统一复用）
// 返回: { selectedDevices: apiUdid[]（与路径 /api/devices/:udids 一致）, selectedDeviceObjects }
async function getSelectedAndroidDevices(errorMessage = '请先选择要操作的目标设备') {
    const selectedSlotIds = getSelectedDevices();
    if (selectedSlotIds.length === 0) {
        showNotification(errorMessage, 'error');
        return null;
    }

    const devices = await getAndroidDevices();
    const selectedDeviceObjects = devices.filter(device =>
        selectedSlotIds.includes(device.slotId)
    );

    if (selectedDeviceObjects.length === 0) {
        showNotification('选中的设备中没有可用的Android设备', 'error');
        return null;
    }

    const selectedApiUdids = selectedDeviceObjects.map(d => d.udid);

    return {
        selectedDevices: selectedApiUdids,
        selectedDeviceObjects
    };
}

// 渲染设备列表HTML（统一复用）
function renderDeviceListHTML(devices) {
    return devices.map(device => {
        // 如果设备对象有完整信息，直接使用；否则从DOM查找（fallback）
        let deviceName, deviceModel, devicePlatform;
        if (device.name !== undefined || device.model !== undefined || device.platform !== undefined) {
            // 设备对象有完整信息
            deviceName = device.name || device.udid;
            deviceModel = device.model || '';
            devicePlatform = device.platform || 'android';
        } else {
            // 只有udid，从DOM查找（fallback）
            const deviceInfo = getDeviceInfos([device.udid])[0];
            deviceName = deviceInfo.name;
            deviceModel = deviceInfo.model;
            devicePlatform = deviceInfo.platform;
        }
        
        // 如果name就是udid，只显示name；否则显示name和udid
        const showUdid = deviceName !== device.udid;
        // 只显示model，不显示"未知型号"
        const modelText = deviceModel ? `${deviceModel} • ` : '';
        return `
            <div class="device-item-info">
                <div class="device-name-row">
                    <strong>${deviceName}</strong>
                    ${showUdid ? `<span style="font-size: 12px; color: #7f8c8d; margin-left: 8px;">${device.udid}</span>` : ''}
                </div>
                <div style="font-size: 12px; color: #7f8c8d; margin-top: 4px;">
                    ${modelText}Android
                </div>
            </div>
        `;
    }).join('');
}

// 将后端批量结果统一为数组格式。支持 data 为 { succeeded, failed } 或旧版数组
// 注意：Go 中空 slice 若从未 append 会 JSON 成 null，不能用 Array.isArray(null) 判死
function normalizeBatchData(data) {
    if (Array.isArray(data)) return data;
    if (!data || typeof data !== 'object') return [];
    const succ = Array.isArray(data.succeeded) ? data.succeeded : (data.succeeded == null ? [] : null);
    const fail = Array.isArray(data.failed) ? data.failed : (data.failed == null ? [] : null);
    if (succ === null || fail === null) return [];
    const a = [];
    succ.forEach(r => a.push({ ...r, success: true, message: r.message }));
    fail.forEach(r => a.push({
        ...r,
        success: false,
        message: (r.error != null && String(r.error) !== '') ? r.error : r.message
    }));
    return a;
}

// 显示批量操作结果对话框（安装/发送完成，与批量 Shell 的 showShellCommandResults 一致：每台必有回显区）
// results: [{ udid, file_id, success, message, output?, target_dir? }]
function showBatchOperationResultDialog(title, results) {
    if (!results || results.length === 0) return;
    const existing = document.querySelector('.batch-shell-modal[data-batch-result="1"]');
    if (existing) existing.remove();

    const successCount = results.filter(r => r.success).length;
    const failCount = results.length - successCount;

    const getFileName = (fileId) => {
        const f = fileList.find(x => x.id === fileId);
        return f ? f.name : fileId;
    };

    /** 与 Shell 弹窗一致：优先设备回显 output，否则 message / error 文案 */
    const echoText = (r) => {
        const out = (r.output != null && String(r.output).trim() !== '') ? String(r.output) : '';
        if (out) return out;
        const msg = (r.message != null && String(r.message).trim() !== '') ? String(r.message) : '';
        if (msg) return msg;
        return r.success ? '(无输出)' : '未知错误';
    };

    const modal = document.createElement('div');
    modal.className = 'batch-shell-modal';
    modal.dataset.batchResult = '1';
    const bodyHTML = `
            <div style="padding: 15px; border-bottom: 1px solid #eee; margin-bottom: 15px;">
                <div style="display: flex; gap: 20px; align-items: center;">
                    <span style="color: #27ae60; font-weight: 500;">✅ 成功: ${successCount}</span>
                    <span style="color: #e74c3c; font-weight: 500;">❌ 失败: ${failCount}</span>
                </div>
            </div>
            <div style="max-height: 500px; overflow-y: auto;">
                ${results.map(r => {
                    const fileName = getFileName(r.file_id);
                    const targetInfo = r.target_dir ? ` → ${escapeHtml(r.target_dir)}` : '';
                    const body = echoText(r);
                    return `
                        <div style="margin-bottom: 15px; padding: 12px; border: 1px solid #eee; border-radius: 6px; background: ${r.success ? '#f0f9f4' : '#fef0f0'};">
                            <div style="display: flex; justify-content: space-between; align-items: center; margin-bottom: 8px;">
                                <div>
                                    <strong>${escapeHtml(r.udid)}</strong>
                                    <span style="font-size: 12px; color: #7f8c8d; margin-left: 8px;">${escapeHtml(fileName)}${targetInfo}</span>
                                </div>
                                <span style="color: ${r.success ? '#27ae60' : '#e74c3c'}; font-weight: 500;">
                                    ${r.success ? '✅ 成功' : '❌ 失败'}
                                </span>
                            </div>
                            <div style="background: #2c3e50; color: #ecf0f1; padding: 10px; border-radius: 4px; font-family: monospace; font-size: 12px; max-height: 200px; overflow-y: auto; white-space: pre-wrap; word-break: break-all;">
                                ${escapeHtml(body)}
                            </div>
                        </div>
                    `;
                }).join('')}
            </div>
            <div class="device-select-actions">
                <button class="btn-primary batch-result-close-btn">关闭</button>
            </div>
    `;
    const shell = createDeviceSelectModal({ maxWidth: '800px', title, bodyHTML });
    if (shell && shell.wrap) {
        modal.appendChild(shell.wrap);
    } else {
        modal.innerHTML = `<div class="device-select-content" style="max-width:800px;"><div class="device-select-header"><h3 style="margin:0;">${escapeHtml(title)}</h3><button type="button" class="close-btn batch-result-close">×</button></div><div class="device-select-body">${bodyHTML}</div></div>`;
    }
    document.body.appendChild(modal);

    const close = () => modal.remove();
    const closeBtnEl = modal.querySelector('.close-btn');
    if (closeBtnEl) closeBtnEl.addEventListener('click', close);
    const bottomClose = modal.querySelector('.batch-result-close-btn');
    if (bottomClose) bottomClose.addEventListener('click', close);
    modal.addEventListener('click', (e) => { if (e.target === modal) close(); });
}

function escapeHtml(s) {
    if (s == null) return '';
    const div = document.createElement('div');
    div.textContent = s;
    return div.innerHTML;
}

// 处理批量操作结果（统一复用，仅通知用；有结果弹窗时不再重复通知）
// results 每项可含: success, message, udid, file_id 等
function handleBatchOperationResults(results, operationName, successMessage = null, shouldShowNotification = true) {
    const successCount = results.filter(r => r.success).length;
    const failCount = results.length - successCount;
    const failed = results.filter(r => !r.success);
    const firstFailMsg = failed.length && failed[0].message ? failed[0].message : null;

    if (shouldShowNotification) {
        let text = successMessage
            ? `${successMessage}: 成功 ${successCount}，失败 ${failCount}`
            : `${operationName}完成: 成功 ${successCount}，失败 ${failCount}`;
        if (failCount > 0 && firstFailMsg) {
            const short = firstFailMsg.length > 80 ? firstFailMsg.slice(0, 77) + '...' : firstFailMsg;
            text += `\n${short}`;
        }
        showNotification(text, failCount === 0 ? 'success' : 'info');
    }

    return { successCount, failCount };
}

// 显示设备选择对话框
// pathMode: 'none' | 'perDevice' | 'global' | 'readonly' - 路径设置模式
//   'none': 安装APK，不显示路径设置
//   'perDevice': 每个设备单独设置路径
//   'global': 全局路径设置，可勾选设备
//   'readonly': 全局路径设置，设备列表只读（使用外面选中的设备）
function showDeviceSelectModal(title, devices, onConfirm, pathMode = 'none', onCancel = null) {
    // 移除已存在的模态框
    const existingModal = document.querySelector('.device-select-modal');
    if (existingModal) {
        existingModal.remove();
    }

    const modal = document.createElement('div');
    modal.className = 'device-select-modal';

    // 用于存储路径信息
    const devicePaths = new Map();
    let globalPath = globalPushPath || '/sdcard/Download';

    if (pathMode === 'perDevice') {
        devices.forEach(d => {
            devicePaths.set(d.udid, '/sdcard/Download'); // 默认路径
        });
    }

    const maxWidth = (pathMode === 'global' || pathMode === 'readonly') ? '600px' : '500px';
    const bodyHTML = `
            ${pathMode === 'global' || pathMode === 'readonly' ? `
            <!-- 全局路径设置区域 -->
            <div class="global-path-section" style="padding: 15px; border-bottom: 1px solid #eee; margin-bottom: 15px;">
                <div style="display: flex; align-items: center; gap: 10px;">
                    <label style="font-weight: 500; color: #2c3e50; white-space: nowrap;">推送路径:</label>
                    <input type="text" id="modal-global-path" placeholder="/sdcard/Download"
                           value="${globalPath}"
                           style="flex: 1; padding: 8px 12px; border: 1px solid #ddd; border-radius: 4px; font-size: 14px;">
                    <div style="font-size: 12px; color: #7f8c8d; font-style: italic;">文件夹将自动创建</div>
                </div>
            </div>
            ` : ''}

            ${pathMode === 'readonly' ? `
            <!-- 只读模式：使用批量安装的设备列表样式 -->
            <div class="install-confirm-content" style="padding: 15px;">
                <div class="install-summary">
                    <div class="summary-item">
                        <strong>📱 目标设备 (${devices.length})</strong>
                        <div class="device-list" style="max-height: 300px; overflow-y: auto; margin-top: 10px;">
                            ${renderDeviceListHTML(devices)}
                        </div>
                    </div>
                </div>
            </div>
            ` : `
            <div class="device-select-list" style="max-height: ${pathMode === 'global' ? '300px' : '400px'}; overflow-y: auto;">
                ${devices.map(device => `
                    <div class="device-select-item" style="display: flex; align-items: center; padding: 12px; border: 1px solid #eee; border-radius: 6px; margin-bottom: 8px; background: #f9f9f9;">
                        ${pathMode === 'global' ? `<input type="checkbox" checked class="device-checkbox" data-udid="${device.udid}" style="margin-right: 12px; width: 16px; height: 16px;">` : ''}
                        <div class="device-info" style="flex: 1;">
                            <div class="device-name-row">
                                <strong>${device.name || device.udid}</strong>
                                <span style="font-size: 12px; color: #7f8c8d; margin-left: 8px;">${device.udid}</span>
                            </div>
                            <div style="font-size: 12px; color: #7f8c8d; margin-top: 4px;">
                                ${device.model || ''} • ${device.os_version || ''}
                            </div>
                        </div>
                        <button class="info-btn" data-udid="${device.udid}" title="设备信息" style="margin-left: 8px; background: none; border: none; cursor: pointer; font-size: 16px;">ℹ️</button>
                        ${pathMode === 'perDevice' ? `
                            <div style="display: flex; flex-direction: column; margin-left: 10px; gap: 4px;">
                                <input type="text" class="device-path-input" data-udid="${device.udid}"
                                       placeholder="/sdcard/Download" value="/sdcard/Download"
                                       style="padding: 6px 10px; border: 1px solid #ddd; border-radius: 4px; font-size: 12px; width: 200px;">
                                <div style="font-size: 11px; color: #7f8c8d; font-style: italic;">文件夹将自动创建</div>
                            </div>
                        ` : ''}
                    </div>
                `).join('')}
            </div>
            `}
            <div class="device-select-actions">
                <button class="btn-secondary" onclick="this.closest('.device-select-modal').remove()">取消</button>
                <button class="btn-primary" id="confirm-device-select">确定</button>
            </div>
    `;
    const shell = createDeviceSelectModal({ maxWidth, title, bodyHTML });
    if (shell && shell.wrap) modal.appendChild(shell.wrap);

    document.body.appendChild(modal);

    // 绑定关闭按钮事件
    const closeBtn = (shell && shell.closeBtn) || modal.querySelector('.close-btn');
    if (closeBtn) {
        closeBtn.addEventListener('click', () => {
            modal.remove();
            if (onCancel) onCancel();
        });
    }

    // 绑定路径输入框和信息按钮事件
    devices.forEach(device => {
        const pathInput = modal.querySelector(`.device-path-input[data-udid="${CSS.escape(device.udid)}"]`);
        const infoBtn = modal.querySelector(`.info-btn[data-udid="${CSS.escape(device.udid)}"]`);

        // 路径输入框值变化时更新devicePaths
        if (pathInput) {
            pathInput.addEventListener('input', (e) => {
                devicePaths.set(device.udid, e.target.value.trim());
            });
        }

        // 绑定信息按钮事件
        if (infoBtn) {
            infoBtn.addEventListener('click', (e) => {
                e.stopPropagation();
                showDeviceInfoModal(device);
            });
        }
    });

    // 确认按钮
    const confirmBtn = modal.querySelector('#confirm-device-select');
    confirmBtn.addEventListener('click', () => {
        modal.remove();

        if (pathMode === 'none') {
            // 安装APK时只返回设备列表
            const allUDIDs = devices.map(d => d.udid);
            onConfirm(allUDIDs);
        } else if (pathMode === 'perDevice') {
            // 单点发送时返回所有设备和各自的路径
            const allUDIDs = devices.map(d => d.udid);
            onConfirm(allUDIDs, devicePaths);
        } else if (pathMode === 'global') {
            // 批量发送时返回选中的设备和全局路径
            const selectedUDIDs = Array.from(modal.querySelectorAll('.device-checkbox:checked'))
                .map(cb => cb.dataset.udid);
            const globalPathInput = modal.querySelector('#modal-global-path');
            const globalPath = globalPathInput ? globalPathInput.value.trim() : globalPath;
            onConfirm(selectedUDIDs, globalPath);
        } else if (pathMode === 'readonly') {
            // 只读模式：使用外面选中的设备，只设置全局路径
            const allUDIDs = devices.map(d => d.udid);
            const globalPathInput = modal.querySelector('#modal-global-path');
            const globalPath = globalPathInput ? globalPathInput.value.trim() : globalPath;
            onConfirm(allUDIDs, globalPath);
        }
    });

    // 点击背景关闭
    modal.addEventListener('click', (e) => {
        if (e.target === modal) {
            modal.remove();
        }
    });
}

// 格式化文件大小
function formatFileSize(bytes) {
    if (bytes === 0) return '0 B';
    const k = 1024;
    const sizes = ['B', 'KB', 'MB', 'GB'];
    const i = Math.floor(Math.log(bytes) / Math.log(k));
    return Math.round(bytes / Math.pow(k, i) * 100) / 100 + ' ' + sizes[i];
}

// 格式化日期（upload_time 为 Unix 秒时间戳，需乘 1000 转成毫秒）
function formatDate(dateString) {
    const ms = typeof dateString === 'number' ? dateString * 1000 : dateString;
    const date = new Date(ms);
    return date.toLocaleString('zh-CN', {
        year: 'numeric',
        month: '2-digit',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit'
    });
}

// 格式化速度
function formatSpeed(bytesPerSecond) {
    if (bytesPerSecond === 0) return '0 B/s';
    const k = 1024;
    const sizes = ['B/s', 'KB/s', 'MB/s', 'GB/s'];
    const i = Math.floor(Math.log(bytesPerSecond) / Math.log(k));
    return Math.round(bytesPerSecond / Math.pow(k, i) * 100) / 100 + ' ' + sizes[i];
}

// 显示上传进度UI
function showUploadProgress(fileName, fileSize) {
    const uploadArea = document.getElementById('file-upload-area');
    if (!uploadArea) return;

    // 创建进度条容器
    const progressId = `upload-progress-${fileName.replace(/[^a-zA-Z0-9]/g, '_')}`;
    let progressContainer = document.getElementById(progressId);
    
    if (!progressContainer) {
        progressContainer = document.createElement('div');
        progressContainer.id = progressId;
        progressContainer.className = 'upload-progress-item';
        progressContainer.dataset.fileName = fileName;
        uploadArea.appendChild(progressContainer);
    }

    progressContainer.innerHTML = `
        <div class="upload-progress-info">
            <div class="upload-progress-name" title="${fileName}">${fileName}</div>
            <div class="upload-progress-status">准备中...</div>
        </div>
        <div class="upload-progress-bar-container">
            <div class="upload-progress-bar" style="width: 0%"></div>
        </div>
        <div class="upload-progress-details">
            <span class="upload-progress-size">0 / ${formatFileSize(fileSize)}</span>
            <span class="upload-progress-speed">-</span>
        </div>
    `;
}

// 更新上传进度
function updateUploadProgress(fileName, percent, speed, loaded, total) {
    const progressId = `upload-progress-${fileName.replace(/[^a-zA-Z0-9]/g, '_')}`;
    const progressContainer = document.getElementById(progressId);
    if (!progressContainer) return;

    const bar = progressContainer.querySelector('.upload-progress-bar');
    const sizeSpan = progressContainer.querySelector('.upload-progress-size');
    const speedSpan = progressContainer.querySelector('.upload-progress-speed');

    if (bar) {
        bar.style.width = percent + '%';
    }
    if (sizeSpan) {
        sizeSpan.textContent = `${formatFileSize(loaded)} / ${formatFileSize(total)}`;
    }
    if (speedSpan) {
        speedSpan.textContent = formatSpeed(speed);
    }
}

// 更新上传状态文本
function updateUploadStatus(fileName, status, percent) {
    const progressId = `upload-progress-${fileName.replace(/[^a-zA-Z0-9]/g, '_')}`;
    const progressContainer = document.getElementById(progressId);
    if (!progressContainer) return;

    const statusEl = progressContainer.querySelector('.upload-progress-status');
    const bar = progressContainer.querySelector('.upload-progress-bar');
    
    if (statusEl) {
        statusEl.textContent = status;
    }
    if (bar && percent !== undefined) {
        bar.style.width = percent + '%';
    }
}

// 打开清理对话框（按时间删除）
function openCleanupDialog() {
    const existing = document.querySelector('.cleanup-files-modal');
    if (existing) existing.remove();
    const modal = document.createElement('div');
    modal.className = 'cleanup-files-modal';
    const bodyHTML = `
            <div style="padding: 16px;">
                <p style="margin: 0 0 12px 0; color: #555;">删除多少秒之前上传的文件？（单位：秒）</p>
                <input type="number" id="cleanup-seconds" min="1" value="86400" placeholder="86400" style="width: 100%; padding: 8px 12px; border: 1px solid #ddd; border-radius: 4px; font-size: 14px; box-sizing: border-box;">
            </div>
            <div class="device-select-actions" style="padding: 12px 16px; border-top: 1px solid #eee;">
                <button class="btn-secondary cleanup-cancel">取消</button>
                <button class="btn-primary" id="cleanup-confirm">清理</button>
            </div>
    `;
    const shell = createDeviceSelectModal({ maxWidth: '360px', title: '清理文件', bodyHTML });
    if (shell && shell.wrap) {
        modal.appendChild(shell.wrap);
        if (shell.closeBtn) shell.closeBtn.classList.add('cleanup-close');
    } else {
        modal.innerHTML = `<div class="device-select-content" style="max-width:360px;"><div class="device-select-header"><h3 style="margin:0;">清理文件</h3><button class="close-btn cleanup-close">×</button></div><div class="device-select-body">${bodyHTML}</div></div>`;
    }
    modal.style.cssText = 'position: fixed; inset: 0; background: rgba(0,0,0,0.4); display: flex; align-items: center; justify-content: center; z-index: 10000;';
    document.body.appendChild(modal);
    const close = () => modal.remove();
    const cleanupCloseEl = modal.querySelector('.cleanup-close');
    if (cleanupCloseEl) cleanupCloseEl.onclick = close;
    modal.querySelector('.cleanup-cancel').onclick = close;
    modal.onclick = (e) => { if (e.target === modal) close(); };
    modal.querySelector('#cleanup-confirm').onclick = () => {
        const seconds = modal.querySelector('#cleanup-seconds').value;
        modal.remove();
        const sec = parseInt(seconds, 10);
        if (sec > 0) {
            const beforeTs = Math.floor(Date.now() / 1000) - sec;
            runCleanup(beforeTs);
        }
    };
}

// 执行清理（upload_time_before 为 Unix 秒时间戳，删除早于该时间的文件）
async function runCleanup(uploadTimeBefore) {
    if (uploadTimeBefore == null || uploadTimeBefore < 0) return;
    showNotification('正在按时间清理文件...', 'info');
    try {
        const res = await fetch(`/api/files?upload_time_before=${uploadTimeBefore}`, { method: 'DELETE' });
        const data = await res.json();
        if (!data.success) {
            showNotification(`清理失败: ${(typeof data.error === 'string' ? data.error : data.error?.message) || '未知错误'}`, 'error');
            return;
        }
        const deleted = (data.data && data.data.deleted) ? data.data.deleted.length : 0;
        const failed = (data.data && data.data.failed) ? data.data.failed.length : 0;
        if (deleted > 0) showNotification(`已清理 ${deleted} 个文件${failed > 0 ? `，${failed} 个失败` : ''}`, failed > 0 ? 'warning' : 'success');
        else showNotification(failed > 0 ? `清理失败: ${failed} 个` : '没有符合条件的文件', failed > 0 ? 'error' : 'info');
        selectedFiles.clear();
        loadFileList();
    } catch (e) {
        showNotification(`清理失败: ${e.message}`, 'error');
    }
}

// 批量删除文件（一次请求）
async function handleBatchDelete() {
    if (selectedFiles.size === 0) {
        showNotification('请选择要删除的文件', 'error');
        return;
    }

    if (!confirm(`确定要删除选中的 ${selectedFiles.size} 个文件吗？此操作不可恢复！`)) {
        return;
    }

    showNotification(`🗑️ 正在删除 ${selectedFiles.size} 个文件...`, null, 2000, 'info');

    try {
        const ids = Array.from(selectedFiles).join(',');
        const response = await fetch(`/api/files/${encodeURIComponent(ids)}`, {
            method: 'DELETE'
        });
        const data = await response.json();
        if (!data.success) {
            showNotification(`删除失败: ${(typeof data.error === 'string' ? data.error : data.error?.message) || '未知错误'}`, 'error');
            return;
        }
        const deleted = (data.data && data.data.deleted) ? data.data.deleted.length : 0;
        const failed = (data.data && data.data.failed) ? data.data.failed.length : 0;
        if (failed === 0) {
            showNotification(`✅ 成功删除 ${deleted} 个文件`, null, 3000, 'success');
        } else if (deleted === 0) {
            showNotification(`❌ 删除失败：${failed} 个文件删除失败`, null, 4000, 'error');
        } else {
            showNotification(`⚠️ 删除完成：成功 ${deleted} 个，失败 ${failed} 个`, null, 4000, 'warning');
        }
        selectedFiles.clear();
        loadFileList();
    } catch (error) {
        console.error('删除失败:', error);
        showNotification(`删除失败: ${error.message}`, 'error');
    }
}

// 显示安装确认模态框
function showInstallConfirmModal(apkFileIds, deviceUdids, onCancel = null) {
    return new Promise((resolve) => {
        // 移除已存在的模态框
        const existingModal = document.querySelector('.install-confirm-modal');
        if (existingModal) {
            existingModal.remove();
        }

        // 获取APK和设备信息
        const apkInfos = apkFileIds.map(fileId => {
            const file = fileList.find(f => f.id === fileId);
            return file ? {
                id: file.id,
                name: file.name,
                size: file.size
            } : null;
        }).filter(Boolean);

        // 获取设备信息（复用统一函数）
        const deviceInfos = getDeviceInfos(deviceUdids);

        const modal = document.createElement('div');
        modal.className = 'install-confirm-modal device-select-modal'; // 复用样式

        const bodyHTML = `
                <div class="install-confirm-content">
                    <div class="install-summary">
                        <div class="summary-item">
                            <strong>📱 APK文件 (${apkInfos.length})</strong>
                            <div class="file-list">
                                ${apkInfos.map(apk => `
                                    <div class="file-item-info">
                                        <div class="file-item-name">${apk.name}</div>
                                        <div class="file-item-meta">${formatFileSize(apk.size)}</div>
                                    </div>
                                `).join('')}
                            </div>
                        </div>
                        <div class="summary-item">
                            <strong>📱 目标设备 (${deviceInfos.length})</strong>
                            <div class="device-list">
                                ${renderDeviceListHTML(deviceInfos)}
                            </div>
                        </div>
                        <div class="install-stats">
                            <div class="stat-item">📊 总任务数: ${apkInfos.length * deviceInfos.length}</div>
                        </div>
                    </div>
                </div>
                <div class="device-select-actions">
                    <button class="btn-secondary" id="cancel-install">取消</button>
                    <button class="btn-primary" id="confirm-install">确认安装</button>
                </div>
        `;
        const shell = createDeviceSelectModal({ maxWidth: '700px', title: '📦 确认安装APK', bodyHTML });
        if (shell && shell.wrap) modal.appendChild(shell.wrap);

        document.body.appendChild(modal);

        // 绑定关闭按钮事件
        const closeBtn = (shell && shell.closeBtn) || modal.querySelector('.close-btn');
        if (closeBtn) {
            closeBtn.addEventListener('click', () => {
                modal.remove();
                if (onCancel) onCancel();
                resolve(false);
            });
        }

        // 绑定事件
        const cancelBtn = modal.querySelector('#cancel-install');
        const confirmBtn = modal.querySelector('#confirm-install');

        cancelBtn.addEventListener('click', () => {
            modal.remove();
            if (onCancel) onCancel();
            resolve(false);
        });

        confirmBtn.addEventListener('click', () => {
            modal.remove();
            resolve(true);
        });

        // 点击背景关闭
        modal.addEventListener('click', (e) => {
            if (e.target === modal) {
                modal.remove();
                if (onCancel) onCancel();
                resolve(false);
            }
        });
    });
}

// 显示设备信息模态框（用于模态框环境）
function showDeviceInfoModal(device) {
    // 移除已存在的设备信息模态框
    const existingModal = document.querySelector('.device-info-modal');
    if (existingModal) {
        existingModal.remove();
    }

    const modal = document.createElement('div');
    modal.className = 'device-info-modal device-select-modal'; // 复用样式

    const bodyHTML = `
            <div class="device-info-content" style="padding: 20px;">
                <div class="device-info-grid">
                    <div class="info-row">
                        <strong>设备名称:</strong>
                        <span>${device.name || '未命名'}</span>
                    </div>
                    <div class="info-row">
                        <strong>UDID:</strong>
                        <span style="font-family: monospace; font-size: 12px;">${device.udid}</span>
                    </div>
                    <div class="info-row">
                        <strong>型号:</strong>
                        <span>${device.model || '未知'}</span>
                    </div>
                    <div class="info-row">
                        <strong>系统版本:</strong>
                        <span>${device.os_version || '未知'}</span>
                    </div>
                    <div class="info-row">
                        <strong>平台:</strong>
                        <span>🤖 Android</span>
                    </div>
                    <div class="info-row">
                        <strong>连接状态:</strong>
                        <span>${device.status === 'online' ? '🟢 在线' : device.status === 'offline' ? '🔴 离线' : '🟡 ' + (device.status || '未知')}</span>
                    </div>
                    ${device.ip ? `
                    <div class="info-row">
                        <strong>IP地址:</strong>
                        <span>${device.ip}</span>
                    </div>
                    ` : ''}
                    ${device.port ? `
                    <div class="info-row">
                        <strong>端口:</strong>
                        <span>${device.port}</span>
                    </div>
                    ` : ''}
                </div>
            </div>
    `;
    const shell = createDeviceSelectModal({ maxWidth: '500px', title: `📱 设备信息 - ${device.name || device.udid}`, bodyHTML });
    if (shell && shell.wrap) modal.appendChild(shell.wrap);

    document.body.appendChild(modal);

    // 绑定关闭按钮事件
    const closeBtn = (shell && shell.closeBtn) || modal.querySelector('.close-btn');
    if (closeBtn) {
        closeBtn.addEventListener('click', () => {
            modal.remove();
        });
    }

    // 点击背景关闭
    modal.addEventListener('click', (e) => {
        if (e.target === modal) {
            modal.remove();
        }
    });
}

// 隐藏上传进度
function hideUploadProgress(fileName) {
    const progressId = `upload-progress-${fileName.replace(/[^a-zA-Z0-9]/g, '_')}`;
    const progressContainer = document.getElementById(progressId);
    if (progressContainer) {
        // 延迟移除，让用户看到100%完成
        setTimeout(() => {
            progressContainer.remove();
        }, 1000);
    }
}

