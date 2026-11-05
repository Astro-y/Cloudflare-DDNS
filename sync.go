package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"
)

type SyncEngine struct {
	API      CloudAPI
	Resolver AddressResolver
	Now      func() time.Time
	mu       sync.Mutex
}

type SyncResult struct {
	Type    string
	Address string
	Changed bool
	Err     error
}

func sameDNSName(left, right string) bool {
	return strings.EqualFold(strings.TrimSuffix(left, "."), strings.TrimSuffix(right, "."))
}

func (e *SyncEngine) Sync(ctx context.Context, cfg *Config, state *State, force bool) []SyncResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	if state.Records == nil {
		state.Records = make(map[string]*RecordState)
	}
	state.Version = configVersion
	var results []SyncResult
	for index := range cfg.Records {
		record := &cfg.Records[index]
		if !record.Enabled {
			continue
		}
		current := state.Records[record.Type]
		if current == nil || !sameDNSName(current.Name, record.Name) {
			current = &RecordState{Name: record.Name}
			state.Records[record.Type] = current
		}
		now := time.Now()
		if e.Now != nil {
			now = e.Now()
		}
		current.LastCheck = now
		result := SyncResult{Type: record.Type}
		if e.API == nil || e.Resolver == nil {
			result.Err = errors.New("同步组件未初始化")
		} else {
			result.Address, result.Changed, result.Err = e.syncRecord(ctx, record, cfg.TTL, current, now, force)
		}
		if result.Changed {
			current.LastChange = now
		}
		if result.Err != nil {
			current.LastError = result.Err.Error()
		} else {
			current.LastSuccess = now
			current.LastError = ""
		}
		results = append(results, result)
	}
	return results
}

func (e *SyncEngine) syncRecord(ctx context.Context, cfg *RecordConfig, ttl int, state *RecordState, now time.Time, force bool) (string, bool, error) {
	address, err := e.Resolver.Resolve(ctx, cfg.Source, cfg.Type, state.Address)
	if err != nil {
		return "", false, fmt.Errorf("获取 %s 公网地址失败：%w", cfg.Type, err)
	}
	ip, err := netip.ParseAddr(address)
	if err != nil || !IsPublicIP(address) || (cfg.Type == "A" && !ip.Is4()) || (cfg.Type == "AAAA" && (!ip.Is6() || ip.Is4In6())) {
		return "", false, errors.New("IP 来源未返回有效的对应协议公网地址；保留旧 DNS")
	}
	address = ip.String()
	if !force && state.LastError == "" && cfg.RecordID != "" && address == state.Address && !state.LastCloudCheck.IsZero() && now.Sub(state.LastCloudCheck) < 10*time.Minute && !now.Before(state.LastCloudCheck) {
		return address, false, nil
	}
	cloud, found, err := e.findManagedRecord(ctx, cfg)
	if err != nil {
		return address, false, err
	}
	changed := false
	if !found {
		cloud, err = e.API.CreateRecord(ctx, cfg.ZoneID, DNSRecord{Type: cfg.Type, Name: cfg.Name, Content: address, TTL: ttl, Proxied: false})
		if err != nil {
			// A lost response does not prove that POST failed. Re-read before any later POST.
			original := err
			cloud, found, err = e.lookupRecord(ctx, cfg)
			if err != nil {
				return address, false, fmt.Errorf("创建结果未确认，重新查询失败：%w；本轮不重复创建", err)
			}
			if !found {
				return address, false, fmt.Errorf("创建结果未确认：%w；已查询云端，本轮不重复创建", original)
			}
		}
		changed = true
	}
	if cloud.ID == "" || cloud.Type != cfg.Type || !sameDNSName(cloud.Name, cfg.Name) {
		return address, changed, errors.New("Cloudflare 返回的记录与配置不匹配，已停止更新")
	}
	cfg.RecordID = cloud.ID
	fields := make(map[string]any)
	cloudIP, parseErr := netip.ParseAddr(cloud.Content)
	if parseErr != nil || cloudIP != ip {
		fields["content"] = address
	}
	if cloud.Proxied {
		fields["proxied"] = false
	}
	if cloud.TTL != ttl {
		fields["ttl"] = ttl
	}
	if len(fields) > 0 {
		_, err = e.API.PatchRecord(ctx, cfg.ZoneID, cloud.ID, fields)
		if IsNotFound(err) {
			cfg.RecordID = ""
			return address, changed, errors.New("更新时记录 ID 已失效，下轮将重新查找；保留旧记录")
		}
		if err != nil {
			return address, changed, fmt.Errorf("更新 DNS 记录失败：%w", err)
		}
		changed = true
	}
	state.Address = address
	state.LastCloudCheck = now
	return address, changed, nil
}

func (e *SyncEngine) findManagedRecord(ctx context.Context, cfg *RecordConfig) (DNSRecord, bool, error) {
	if cfg.RecordID != "" {
		record, err := e.API.GetRecord(ctx, cfg.ZoneID, cfg.RecordID)
		if err == nil && record.Type == cfg.Type && sameDNSName(record.Name, cfg.Name) {
			// IDs selected automatically must not silently continue if another
			// same-type record appears later. Only an explicit panel selection
			// permits updating one member of a multi-record name.
			matches, lookupErr := e.matchingRecords(ctx, cfg)
			if lookupErr != nil {
				return DNSRecord{}, false, lookupErr
			}
			if len(matches) > 1 && !cfg.AllowMultiple {
				return DNSRecord{}, false, errors.New("存在多条同名同类型记录，请在面板中明确选择接管记录")
			}
			if len(matches) > 0 {
				selectedPresent := false
				for _, candidate := range matches {
					if candidate.ID == cfg.RecordID {
						selectedPresent = true
						break
					}
				}
				if !selectedPresent {
					return DNSRecord{}, false, errors.New("云端记录 ID 与列表不一致，已停止更新，请重新选择记录")
				}
			}
			return record, true, nil
		}
		if err != nil && !IsNotFound(err) {
			return DNSRecord{}, false, fmt.Errorf("读取云端记录失败：%w", err)
		}
		cfg.RecordID = ""
		cfg.AllowMultiple = false
	}
	return e.lookupRecord(ctx, cfg)
}

func (e *SyncEngine) lookupRecord(ctx context.Context, cfg *RecordConfig) (DNSRecord, bool, error) {
	matches, err := e.matchingRecords(ctx, cfg)
	if err != nil {
		return DNSRecord{}, false, err
	}
	if len(matches) > 1 {
		return DNSRecord{}, false, errors.New("存在多条同名同类型记录，请在面板中明确选择接管记录")
	}
	if len(matches) == 1 {
		cfg.RecordID = matches[0].ID
		return matches[0], true, nil
	}
	return DNSRecord{}, false, nil
}

func (e *SyncEngine) matchingRecords(ctx context.Context, cfg *RecordConfig) ([]DNSRecord, error) {
	records, err := e.API.FindRecords(ctx, cfg.ZoneID, cfg.Name)
	if err != nil {
		return nil, fmt.Errorf("查询云端记录失败：%w", err)
	}
	var matches []DNSRecord
	for _, record := range records {
		if !sameDNSName(record.Name, cfg.Name) {
			continue
		}
		if record.Type == "CNAME" {
			return nil, errors.New("同名 CNAME 记录冲突，请先在 Cloudflare 处理后重试")
		}
		if record.Type == cfg.Type {
			matches = append(matches, record)
		}
	}
	return matches, nil
}
