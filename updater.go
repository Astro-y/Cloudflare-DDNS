package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const updateRepository = "https://github.com/Astro-y/Cloudflare-DDNS"
const maxUpdateMetadata = 2 << 20
const maxUpdateManifest = 16 << 10
const maxUpdateBinary = 64 << 20

type ReleaseInfo struct {
	Tag         string
	URL         string
	BinaryURL   string
	ChecksumURL string
}

type updaterClient struct {
	httpClient   *http.Client
	apiURL       string
	releaseBase  string
	pageBase     string
	architecture string
	system       string
	validURL     func(*url.URL) bool
}

func trustedUpdateURL(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" || u.Opaque != "" || u.Port() != "" {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "api.github.com", "github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com", "github-releases.githubusercontent.com":
		return true
	default:
		return false
	}
}

func newUpdaterClient() *updaterClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Updates use their own anonymous GitHub transport. Users may still route
	// GitHub downloads through an environment-configured HTTP(S) proxy.
	transport.Proxy = http.ProxyFromEnvironment
	transport.TLSHandshakeTimeout = 15 * time.Second
	transport.ResponseHeaderTimeout = 20 * time.Second
	transport.MaxResponseHeaderBytes = 64 << 10
	transport.IdleConnTimeout = 30 * time.Second
	return &updaterClient{
		httpClient:   &http.Client{Transport: transport, Timeout: 3 * time.Minute},
		apiURL:       "https://api.github.com/repos/Astro-y/Cloudflare-DDNS/releases/latest",
		releaseBase:  updateRepository + "/releases/download",
		pageBase:     updateRepository + "/releases/tag",
		architecture: runtime.GOARCH,
		system:       runtime.GOOS,
		validURL:     trustedUpdateURL,
	}
}

var stableReleaseVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func releaseVersionParts(value string) ([3]uint64, error) {
	var parts [3]uint64
	value = strings.TrimPrefix(value, "v")
	if len(value) > 64 || !stableReleaseVersion.MatchString(value) {
		return parts, errors.New("版本必须为 X.Y.Z 或 vX.Y.Z 格式的正式版本")
	}
	for index, number := range strings.Split(value, ".") {
		parsed, err := strconv.ParseUint(number, 10, 64)
		if err != nil {
			return parts, errors.New("版本号超出支持范围")
		}
		parts[index] = parsed
	}
	return parts, nil
}

// CompareReleaseVersions compares numbers, so v0.10.0 correctly follows v0.9.9.
// A local development build may be replaced with a valid published release.
func CompareReleaseVersions(current, target string) (int, error) {
	to, err := releaseVersionParts(target)
	if err != nil {
		return 0, err
	}
	if current == "dev" {
		return -1, nil
	}
	from, err := releaseVersionParts(current)
	if err != nil {
		return 0, err
	}
	for index := range from {
		if from[index] < to[index] {
			return -1, nil
		}
		if from[index] > to[index] {
			return 1, nil
		}
	}
	return 0, nil
}

func (u *updaterClient) assetName() (string, error) {
	if u.system != "linux" || (u.architecture != "amd64" && u.architecture != "arm64") {
		return "", errors.New("自动更新仅支持 Linux amd64 / arm64")
	}
	return "cfddns-linux-" + u.architecture, nil
}

func (u *updaterClient) infoForTag(tag string) (ReleaseInfo, error) {
	asset, err := u.assetName()
	if err != nil {
		return ReleaseInfo{}, err
	}
	if !strings.HasPrefix(tag, "v") {
		return ReleaseInfo{}, errors.New("GitHub Release 标签必须为 vX.Y.Z 格式的正式版本")
	}
	if _, err := releaseVersionParts(tag); err != nil {
		return ReleaseInfo{}, err
	}
	base := u.releaseBase + "/" + tag
	return ReleaseInfo{Tag: tag, URL: u.pageBase + "/" + tag, BinaryURL: base + "/" + asset, ChecksumURL: base + "/SHA256SUMS"}, nil
}

func (u *updaterClient) response(ctx context.Context, address string) (*http.Response, error) {
	parsed, err := url.Parse(address)
	if err != nil || u.validURL == nil || !u.validURL(parsed) {
		return nil, errors.New("更新地址不属于受信任的 GitHub HTTPS 地址")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, errors.New("无法构造更新请求")
	}
	request.Header.Set("Accept", "application/vnd.github+json, application/octet-stream;q=0.9")
	request.Header.Set("User-Agent", "cfddns-updater")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := *u.httpClient
	client.Jar = nil
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !u.validURL(next.URL) {
			return errors.New("更新重定向不安全或次数过多")
		}
		return nil
	}
	response, err := client.Do(request)
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("更新请求取消或超时：%w", ctx.Err())
		}
		return nil, errors.New("GitHub 更新请求失败，请检查 HTTPS 网络及系统 CA")
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		switch response.StatusCode {
		case http.StatusNotFound:
			return nil, errors.New("GitHub 正式版或对应附件尚未发布")
		case http.StatusForbidden, http.StatusTooManyRequests:
			return nil, errors.New("GitHub 请求受限，请稍后重试")
		default:
			return nil, fmt.Errorf("GitHub 更新请求失败（HTTP %d）", response.StatusCode)
		}
	}
	return response, nil
}

func (u *updaterClient) read(ctx context.Context, address string, limit int64) ([]byte, error) {
	response, err := u.response(ctx, address)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.ContentLength > limit {
		return nil, errors.New("GitHub 更新响应超过大小限制")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("GitHub 更新响应读取失败")
	}
	if int64(len(data)) > limit {
		return nil, errors.New("GitHub 更新响应超过大小限制")
	}
	return data, nil
}

func CheckLatestRelease(ctx context.Context) (ReleaseInfo, error) {
	return newUpdaterClient().check(ctx)
}

func (u *updaterClient) check(ctx context.Context) (ReleaseInfo, error) {
	asset, err := u.assetName()
	if err != nil {
		return ReleaseInfo{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, err := u.read(ctx, u.apiURL, maxUpdateMetadata)
	if err != nil {
		return ReleaseInfo{}, err
	}
	var metadata struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"assets"`
	}
	if json.Unmarshal(data, &metadata) != nil {
		return ReleaseInfo{}, errors.New("GitHub Release 响应不是有效 JSON")
	}
	if metadata.Draft || metadata.Prerelease {
		return ReleaseInfo{}, errors.New("自动更新只接受已发布的正式版本")
	}
	info, err := u.infoForTag(metadata.Tag)
	if err != nil {
		return ReleaseInfo{}, err
	}
	counts := map[string]int{asset: 0, "SHA256SUMS": 0}
	for _, item := range metadata.Assets {
		if _, needed := counts[item.Name]; needed {
			if item.State != "" && item.State != "uploaded" {
				return ReleaseInfo{}, errors.New("更新附件尚未完成上传")
			}
			counts[item.Name]++
		}
	}
	if counts[asset] != 1 || counts["SHA256SUMS"] != 1 {
		return ReleaseInfo{}, errors.New("正式版缺少唯一的对应架构程序或 SHA256SUMS 附件")
	}
	return info, nil
}

func updateChecksum(manifest []byte, asset string) ([sha256.Size]byte, error) {
	var expected [sha256.Size]byte
	scanner := bufio.NewScanner(bytes.NewReader(manifest))
	matches := 0
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.TrimPrefix(fields[len(fields)-1], "*") != asset {
			continue
		}
		matches++
		if len(fields) != 2 || len(fields[0]) != sha256.Size*2 {
			return expected, errors.New("对应程序的 SHA-256 条目无效")
		}
		decoded, err := hex.DecodeString(fields[0])
		if err != nil {
			return expected, errors.New("对应程序的 SHA-256 条目无效")
		}
		copy(expected[:], decoded)
	}
	if scanner.Err() != nil || matches != 1 {
		return expected, errors.New("校验文件没有唯一、有效的对应架构条目")
	}
	return expected, nil
}

func DownloadRelease(ctx context.Context, release ReleaseInfo) (string, func(), error) {
	return newUpdaterClient().download(ctx, release)
}

func (u *updaterClient) download(ctx context.Context, release ReleaseInfo) (string, func(), error) {
	noop := func() {}
	expectedRelease, err := u.infoForTag(release.Tag)
	if err != nil {
		return "", noop, err
	}
	if release != expectedRelease {
		return "", noop, errors.New("更新附件地址与受信任的仓库版本不匹配")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	asset, _ := u.assetName()
	manifest, err := u.read(ctx, release.ChecksumURL, maxUpdateManifest)
	if err != nil {
		return "", noop, err
	}
	expected, err := updateChecksum(manifest, asset)
	if err != nil {
		return "", noop, err
	}
	dir, err := os.MkdirTemp("", "cfddns-update-*")
	if err != nil {
		return "", noop, errors.New("无法创建更新临时目录")
	}
	path := filepath.Join(dir, asset)
	cleanup := func() { _ = os.Remove(path); _ = os.Remove(dir) }
	if err := os.Chmod(dir, 0700); err != nil {
		cleanup()
		return "", noop, errors.New("无法保护更新临时目录")
	}
	if err := u.downloadBinary(ctx, release.BinaryURL, path, expected); err != nil {
		cleanup()
		return "", noop, err
	}
	return path, cleanup, nil
}

func (u *updaterClient) downloadBinary(ctx context.Context, address, path string, expected [sha256.Size]byte) error {
	response, err := u.response(ctx, address)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.ContentLength > maxUpdateBinary {
		return errors.New("更新程序超过 64 MiB 限制")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("无法保存更新程序")
	}
	defer file.Close()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, maxUpdateBinary+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("更新程序下载中断")
	}
	if written > maxUpdateBinary {
		return errors.New("更新程序超过 64 MiB 限制")
	}
	if !bytes.Equal(hash.Sum(nil), expected[:]) {
		return errors.New("更新程序 SHA-256 校验不匹配，未替换已安装程序")
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := file.Sync(); err != nil {
		return errors.New("无法同步更新程序到磁盘")
	}
	if err := file.Close(); err != nil {
		return errors.New("无法保存更新程序")
	}
	if err := os.Chmod(path, 0755); err != nil {
		return errors.New("无法设置更新程序执行权限")
	}
	return nil
}

func installDownloadedRelease(ctx context.Context, paths Paths, store *Store, cfg Config, credentials Credentials, artifact string, systemd bool, prior ServiceStatus, run func(context.Context, ...string) error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := validateCredentials(credentials); err != nil {
		return err
	}
	return installFiles(ctx, paths, store, cfg, credentials.Token, systemd, prior, func() (string, error) { return artifact, nil }, run, credentials.AccountID)
}
