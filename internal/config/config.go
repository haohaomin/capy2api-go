// Package config 负责 capy2api 的 JSON 配置：默认值、加载、保存与环境变量覆盖。
package config

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Workspace 描述在 Capy 设备上执行线程时使用的工作目录策略。
//
//	mode=worktree：每个请求在 root 下新建 jam_xxx 目录（Capy 桌面端默认做法）
//	mode=linked：所有请求复用同一个已存在的目录（省去建目录开销）
type Workspace struct {
	Mode string `json:"mode"`
	Root string `json:"root"`
}

// Capy 是上游协议与执行行为相关的配置。
type Capy struct {
	APIBase        string `json:"api_base"`
	ClerkBase      string `json:"clerk_base"`
	ClerkJSVersion string `json:"clerk_js_version"`
	Features       string `json:"client_features"`
	Surface        string `json:"client_surface"`
	Timezone       string `json:"client_timezone"`
	ClientID       string `json:"client_id"`

	// Mode 决定线程执行位置：
	//   thread = 复用已有设备线程派生子线程（无需桌面端、无需云机器额度，默认）
	//   web    = 项目 + 云机器（无需桌面端，消耗 Capy 额度）
	//   device = 设备上的 Capy 桌面端执行（桌面端必须在线）
	Mode           string    `json:"mode"`
	TemplateJam    string    `json:"template_jam"`
	ProjectID      string    `json:"project_id"`
	Workspace      Workspace `json:"workspace"`
	Reasoning      string    `json:"reasoning"`
	TurnTimeoutSec int       `json:"turn_timeout_sec"`
	PollIntervalMS int       `json:"poll_interval_ms"`
	DeleteAfter    bool      `json:"delete_after"`
	Realtime       bool      `json:"realtime"`
	// 会话复用：同一会话的第二轮起复用同一条上游线程（缓存/上下文都留在上游）。
	ReuseConversation bool `json:"reuse_conversation"`
	// ThreadMode: root=根线程（隔离，创建时设备需在线）; side=子线程（无需设备，但会继承父线程上下文）
	ThreadMode string `json:"thread_mode"`
	PoolSize   int    `json:"pool_size"`
	// MemoryGuard 在提示词里加一条约束：不要把对话写进长期记忆/AGENTS.md。
	MemoryGuard bool `json:"memory_guard"`
	// IncludeReasoning 为 true 时把思考内容放进 message.reasoning_content（DeepSeek 风格客户端会读）。
	IncludeReasoning bool `json:"include_reasoning"`
	// DeviceDaemon 用设备凭据自建「假设备」直连隧道，实现零 App 执行。
	DeviceDaemon    *bool  `json:"device_daemon"`
	DeviceWorkspace string `json:"device_workspace"`
	DeviceToken     string `json:"device_token"`
	DeviceID        string `json:"device_id"`
	BashAllowAll    bool   `json:"bash_allow_all"`
	DevicePolicy    string `json:"device_policy"`
	MaxToolSteps    int    `json:"max_tool_steps"`
	QueueTimeoutSec int    `json:"queue_timeout_sec"`
	// 线程空闲多久后回收（秒）。
	ConversationTTLSec int      `json:"conversation_ttl_sec"`
	MaxConcurrent      int      `json:"max_concurrent"`
	Typewriter         bool     `json:"typewriter"`
	AllowedModels      []string `json:"allowed_models"`
	PromptPrefix       string   `json:"prompt_prefix"`
}

// Panel 是 Web 管理面板的展示参数。
type Panel struct {
	RefreshSec int `json:"refresh_sec"`
}

// Config 顶层配置。
type Config struct {
	Listen  string `json:"listen"`
	APIKey  string `json:"api_key"`
	AuthDir string `json:"auth_dir"`
	Capy    Capy   `json:"capy"`
	Panel   Panel  `json:"panel"`

	path string
}

// DefaultPath 返回默认配置文件路径（当前目录 config.json）。
func DefaultPath() string {
	if p := strings.TrimSpace(os.Getenv("CAPY2API_CONFIG")); p != "" {
		return p
	}
	return "config.json"
}

// Default 生成一份可直接运行的默认配置。
func Default() *Config {
	home, _ := os.UserHomeDir()
	root := filepath.Join(home, ".capy", "worktrees")
	return &Config{
		Listen:  "127.0.0.1:7864",
		APIKey:  randomKey(),
		AuthDir: "auths",
		Capy: Capy{
			APIBase:            "https://api.capy.ai",
			ClerkBase:          "https://clerk.capy.ai",
			ClerkJSVersion:     "6.43.0",
			Features:           "message-edit,visual,ads-models",
			Surface:            "web",
			Timezone:           "Asia/Shanghai",
			Mode:               "thread",
			TemplateJam:        "",
			ProjectID:          "",
			Workspace:          Workspace{Mode: "worktree", Root: root},
			Reasoning:          "",
			TurnTimeoutSec:     360,
			PollIntervalMS:     700,
			DeleteAfter:        true,
			Realtime:           true,
			ReuseConversation:  true,
			ConversationTTLSec: 1800,
			MaxConcurrent:      2,
			Typewriter:         true,
		},
		Panel: Panel{RefreshSec: 5},
	}
}

// DeviceDaemonOn 伪造设备默认开启（显式 false 才关闭）。
func (c *Config) DeviceDaemonOn() bool {
	return c.Capy.DeviceDaemon == nil || *c.Capy.DeviceDaemon
}

// Path 返回当前配置来源路径。
func (c *Config) Path() string { return c.path }

// Load 读取配置文件；不存在时写入默认配置并返回。
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		c := Default()
		c.path = abs
		if err := c.Save(); err != nil {
			return nil, fmt.Errorf("写入默认配置失败: %w", err)
		}
		return c, nil
	}
	c := Default()
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", abs, err)
	}
	c.path = abs
	c.normalize()
	c.applyEnv()
	c.normalize()
	if c.APIKey == "" {
		c.APIKey = randomKey()
		if err := c.Save(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *Config) normalize() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:7864"
	}
	if c.AuthDir == "" {
		c.AuthDir = "auths"
	}
	d := Default()
	if c.Capy.APIBase == "" {
		c.Capy.APIBase = d.Capy.APIBase
	}
	if c.Capy.ClerkBase == "" {
		c.Capy.ClerkBase = d.Capy.ClerkBase
	}
	if c.Capy.ClerkJSVersion == "" {
		c.Capy.ClerkJSVersion = d.Capy.ClerkJSVersion
	}
	if c.Capy.Features == "" {
		c.Capy.Features = d.Capy.Features
	}
	if c.Capy.Surface == "" {
		c.Capy.Surface = d.Capy.Surface
	}
	if c.Capy.Timezone == "" {
		c.Capy.Timezone = d.Capy.Timezone
	}
	if c.Capy.Mode == "" {
		c.Capy.Mode = "thread"
	}
	c.Capy.Mode = strings.ToLower(c.Capy.Mode)
	switch c.Capy.Mode {
	case "thread", "web", "device":
	default:
		c.Capy.Mode = "thread"
	}
	if c.Capy.Workspace.Mode == "" {
		c.Capy.Workspace.Mode = "worktree"
	}
	if c.Capy.Workspace.Root == "" {
		c.Capy.Workspace.Root = d.Capy.Workspace.Root
	}
	if c.Capy.Workspace.Root != "" {
		if expanded, err := ExpandHome(c.Capy.Workspace.Root); err == nil {
			c.Capy.Workspace.Root = expanded
		}
	}
	if c.Capy.TurnTimeoutSec <= 0 {
		c.Capy.TurnTimeoutSec = 240
	}
	if c.Capy.PollIntervalMS < 200 {
		c.Capy.PollIntervalMS = 700
	}
	if c.Capy.ThreadMode == "" {
		c.Capy.ThreadMode = "root"
	}
	c.Capy.ThreadMode = strings.ToLower(c.Capy.ThreadMode)
	if c.Capy.ThreadMode != "root" && c.Capy.ThreadMode != "side" {
		c.Capy.ThreadMode = "root"
	}
	if c.Capy.PoolSize <= 0 {
		c.Capy.PoolSize = 2
	}
	if c.Capy.QueueTimeoutSec <= 0 {
		c.Capy.QueueTimeoutSec = 30
	}
	if c.Capy.MaxToolSteps <= 0 {
		c.Capy.MaxToolSteps = 12
	}
	if c.Capy.DevicePolicy == "" {
		c.Capy.DevicePolicy = "text"
	}
	if c.Capy.BashAllowAll {
		c.Capy.DevicePolicy = "all"
	}
	if c.Capy.DeviceWorkspace == "" {
		home, _ := os.UserHomeDir()
		c.Capy.DeviceWorkspace = filepath.Join(home, "capy2api-workspace")
	}
	if c.Capy.ConversationTTLSec <= 0 {
		c.Capy.ConversationTTLSec = 1800
	}
	if c.Capy.MaxConcurrent <= 0 {
		c.Capy.MaxConcurrent = 2
	}
	if c.Panel.RefreshSec <= 0 {
		c.Panel.RefreshSec = 5
	}
	c.Capy.APIBase = strings.TrimRight(c.Capy.APIBase, "/")
	c.Capy.ClerkBase = strings.TrimRight(c.Capy.ClerkBase, "/")
}

func (c *Config) applyEnv() {
	if v := strings.TrimSpace(os.Getenv("CAPY2API_LISTEN")); v != "" {
		c.Listen = v
	}
	if v := strings.TrimSpace(os.Getenv("CAPY2API_KEY")); v != "" {
		c.APIKey = v
	}
	if v := strings.TrimSpace(os.Getenv("CAPY2API_AUTHDIR")); v != "" {
		c.AuthDir = v
	}
	if v := strings.TrimSpace(os.Getenv("CAPY2API_API_BASE")); v != "" {
		c.Capy.APIBase = v
	}
	if v := strings.TrimSpace(os.Getenv("CAPY2API_CLERK_BASE")); v != "" {
		c.Capy.ClerkBase = v
	}
	if v := strings.TrimSpace(os.Getenv("CAPY2API_WORKSPACE_ROOT")); v != "" {
		c.Capy.Workspace.Root = v
	}
	if v := strings.TrimSpace(os.Getenv("CAPY2API_MODE")); v != "" {
		c.Capy.Mode = v
	}
	if v := strings.TrimSpace(os.Getenv("CAPY2API_TEMPLATE_JAM")); v != "" {
		c.Capy.TemplateJam = v
	}
	if v := strings.TrimSpace(os.Getenv("CAPY2API_PROJECT_ID")); v != "" {
		c.Capy.ProjectID = v
	}
	if v := strings.TrimSpace(os.Getenv("CAPY2API_REASONING")); v != "" {
		c.Capy.Reasoning = v
	}
	if v := strings.TrimSpace(os.Getenv("CAPY2API_CONCURRENCY")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Capy.MaxConcurrent = n
		}
	}
}

// Save 原子写入配置文件（0600）。
func (c *Config) Save() error {
	if c.path == "" {
		c.path = DefaultPath()
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// ExpandHome 展开路径开头的 ~。
func ExpandHome(p string) (string, error) {
	if !strings.HasPrefix(p, "~") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p, err
	}
	if p == "~" {
		return home, nil
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:]), nil
	}
	return p, nil
}

func randomKey() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "capy2api-dev-key"
	}
	return "sk-capy-" + base64.RawURLEncoding.EncodeToString(buf)
}
