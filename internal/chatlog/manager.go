package chatlog

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/sjzar/chatlog/internal/chatlog/conf"
	"github.com/sjzar/chatlog/internal/chatlog/ctx"
	"github.com/sjzar/chatlog/internal/chatlog/database"
	"github.com/sjzar/chatlog/internal/chatlog/http"
	"github.com/sjzar/chatlog/internal/chatlog/wechat"
	"github.com/sjzar/chatlog/internal/model"
	iwechat "github.com/sjzar/chatlog/internal/wechat"
	"github.com/sjzar/chatlog/pkg/config"
	"github.com/sjzar/chatlog/pkg/util"
	"github.com/sjzar/chatlog/pkg/util/dat2img"
)

// Manager 管理聊天日志应用
type Manager struct {
	ctx *ctx.Context
	sc  *conf.ServerConfig
	scm *config.Manager

	// Services
	db     *database.Service
	http   *http.Service
	wechat *wechat.Service

	// Terminal UI
	app *App
}

func New() *Manager {
	return &Manager{}
}

func (m *Manager) Run(configPath string) error {

	var err error
	m.ctx, err = ctx.New(configPath)
	if err != nil {
		return err
	}

	m.wechat = wechat.NewService(m.ctx)

	m.db = database.NewService(m.ctx)

	m.http = http.NewService(m.ctx, m.db)

	m.ctx.WeChatInstances = m.wechat.GetWeChatInstances()
	if len(m.ctx.WeChatInstances) >= 1 {
		m.ctx.SwitchCurrent(m.ctx.WeChatInstances[0])
	}

	if m.ctx.HTTPEnabled {
		// 启动HTTP服务
		if err := m.StartService(); err != nil {
			m.StopService()
		}
	}
	// 启动终端UI
	m.app = NewApp(m.ctx, m)
	m.app.Run() // 阻塞
	return nil
}

func (m *Manager) Switch(info *iwechat.Account, history string) error {
	if m.ctx.HTTPEnabled {
		if err := m.stopService(); err != nil {
			return err
		}
	}
	if info != nil {
		m.ctx.SwitchCurrent(info)
	} else {
		m.ctx.SwitchHistory(history)
	}

	if m.ctx.HTTPEnabled {
		// 启动HTTP服务
		if err := m.StartService(); err != nil {
			log.Info().Err(err).Msg("启动服务失败")
			m.StopService()
		}
	}
	return nil
}

func (m *Manager) StartService() error {

	// 按依赖顺序启动服务
	if err := m.db.Start(); err != nil {
		return err
	}

	if err := m.http.Start(); err != nil {
		m.db.Stop()
		return err
	}

	// 如果是 4.0 版本，更新下 xorkey
	if m.ctx.Version == 4 {
		dat2img.SetAesKey(m.ctx.ImgKey)
		go dat2img.ScanAndSetXorKey(m.ctx.DataDir)
	}

	// 更新状态
	m.ctx.SetHTTPEnabled(true)

	return nil
}

func (m *Manager) StopService() error {
	if err := m.stopService(); err != nil {
		return err
	}

	// 更新状态
	m.ctx.SetHTTPEnabled(false)

	return nil
}

func (m *Manager) stopService() error {
	// 按依赖的反序停止服务
	var errs []error

	if err := m.http.Stop(); err != nil {
		errs = append(errs, err)
	}

	if err := m.db.Stop(); err != nil {
		errs = append(errs, err)
	}

	// 如果有错误，返回第一个错误
	if len(errs) > 0 {
		return errs[0]
	}

	return nil
}

func (m *Manager) SetHTTPAddr(text string) error {
	var addr string
	if util.IsNumeric(text) {
		addr = fmt.Sprintf("127.0.0.1:%s", text)
	} else if strings.HasPrefix(text, "http://") {
		addr = strings.TrimPrefix(text, "http://")
	} else if strings.HasPrefix(text, "https://") {
		addr = strings.TrimPrefix(text, "https://")
	} else {
		addr = text
	}
	m.ctx.SetHTTPAddr(addr)
	return nil
}

func (m *Manager) GetDataKey() error {
	if m.ctx.Current == nil {
		return fmt.Errorf("未选择任何账号")
	}
	if _, err := m.wechat.GetDataKey(m.ctx.Current); err != nil {
		return err
	}
	m.ctx.Refresh()
	m.ctx.UpdateConfig()
	return nil
}

func (m *Manager) GetImageKey() error {
	if m.ctx.Current == nil {
		return fmt.Errorf("未选择任何账号")
	}
	imgKey, err := m.wechat.GetImageKey(m.ctx.Current)
	if err != nil {
		return err
	}
	if imgKey != "" {
		m.ctx.ImgKey = imgKey
		if m.ctx.Current != nil {
			m.ctx.Current.ImgKey = imgKey
		}
		// Keep runtime decoder in sync immediately (no need to restart HTTP service).
		dat2img.SetAesKey(imgKey)
		if m.ctx.DataDir != "" {
			go dat2img.ScanAndSetXorKey(m.ctx.DataDir)
		}
	}
	m.ctx.Refresh()
	m.ctx.UpdateConfig()
	return nil
}

func (m *Manager) RestartAndGetDataKey(onStatus func(string)) error {
	if m.ctx.Current == nil {
		return fmt.Errorf("未选择任何账号")
	}

	// ===== 阶段1: 获取当前微信进程信息 =====
	instances := m.wechat.GetWeChatInstances()
	m.ctx.WeChatInstances = instances

	// 选择最佳实例（优先主进程）
	if best := pickBestWeChatInstance(instances, m.ctx.Current.ExePath, m.ctx.Current.Platform); best != nil {
		m.ctx.SwitchCurrent(best)
	} else if len(instances) > 0 {
		m.ctx.SwitchCurrent(instances[0])
	}

	pid := m.ctx.Current.PID
	if pid == 0 {
		return fmt.Errorf("微信进程未运行，请先启动微信后再操作")
	}

	// 保存 exePath（终止进程前保存，防止丢失）
	exePath := m.ctx.Current.ExePath
	if exePath == "" {
		// 从进程列表中查找
		for _, inst := range instances {
			if inst.ExePath != "" && !inst.IsRenderer {
				exePath = inst.ExePath
				break
			}
		}
	}
	log.Info().Msgf("[重启获取密钥] 当前进程 PID=%d, exePath=%s", pid, exePath)

	// ===== 阶段2: 终止所有微信进程 =====
	if onStatus != nil {
		onStatus("正在结束微信进程...")
	}
	// 使用 taskkill 强制终止所有微信相关进程
	for _, procName := range []string{"Weixin.exe", "WeChatAppEx.exe", "WeChat.exe"} {
		exec.Command("taskkill", "/F", "/IM", procName).CombinedOutput()
	}
	log.Info().Msg("[重启获取密钥] taskkill 已执行")

	// 等待所有微信进程消失（最多15秒）
	for i := 0; i < 15; i++ {
		instances = m.wechat.GetWeChatInstances()
		if len(instances) == 0 {
			break
		}
		log.Info().Msgf("[重启获取密钥] 等待进程退出... 还有 %d 个进程", len(instances))
		time.Sleep(1 * time.Second)
	}

	// ===== 阶段3: 启动微信 =====
	if onStatus != nil {
		onStatus("正在启动微信...")
	}
	wechatStarted := false
	if exePath != "" {
		if _, err := os.Stat(exePath); err == nil {
			wechatStarted = startWeChatOnWindows(exePath)
		} else {
			log.Warn().Err(err).Msgf("[重启获取密钥] exePath 不存在: %s", exePath)
		}
	} else {
		log.Warn().Msg("[重启获取密钥] exePath 为空，无法自动启动")
	}
	if !wechatStarted {
		log.Warn().Msg("[重启获取密钥] 自动启动失败，等待用户手动启动")
		if onStatus != nil {
			onStatus("请手动启动微信并登录...")
		}
	} else {
		// 启动后等待2秒，给微信进程初始化时间
		time.Sleep(2 * time.Second)
	}

	// ===== 阶段4: 等待微信进程出现并登录 =====
	if onStatus != nil {
		onStatus("正在等待微信启动并登录...")
	}
	deadline := time.Now().Add(120 * time.Second)
	loopCount := 0
	for {
		loopCount++
		instances = m.wechat.GetWeChatInstances()
		if len(instances) > 0 {
			log.Info().Msgf("[重启获取密钥] 检测到 %d 个微信进程", len(instances))
			// 优先选择主进程且已登录的实例
			var bestInstance *iwechat.Account
			for _, inst := range instances {
				if !inst.IsRenderer && inst.DataDir != "" {
					bestInstance = inst
					break
				}
			}
			// 没有已登录的主进程，选已登录的渲染进程
			if bestInstance == nil {
				for _, inst := range instances {
					if inst.DataDir != "" {
						bestInstance = inst
						break
					}
				}
			}
			// 没有已登录的实例，选主进程
			if bestInstance == nil {
				for _, inst := range instances {
					if !inst.IsRenderer {
						bestInstance = inst
						break
					}
				}
			}
			// 最后选任意实例
			if bestInstance == nil {
				bestInstance = instances[0]
			}

			m.ctx.SwitchCurrent(bestInstance)

			if bestInstance.DataDir != "" {
				log.Info().Msgf("[重启获取密钥] 微信已登录, PID=%d, DataDir=%s", bestInstance.PID, bestInstance.DataDir)
				break
			}

			if onStatus != nil {
				onStatus("请扫码登录微信...")
			}
		} else {
			if loopCount%5 == 1 {
				log.Info().Msg("[重启获取密钥] 未检测到微信进程，继续等待...")
			}
			if onStatus != nil {
				onStatus("请启动微信并登录...")
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("获取密钥超时: 微信启动或登录未完成，请重试")
		}
		time.Sleep(1 * time.Second)
	}

	// ===== 阶段5: 获取密钥 =====
	if onStatus != nil {
		onStatus("正在扫描并验证密钥...")
	}
	log.Info().Msg("[重启获取密钥] 开始获取密钥...")

	ctx := context.WithValue(context.Background(), "status_callback", onStatus)
	ctx = context.WithValue(ctx, "force_key_refresh", true)
	ctx = context.WithValue(ctx, "force_rescan_memory", true)

	keyDeadline := time.Now().Add(60 * time.Second) // 密钥获取最多60秒
	var key, imgKey string
	var err error
	for {
		key, imgKey, err = m.ctx.Current.GetKey(ctx)
		if err == nil {
			break
		}

		err = normalizeKeyAcquireError(err)
		if !isRetryableKeyErr(err) {
			return err
		}

		if time.Now().After(keyDeadline) {
			return fmt.Errorf("获取密钥超时: %v", err)
		}

		log.Debug().Err(err).Msg("获取密钥尝试失败，准备重试")
		if onStatus != nil {
			onStatus("正在重试获取密钥...")
		}
		time.Sleep(1 * time.Second)
	}

	m.ctx.DataKey = key
	m.ctx.ImgKey = imgKey
	if imgKey != "" {
		dat2img.SetAesKey(imgKey)
	}
	if m.ctx.DataDir != "" {
		go dat2img.ScanAndSetXorKey(m.ctx.DataDir)
	}
	m.ctx.Refresh()
	m.ctx.UpdateConfig()

	log.Info().Msg("[重启获取密钥] 成功获取密钥")
	return nil
}

// startWeChatOnWindows 尝试多种方式在 Windows 上启动微信
func startWeChatOnWindows(exePath string) bool {
	// 方法1: PowerShell Start-Process（最可靠，完全脱离父进程）
	cmd1 := exec.Command("powershell", "-Command", "Start-Process", "-FilePath", exePath)
	if err := cmd1.Run(); err == nil {
		log.Info().Msg("[启动微信] 方法1 PowerShell 启动成功")
		return true
	}

	// 方法2: cmd start
	cmd2 := exec.Command("cmd", "/c", "start", "", exePath)
	cmd2.Dir = filepath.Dir(exePath)
	if err := cmd2.Run(); err == nil {
		log.Info().Msg("[启动微信] 方法2 cmd start 启动成功")
		return true
	}

	// 方法3: 直接启动（最后手段，进程可能随父进程退出）
	cmd3 := exec.Command(exePath)
	cmd3.Dir = filepath.Dir(exePath)
	if err := cmd3.Start(); err == nil {
		log.Info().Msg("[启动微信] 方法3 直接启动成功")
		return true
	}

	log.Warn().Msg("[启动微信] 所有自动启动方式均失败")
	return false
}

func normalizeKeyAcquireError(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "scan memory failed") || strings.Contains(msg, "task_for_pid") || strings.Contains(msg, "code=-2") {
		return fmt.Errorf("获取密钥失败：进程内存读取权限不足，请以管理员权限运行本程序。原始错误: %w", err)
	}
	return err
}

func isRetryableKeyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "数据目录未就绪"):
		return true
	case strings.Contains(msg, "wechat process not found"):
		return true
	case strings.Contains(msg, "初始化"):
		return true
	case strings.Contains(msg, "未找到可用的 message_0.db data key"):
		return true
	case strings.Contains(msg, "未找到 all_keys.json"):
		return true
	case strings.Contains(msg, "all_keys.json 为空"):
		return true
	case strings.Contains(msg, "内存扫描未发现候选 key/salt"):
		return true
	case strings.Contains(msg, "扫描到候选 key，但未匹配到任意数据库 salt"):
		return true
	case strings.Contains(msg, "未命中"):
		return true
	default:
		return false
	}
}

func pickBestWeChatInstance(instances []*iwechat.Account, exePath, platform string) *iwechat.Account {
	var best *iwechat.Account
	var mainProcess *iwechat.Account  // 主进程（Weixin.exe 或 WeChat.exe）
	var rendererProcess *iwechat.Account  // 渲染进程（WeChatAppEx.exe）

	for _, inst := range instances {
		if inst == nil {
			continue
		}
		if platform != "" && inst.Platform != platform {
			continue
		}
		if exePath != "" && inst.ExePath != exePath {
			continue
		}

		// 区分主进程和渲染进程
		// WeChatAppEx.exe 是渲染进程，Weixin.exe 是主进程
		if inst.IsRenderer {
			// 渲染进程：优先有 DataDir 的；同等条件下选 PID 更大的
			if rendererProcess == nil {
				rendererProcess = inst
				continue
			}
			rendererHasData := rendererProcess.DataDir != ""
			curHasData := inst.DataDir != ""
			if curHasData && !rendererHasData {
				rendererProcess = inst
				continue
			}
			if curHasData == rendererHasData && inst.PID > rendererProcess.PID {
				rendererProcess = inst
			}
		} else {
			// 主进程：优先有 DataDir 的；同等条件下选 PID 更大的
			if mainProcess == nil {
				mainProcess = inst
				continue
			}
			mainHasData := mainProcess.DataDir != ""
			curHasData := inst.DataDir != ""
			if curHasData && !mainHasData {
				mainProcess = inst
				continue
			}
			if curHasData == mainHasData && inst.PID > mainProcess.PID {
				mainProcess = inst
			}
		}
	}

	// 优先选择主进程（Weixin.exe），如果没有主进程则选择渲染进程
	if mainProcess != nil {
		best = mainProcess
	} else if rendererProcess != nil {
		best = rendererProcess
	}

	return best
}

func (m *Manager) DecryptDBFiles() error {
	if m.ctx.DataKey == "" {
		if m.ctx.Current == nil {
			return fmt.Errorf("未选择任何账号")
		}
		if err := m.GetDataKey(); err != nil {
			return err
		}
	}
	if m.ctx.WorkDir == "" {
		m.ctx.WorkDir = util.DefaultWorkDir(m.ctx.Account)
	}

	if err := m.wechat.DecryptDBFiles(); err != nil {
		return err
	}
	m.ctx.Refresh()
	m.ctx.UpdateConfig()
	return nil
}

func (m *Manager) StartAutoDecrypt() error {
	if m.ctx.DataKey == "" || m.ctx.DataDir == "" {
		return fmt.Errorf("请先获取密钥")
	}

	// 尝试运行一次解密，验证环境和密钥是否正常
	// 如果解密失败，说明配置或环境有问题，不应开启自动解密
	if err := m.DecryptDBFiles(); err != nil {
		return fmt.Errorf("初始解密失败，无法开启自动解密: %w", err)
	}

	if m.ctx.WorkDir == "" {
		return fmt.Errorf("请先执行解密数据")
	}

	m.wechat.SetAutoDecryptErrorHandler(func(err error) {
		log.Error().Err(err).Msg("自动解密失败，停止服务")
		m.StopAutoDecrypt()

		if m.app != nil {
			m.app.QueueUpdateDraw(func() {
				m.app.showError(fmt.Errorf("自动解密失败，已停止服务: %v", err))
				m.app.updateMenuItemsState()
			})
		}
	})

	if err := m.wechat.StartAutoDecrypt(); err != nil {
		return err
	}

	m.ctx.SetAutoDecrypt(true)
	return nil
}

func (m *Manager) StopAutoDecrypt() error {
	if err := m.wechat.StopAutoDecrypt(); err != nil {
		return err
	}

	m.ctx.SetAutoDecrypt(false)
	return nil
}

func (m *Manager) RefreshSession() error {
	if m.db.GetDB() == nil {
		if err := m.db.Start(); err != nil {
			return err
		}
	}
	resp, err := m.db.GetSessions("", 1, 0)
	if err != nil {
		return err
	}
	if len(resp.Items) == 0 {
		return nil
	}
	m.ctx.LastSession = resp.Items[0].NTime
	return nil
}

func (m *Manager) GetLatestSession() (*model.Session, error) {
	if m.db == nil || m.db.GetDB() == nil {
		return nil, nil
	}
	resp, err := m.db.GetSessions("", 1, 0)
	if err != nil {
		return nil, err
	}
	if len(resp.Items) > 0 {
		return resp.Items[0], nil
	}
	return nil, nil
}

func (m *Manager) CommandKey(configPath string, pid int, force bool, showXorKey bool) (string, error) {

	var err error
	m.ctx, err = ctx.New(configPath)
	if err != nil {
		return "", err
	}

	m.wechat = wechat.NewService(m.ctx)

	m.ctx.WeChatInstances = m.wechat.GetWeChatInstances()
	if len(m.ctx.WeChatInstances) == 0 {
		return "", fmt.Errorf("wechat process not found")
	}

	if len(m.ctx.WeChatInstances) == 1 {
		// 确保当前账户已设置
		if m.ctx.Current == nil {
			m.ctx.SwitchCurrent(m.ctx.WeChatInstances[0])
		}

		key, imgKey := m.ctx.DataKey, m.ctx.ImgKey
		if len(key) == 0 || len(imgKey) == 0 || force {
			key, imgKey, err = m.ctx.WeChatInstances[0].GetKey(context.Background())
			if err != nil {
				return "", err
			}
			m.ctx.Refresh()
			m.ctx.UpdateConfig()
		}

		result := fmt.Sprintf("Data Key: [%s]\nImage Key: [%s]", key, imgKey)
		if m.ctx.Version == 4 && showXorKey {
			if b, err := dat2img.ScanAndSetXorKey(m.ctx.DataDir); err == nil {
				result += fmt.Sprintf("\nXor Key: [0x%X]", b)
			}
		}

		return result, nil
	}
	if pid == 0 {
		str := "Select a process:\n"
		for _, ins := range m.ctx.WeChatInstances {
			str += fmt.Sprintf("PID: %d. %s[Version: %s Data Dir: %s ]\n", ins.PID, ins.Name, ins.FullVersion, ins.DataDir)
		}
		return str, nil
	}
	for _, ins := range m.ctx.WeChatInstances {
		if ins.PID == uint32(pid) {
			// 确保当前账户已设置
			if m.ctx.Current == nil || m.ctx.Current.PID != ins.PID {
				m.ctx.SwitchCurrent(ins)
			}

			key, imgKey := ins.Key, ins.ImgKey
			if len(key) == 0 || len(imgKey) == 0 || force {
				key, imgKey, err = ins.GetKey(context.Background())
				if err != nil {
					return "", err
				}
				m.ctx.Refresh()
				m.ctx.UpdateConfig()
			}
			result := fmt.Sprintf("Data Key: [%s]\nImage Key: [%s]", key, imgKey)
			if m.ctx.Version == 4 && showXorKey {
				if b, err := dat2img.ScanAndSetXorKey(m.ctx.DataDir); err == nil {
					result += fmt.Sprintf("\nXor Key: [0x%X]", b)
				}
			}
			return result, nil
		}
	}
	return "", fmt.Errorf("wechat process not found")
}

func (m *Manager) CommandDecrypt(configPath string, cmdConf map[string]any) error {

	var err error
	m.sc, m.scm, err = conf.LoadServiceConfig(configPath, cmdConf)
	if err != nil {
		return err
	}

	dataDir := m.sc.GetDataDir()
	if len(dataDir) == 0 {
		return fmt.Errorf("dataDir is required")
	}

	dataKey := m.sc.GetDataKey()
	if len(dataKey) == 0 {
		return fmt.Errorf("dataKey is required")
	}

	m.wechat = wechat.NewService(m.sc)

	if err := m.wechat.DecryptDBFiles(); err != nil {
		return err
	}

	return nil
}

func (m *Manager) CommandHTTPServer(configPath string, cmdConf map[string]any) error {

	var err error
	m.sc, m.scm, err = conf.LoadServiceConfig(configPath, cmdConf)
	if err != nil {
		return err
	}

	dataDir := m.sc.GetDataDir()
	workDir := m.sc.GetWorkDir()
	if len(dataDir) == 0 && len(workDir) == 0 {
		return fmt.Errorf("dataDir or workDir is required")
	}

	dataKey := m.sc.GetDataKey()
	if len(dataKey) == 0 {
		return fmt.Errorf("dataKey is required")
	}

	// 如果是 4.0 版本，处理图片密钥
	version := m.sc.GetVersion()
	if version == 4 && len(dataDir) != 0 {
		dat2img.SetAesKey(m.sc.GetImgKey())
		go dat2img.ScanAndSetXorKey(dataDir)
	}

	log.Info().Msgf("server config: %+v", m.sc)

	m.wechat = wechat.NewService(m.sc)

	m.db = database.NewService(m.sc)

	m.http = http.NewService(m.sc, m.db)

	// init db
	go func() {
		// 如果工作目录为空，则解密数据
		if entries, err := os.ReadDir(workDir); err == nil && len(entries) == 0 {
			log.Info().Msgf("work dir is empty, decrypt data.")
			m.db.SetDecrypting()
			if err := m.wechat.DecryptDBFiles(); err != nil {
				log.Info().Msgf("decrypt data failed: %v", err)
				return
			}
			log.Info().Msg("decrypt data success")
		}

		// 按依赖顺序启动服务
		if err := m.db.Start(); err != nil {
			log.Info().Msgf("start db failed, try to decrypt data.")
			m.db.SetDecrypting()
			if err := m.wechat.DecryptDBFiles(); err != nil {
				log.Info().Msgf("decrypt data failed: %v", err)
				return
			}
			log.Info().Msg("decrypt data success")
			if err := m.db.Start(); err != nil {
				log.Info().Msgf("start db failed: %v", err)
				m.db.SetError(err.Error())
				return
			}
		}
	}()

	return m.http.ListenAndServe()
}
