package config

import (
	"fmt"
	"os"
)

// BackupPath 返回配置文件的备份路径：与配置文件同级，追加 .bak 后缀。
// 例如 config.yaml -> config.yaml.bak。
func BackupPath(path string) string {
	return path + ".bak"
}

// WriteBackup 将配置内容原子地写入备份文件：先写临时文件再重命名，
// 避免进程中途退出导致备份文件内容损坏。
// 备份路径可通过 BackupPath 获得。
func WriteBackup(path string, data []byte) error {
	dst := BackupPath(path)
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("写入备份文件失败: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("替换备份文件失败: %w", err)
	}
	return nil
}
