package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxCFResponse = 2 << 20

// A daemon reloads credentials/configuration each round. Keep rate limits across
// short-lived clients without keeping an additional plaintext copy of the token.
var cfRateLimits = struct {
	sync.Mutex
	until map[[32]byte]time.Time
}{until: make(map[[32]byte]time.Time)}

func (c *CFClient) rateKey() [32]byte { return sha256.Sum256([]byte(c.BaseURL + "\x00" + c.token)) }

func waitCFRateLimit(ctx context.Context, key [32]byte) error {
	for {
		cfRateLimits.Lock()
		until := cfRateLimits.until[key]
		if !until.After(time.Now()) {
			delete(cfRateLimits.until, key)
		}
		cfRateLimits.Unlock()
		if !until.After(time.Now()) {
			return nil
		}
		if err := sleepContext(ctx, time.Until(until)); err != nil {
			return err
		}
	}
}

func setCFRateLimit(key [32]byte, delay time.Duration) {
	cfRateLimits.Lock()
	defer cfRateLimits.Unlock()
	until := time.Now().Add(delay)
	if until.After(cfRateLimits.until[key]) {
		cfRateLimits.until[key] = until
	}
}

// CFClient only sends its token to the configured Cloudflare API, never an IP source.
type CFClient struct {
	HTTPClient *http.Client
	BaseURL    string
	token      string
}

type APIError struct {
	StatusCode int
	Codes      []int
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	message := fmt.Sprintf("Cloudflare API 请求失败（HTTP %d", e.StatusCode)
	if len(e.Codes) > 0 {
		message += fmt.Sprintf("，错误码 %v", e.Codes)
	}
	if e.StatusCode == http.StatusTooManyRequests {
		message += "，请求受限"
	}
	if e.StatusCode == http.StatusUnauthorized || e.StatusCode == http.StatusForbidden {
		message += "，请检查 Token 和 Zone Read / DNS Edit 权限"
	}
	return message + "）"
}

func IsNotFound(err error) bool {
	var api *APIError
	if !errors.As(err, &api) {
		return false
	}
	if api.StatusCode == http.StatusNotFound {
		return true
	}
	for _, code := range api.Codes {
		if code == 81044 {
			return true
		}
	}
	return false
}

func NewCFClient(token string) *CFClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 15 * time.Second
	transport.MaxResponseHeaderBytes = 64 << 10
	return &CFClient{
		BaseURL: "https://api.cloudflare.com/client/v4",
		token:   strings.TrimSpace(token),
		HTTPClient: &http.Client{Transport: transport, Timeout: 20 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return errors.New("Cloudflare API 不允许重定向") }},
	}
}

type cfEnvelope struct {
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
	Errors  []struct {
		Code int `json:"code"`
	} `json:"errors"`
	ResultInfo struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
		Count      int `json:"count"`
	} `json:"result_info"`
}

func retryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 {
		if seconds > int64(time.Duration(1<<63-1)/time.Second) {
			return time.Duration(1<<63 - 1)
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date.Sub(now)
	}
	return time.Second
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *CFClient) request(ctx context.Context, method, path string, body any) (cfEnvelope, error) {
	var empty cfEnvelope
	if c.token == "" {
		return empty, errors.New("未配置 Cloudflare API Token")
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return empty, errors.New("无法编码 Cloudflare API 请求")
	}
	if body == nil {
		encoded = nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	client := c.HTTPClient
	if client == nil {
		client = NewCFClient("").HTTPClient
	}
	// Copy the client so test/custom clients cannot accidentally forward credentials.
	safeClient := *client
	safeClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return errors.New("不允许 API 重定向") }
	if safeClient.Timeout == 0 {
		safeClient.Timeout = 20 * time.Second
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := waitCFRateLimit(ctx, c.rateKey()); err != nil {
			return empty, fmt.Errorf("Cloudflare API 等待限流结束：%w", err)
		}
		req, reqErr := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(encoded))
		if reqErr != nil {
			return empty, errors.New("Cloudflare API 地址无效")
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "cfddns/1")
		response, doErr := safeClient.Do(req)
		if doErr != nil {
			if response != nil && response.Body != nil {
				response.Body.Close()
			}
			if ctx.Err() != nil {
				return empty, fmt.Errorf("Cloudflare API 请求取消或超时：%w", ctx.Err())
			}
			if method != http.MethodPost && attempt < 2 {
				if err = sleepContext(ctx, time.Duration(1<<attempt)*time.Second); err != nil {
					return empty, err
				}
				continue
			}
			// Transport errors can contain URLs or reflected secrets: do not print them.
			return empty, errors.New("Cloudflare API 网络请求失败或超时（HTTPS 证书仍会验证）")
		}
		data, readErr := io.ReadAll(io.LimitReader(response.Body, maxCFResponse+1))
		response.Body.Close()
		if len(data) > maxCFResponse {
			return empty, errors.New("Cloudflare API 响应超过 2 MiB 限制")
		}
		var envelope cfEnvelope
		decodeErr := json.Unmarshal(data, &envelope)
		if response.StatusCode >= 200 && response.StatusCode < 300 && readErr == nil && decodeErr == nil && envelope.Success {
			return envelope, nil
		}
		apiErr := &APIError{StatusCode: response.StatusCode}
		for _, item := range envelope.Errors {
			apiErr.Codes = append(apiErr.Codes, item.Code)
		}
		if response.StatusCode == http.StatusTooManyRequests {
			apiErr.RetryAfter = retryAfter(response.Header.Get("Retry-After"), time.Now())
			setCFRateLimit(c.rateKey(), apiErr.RetryAfter)
		}
		canRetry := response.StatusCode == http.StatusTooManyRequests || (method != http.MethodPost && (response.StatusCode >= 500 || readErr != nil))
		if canRetry && attempt < 2 {
			delay := time.Duration(1<<attempt) * time.Second
			if response.StatusCode == http.StatusTooManyRequests {
				delay = apiErr.RetryAfter
			}
			if err = sleepContext(ctx, delay); err != nil {
				return empty, fmt.Errorf("Cloudflare API 等待重试结束：%w", err)
			}
			continue
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 && (readErr != nil || decodeErr != nil) {
			return empty, errors.New("Cloudflare API 响应不完整或不是有效 JSON")
		}
		return empty, apiErr
	}
	return empty, errors.New("Cloudflare API 重试次数已用尽")
}

func (c *CFClient) VerifyToken(ctx context.Context) error {
	envelope, err := c.request(ctx, http.MethodGet, "/user/tokens/verify", nil)
	if err != nil {
		return err
	}
	var token struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(envelope.Result, &token) != nil || token.Status != "active" {
		return errors.New("Cloudflare Token 未处于 active 状态")
	}
	return nil
}

func (c *CFClient) ListZones(ctx context.Context) ([]Zone, error) {
	var zones []Zone
	for page := 1; page <= 10000; page++ {
		envelope, err := c.request(ctx, http.MethodGet, fmt.Sprintf("/zones?per_page=50&page=%d", page), nil)
		if err != nil {
			return nil, err
		}
		var batch []Zone
		if json.Unmarshal(envelope.Result, &batch) != nil {
			return nil, errors.New("Cloudflare Zone 响应无效")
		}
		zones = append(zones, batch...)
		if envelope.ResultInfo.TotalPages > 0 {
			if page >= envelope.ResultInfo.TotalPages {
				return zones, nil
			}
		} else if len(batch) < 50 {
			return zones, nil
		}
	}
	return nil, errors.New("Cloudflare Zone 分页超出安全限制")
}

func apiID(id string) (string, error) {
	if id == "" || len(id) > 128 {
		return "", errors.New("Cloudflare ID 无效")
	}
	for _, char := range id {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_') {
			return "", errors.New("Cloudflare ID 包含无效字符")
		}
	}
	return id, nil
}

func (c *CFClient) FindRecords(ctx context.Context, zoneID, name string) ([]DNSRecord, error) {
	id, err := apiID(zoneID)
	if err != nil {
		return nil, err
	}
	var records []DNSRecord
	for page := 1; page <= 10000; page++ {
		query := url.Values{"name": {name}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
		envelope, err := c.request(ctx, http.MethodGet, "/zones/"+id+"/dns_records?"+query.Encode(), nil)
		if err != nil {
			return nil, err
		}
		var batch []DNSRecord
		if json.Unmarshal(envelope.Result, &batch) != nil {
			return nil, errors.New("Cloudflare DNS 列表响应无效")
		}
		records = append(records, batch...)
		if envelope.ResultInfo.TotalPages > 0 {
			if page >= envelope.ResultInfo.TotalPages {
				return records, nil
			}
		} else if len(batch) < 100 {
			return records, nil
		}
	}
	return nil, errors.New("Cloudflare DNS 分页超出安全限制")
}

func (c *CFClient) GetRecord(ctx context.Context, zoneID, recordID string) (DNSRecord, error) {
	zone, err := apiID(zoneID)
	if err != nil {
		return DNSRecord{}, err
	}
	id, err := apiID(recordID)
	if err != nil {
		return DNSRecord{}, err
	}
	envelope, err := c.request(ctx, http.MethodGet, "/zones/"+zone+"/dns_records/"+id, nil)
	return decodeRecord(envelope, err)
}

func decodeRecord(envelope cfEnvelope, err error) (DNSRecord, error) {
	if err != nil {
		return DNSRecord{}, err
	}
	var record DNSRecord
	if json.Unmarshal(envelope.Result, &record) != nil || record.ID == "" {
		return DNSRecord{}, errors.New("Cloudflare DNS 记录响应无效")
	}
	return record, nil
}

func (c *CFClient) CreateRecord(ctx context.Context, zoneID string, record DNSRecord) (DNSRecord, error) {
	zone, err := apiID(zoneID)
	if err != nil {
		return DNSRecord{}, err
	}
	body := map[string]any{"type": record.Type, "name": record.Name, "content": record.Content, "ttl": record.TTL, "proxied": false}
	envelope, err := c.request(ctx, http.MethodPost, "/zones/"+zone+"/dns_records", body)
	return decodeRecord(envelope, err)
}

func (c *CFClient) PatchRecord(ctx context.Context, zoneID, recordID string, fields map[string]any) (DNSRecord, error) {
	zone, err := apiID(zoneID)
	if err != nil {
		return DNSRecord{}, err
	}
	id, err := apiID(recordID)
	if err != nil {
		return DNSRecord{}, err
	}
	for key, value := range fields {
		if key != "content" && key != "ttl" && key != "proxied" {
			return DNSRecord{}, errors.New("只允许修改 DNS 地址、TTL 和代理状态")
		}
		if key == "proxied" && value != false {
			return DNSRecord{}, errors.New("DDNS 记录必须使用仅 DNS 模式")
		}
	}
	envelope, err := c.request(ctx, http.MethodPatch, "/zones/"+zone+"/dns_records/"+id, fields)
	return decodeRecord(envelope, err)
}
