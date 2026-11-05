package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func (p *Panel) services(ctx context.Context) error {
	if err := RequireAdmin(); err != nil {
		return err
	}
	if !HasSystemd() {
		p.printf("当前环境无 systemd。可在此会话前台常驻，关闭 SSH 会话或按 Ctrl+C 会停止同步。\n")
		ok, err := p.confirm("进入前台常驻", false)
		if err != nil || !ok {
			return err
		}
		// A separate signal context lets the process return cleanly after foreground operation.
		foreground, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		err = runDaemon(foreground, p.store, log.New(p.out, "", log.LstdFlags))
		if err != nil {
			return err
		}
		return errPanelExit
	}
	s := GetServiceStatus()
	p.printf("当前：%s；开机自启：%s\n1. 启动\n2. 停止\n3. 重启\n4. 开启自启\n5. 关闭自启\n0. 返回\n", yesNo(s.Active, "运行中", "已停止"), yesNo(s.Enabled, "开启", "关闭"))
	v, err := p.ask("选择操作", "0")
	if err != nil {
		return err
	}
	actions := map[string]string{"1": "start", "2": "stop", "3": "restart", "4": "enable", "5": "disable"}
	if v == "0" {
		return nil
	}
	action, ok := actions[v]
	if !ok {
		return errors.New("无效服务操作")
	}
	// Serialize service actions with other panels; daemons acquire only around sync rounds.
	unlock, err := p.store.Acquire(ctx)
	if err != nil {
		return err
	}
	err = ServiceAction(ctx, action)
	unlock()
	if err == nil {
		p.printf("服务操作已完成。开关自启不会改变当前运行状态。\n")
	}
	return err
}

func (p *Panel) diagnose(ctx context.Context) error {
	p.printf("网络与配置诊断（只读，不修改 DNS）\n管理员：%s；systemd：%s\n", yesNo(IsAdmin(), "是", "否"), yesNo(HasSystemd(), "可用", "不可用"))
	cfg, err := p.store.LoadConfig()
	if err != nil {
		return err
	}
	if err := ValidateConfig(cfg); err != nil {
		p.printf("配置校验失败：%v\n", err)
	} else {
		p.printf("配置校验：通过；固定仅 DNS；HTTPS 使用系统 CA 并验证服务器证书。\n")
	}
	p.printInterfaces()
	api, err := p.api()
	if err != nil {
		return err
	}
	if err := api.VerifyToken(ctx); err != nil {
		p.printf("Token 验证失败：%v\n", err)
	} else {
		p.printf("Token 状态：有效（凭据不显示）。\n")
	}
	for _, r := range cfg.Records {
		p.printf("\n%s %s（%s）\n", protocolText(r.Type), r.Name, yesNo(r.Enabled, "启用", "关闭"))
		probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		address, probeErr := p.resolver.Resolve(probeCtx, r.Source, r.Type, "")
		cancel()
		if probeErr != nil {
			p.printf("IP 探测失败：%v\n", probeErr)
		} else {
			p.printf("IP 探测成功：%s；公网出口不保证入站可达。\n", address)
		}
		records, recordErr := api.FindRecords(ctx, r.ZoneID, r.Name)
		if recordErr != nil {
			p.printf("云端读取失败：%v\n", recordErr)
			continue
		}
		matches := 0
		selected := false
		cname := false
		for _, rr := range records {
			if rr.Type == "CNAME" {
				cname = true
			}
			if rr.Type == r.Type {
				matches++
				if rr.ID == r.RecordID {
					selected = true
				}
			}
		}
		if cname {
			p.printf("冲突：存在 CNAME，将停止此记录更新。\n")
		} else if matches > 1 && !selected {
			p.printf("冲突：%d 条同类型记录且没有有效选定 ID，请从菜单 3 重新选择。\n", matches)
		} else if matches == 0 {
			p.printf("记录缺失，成功探测地址后同步会创建。\n")
		} else {
			p.printf("云端 DNS 读取正常（%d 条同类型记录）。\n", matches)
		}
	}
	p.printf("诊断完成。DNS Edit 权限需要实际写入时校验；可从菜单 7 执行同步。\n")
	return nil
}

func (p *Panel) uninstall(ctx context.Context) error {
	if err := RequireAdmin(); err != nil {
		return err
	}
	p.printf("卸载会停止并禁用服务，删除已安装程序和服务文件。云端 DNS 记录默认保留。\n")
	keep, err := p.confirm("保留本地配置、Token 和状态以便重装", true)
	if err != nil {
		return err
	}
	ok, err := p.confirm("确认卸载 DDNS", false)
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
	err = UninstallLocal(ctx, p.store.Paths, keep)
	unlock()
	if err != nil {
		return fmt.Errorf("卸载未全部完成：%w", err)
	}
	p.printf("已卸载；云端 DNS 记录保留。%s\n", yesNo(keep, "本地配置已保留。", "本地凭据与配置已删除，锁文件保留用于并发安全。"))
	return errPanelExit
}
