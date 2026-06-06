package windows

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/shirou/gopsutil/v4/process"

	"github.com/sjzar/chatlog/internal/wechat/model"
)

// initializeProcessInfo 获取进程的数据目录和账户名
func initializeProcessInfo(p *process.Process, info *model.Process) error {
	files, err := p.OpenFiles()
	if err != nil {
		log.Warn().Err(err).Msgf("获取进程 %d 的打开文件失败，尝试备用方式推断 DataDir", p.Pid)
		// 备用方式：通过文件系统推断 DataDir
		if tryInferDataDirFromFilesystem(p, info) {
			return nil
		}
		info.AccountName = fmt.Sprintf("未登录微信_%d", p.Pid)
		return nil
	}

	dbPath := V3DBFile
	if info.Version == 4 {
		dbPath = V4DBFile
	}

	for _, f := range files {
		if strings.HasSuffix(f.Path, dbPath) {
			filePath := f.Path
			if strings.HasPrefix(filePath, `\\?\`) && len(filePath) > 4 {
				filePath = filePath[4:] // 移除 "\\?\" 前缀
			}
			parts := strings.Split(filePath, string(filepath.Separator))
			if len(parts) < 4 {
				log.Debug().Msg("无效的文件路径: " + filePath)
				continue
			}

			info.Status = model.StatusOnline
			if info.Version == 4 {
				info.DataDir = strings.Join(parts[:len(parts)-3], string(filepath.Separator))
				info.AccountName = parts[len(parts)-4]
			} else {
				info.DataDir = strings.Join(parts[:len(parts)-2], string(filepath.Separator))
				info.AccountName = parts[len(parts)-3]
			}
			return nil
		}
	}

	// OpenFiles 成功但没找到数据库文件，尝试备用方式
	if tryInferDataDirFromFilesystem(p, info) {
		return nil
	}

	// 如果没有找到数据库文件，进程仍然存在，只是未登录
	info.AccountName = fmt.Sprintf("未登录微信_%d", p.Pid)
	return nil
}

// tryInferDataDirFromFilesystem 通过文件系统推断微信数据目录
// 微信 V4 数据目录通常在: %USERPROFILE%\AppData\Roaming\Tencent\Weixin\{account_name}\db_storage\session\session.db
// 微信 V3 数据目录通常在: %USERPROFILE%\Documents\WeChat Files\{account_name}\Msg\Misc.db
func tryInferDataDirFromFilesystem(p *process.Process, info *model.Process) bool {
	// 获取进程的用户名
	username, err := p.Username()
	if err != nil {
		log.Debug().Err(err).Msgf("获取进程 %d 的用户名失败", p.Pid)
		return false
	}

	// Windows 用户名格式可能是 "DOMAIN\user" 或 "COMPUTERNAME\user"
	parts := strings.Split(username, `\`)
	user := username
	if len(parts) > 1 {
		user = parts[len(parts)-1]
	}

	if info.Version == 4 {
		// V4: 查找 %USERPROFILE%\AppData\Roaming\Tencent\Weixin\ 下的账号目录
		baseDir := filepath.Join("C:", "Users", user, "AppData", "Roaming", "Tencent", "Weixin")
		if dir, accountName := findWeChatDataDir(baseDir, "db_storage", info.Version); dir != "" {
			info.DataDir = dir
			info.AccountName = accountName
			info.Status = model.StatusOnline
			log.Info().Msgf("通过文件系统推断 V4 DataDir: %s, AccountName: %s", dir, accountName)
			return true
		}
	} else {
		// V3: 查找 %USERPROFILE%\Documents\WeChat Files\ 下的账号目录
		baseDir := filepath.Join("C:", "Users", user, "Documents", "WeChat Files")
		if dir, accountName := findWeChatDataDir(baseDir, "Msg", info.Version); dir != "" {
			info.DataDir = dir
			info.AccountName = accountName
			info.Status = model.StatusOnline
			log.Info().Msgf("通过文件系统推断 V3 DataDir: %s, AccountName: %s", dir, accountName)
			return true
		}
	}

	return false
}

// findWeChatDataDir 在基础目录下查找微信数据目录
func findWeChatDataDir(baseDir string, subDir string, version int) (string, string) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		log.Debug().Err(err).Msgf("读取目录 %s 失败", baseDir)
		return "", ""
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		// 跳过 "All Users" 等系统目录
		if name == "All Users" || strings.HasPrefix(name, ".") {
			continue
		}

		// 检查是否存在关键子目录/文件
		candidatePath := filepath.Join(baseDir, name)
		checkPath := filepath.Join(candidatePath, subDir)
		if info, err := os.Stat(checkPath); err == nil && info.IsDir() {
			return candidatePath, name
		}

		// V4: 也检查 db_storage\session\session.db
		if version == 4 {
			sessionDB := filepath.Join(candidatePath, "db_storage", "session", "session.db")
			if _, err := os.Stat(sessionDB); err == nil {
				return candidatePath, name
			}
		}
		// V3: 也检查 Msg\Misc.db
		if version == 3 {
			miscDB := filepath.Join(candidatePath, "Msg", "Misc.db")
			if _, err := os.Stat(miscDB); err == nil {
				return candidatePath, name
			}
		}
	}

	return "", ""
}
