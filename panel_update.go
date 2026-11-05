package main

import (
	"context"
	"errors"
	"fmt"
)

func (p *Panel) updateProgram(ctx context.Context) error {
	if err := RequireAdmin(); err != nil {
		return err
	}
	p.printf("正在检查 GitHub 最新稳定版……\n")
	release, err := CheckLatestRelease(ctx)
	if err != nil {
		return err
	}
	p.printf("当前面板：%s；最新稳定版：%s\n", version, release.Tag)
	minimum, err := CompareReleaseVersions(release.Tag, "v0.1.1")
	if err != nil || minimum < 0 {
		return errors.New("发布服务器尚未提供支持新版凭据的版本，请稍后重试；未修改已安装程序")
	}
	comparison, err := CompareReleaseVersions(version, release.Tag)
	if err != nil {
		return err
	}
	if comparison > 0 {
		p.printf("当前面板版本高于最新稳定版，未执行降级。\n")
		return nil
	}
	if comparison == 0 {
		p.printf("当前面板已是最新版；如果由一键入口启动，可以重新安装以更新系统中的 cfddns。\n")
	}
	p.printf("将下载并核对 SHA-256，保留现有配置、Token、状态及服务启停 / 自启状态。\n")
	ok, err := p.confirm("确认安装 "+release.Tag+"（成功后退出当前面板）", false)
	if err != nil {
		return err
	}
	if !ok {
		return errCanceled
	}
	p.printf("正在下载并校验更新……\n")
	artifact, cleanup, err := DownloadRelease(ctx, release)
	if err != nil {
		return err
	}
	defer cleanup()
	unlock, err := p.store.Acquire(ctx)
	if err != nil {
		return err
	}
	cfg, err := p.store.LoadConfig()
	var credentials Credentials
	if err == nil {
		credentials, err = p.store.LoadCredentials()
	}
	if err == nil {
		err = InstallDownloadedRelease(ctx, p.store.Paths, p.store, cfg, credentials, artifact)
	}
	unlock()
	if err != nil {
		return fmt.Errorf("更新未完成，已尝试回滚本地程序和设置：%w", err)
	}
	p.printf("已更新到 %s。当前面板将退出，请重新输入 cfddns（普通账号使用 sudo cfddns）。\n", release.Tag)
	return errPanelExit
}
