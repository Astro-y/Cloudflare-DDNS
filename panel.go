package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

var errCanceled = errors.New("已取消")
var errPanelExit = errors.New("面板结束")

type Panel struct {
	ctx        context.Context
	store      *Store
	in         *bufio.Reader
	out        io.Writer
	inputFile  *os.File
	apiFactory func(string) CloudAPI
	resolver   AddressResolver
}

func NewPanel(store *Store, input io.Reader, output io.Writer) *Panel {
	p := &Panel{ctx: context.Background(), store: store, in: bufio.NewReader(input), out: output,
		apiFactory: func(token string) CloudAPI { return NewCFClient(token) }, resolver: NewIPResolver()}
	p.inputFile, _ = input.(*os.File)
	return p
}

func (p *Panel) printf(format string, args ...any) { fmt.Fprintf(p.out, format, args...) }

func (p *Panel) ask(prompt, fallback string) (string, error) {
	if fallback != "" {
		p.printf("%s [%s]：", prompt, fallback)
	} else {
		p.printf("%s：", prompt)
	}
	type inputResult struct {
		line string
		err  error
	}
	result := make(chan inputResult, 1)
	go func() { line, err := p.in.ReadString('\n'); result <- inputResult{line, err} }()
	var line string
	var err error
	select {
	case <-p.ctx.Done():
		return "", p.ctx.Err()
	case input := <-result:
		line, err = input.line, input.err
	}
	if err != nil {
		return "", err
	}
	line = strings.TrimSpace(line)
	if strings.EqualFold(line, "q") {
		return "", errCanceled
	}
	if line == "" {
		line = fallback
	}
	return line, nil
}

func (p *Panel) confirm(prompt string, fallback bool) (bool, error) {
	defaultText := "n"
	if fallback {
		defaultText = "y"
	}
	for {
		v, err := p.ask(prompt+" (y/n，q 取消)", defaultText)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(v) {
		case "y", "yes", "是":
			return true, nil
		case "n", "no", "否":
			return false, nil
		default:
			p.printf("请输入 y 或 n。\n")
		}
	}
}

func (p *Panel) secret() (string, error) {
	if p.inputFile == nil || !term.IsTerminal(int(p.inputFile.Fd())) {
		return "", errors.New("Token 必须在交互终端中隐藏输入")
	}
	p.printf("Cloudflare API Token（不回显，空输入取消）：")
	data, err := readPasswordContext(p.ctx, p.inputFile)
	p.printf("\n")
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	for i := range data {
		data[i] = 0
	}
	if token == "" {
		return "", errCanceled
	}
	if strings.ContainsAny(token, "\r\n\t ") {
		return "", errors.New("Token 不能包含空白字符")
	}
	return token, nil
}

func (p *Panel) Run(ctx context.Context) error {
	p.ctx = ctx
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		p.showMenu()
		choice, err := p.ask("请选择（0 退出）", "")
		if errors.Is(err, io.EOF) {
			return nil
		}
		if errors.Is(err, errCanceled) {
			continue
		}
		if err != nil {
			return err
		}
		if choice == "" {
			continue
		}
		if choice == "0" {
			p.printf("已退出面板，已运行的后台服务继续运行。\n")
			return nil
		}
		err = p.dispatch(ctx, choice)
		if errors.Is(err, errPanelExit) || errors.Is(err, io.EOF) {
			return nil
		}
		if errors.Is(err, errCanceled) {
			p.printf("已取消，未提交当前操作。\n")
		} else if err != nil {
			p.printf("操作未完成：%v\n", err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := p.ask("按回车返回菜单", ""); errors.Is(err, io.EOF) {
			return nil
		}
	}
}

func (p *Panel) showMenu() {
	p.printf("\n╭──────────────────────────────────────╮\n│          CFDDNS 管理面板              │\n╰──────────────────────────────────────╯\n版本：%s\n", version)
	cfg, err := p.store.LoadConfig()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			p.printf("安装状态：未安装，请选择 1 安装 / 初始化。\n")
		} else {
			p.printf("安装状态：配置不可读取（%v）\n", err)
		}
	} else {
		if info, err := os.Stat(p.store.Paths.Binary); err == nil && info.Mode().IsRegular() {
			p.printf("安装状态：已安装\n")
		} else {
			p.printf("安装状态：已有配置，程序未安装或不可读取（选择 1 恢复安装）\n")
		}
		if HasSystemd() {
			s := GetServiceStatus()
			p.printf("后台服务：%s    开机自启：%s\n", s.Description, yesNo(s.Enabled, "已开启", "未开启"))
		} else {
			p.printf("后台服务：无 systemd，请从菜单 8 前台运行。\n")
		}
		p.printf("检查间隔：%d 秒    DNS TTL：%s\n", cfg.IntervalSeconds, ttlText(cfg.TTL))
		state, stateErr := p.store.LoadState()
		if stateErr != nil {
			p.printf("本地状态读取失败：%v\n", stateErr)
		}
		for _, typ := range []string{"A", "AAAA"} {
			r := findRecord(&cfg, typ)
			if r == nil {
				p.printf("\n%s：未配置\n", protocolText(typ))
				continue
			}
			p.printf("\n%s：%s（%s）\n", protocolText(typ), r.Name, yesNo(r.Enabled, "启用", "关闭"))
			st := state.Records[typ]
			if st == nil || st.Name != r.Name {
				p.printf("地址：待检测    同步状态：待同步\n")
				continue
			}
			status := "待同步"
			if !st.LastSuccess.IsZero() {
				status = "最近成功 " + formatTime(st.LastSuccess)
			}
			if st.LastError != "" {
				status = "失败：" + st.LastError
			}
			p.printf("地址：%s    同步状态：%s\n", orText(st.Address, "待检测"), status)
		}
	}
	if runtime.GOOS != "linux" {
		p.printf("运行环境：%s/%s（安装和后台运行仅支持 Linux amd64/arm64）\n", runtime.GOOS, runtime.GOARCH)
	}
	p.printf("\n 1. 安装 / 初始化 DDNS\n 2. 查看详细状态\n 3. 修改域名 / 启用或关闭 IPv4、IPv6\n 4. 更新 Cloudflare API Token\n 5. 设置检查间隔\n 6. 设置 IP 来源 / 网卡\n 7. 立即同步\n 8. 管理后台服务\n 9. 查看运行日志\n10. 网络与配置诊断\n11. 设置 DNS TTL\n12. 卸载 DDNS\n 0. 退出\n")
}

func (p *Panel) dispatch(ctx context.Context, choice string) error {
	if choice == "1" {
		return p.install(ctx)
	}
	switch choice {
	case "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12":
	default:
		p.printf("无效选项，请输入 0 到 12。\n")
		return nil
	}
	if _, err := p.store.LoadConfig(); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("尚未初始化，请先选择 1 安装 DDNS")
		}
		return fmt.Errorf("无法读取配置，请使用管理员权限运行：%w", err)
	}
	switch choice {
	case "2":
		return p.details(ctx)
	case "3":
		return p.edit(ctx, p.editRecords)
	case "4":
		return p.updateToken(ctx)
	case "5":
		return p.edit(ctx, func(_ context.Context, c *Config) error {
			v, err := p.askInterval(c.IntervalSeconds)
			if err == nil {
				c.IntervalSeconds = v
			}
			return err
		})
	case "6":
		return p.edit(ctx, p.editSources)
	case "7":
		if err := RequireAdmin(); err != nil {
			return err
		}
		p.printf("正在检测地址并核对云端记录……\n")
		r, err := synchronize(ctx, p.store, true)
		p.printResults(r)
		return err
	case "8":
		return p.services(ctx)
	case "9":
		if err := RequireAdmin(); err != nil {
			return err
		}
		logs, err := ReadLogs(ctx)
		if err == nil {
			p.printf("%s\n", logs)
		}
		return err
	case "10":
		return p.diagnose(ctx)
	case "11":
		return p.edit(ctx, func(_ context.Context, c *Config) error {
			v, err := p.askTTL(c.TTL)
			if err == nil {
				c.TTL = v
			}
			return err
		})
	case "12":
		return p.uninstall(ctx)
	}
	return nil
}

func (p *Panel) edit(ctx context.Context, fn func(context.Context, *Config) error) error {
	if err := RequireAdmin(); err != nil {
		return err
	}
	unlock, err := p.store.Acquire(ctx)
	if err != nil {
		return err
	}
	// Hold the shared operation lock throughout an edit, including cloud validation.
	cfg, err := p.store.LoadConfig()
	wasActive := HasSystemd() && GetServiceStatus().Active
	if err == nil {
		err = fn(ctx, &cfg)
	}
	if err == nil {
		err = ValidateConfig(cfg)
	}
	if err == nil {
		err = p.store.SaveConfig(cfg)
	}
	unlock()
	if err != nil {
		return err
	}
	p.printf("配置已保存。\n")
	if wasActive {
		if err := ServiceAction(ctx, "restart"); err != nil {
			return fmt.Errorf("配置已保存，但重启失败，请从菜单 8 重试：%w", err)
		}
		p.printf("已重启运行中的服务以应用配置。\n")
	} else if HasSystemd() {
		p.printf("服务保持停止状态；可选择 7 立即同步或从菜单 8 启动。\n")
	} else {
		p.printf("前台常驻会话将在下一轮读取新配置；可从菜单 8 进入前台运行。\n")
	}
	return nil
}

func (p *Panel) api() (CloudAPI, error) {
	token, err := p.store.LoadToken()
	if err != nil {
		return nil, err
	}
	return p.apiFactory(token), nil
}

func (p *Panel) details(ctx context.Context) error {
	cfg, err := p.store.LoadConfig()
	if err != nil {
		return err
	}
	state, err := p.store.LoadState()
	if err != nil {
		return err
	}
	p.summary(cfg)
	for _, r := range cfg.Records {
		p.printf("\n%s Zone ID：%s\n记录 ID：%s\n", protocolText(r.Type), r.ZoneID, orText(r.RecordID, "待首次创建"))
		if st := state.Records[r.Type]; st != nil && st.Name == r.Name {
			p.printf("最近检查：%s\n最近成功：%s\n最近变更：%s\n最近云端核对：%s\n错误：%s\n", formatTime(st.LastCheck), formatTime(st.LastSuccess), formatTime(st.LastChange), formatTime(st.LastCloudCheck), orText(st.LastError, "无"))
		}
	}
	refresh, err := p.confirm("是否只读刷新云端数据（不会修改 DNS）", false)
	if err != nil || !refresh {
		return err
	}
	api, err := p.api()
	if err != nil {
		return err
	}
	for _, r := range cfg.Records {
		records, err := api.FindRecords(ctx, r.ZoneID, r.Name)
		if err != nil {
			p.printf("%s 云端读取失败：%v\n", r.Type, err)
			continue
		}
		p.printf("%s 的云端记录：\n", r.Name)
		if len(records) == 0 {
			p.printf("  暂无记录\n")
		}
		for _, rr := range records {
			p.printf("  %s %s TTL=%s %s ID=%s\n", rr.Type, rr.Content, ttlText(rr.TTL), yesNo(rr.Proxied, "代理已开启（同步会关闭）", "仅 DNS"), rr.ID)
		}
	}
	return nil
}

func (p *Panel) printResults(results []SyncResult) {
	if len(results) == 0 {
		p.printf("没有启用的记录需要同步。\n")
	}
	for _, r := range results {
		if r.Err != nil {
			p.printf("%s：失败，%v\n", protocolText(r.Type), r.Err)
			if r.Changed {
				p.printf("  云端已发生部分变更，请查看详细状态并重试同步。\n")
			}
		} else {
			p.printf("%s：%s，%s\n", protocolText(r.Type), yesNo(r.Changed, "已更新", "已核对 / 无需变更"), r.Address)
		}
	}
}

func (p *Panel) summary(cfg Config) {
	p.printf("\n检查间隔：%d 秒；TTL：%s（DNS 缓存时间，与检查频率独立）。\n", cfg.IntervalSeconds, ttlText(cfg.TTL))
	for _, r := range cfg.Records {
		p.printf("%s：%s，%s，来源：%s，固定仅 DNS。\n", protocolText(r.Type), r.Name, yesNo(r.Enabled, "启用", "关闭"), sourceText(r.Source))
	}
}

func findRecord(cfg *Config, typ string) *RecordConfig {
	for i := range cfg.Records {
		if cfg.Records[i].Type == typ {
			return &cfg.Records[i]
		}
	}
	return nil
}
func yesNo(v bool, yes, no string) string {
	if v {
		return yes
	}
	return no
}
func orText(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
func protocolText(typ string) string {
	if typ == "A" {
		return "IPv4"
	}
	return "IPv6"
}
func ttlText(ttl int) string {
	if ttl == 1 {
		return "Auto"
	}
	return strconv.Itoa(ttl) + " 秒"
}
func formatTime(t time.Time) string {
	if t.IsZero() {
		return "暂无"
	}
	return t.Local().Format("2006-01-02 15:04:05 MST")
}
func sourceText(s IPSource) string {
	if s.Mode == "interface" {
		return "网卡 " + s.Interface
	}
	if s.URL != "" {
		return "自定义 HTTPS + 公共备用接口"
	}
	return "公网 HTTPS 查询（ipify / Cloudflare trace）"
}

func parseInterval(s string) (int, error) {
	if n, err := strconv.Atoi(s); err == nil {
		if n >= 30 && n <= 86400 {
			return n, nil
		}
		return 0, errors.New("检查间隔必须为 30 秒至 24 小时")
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 30*time.Second || d > 24*time.Hour || d%time.Second != 0 {
		return 0, errors.New("请输入 30s 至 24h 的整秒时长，例如 60s、5m、1h")
	}
	return int(d / time.Second), nil
}

func (p *Panel) askInterval(current int) (int, error) {
	p.printf("检查间隔控制检测频率，允许 30s 至 24h。\n")
	for {
		v, err := p.ask("检查间隔（q 取消）", strconv.Itoa(current)+"s")
		if err != nil {
			return 0, err
		}
		n, err := parseInterval(v)
		if err == nil {
			return n, nil
		}
		p.printf("%v\n", err)
	}
}

func (p *Panel) askTTL(current int) (int, error) {
	p.printf("TTL 控制 DNS 缓存时间，与检查间隔独立；Auto 或 60 至 86400 秒。\n")
	defaultText := strconv.Itoa(current)
	if current == 1 {
		defaultText = "auto"
	}
	for {
		v, err := p.ask("DNS TTL（q 取消）", defaultText)
		if err != nil {
			return 0, err
		}
		if strings.EqualFold(v, "auto") || v == "1" {
			return 1, nil
		}
		n, err := strconv.Atoi(v)
		if err == nil && n >= 60 && n <= 86400 {
			return n, nil
		}
		p.printf("请输入 auto 或 60 到 86400 之间的整数。\n")
	}
}
