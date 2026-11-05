package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

func (p *Panel) install(ctx context.Context) error {
	if runtime.GOOS != "linux" || (runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64") {
		return errors.New("安装仅支持 Linux amd64 / arm64，请将 dist 中对应架构的文件复制到 Linux 主机运行")
	}
	if err := RequireAdmin(); err != nil {
		return err
	}
	p.printf("系统：%s/%s；管理员：是；systemd：%s。\n", runtime.GOOS, runtime.GOARCH, yesNo(HasSystemd(), "可用", "不可用"))
	p.printInterfaces()
	cfg, err := p.store.LoadConfig()
	hadConfig := err == nil
	baseline := cfg
	var credentials Credentials
	if err == nil {
		p.printf("检测到已有配置，重复安装将保留域名、凭据和设置。\n")
		credentials, err = p.store.LoadCredentials()
		if err != nil {
			return err
		}
	} else if errors.Is(err, os.ErrNotExist) {
		p.printf("准备目标 Zone 的 Zone Read、DNS Edit 权限的 API Token。仅支持 API Token，不支持全局 API Key。\n全部管理记录会设为仅 DNS，不需要配置域名证书。输入 q 可取消向导。\n")
		credentials, err = p.readCredentials("")
		if err != nil {
			return err
		}
		api := p.apiFactory(credentials.Token, credentials.AccountID)
		p.printf("正在验证 Token 并分页读取 Zone……\n")
		if err := api.VerifyToken(ctx); err != nil {
			return err
		}
		zones, err := api.ListZones(ctx)
		if err != nil {
			return err
		}
		if len(zones) == 0 {
			return errors.New("Token 没有可访问的 Zone，请检查 Zone Read 权限和资源范围")
		}
		p.printf("正在分别探测公网 IPv4 / IPv6（失败仍可保存配置等待网络恢复）……\n")
		addresses := make([]string, 2)
		probeErrors := make([]error, 2)
		var wg sync.WaitGroup
		for i, typ := range []string{"A", "AAAA"} {
			wg.Add(1)
			go func(i int, typ string) {
				defer wg.Done()
				probeCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
				defer cancel()
				addresses[i], probeErrors[i] = p.resolver.Resolve(probeCtx, IPSource{Mode: "external"}, typ, "")
			}(i, typ)
		}
		wg.Wait()
		for i, typ := range []string{"A", "AAAA"} {
			if probeErrors[i] != nil {
				p.printf("%s：探测不可用（%v）\n", protocolText(typ), probeErrors[i])
			} else {
				p.printf("%s：%s\n", protocolText(typ), addresses[i])
			}
		}
		p.printf("建议：%s。公网出口地址可检测不代表外网能够入站访问。\n", recommendation(addresses))
		cfg = Config{Version: configVersion, IntervalSeconds: 60, TTL: 1}
		for i, typ := range []string{"A", "AAAA"} {
			defaultOn := addresses[i] != "" || (i == 0 && addresses[1] == "")
			enabled, err := p.confirm("是否启用 "+protocolText(typ), defaultOn)
			if err != nil {
				return err
			}
			if !enabled {
				continue
			}
			r, err := p.chooseRecord(ctx, api, zones, typ, nil)
			if err != nil {
				return err
			}
			r.Source, err = p.chooseSource(ctx, typ, IPSource{Mode: "external"})
			if err != nil {
				return err
			}
			cfg.Records = append(cfg.Records, r)
		}
		if len(cfg.Records) == 0 {
			return errors.New("初始化时请至少配置一种协议")
		}
		cfg.IntervalSeconds, err = p.askInterval(60)
		if err != nil {
			return err
		}
		cfg.TTL, err = p.askTTL(1)
		if err != nil {
			return err
		}
	} else {
		return fmt.Errorf("现有配置读取失败，为防止覆盖已停止安装：%w", err)
	}
	if err := ValidateConfig(cfg); err != nil {
		return err
	}
	p.summary(cfg)
	p.describeCredentials(credentials)
	p.printf("程序：%s\n配置目录：%s\n状态目录：%s\n", p.store.Paths.Binary, p.store.Paths.ConfigDir, p.store.Paths.StateDir)
	if !HasSystemd() {
		p.printf("未检测到 systemd：安装后请从菜单 8 选择前台常驻；关闭会话会停止同步。\n")
	}
	ok, err := p.confirm("确认安装以上配置", false)
	if err != nil {
		return err
	}
	if !ok {
		return errCanceled
	}
	unlock, err := p.store.Acquire(ctx)
	if err != nil {
		return err
	}
	current, currentErr := p.store.LoadConfig()
	if hadConfig {
		currentCredentials, tokenErr := p.store.LoadCredentials()
		if currentErr != nil || tokenErr != nil || !reflect.DeepEqual(current, baseline) || currentCredentials != credentials {
			unlock()
			return errors.New("配置或凭据已在向导期间被更新，请重新进入安装以保留最新设置")
		}
	} else if !errors.Is(currentErr, os.ErrNotExist) {
		unlock()
		return errors.New("配置已在向导期间被其他进程创建或不可读取，请重新打开面板")
	}
	err = InstallLocal(ctx, p.store.Paths, p.store, cfg, credentials.Token, credentials.AccountID)
	unlock()
	if err != nil {
		return fmt.Errorf("本地安装失败（尚未发起云端 DNS 写入）：%w", err)
	}
	p.printf("本地安装完成。正在执行首次同步；以下云端结果独立于本地安装结果。\n")
	results, syncErr := synchronize(ctx, p.store, true)
	p.printResults(results)
	if syncErr != nil {
		p.printf("首次同步未完成：%v；本地安装已保留，可从菜单 7 重试。\n", syncErr)
	}
	p.printf("以后直接输入 cfddns 打开面板。\n")
	return nil
}

func recommendation(addresses []string) string {
	if addresses[0] != "" && addresses[1] != "" {
		return "双栈 IPv4 + IPv6"
	}
	if addresses[1] != "" {
		return "IPv6"
	}
	if addresses[0] != "" {
		return "IPv4"
	}
	return "暂未探测到公网地址，可配置 IPv4 或 IPv6 并等待网络恢复"
}

func (p *Panel) printInterfaces() {
	interfaces, err := ListInterfaces()
	if err != nil {
		p.printf("网卡读取失败：%v\n", err)
		return
	}
	p.printf("检测到的网卡（仅列出可用公网地址）：\n")
	for _, inf := range interfaces {
		p.printf("  %s：%s\n", inf.Name, orText(strings.Join(inf.Addresses, ", "), "暂无公网地址"))
	}
}

func (p *Panel) selectZone(zones []Zone, current string) (Zone, error) {
	defaultChoice := "1"
	for i, z := range zones {
		p.printf("%d. %s\n", i+1, z.Name)
		if z.ID == current {
			defaultChoice = strconv.Itoa(i + 1)
		}
	}
	for {
		v, err := p.ask("选择 Zone（q 取消）", defaultChoice)
		if err != nil {
			return Zone{}, err
		}
		n, err := strconv.Atoi(v)
		if err == nil && n >= 1 && n <= len(zones) {
			return zones[n-1], nil
		}
		p.printf("请输入 1 到 %d。\n", len(zones))
	}
}

func (p *Panel) chooseRecord(ctx context.Context, api CloudAPI, zones []Zone, typ string, old *RecordConfig) (RecordConfig, error) {
	currentZone, currentName := "", "@"
	if old != nil {
		currentZone = old.ZoneID
		currentName = old.Name
	}
	p.printf("\n配置 %s 域名：\n", protocolText(typ))
	zone, err := p.selectZone(zones, currentZone)
	if err != nil {
		return RecordConfig{}, err
	}
	var name string
	for {
		v, err := p.ask("域名（@ 为根域，支持子域名或完整域名，q 取消）", currentName)
		if err != nil {
			return RecordConfig{}, err
		}
		name, err = NormalizeDomain(v, zone.Name)
		if err == nil {
			break
		}
		p.printf("%v\n", err)
	}
	records, err := api.FindRecords(ctx, zone.ID, name)
	if err != nil {
		return RecordConfig{}, err
	}
	var matches []DNSRecord
	for _, r := range records {
		if r.Type == "CNAME" {
			return RecordConfig{}, errors.New("该域名存在 CNAME 冲突，请先在 Cloudflare 处理后重试；面板不会删除它")
		}
		if r.Type == typ {
			matches = append(matches, r)
		}
	}
	result := RecordConfig{Type: typ, Name: name, ZoneID: zone.ID, ZoneName: zone.Name, Enabled: true, Source: IPSource{Mode: "external"}}
	if old != nil {
		result.Source = old.Source
	}
	var selected *DNSRecord
	if len(matches) == 1 {
		p.printf("发现现有 %s：%s（ID %s）。\n", typ, matches[0].Content, matches[0].ID)
		ok, err := p.confirm("是否接管此记录", true)
		if err != nil {
			return RecordConfig{}, err
		}
		if !ok {
			return RecordConfig{}, errCanceled
		}
		selected = &matches[0]
	} else if len(matches) > 1 {
		result.AllowMultiple = true
		p.printf("发现多条同类型记录，必须明确选择要接管的一条：\n")
		for i, r := range matches {
			p.printf("%d. %s（ID %s，%s）\n", i+1, r.Content, r.ID, yesNo(r.Proxied, "代理已开", "仅 DNS"))
		}
		for {
			v, err := p.ask("选择记录编号（q 取消）", "")
			if err != nil {
				return RecordConfig{}, err
			}
			n, err := strconv.Atoi(v)
			if err == nil && n >= 1 && n <= len(matches) {
				selected = &matches[n-1]
				break
			}
			p.printf("必须选择 1 到 %d 之间的编号。\n", len(matches))
		}
	} else {
		p.printf("未发现现有 %s，首次获得有效公网 IP 时创建记录。\n", typ)
	}
	if selected != nil {
		result.RecordID = selected.ID
		if selected.Proxied {
			p.printf("注意：该记录目前开启小黄云，接管同步会将其关闭，固定为仅 DNS。\n")
			ok, err := p.confirm("同意同步时关闭小黄云", false)
			if err != nil {
				return RecordConfig{}, err
			}
			if !ok {
				return RecordConfig{}, errCanceled
			}
		}
	}
	if old != nil && old.Name != result.Name {
		p.printf("域名变更后，旧域名 %s 的云端记录会保留，不再管理。\n", old.Name)
	}
	return result, nil
}

func (p *Panel) chooseSource(ctx context.Context, typ string, current IPSource) (IPSource, error) {
	p.printf("\n%s IP 来源：\n1. 公网 HTTPS 查询（按协议连接，不使用环境 HTTP 代理）\n2. 指定网卡（每轮重新读取）\n", protocolText(typ))
	defaultChoice := "1"
	if current.Mode == "interface" {
		defaultChoice = "2"
	}
	var source IPSource
	for {
		v, err := p.ask("选择来源（q 取消）", defaultChoice)
		if err != nil {
			return source, err
		}
		if v == "1" {
			urlDefault := current.URL
			if urlDefault == "" {
				urlDefault = "默认"
			}
			u, err := p.ask("自定义 HTTPS 纯文本 IP 接口（输入 默认 使用公共接口）", urlDefault)
			if err != nil {
				return source, err
			}
			if u == "默认" {
				u = ""
			}
			source = IPSource{Mode: "external", URL: u}
		} else if v == "2" {
			p.printInterfaces()
			name, err := p.ask("网卡名称（可预配置尚未出现的网卡）", current.Interface)
			if err != nil {
				return source, err
			}
			source = IPSource{Mode: "interface", Interface: name}
		} else {
			p.printf("请选择 1 或 2。\n")
			continue
		}
		if err := ValidateSource(source); err != nil {
			p.printf("%v\n", err)
			continue
		}
		break
	}
	p.printf("正在测试 %s 来源……\n", protocolText(typ))
	probeCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	addr, err := p.resolver.Resolve(probeCtx, source, typ, "")
	if err != nil {
		p.printf("当前探测失败：%v。配置仍可保存；没有有效 IP 时会保留旧 DNS 记录。\n", err)
	} else {
		p.printf("检测到：%s（不代表入站可达）。\n", addr)
	}
	return source, nil
}

func (p *Panel) editRecords(ctx context.Context, cfg *Config) error {
	api, err := p.api()
	if err != nil {
		return err
	}
	for _, typ := range []string{"A", "AAAA"} {
		r := findRecord(cfg, typ)
		enabled, err := p.confirm("是否启用 "+protocolText(typ), r != nil && r.Enabled)
		if err != nil {
			return err
		}
		if !enabled {
			if r != nil {
				r.Enabled = false
			}
			continue
		}
		change := r == nil
		if r != nil {
			r.Enabled = true
			change, err = p.confirm("是否修改 "+r.Name+"（旧域名云端记录将保留）", false)
			if err != nil {
				return err
			}
		}
		if !change {
			continue
		}
		zones, err := api.ListZones(ctx)
		if err != nil {
			return err
		}
		if len(zones) == 0 {
			return errors.New("Token 没有可访问的 Zone")
		}
		updated, err := p.chooseRecord(ctx, api, zones, typ, r)
		if err != nil {
			return err
		}
		if r == nil {
			updated.Source, err = p.chooseSource(ctx, typ, updated.Source)
			if err != nil {
				return err
			}
			cfg.Records = append(cfg.Records, updated)
		} else {
			*r = updated
		}
	}
	p.summary(*cfg)
	ok, err := p.confirm("保存以上域名和启用设置", true)
	if err != nil {
		return err
	}
	if !ok {
		return errCanceled
	}
	return nil
}

func (p *Panel) editSources(ctx context.Context, cfg *Config) error {
	for i := range cfg.Records {
		r := &cfg.Records[i]
		change, err := p.confirm("是否设置 "+protocolText(r.Type)+"（"+r.Name+"）的 IP 来源", r.Enabled)
		if err != nil {
			return err
		}
		if change {
			r.Source, err = p.chooseSource(ctx, r.Type, r.Source)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Panel) updateToken(ctx context.Context) error {
	if err := RequireAdmin(); err != nil {
		return err
	}
	p.printf("新 Token 需要管理 Zone 的 Zone Read 和 DNS Edit 权限。验证失败将保留旧凭据。\n")
	previous, err := p.store.LoadCredentials()
	if err != nil {
		return err
	}
	credentials, err := p.readCredentials(previous.AccountID)
	if err != nil {
		return err
	}
	wasActive, err := p.replaceCredentials(ctx, credentials)
	if err != nil {
		return err
	}
	p.printf("Token 及所属类型已验证并安全替换（不显示凭据）。DNS Edit 权限由 Cloudflare 在写入时校验。\n")
	if wasActive {
		if err := ServiceAction(ctx, "restart"); err != nil {
			return fmt.Errorf("Token 已保存，但服务重启失败：%w", err)
		}
	}
	return nil
}
