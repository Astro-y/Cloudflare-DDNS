package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"time"
)

const maxIPResponse = 4096

// IPResolver deliberately owns separate transports with no environment proxy.
// API credentials never enter either transport or the public-IP requests.
type IPResolver struct {
	clients map[string]*http.Client
	urls    map[string][]string
}

func NewIPResolver() *IPResolver {
	return &IPResolver{
		clients: map[string]*http.Client{"A": newIPHTTPClient("tcp4", nil), "AAAA": newIPHTTPClient("tcp6", nil)},
		urls: map[string][]string{
			"A":    {"https://api.ipify.org", "https://1.1.1.1/cdn-cgi/trace"},
			"AAAA": {"https://api6.ipify.org", "https://[2606:4700:4700::1111]/cdn-cgi/trace"},
		},
	}
}

func newIPHTTPClient(network string, dial func(context.Context, string, string) (net.Conn, error)) *http.Client {
	if dial == nil {
		dialer := &net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}
		dial = dialer.DialContext
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _ string, address string) (net.Conn, error) {
			return dial(ctx, network, address)
		},
		TLSHandshakeTimeout:    8 * time.Second,
		ResponseHeaderTimeout:  8 * time.Second,
		IdleConnTimeout:        30 * time.Second,
		MaxIdleConns:           4,
		MaxResponseHeaderBytes: 16 << 10,
		ForceAttemptHTTP2:      true,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   12 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("IP 查询重定向过多")
			}
			return validateIPURL(req.URL)
		},
	}
}

func validateIPURL(u *url.URL) error {
	if u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return errors.New("IP 接口必须为无用户名、密码及片段的 HTTPS URL")
	}
	return nil
}

func ValidateSource(source IPSource) error {
	switch source.Mode {
	case "external":
		if source.URL == "" {
			return nil
		}
		u, err := url.Parse(source.URL)
		if err != nil {
			return errors.New("IP 接口 URL 无效")
		}
		return validateIPURL(u)
	case "interface":
		if strings.TrimSpace(source.Interface) == "" || strings.ContainsAny(source.Interface, "\r\n\x00") {
			return errors.New("请指定有效的网卡名称")
		}
		return nil
	default:
		return errors.New("IP 来源必须是 external 或 interface")
	}
}

func (r *IPResolver) Resolve(ctx context.Context, source IPSource, recordType, previous string) (string, error) {
	if recordType != "A" && recordType != "AAAA" {
		return "", errors.New("不支持的地址协议")
	}
	if err := ValidateSource(source); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if source.Mode == "interface" {
		candidates, err := interfaceCandidates(source.Interface, recordType)
		if err != nil {
			return "", err
		}
		return selectAddress(candidates, recordType, previous)
	}
	if r == nil || r.clients == nil || r.urls == nil {
		r = NewIPResolver()
	}
	endpoints := make([]string, 0, len(r.urls[recordType])+1)
	if source.URL != "" {
		endpoints = append(endpoints, source.URL)
	}
	endpoints = append(endpoints, r.urls[recordType]...)
	var failures []string
	for i, endpoint := range endpoints {
		address, err := r.query(ctx, endpoint, recordType)
		if err == nil {
			return address, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// Do not print the custom URL/query; it may contain a private query key.
		failures = append(failures, fmt.Sprintf("来源%d：%v", i+1, err))
	}
	return "", fmt.Errorf("公网 IP 探测失败（%s）；保留原 DNS", strings.Join(failures, "；"))
}

func (r *IPResolver) query(ctx context.Context, endpoint, recordType string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || validateIPURL(u) != nil {
		return "", errors.New("接口 URL 不安全或无效")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", errors.New("无法构造 IP 请求")
	}
	req.Header.Set("Accept", "text/plain")
	req.Header.Set("User-Agent", "cfddns/1")
	resp, err := r.clients[recordType].Do(req)
	if err != nil {
		// net/url errors include the request URL. Keep credentials in custom query
		// parameters out of both the panel and journald.
		return "", errors.New("HTTPS 请求失败（检查网络、地址协议和系统 CA）")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP 状态 %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIPResponse+1))
	if err != nil {
		return "", errors.New("读取 IP 响应失败")
	}
	if len(body) > maxIPResponse {
		return "", errors.New("IP 响应超过大小限制")
	}
	value := strings.TrimSpace(string(body))
	// Cloudflare trace is a key=value format; a custom endpoint may also use it.
	for _, line := range strings.Split(value, "\n") {
		if strings.HasPrefix(line, "ip=") {
			value = strings.TrimSpace(strings.TrimPrefix(line, "ip="))
			break
		}
	}
	address, err := netip.ParseAddr(value)
	if err != nil || !validForType(address, recordType) || !isPublicAddr(address) {
		return "", errors.New("接口未返回对应协议的有效公网 IP")
	}
	return address.String(), nil
}

type InterfaceInfo struct {
	Name      string
	Addresses []string
}

func ListInterfaces() ([]InterfaceInfo, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("读取网卡失败：%w", err)
	}
	var result []InterfaceInfo
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		info := InterfaceInfo{Name: iface.Name}
		for _, recordType := range []string{"A", "AAAA"} {
			addresses, err := interfaceCandidates(iface.Name, recordType)
			if err != nil {
				continue
			}
			for _, address := range addresses {
				info.Addresses = append(info.Addresses, address.Address.String())
			}
		}
		sort.Strings(info.Addresses)
		result = append(result, info)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

type addressCandidate struct {
	Address   netip.Addr
	Temporary bool
}

func genericInterfaceCandidates(name, recordType string) ([]addressCandidate, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, fmt.Errorf("网卡 %s 不存在", name)
	}
	if iface.Flags&net.FlagUp == 0 {
		return nil, fmt.Errorf("网卡 %s 未启用", name)
	}
	addresses, err := iface.Addrs()
	if err != nil {
		return nil, fmt.Errorf("读取网卡地址失败：%w", err)
	}
	var candidates []addressCandidate
	for _, address := range addresses {
		prefix, err := netip.ParsePrefix(address.String())
		if err == nil && validForType(prefix.Addr(), recordType) && isPublicAddr(prefix.Addr()) {
			candidates = append(candidates, addressCandidate{Address: prefix.Addr()})
		}
	}
	return candidates, nil
}

func selectAddress(candidates []addressCandidate, recordType, previous string) (string, error) {
	filtered := make([]addressCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if validForType(candidate.Address, recordType) && isPublicAddr(candidate.Address) {
			filtered = append(filtered, candidate)
		}
	}
	old, _ := netip.ParseAddr(previous)
	for _, candidate := range filtered {
		if candidate.Address == old {
			return old.String(), nil
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].Temporary != filtered[j].Temporary {
			return !filtered[i].Temporary
		}
		return filtered[i].Address.Compare(filtered[j].Address) < 0
	})
	if len(filtered) == 0 {
		return "", errors.New("网卡没有可用的对应协议公网地址；保留原 DNS")
	}
	return filtered[0].Address.String(), nil
}

func validForType(address netip.Addr, recordType string) bool {
	return address.IsValid() && address.Zone() == "" && !address.Is4In6() && ((recordType == "A" && address.Is4()) || (recordType == "AAAA" && address.Is6()))
}

var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	// IPv6 currently allocated global unicast is 2000::/3; reject the
	// special-use assignments within it (documentation, ORCHID, benchmarking,
	// protocol assignments, deprecated 6to4, and the new documentation range).
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
}

func isPublicAddr(address netip.Addr) bool {
	if !address.IsValid() || address.Zone() != "" || address.Is4In6() || !address.IsGlobalUnicast() || address.IsPrivate() {
		return false
	}
	if address.Is6() && !netip.MustParsePrefix("2000::/3").Contains(address) {
		return false
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func IsPublicIP(value string) bool {
	address, err := netip.ParseAddr(value)
	return err == nil && isPublicAddr(address)
}
