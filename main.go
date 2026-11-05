package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"reflect"
	"syscall"
	"time"

	"golang.org/x/term"
)

var version = "dev"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--internal-version" {
		fmt.Println(version)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store := NewStore(DefaultPaths())
	if len(os.Args) == 2 && os.Args[1] == "--internal-daemon" {
		if err := RequireAdmin(); err != nil {
			log.Print(err)
			os.Exit(1)
		}
		if err := runDaemon(ctx, store, log.Default()); err != nil {
			log.Print(err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "请直接运行 cfddns，所有管理操作均在中文面板中完成。")
		os.Exit(2)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Fprintln(os.Stderr, "CFDDNS 需要交互终端。请在 SSH 终端直接输入 cfddns；不要通过管道输入凭据。")
		os.Exit(2)
	}
	p := NewPanel(store, os.Stdin, os.Stdout)
	if err := p.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "面板退出：", err)
		os.Exit(1)
	}
}

func synchronize(ctx context.Context, store *Store, force bool) ([]SyncResult, error) {
	unlock, err := store.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	cfg, err := store.LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("读取配置：%w", err)
	}
	credentials, err := store.LoadCredentials()
	if err != nil {
		return nil, fmt.Errorf("读取凭据：%w", err)
	}
	state, err := store.LoadState()
	if err != nil {
		return nil, fmt.Errorf("读取状态：%w", err)
	}
	engine := SyncEngine{API: NewCFClient(credentials.Token, credentials.AccountID), Resolver: NewIPResolver()}
	previousConfig := cfg
	previousConfig.Records = append([]RecordConfig(nil), cfg.Records...)
	results := engine.Sync(ctx, &cfg, &state, force)
	// Record IDs discovered by the engine must survive a restart.
	if !reflect.DeepEqual(previousConfig, cfg) {
		if err := store.SaveConfig(cfg); err != nil {
			return results, fmt.Errorf("保存记录 ID：%w", err)
		}
	}
	if err := store.SaveState(state); err != nil {
		return results, fmt.Errorf("保存状态：%w", err)
	}
	return results, nil
}

func runDaemon(ctx context.Context, store *Store, logger *log.Logger) error {
	logger.Printf("CFDDNS %s 启动", version)
	first := true
	for {
		if ctx.Err() != nil {
			return nil
		}
		results, err := synchronize(ctx, store, first)
		first = false
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			logger.Printf("检查失败：%v", err)
		} else {
			for _, r := range results {
				if r.Err != nil {
					logger.Printf("%s 同步失败：%v", r.Type, r.Err)
					if r.Changed {
						logger.Printf("%s 云端已发生部分变更，等待后续核对", r.Type)
					}
				} else if r.Changed {
					logger.Printf("%s 已更新：%s", r.Type, r.Address)
				}
			}
		}
		interval := time.Minute
		if cfg, err := store.LoadConfig(); err == nil {
			interval = time.Duration(cfg.IntervalSeconds) * time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
