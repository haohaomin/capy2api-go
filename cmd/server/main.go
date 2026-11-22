// capy2api：把 Capy 的广告赞助模型反代成 OpenAI / Anthropic 兼容接口。
//
// 命令行：
//
//	capy2api                 # 启动网关（默认命令，等价 serve）
//	capy2api serve -config ./config.json
//	capy2api login --cookie -  # 用网页会话登录（不需要 Capy 桌面端）
//	capy2api import           # macOS：从本机 Capy.app 导入登录态与设备
//	capy2api accounts         # 列账号
//	capy2api models           # 列可用免费模型
//	capy2api version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/haohaomin/capy2api/internal/capy"
	"github.com/haohaomin/capy2api/internal/capyapp"
	"github.com/haohaomin/capy2api/internal/config"
	"github.com/haohaomin/capy2api/internal/devicedaemon"
	"github.com/haohaomin/capy2api/internal/server"
	"github.com/haohaomin/capy2api/internal/store"
)

func main() {
	args := os.Args[1:]
	if len(args) > 0 {
		switch args[0] {
		case "serve", "server", "run":
			runServe(args[1:])
			return
		case "login":
			runLogin(args[1:])
			return
		case "import":
			runImport(args[1:])
			return
		case "accounts":
			runAccounts(args[1:])
			return
		case "models":
			runModels(args[1:])
			return
		case "threads":
			runThreads(args[1:])
			return
		case "version", "-version", "--version":
			fmt.Println(server.Version)
			return
		case "help", "-h", "--help":
			usage()
			return
		default:
			if !strings.HasPrefix(args[0], "-") {
				fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", args[0])
				usage()
				os.Exit(2)
			}
		}
	}
	runServe(args)
}

func usage() {
	fmt.Print(`capy2api · Capy → OpenAI / Anthropic 网关

用法:
  capy2api [serve] [-config 路径]    启动网关（默认 127.0.0.1:7864）
  capy2api login --cookie -          用 __client cookie 登录（不需要桌面端）
      --session <sess_id>  --org <org_id>  --name <名字>  --mode web|device
  capy2api import                    macOS：从 Capy.app 读取会话 + 设备 id
  capy2api accounts                  列出账号
  capy2api models                    列出当前账号的免费广告模型
  capy2api threads [--set jam_xxx]   列出线程 / 指定 thread 模式的模板线程
  capy2api version

环境变量: CAPY2API_CONFIG / CAPY2API_LISTEN / CAPY2API_KEY / CAPY2API_AUTHDIR
          CAPY2API_API_BASE / CAPY2API_CLERK_BASE / CAPY2API_MODE
`)
}

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "", "配置文件路径（默认 ./config.json）")
	showVer := fs.Bool("version", false, "打印版本后退出")
	_ = fs.Parse(args)
	if *showVer {
		fmt.Println(server.Version)
		return
	}
	log.SetFlags(log.LstdFlags)
	log.SetPrefix("[capy2api] ")

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	st, err := store.Open(cfg.AuthDir)
	if err != nil {
		log.Fatalf("加载账号池失败: %v", err)
	}
	keys, err := store.OpenKeys(cfg.AuthDir)
	if err != nil {
		log.Fatalf("加载 API key 表失败: %v", err)
	}
	reqLog := store.NewLog(300)
	srv := server.New(cfg, st, keys, reqLog)

	addr := cfg.Listen
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("监听 %s 失败: %v", addr, err)
	}

	banner(cfg, st.Count(), ln.Addr().String())
	checkTemplate(cfg, st)
	if warn := insecureWarning(addr, cfg.APIKey); warn != "" {
		log.Printf("警告：%s", warn)
	}

	httpServer := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	// 伪造设备：优先用账号自带的设备凭据（浏览器登录时自动注册），否则回退读取本机 Capy.app。
	srv.EnsureDaemonFromStore()
	if cfg.DeviceDaemonOn() && srv.HasDaemon() == false {
		token, deviceID := cfg.Capy.DeviceToken, cfg.Capy.DeviceID
		if token == "" || deviceID == "" {
			if det, derr := capyapp.Detect(context.Background()); derr == nil && len(det.Orgs) > 0 {
				token, deviceID = det.Orgs[0].Token, det.Orgs[0].DeviceID
			}
		}
		if token != "" && deviceID != "" {
			d := devicedaemon.Start(bgCtx, devicedaemon.Config{
				Token: token, DeviceID: deviceID,
				Workspace: cfg.Capy.DeviceWorkspace, Policy: cfg.Capy.DevicePolicy, BashAllowAll: cfg.Capy.BashAllowAll,
			})
			srv.SetDaemon(d)
			log.Printf("伪造设备已启动：%s（workspace %s）", deviceID, cfg.Capy.DeviceWorkspace)
		} else {
			log.Printf("WARN 未找到设备凭据，跳过伪造设备（device_token/device_id 或本机 Capy.app）")
		}
	}
	srv.StartConversationGC(bgCtx)
	srv.StartThreadPool(bgCtx)
	go func() {
		// 等 HTTP 起来后再清理，避免和启动流程抢上游
		time.Sleep(5 * time.Second)
		srv.CleanupStale(bgCtx)
	}()

	go func() {
		if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP 服务异常退出: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("收到退出信号，正在关闭…")
	keys.Flush()
	st.Flush()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
}

// checkTemplate 启动时确认 thread 模式的模板线程是否存在，并给出可操作的提示。
func checkTemplate(cfg *config.Config, st *store.Store) {
	if cfg.Capy.Mode != "thread" || st.Count() == 0 {
		return
	}
	acc, err := st.Next()
	if err != nil {
		log.Printf("WARN thread 模式：%v", err)
		return
	}
	client := capy.NewClient(cfg.Capy.APIBase, cfg.Capy.ClerkBase, cfg.Capy.ClerkJSVersion,
		cfg.Capy.Features, cfg.Capy.Surface, cfg.Capy.Timezone, cfg.Capy.ClientID)
	sess := &capy.Session{Client: client, Cookie: acc.Cookie, SessionID: acc.SessionID}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	jam, err := sess.EnsureTemplateJam(ctx, acc.OrgID, cfg.Capy.TemplateJam)
	if err != nil {
		log.Printf("WARN thread 模式缺少模板线程：%v", err)
		log.Printf("      免费账号首次使用需要一条历史设备线程：在任意机器上装一次 Capy、登录并开一条线程即可，之后不用再开。")
		return
	}
	log.Printf("模板线程：%s（thread 模式无需 App、无需额度）", jam)
	if capyapp.AppRunning() {
		log.Printf("提示：Capy 桌面端正在运行 —— agent 此时可以用设备 bash 读写 ~/.capy/drive/user-*/AGENTS.md（长期记忆）。对外服务建议：预建好线程池后关闭 Capy，或每个 API key 绑定独立账号。")
	}
}

func banner(cfg *config.Config, accounts int, addr string) {
	base := "http://" + addr
	if strings.HasPrefix(addr, "[::]") {
		base = "http://127.0.0.1:" + portOf(addr)
	}
	if strings.HasPrefix(addr, "0.0.0.0") {
		base = "http://127.0.0.1:" + portOf(addr)
	}
	mode := cfg.Capy.Mode
	modeText := "thread（复用已有线程派生子线程：无需 App、无需额度）"
	switch mode {
	case "web":
		modeText = "web（项目 + 云机器：不需要 App，需要 Capy 额度）"
	case "device":
		modeText = "device（设备上的 Capy 桌面端执行：不需要额度，需要 App 在线）"
	}
	fmt.Println("")
	fmt.Println("  capy2api · Capy → OpenAI / Anthropic 网关")
	fmt.Println("  ─────────────────────────────────────────────")
	fmt.Printf("  版本        %s\n", server.Version)
	fmt.Printf("  监听        %s\n", addr)
	fmt.Printf("  面板        %s/panel/\n", base)
	fmt.Printf("  OpenAI      %s/v1  (Bearer %s)\n", base, mask(cfg.APIKey))
	fmt.Printf("  Anthropic   %s     (x-api-key: %s)\n", base, mask(cfg.APIKey))
	fmt.Printf("  账号        %d 个（%s）\n", accounts, cfg.AuthDir)
	fmt.Printf("  执行模式    %s\n", modeText)
	switch mode {
	case "device":
		fmt.Printf("  工作目录    %s（%s）\n", cfg.Capy.Workspace.Root, cfg.Capy.Workspace.Mode)
	case "web":
		if cfg.Capy.ProjectID != "" {
			fmt.Printf("  项目        %s\n", cfg.Capy.ProjectID)
		}
	default:
		tpl := cfg.Capy.TemplateJam
		if tpl == "" {
			tpl = "自动选择一条已有设备线程"
		}
		fmt.Printf("  模板线程    %s\n", tpl)
	}
	fmt.Printf("  配置文件    %s\n", cfg.Path())
	fmt.Println("  ─────────────────────────────────────────────")
	switch mode {
	case "device":
		fmt.Println("  提示：device 模式依赖账号绑定的那台设备上 Capy 桌面端在线。")
	case "web":
		fmt.Println("  提示：web 模式在 Capy 云端跑机器；账号没有额度时线程会被上游暂停。")
	default:
		fmt.Println("  提示：thread 模式完全不需要 Capy 桌面端，也不需要云机器额度。")
	}
	fmt.Println("")
}

func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i+1:]
	}
	return addr
}

func mask(key string) string {
	if len(key) <= 12 {
		return "配置值"
	}
	return key[:8] + "…" + key[len(key)-4:]
}

func insecureWarning(listen, key string) string {
	if key != "" && key != "test_key" {
		return ""
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		host = listen
	}
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return ""
	}
	return fmt.Sprintf("api_key 为空或使用样例值，且监听 %s 非回环地址——网关对外无有效鉴权，请尽快修改配置里的 api_key", listen)
}
