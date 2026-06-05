package model

type Process struct {
	PID         uint32
	ExePath     string
	Platform    string
	Version     int
	FullVersion string
	Status      string
	DataDir     string
	AccountName string
	IsRenderer  bool  // 是否是渲染进程（WeChatAppEx.exe）
}

// 平台常量定义
const (
	PlatformDarwin  = "darwin"
	PlatformWindows = "windows"
)

const (
	StatusInit    = ""
	StatusOffline = "offline"
	StatusOnline  = "online"
)
