package windows

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog/log"
	"github.com/shirou/gopsutil/v4/process"

	"github.com/sjzar/chatlog/internal/wechat/model"
	"github.com/sjzar/chatlog/pkg/appver"
)

const (
	V3ProcessName  = "WeChat"
	V4ProcessName  = "Weixin"
	V4XProcessName = "WeChatAppEx"
	V3DBFile       = `Msg\Misc.db`
	V4DBFile       = `db_storage\session\session.db`
)

// Detector 实现 Windows 平台的进程检测器
type Detector struct{}

// NewDetector 创建一个新的 Windows 检测器
func NewDetector() *Detector {
	return &Detector{}
}

// FindProcesses 查找所有微信进程并返回它们的信息
func (d *Detector) FindProcesses() ([]*model.Process, error) {
	processes, err := process.Processes()
	if err != nil {
		log.Err(err).Msg("获取进程列表失败")
		return nil, err
	}

	var result []*model.Process
	for _, p := range processes {
		name, err := p.Name()
		name = strings.TrimSuffix(name, ".exe")
		if err != nil || (name != V3ProcessName && name != V4ProcessName && name != V4XProcessName) {
			continue
		}

		// v4 存在同名进程，需要继续判断 cmdline
		// 只过滤 --type= 子进程（renderer/gpu/utility），不过滤主进程
		if name == V4ProcessName {
			cmdline, err := p.Cmdline()
			if err != nil {
				// cmdline 获取失败时，仍然保留该进程（可能是权限问题）
				// 主进程通常没有 --type= 参数，保留它是安全的
				log.Warn().Err(err).Msgf("获取进程 %d 的命令行失败，仍保留该进程", p.Pid)
			} else if strings.Contains(cmdline, "--type=") {
				// --type=renderer, --type=gpu-process, --type=utility 等是子进程，跳过
				continue
			}
		}

		// 获取进程信息
		procInfo, err := d.getProcessInfo(p)
		if err != nil {
			log.Err(err).Msgf("获取进程 %d 的信息失败", p.Pid)
			continue
		}

		// WeChatAppEx 是基于 Chromium 的渲染进程，其文件版本号不是微信版本号，需要强制设为 v4
		if name == V4XProcessName {
			procInfo.Version = 4
			procInfo.IsRenderer = true  // 标记为渲染进程
		}

		result = append(result, procInfo)
	}

	return result, nil
}

// getProcessInfo 获取微信进程的详细信息
func (d *Detector) getProcessInfo(p *process.Process) (*model.Process, error) {
	procInfo := &model.Process{
		PID:      uint32(p.Pid),
		Status:   model.StatusOffline,
		Platform: model.PlatformWindows,
	}

	// 获取可执行文件路径
	exePath, err := p.Exe()
	if err != nil {
		log.Warn().Err(err).Msgf("获取进程 %d 的可执行文件路径失败，尝试通过 Cwd 推断", p.Pid)
		// 尝试通过工作目录推断路径
		cwd, cwdErr := p.Cwd()
		if cwdErr != nil {
			log.Err(cwdErr).Msgf("获取进程 %d 的工作目录也失败，跳过该进程", p.Pid)
			return nil, cwdErr
		}
		// 工作目录通常是微信安装目录
		exePath = filepath.Join(cwd, "Weixin.exe")
		if _, statErr := os.Stat(exePath); statErr != nil {
			exePath = filepath.Join(cwd, "WeChat.exe")
		}
		log.Info().Msgf("通过工作目录推断 exePath: %s", exePath)
	}
	procInfo.ExePath = exePath

	// 获取进程名（不含.exe后缀）
	procName, _ := p.Name()
	procName = strings.TrimSuffix(procName, ".exe")

	// WeChatAppEx 不能独立启动，需要找到 Weixin.exe 作为启动器
	if procName == V4XProcessName {
		if launcher := findWeChatLauncher(exePath); launcher != "" {
			log.Debug().Msgf("WeChatAppEx 检测到启动器: %s", launcher)
			procInfo.ExePath = launcher
		}
	}

	// 获取版本信息
	versionInfo, err := appver.New(exePath)
	if err != nil {
		log.Debug().Err(err).Msg("获取版本信息失败，回退到微信V4")
		procInfo.Version = 4
		procInfo.FullVersion = "4.0.0.0"
	} else {
		procInfo.Version = versionInfo.Version
		procInfo.FullVersion = versionInfo.FullVersion
	}

	// 初始化附加信息（数据目录、账户名）
	if err := initializeProcessInfo(p, procInfo); err != nil {
		log.Err(err).Msg("初始化进程信息失败")
		// 即使初始化失败也返回部分信息
	}

	return procInfo, nil
}

// findWeChatLauncher 在 WeChatAppEx.exe 的上级目录中查找 Weixin.exe 作为启动器
// WeChatAppEx.exe 通常位于 .../WeChat/WeChatAppEx/WeChatAppEx.exe
// 而 Weixin.exe 位于 .../WeChat/Weixin.exe
func findWeChatLauncher(weChatAppExPath string) string {
	dir := filepath.Dir(weChatAppExPath)
	for i := 0; i < 5; i++ {
		candidate := filepath.Join(dir, "Weixin.exe")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}
