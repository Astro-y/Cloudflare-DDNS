package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

func (p *Panel) readCredentials(previousAccountID string) (Credentials, error) {
	token, err := p.secret()
	if err != nil {
		return Credentials{}, err
	}
	accountID, err := p.selectTokenAccount(token, previousAccountID)
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{Version: 1, Token: token, AccountID: accountID}, nil
}

func (p *Panel) selectTokenAccount(token, previousAccountID string) (string, error) {
	defaultChoice := "1"
	if previousAccountID != "" || strings.HasPrefix(token, "cfat_") {
		defaultChoice = "2"
	}
	if strings.HasPrefix(token, "cfut_") {
		defaultChoice = "1"
	}
	p.printf("Token 类型：\n1. 用户级（个人资料 → API Tokens，无需 Account ID）\n2. 账户级（管理账户 → API Tokens，需要 Account ID）\n")
	if strings.HasPrefix(token, "cfat_") {
		p.printf("检测到账户级 Token 格式，建议选择 2。\n")
	}
	for {
		choice, err := p.ask("选择 Token 类型（q 取消）", defaultChoice)
		if err != nil {
			return "", err
		}
		switch choice {
		case "1":
			if strings.HasPrefix(token, "cfat_") {
				p.printf("此 Token 为账户级，用户级验证接口无法验证它，请选择 2 并填写所属 Account ID。\n")
				continue
			}
			return "", nil
		case "2":
			p.printf("Account ID 是账户概览或 Cloudflare 验证示例 URL 中的 32 位十六进制标识，和 Zone ID 不同。\n")
			for {
				id, err := p.ask("所属 Account ID（q 取消）", previousAccountID)
				if err != nil {
					return "", err
				}
				if id == "" {
					p.printf("账户级 Token 必须填写 Account ID。\n")
					continue
				}
				if err := ValidateAccountID(id); err != nil {
					p.printf("%v\n", err)
					continue
				}
				return id, nil
			}
		default:
			p.printf("请选择 1 或 2。\n")
		}
	}
}

func (p *Panel) describeCredentials(credentials Credentials) {
	if credentials.AccountID == "" {
		p.printf("Token 类型：用户级\n")
	} else {
		p.printf("Token 类型：账户级；Account ID：%s\n", credentials.AccountID)
	}
}

// All validation is performed while sharing the operation lock with the daemon.
// SaveCredentials atomically replaces the token and its account metadata together.
func (p *Panel) replaceCredentials(ctx context.Context, credentials Credentials) (bool, error) {
	if credentials.Version != 1 {
		return false, errors.New("不支持的凭据版本")
	}
	if err := validateTokenText(credentials.Token); err != nil {
		return false, err
	}
	if err := ValidateAccountID(credentials.AccountID); err != nil {
		return false, err
	}
	if strings.HasPrefix(credentials.Token, "cfat_") && credentials.AccountID == "" {
		return false, errors.New("账户级 Token 必须填写 Account ID")
	}
	unlock, err := p.store.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer unlock()
	cfg, err := p.store.LoadConfig()
	if err != nil {
		return false, err
	}
	wasActive := HasSystemd() && GetServiceStatus().Active
	api := p.apiFactory(credentials.Token, credentials.AccountID)
	if err := api.VerifyToken(ctx); err != nil {
		return false, err
	}
	zones, err := api.ListZones(ctx)
	if err != nil {
		return false, err
	}
	allowed := make(map[string]bool)
	for _, zone := range zones {
		allowed[zone.ID] = true
	}
	for _, record := range cfg.Records {
		if !allowed[record.ZoneID] {
			return false, fmt.Errorf("新 Token 无权访问 %s，已保留旧凭据", record.ZoneName)
		}
		if _, err := api.FindRecords(ctx, record.ZoneID, record.Name); err != nil {
			return false, fmt.Errorf("验证 DNS 读取权限失败，已保留旧凭据：%w", err)
		}
	}
	if err := EnsureInstalledCredentialSupport(ctx, p.store.Paths); err != nil {
		return false, err
	}
	if err := p.store.SaveCredentials(credentials); err != nil {
		return false, err
	}
	return wasActive, nil
}
