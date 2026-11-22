package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/haohaomin/capy2api/internal/capy"
	"github.com/haohaomin/capy2api/internal/capyapp"
	"github.com/haohaomin/capy2api/internal/config"
	"github.com/haohaomin/capy2api/internal/store"
)

// loadConfig 加载配置（不存在时创建默认配置）。
func loadConfig(path string) (*config.Config, error) {
	return config.Load(path)
}

func newClient(cfg *config.Config) *capy.Client {
	return capy.NewClient(cfg.Capy.APIBase, cfg.Capy.ClerkBase, cfg.Capy.ClerkJSVersion,
		cfg.Capy.Features, cfg.Capy.Surface, cfg.Capy.Timezone, cfg.Capy.ClientID)
}

// validate 用一份临时会话验证 cookie，并补全邮箱/组织。
func validate(ctx context.Context, client *capy.Client, acc store.Account) (store.Account, []string, error) {
	sess := &capy.Session{Client: client, Cookie: acc.Cookie, SessionID: acc.SessionID}
	me, err := sess.Me(ctx)
	if err != nil {
		return acc, nil, err
	}
	if v, ok := me["email"].(string); ok {
		acc.Email = v
	}
	if v, ok := me["id"].(string); ok {
		acc.UserID = v
	}
	orgs, err := sess.Organizations(ctx)
	if err != nil {
		return acc, nil, err
	}
	if len(orgs) == 0 {
		return acc, nil, fmt.Errorf("cookie 下没有组织")
	}
	chosen := orgs[0]
	if acc.OrgID != "" {
		for _, o := range orgs {
			if o.ID == acc.OrgID {
				chosen = o
				break
			}
		}
	}
	acc.OrgID, acc.OrgName = chosen.ID, chosen.Name
	models, err := sess.Models(ctx, acc.OrgID)
	if err != nil {
		return acc, nil, err
	}
	ids := make([]string, 0, 4)
	for _, m := range models {
		if m.FirstParty() {
			ids = append(ids, m.ID)
		}
	}
	if len(ids) == 0 {
		return acc, nil, fmt.Errorf("该账号没有可用的免费广告模型")
	}
	if acc.Name == "" {
		acc.Name = firstNonEmpty(acc.Email, acc.UserID, acc.ID)
	}
	return acc, ids, nil
}

// runLogin 用网页会话（__client cookie）登录，不依赖 Capy 桌面端。
func runLogin(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	configPath := fs.String("config", "", "配置文件路径")
	cookie := fs.String("cookie", "", "__client cookie 值（填 - 从标准输入读取）")
	session := fs.String("session", "", "Clerk session id（可留空自动发现）")
	org := fs.String("org", "", "org id（可留空自动选择）")
	name := fs.String("name", "", "账号显示名")
	device := fs.String("device", "", "device id（device 模式才需要）")
	mode := fs.String("mode", "", "thread / web / device（默认取配置）")
	template := fs.String("template", "", "thread 模式的模板线程 id")
	_ = fs.Parse(args)

	cfg, err := loadConfig(*configPath)
	if err != nil {
		fatal(err)
	}
	value := strings.TrimSpace(*cookie)
	if value == "" || value == "-" {
		fmt.Fprint(os.Stderr, "__client cookie: ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		value = strings.TrimSpace(line)
	}
	if value == "" {
		fatal(fmt.Errorf("cookie 不能为空"))
	}
	acc := store.Account{
		Name:      strings.TrimSpace(*name),
		Cookie:    value,
		SessionID: strings.TrimSpace(*session),
		OrgID:     strings.TrimSpace(*org),
		DeviceID:  strings.TrimSpace(*device),
		Mode:      strings.TrimSpace(*mode),
		Source:    "cli-login",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	validated, models, err := validate(ctx, newClient(cfg), acc)
	if err != nil {
		fatal(fmt.Errorf("校验失败: %w", err))
	}
	st, err := store.Open(cfg.AuthDir)
	if err != nil {
		fatal(err)
	}
	saved, err := st.Add(validated)
	if err != nil {
		fatal(err)
	}
	st.MarkState(saved.ID, "ok", "")
	if strings.TrimSpace(*template) != "" {
		cfg.Capy.TemplateJam = strings.TrimSpace(*template)
		if err := cfg.Save(); err != nil {
			fatal(err)
		}
	}
	fmt.Printf("已登录 %s（org %s）\n模型: %s\n", firstNonEmpty(saved.Email, saved.Name), saved.OrgID, strings.Join(models, ", "))
	fmt.Printf("执行模式: %s\n", firstNonEmpty(saved.Mode, cfg.Capy.Mode))
}

// runImport 从本机 Capy.app 读取登录态（仅 macOS 有效）。
func runImport(args []string) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	configPath := fs.String("config", "", "配置文件路径")
	_ = fs.Parse(args)
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fatal(err)
	}
	if !capyapp.Available() {
		fatal(fmt.Errorf("没有找到本机 Capy 登录态：%s", capyapp.Dir()))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	detected, err := capyapp.Detect(ctx)
	if err != nil {
		fatal(err)
	}
	st, err := store.Open(cfg.AuthDir)
	if err != nil {
		fatal(err)
	}
	client := newClient(cfg)
	var lastErr error
	for _, org := range detected.Orgs {
		acc := store.Account{
			Cookie:    detected.Cookie,
			SessionID: detected.SessionID,
			UserID:    detected.UserID,
			OrgID:     org.OrgID,
			DeviceID:  org.DeviceID,
			Mode:      "device",
			Source:    "capy-app",
		}
		validated, models, err := validate(ctx, client, acc)
		if err != nil {
			lastErr = err
			continue
		}
		saved, err := st.Add(validated)
		if err != nil {
			lastErr = err
			continue
		}
		st.MarkState(saved.ID, "ok", "")
		fmt.Printf("已导入 %s（org %s，device %s）\n模型: %s\n", firstNonEmpty(saved.Email, saved.Name), saved.OrgID, saved.DeviceID, strings.Join(models, ", "))
		return
	}
	if lastErr != nil {
		fatal(lastErr)
	}
	fatal(fmt.Errorf("没有可导入的组织"))
}

func runAccounts(args []string) {
	fs := flag.NewFlagSet("accounts", flag.ExitOnError)
	configPath := fs.String("config", "", "配置文件路径")
	_ = fs.Parse(args)
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fatal(err)
	}
	st, err := store.Open(cfg.AuthDir)
	if err != nil {
		fatal(err)
	}
	list := st.List()
	if len(list) == 0 {
		fmt.Println("还没有账号。用 `capy2api login --cookie -` 或 `capy2api import` 添加。")
		return
	}
	fmt.Printf("%-22s %-28s %-8s %-10s %s\n", "ID", "账号", "状态", "模式", "设备")
	for _, a := range list {
		fmt.Printf("%-22s %-28s %-8s %-10s %s\n", a.ID, firstNonEmpty(a.Email, a.Name), a.State,
			firstNonEmpty(a.Mode, cfg.Capy.Mode), firstNonEmpty(a.DeviceID, "-"))
	}
}

// runThreads 列出可当模板的线程，或用 --set 指定模板。
func runThreads(args []string) {
	fs := flag.NewFlagSet("threads", flag.ExitOnError)
	configPath := fs.String("config", "", "配置文件路径")
	set := fs.String("set", "", "把该 jam id 写进配置当模板")
	_ = fs.Parse(args)
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fatal(err)
	}
	if *set != "" {
		cfg.Capy.TemplateJam = strings.TrimSpace(*set)
		if err := cfg.Save(); err != nil {
			fatal(err)
		}
		fmt.Printf("模板线程已设为 %s\n", cfg.Capy.TemplateJam)
		return
	}
	st, err := store.Open(cfg.AuthDir)
	if err != nil {
		fatal(err)
	}
	acc, err := st.Next()
	if err != nil {
		fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess := &capy.Session{Client: newClient(cfg), Cookie: acc.Cookie, SessionID: acc.SessionID}
	jams, err := sess.ListJams(ctx, acc.OrgID, 50)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("%-32s %-8s %-10s %s\n", "JAM", "机器", "父线程", "标题")
	for _, j := range jams {
		kind := "-"
		if j.PrimaryMachine != nil {
			kind = j.PrimaryMachine.Kind
		}
		parent := j.ParentJamID
		if parent == "" {
			parent = "-"
		}
		fmt.Printf("%-32s %-8s %-10s %s\n", j.ID, kind, parent, j.Title)
	}
	if cfg.Capy.TemplateJam == "" {
		fmt.Println("\n提示：thread 模式会自动选一条 device 线程当模板；要固定就用 --set <jam_id>。")
	}
}

func runModels(args []string) {
	fs := flag.NewFlagSet("models", flag.ExitOnError)
	configPath := fs.String("config", "", "配置文件路径")
	_ = fs.Parse(args)
	cfg, err := loadConfig(*configPath)
	if err != nil {
		fatal(err)
	}
	st, err := store.Open(cfg.AuthDir)
	if err != nil {
		fatal(err)
	}
	acc, err := st.Next()
	if err != nil {
		fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess := &capy.Session{Client: newClient(cfg), Cookie: acc.Cookie, SessionID: acc.SessionID}
	models, err := sess.Models(ctx, acc.OrgID)
	if err != nil {
		fatal(err)
	}
	for _, m := range models {
		if !m.FirstParty() {
			continue
		}
		ctxWin, out := 0, 0
		if m.Limits != nil {
			ctxWin, out = m.Limits.Context, m.Limits.Output
		}
		fmt.Printf("%-40s ctx=%-8d out=%-7d efforts=%s\n", m.ID, ctxWin, out, strings.Join(m.Efforts, "/"))
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "错误: %v\n", err)
	os.Exit(1)
}
