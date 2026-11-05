package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Paths are fixed for the installed program, and may be replaced in isolated tests.
type Paths struct {
	ConfigDir string
	StateDir  string
	Binary    string
	UnitFile  string
}

func DefaultPaths() Paths {
	return Paths{ConfigDir: "/etc/cfddns", StateDir: "/var/lib/cfddns", Binary: "/usr/local/bin/cfddns", UnitFile: "/etc/systemd/system/cfddns.service"}
}

func (p Paths) ConfigFile() string { return filepath.Join(p.ConfigDir, "config.json") }
func (p Paths) TokenFile() string  { return filepath.Join(p.ConfigDir, "token") }
func (p Paths) StateFile() string  { return filepath.Join(p.StateDir, "state.json") }

type Store struct{ Paths Paths }

// Credentials are kept together in the private token file so a crash cannot
// leave a Token paired with a different account's verification endpoint.
type Credentials struct {
	Version   int    `json:"version"`
	Token     string `json:"token"`
	AccountID string `json:"account_id,omitempty"`
}

func NewStore(paths Paths) *Store { return &Store{Paths: paths} }

func (s *Store) LoadConfig() (Config, error) {
	var cfg Config
	if err := readJSON(s.Paths.ConfigFile(), &cfg); err != nil {
		return cfg, err
	}
	return cfg, ValidateConfig(cfg)
}

func (s *Store) SaveConfig(cfg Config) error {
	if err := ValidateConfig(cfg); err != nil {
		return err
	}
	return writeJSON(s.Paths.ConfigFile(), cfg)
}

func (s *Store) LoadState() (State, error) {
	state := State{Version: configVersion, Records: make(map[string]*RecordState)}
	if err := readJSON(s.Paths.StateFile(), &state); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state, nil
		}
		return state, err
	}
	if state.Version != configVersion {
		return state, fmt.Errorf("不支持的状态版本 %d", state.Version)
	}
	if state.Records == nil {
		state.Records = make(map[string]*RecordState)
	}
	return state, nil
}

func (s *Store) SaveState(state State) error {
	state.Version = configVersion
	return writeJSON(s.Paths.StateFile(), state)
}

func (s *Store) LoadToken() (string, error) {
	credentials, err := s.LoadCredentials()
	return credentials.Token, err
}

func (s *Store) LoadCredentials() (Credentials, error) {
	// JSON escaping can make the serialized form larger than the Token itself.
	b, err := readPrivateFile(s.Paths.TokenFile(), 32<<10)
	if err != nil {
		return Credentials{}, err
	}
	defer clear(b)
	data := bytes.TrimSpace(b)
	credentials := Credentials{}
	if len(data) > 0 && (data[0] == '{' || data[0] == '[') {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&credentials) != nil || decoder.Decode(new(any)) != io.EOF {
			// Decoder errors can repeat input text; do not expose secret file data.
			return Credentials{}, errors.New("凭据文件格式无效，请在面板中重新设置 Token")
		}
	} else {
		// v0.1.0 stored a plain-text user Token with no account metadata.
		credentials = Credentials{Version: 1, Token: string(data)}
	}
	if err := validateCredentials(credentials); err != nil {
		return Credentials{}, err
	}
	return credentials, nil
}

func (s *Store) SaveToken(token string) error {
	return s.SaveCredentials(Credentials{Version: 1, Token: token})
}

func (s *Store) SaveCredentials(credentials Credentials) error {
	if err := validateCredentials(credentials); err != nil {
		return err
	}
	b, err := json.MarshalIndent(credentials, "", "  ")
	if err != nil {
		return errors.New("无法编码凭据文件")
	}
	b = append(b, '\n')
	defer clear(b)
	return atomicWrite(s.Paths.TokenFile(), b, 0600, true)
}

func validateCredentials(credentials Credentials) error {
	if credentials.Version != 1 {
		return errors.New("不支持的凭据文件版本")
	}
	if err := validateTokenText(credentials.Token); err != nil {
		return err
	}
	return ValidateAccountID(credentials.AccountID)
}

func validateTokenText(token string) error {
	if len(token) == 0 || len(token) > 4095 || strings.ContainsAny(token, " \r\n\t\x00") {
		return errors.New("API Token 为空或包含无效字符")
	}
	return nil
}

// Acquire serializes complete read-modify-write operations, including network sync.
// The lock file is never unlinked, even during uninstall, to preserve inode identity.
func (s *Store) Acquire(ctx context.Context) (func(), error) {
	if err := ensurePrivateDir(s.Paths.ConfigDir); err != nil {
		return nil, err
	}
	f, err := openLockFile(filepath.Join(s.Paths.ConfigDir, "operation.lock"))
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		locked, err := tryFileLock(f)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("获取进程锁: %w", err)
		}
		if locked {
			return func() { unlockFile(f); _ = f.Close() }, nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

var domainLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func validDomain(name string) bool {
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if !domainLabel.MatchString(label) {
			return false
		}
	}
	return true
}

// NormalizeDomain supports @, a single relative label, and a fully qualified name.
// Internationalized domains must first be entered in ASCII/Punycode form.
func NormalizeDomain(input, zone string) (string, error) {
	zone = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(zone), "."))
	if !validDomain(zone) {
		return "", errors.New("Zone 域名无效，请使用 ASCII/Punycode 域名")
	}
	name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(input), "."))
	if name == "@" || name == "" {
		name = zone
	} else if !strings.Contains(name, ".") && name != zone {
		name += "." + zone
	}
	if !validDomain(name) || (name != zone && !strings.HasSuffix(name, "."+zone)) {
		return "", fmt.Errorf("域名必须位于 %s 下；可输入 @、子域标签或完整域名", zone)
	}
	return name, nil
}

func ValidateConfig(cfg Config) error {
	if cfg.Version != configVersion {
		return fmt.Errorf("不支持的配置版本 %d", cfg.Version)
	}
	if cfg.IntervalSeconds < 30 || cfg.IntervalSeconds > 86400 {
		return errors.New("检查间隔必须为 30 秒至 24 小时")
	}
	if cfg.TTL != 1 && (cfg.TTL < 60 || cfg.TTL > 86400) {
		return errors.New("TTL 必须为 Auto (1) 或 60 至 86400 秒")
	}
	if len(cfg.Records) == 0 || len(cfg.Records) > 2 {
		return errors.New("必须配置一至两条记录，最多一条 A 和一条 AAAA")
	}
	seen := map[string]bool{}
	for _, record := range cfg.Records {
		if (record.Type != "A" && record.Type != "AAAA") || seen[record.Type] {
			return errors.New("最多配置一条 A 和一条 AAAA 记录")
		}
		seen[record.Type] = true
		if !identifier.MatchString(record.ZoneID) || (record.RecordID != "" && !identifier.MatchString(record.RecordID)) {
			return errors.New("Zone ID 或 Record ID 无效")
		}
		name, err := NormalizeDomain(record.Name, record.ZoneName)
		if err != nil || name != record.Name || record.ZoneName != strings.ToLower(record.ZoneName) {
			return fmt.Errorf("%s 域名配置无效", record.Type)
		}
		if err := ValidateSource(record.Source); err != nil {
			return fmt.Errorf("%s IP 来源无效: %w", record.Type, err)
		}
	}
	return nil
}

func readJSON(path string, out any) error {
	b, err := readPrivateFile(path, 1<<20)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return fmt.Errorf("读取 %s: 无效 JSON: %w", filepath.Base(path), err)
	}
	if d.Decode(new(any)) != io.EOF {
		return fmt.Errorf("读取 %s: 文件包含多余 JSON 数据", filepath.Base(path))
	}
	return nil
}

func writeJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(b, '\n'), 0600, true)
}

func readPrivateFile(path string, max int64) ([]byte, error) {
	if err := rejectSymlinkPath(path); err != nil {
		return nil, err
	}
	f, err := openReadFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, errors.New("配置路径不是普通文件")
	}
	if st.Size() > max {
		return nil, errors.New("配置文件超过大小限制")
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("配置文件超过大小限制")
	}
	return b, nil
}

func ensurePrivateDir(path string) error {
	if err := rejectSymlinkPath(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return os.Chmod(path, 0700)
}

func rejectSymlinkPath(path string) error {
	full, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for current := full; ; current = filepath.Dir(current) {
		st, err := os.Lstat(current)
		if err == nil && st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("拒绝符号链接路径: %s", current)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return nil
}

func atomicWrite(path string, data []byte, mode os.FileMode, privateDir bool) error {
	dir := filepath.Dir(path)
	if privateDir {
		if err := ensurePrivateDir(dir); err != nil {
			return err
		}
	}
	if err := rejectSymlinkPath(path); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".cfddns-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := replaceFile(tmp, path); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func removeKnownFile(path string) error {
	if err := rejectSymlinkPath(path); err != nil {
		return err
	}
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("拒绝删除非普通文件: %s", filepath.Base(path))
	}
	return os.Remove(path)
}
