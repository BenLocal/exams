// Command exams runs the exam-announcement aggregator.
//
// Subcommands:
//
//	serve              run the web UI and the crawl scheduler (default)
//	migrate            apply migrations/*.sql
//	collect [key...]   crawl once, synchronously, writing to the database
//	probe <key>        fetch and parse a source without touching the database
//	sources            list registered collectors and their database state
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/BenLocal/exams/internal/collector"
	"github.com/BenLocal/exams/internal/collector/lua"
	"github.com/BenLocal/exams/internal/config"
	"github.com/BenLocal/exams/internal/crawl"
	"github.com/BenLocal/exams/internal/model"
	"github.com/BenLocal/exams/internal/notify"
	"github.com/BenLocal/exams/internal/scheduler"
	"github.com/BenLocal/exams/internal/store"
	"github.com/BenLocal/exams/internal/web"
	"github.com/cloudwego/hertz/pkg/app/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\n错误：%v\n", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	setupLogging(cfg.LogLevel)

	// Load Lua collectors before anything reads the registry, so they appear
	// in the sources table and in `exams probe` exactly like built-in ones.
	// A broken script is reported and skipped rather than taking the service
	// down.
	loadPlugins(cfg)

	ctx := context.Background()
	switch cmd {
	case "serve":
		return cmdServe(ctx, cfg)
	case "migrate":
		return cmdMigrate(ctx, cfg)
	case "collect":
		return cmdCollect(ctx, cfg, args)
	case "probe":
		return cmdProbe(ctx, cfg, args)
	case "sources":
		return cmdSources(ctx, cfg)
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("未知子命令 %q", cmd)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `用法：exams [子命令] [参数]

子命令：
  serve              启动 Web 服务与定时抓取（默认）
  migrate            执行 migrations/ 下的数据库迁移
  collect [标识...]   立即抓取一次（写入数据库）；不带标识则抓取全部已启用数据源
  probe <标识>        抓取并解析单个数据源，不写数据库；用于校正选择器
  sources            列出已注册的采集器及其在数据库中的状态

probe 参数：
  --dump-html <目录>   把抓取到的原始页面保存到目录，便于离线比对选择器
  --with-detail       额外抓取第一条的详情页，用于校验正文选择器
  --limit <n>         最多显示多少条（默认 10）

示例：
  exams probe demo
  exams probe neea --dump-html ./tmp/neea --with-detail
`)
}

func setupLogging(level string) {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv})))
}

// ---------------------------------------------------------------------------

func cmdServe(ctx context.Context, cfg *config.Config) error {
	if err := cfg.RequireDB(); err != nil {
		return err
	}
	log := slog.Default()

	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	// The pool is closed after Spin returns, never from an OnShutdown hook:
	// those run before the listener closes and are abandoned after roughly
	// five seconds, which would cut off in-flight requests and crawls.
	defer st.Close()

	// Serve does not migrate. Migrations are an explicit, reviewable step, so
	// a schema problem surfaces as one clear message rather than a stream of
	// "relation does not exist" errors from whichever query runs first.
	pending, err := st.PendingRequired(ctx)
	if err != nil {
		return fmt.Errorf("检查数据库 schema 失败：%w", err)
	}
	if len(pending) > 0 {
		return fmt.Errorf("数据库 schema 未就绪，缺少迁移：%s\n\n请先执行：\n    exams migrate",
			strings.Join(pending, ", "))
	}

	if err := st.SyncSources(ctx, registrySources()); err != nil {
		return fmt.Errorf("同步数据源失败：%w", err)
	}

	fetcher, err := collector.NewFetcher(cfg.UserAgent, cfg.HTTPProxy, cfg.CrawlTimeout, cfg.Location)
	if err != nil {
		return err
	}

	runner := crawl.New(st, fetcher, log, crawl.Options{
		MaxItems:       cfg.CrawlMaxItems,
		MaxDetails:     cfg.CrawlMaxDetails,
		Timeout:        cfg.CrawlTimeout,
		NotifyOnUpdate: cfg.NotifyOnUpdate,
	}, buildDispatcher(cfg, st, log))

	sched, err := scheduler.New(st, runner, log, scheduler.Options{
		GlobalCron:  cfg.CrawlCron,
		RunOnStart:  cfg.CrawlOnStart,
		Concurrency: cfg.CrawlConcurrency,
	}, cfg.Location)
	if err != nil {
		return err
	}
	if err := sched.Start(ctx); err != nil {
		return fmt.Errorf("启动调度器失败：%w", err)
	}

	webSrv, err := web.New(st, sched, log)
	if err != nil {
		return err
	}

	h := server.Default(
		server.WithHostPorts(cfg.Addr),
		server.WithExitWaitTime(20*time.Second),
	)
	webSrv.Register(h)

	log.Info("服务启动",
		"addr", cfg.Addr,
		"timezone", cfg.LocationName,
		"cron", cfg.CrawlCron,
		"sources", strings.Join(collector.Keys(), ", "))
	if cfg.NotifyEnabled {
		log.Info("通知已启用", "on_update", cfg.NotifyOnUpdate)
	} else {
		log.Info("通知未启用（NOTIFY_ENABLED=false）")
	}
	if isPubliclyBound(cfg.Addr) {
		log.Warn("服务监听在所有网卡上且没有鉴权，请勿直接暴露到公网；" +
			"建议设置 APP_ADDR=127.0.0.1:8080 或置于反向代理之后")
	}

	h.Spin()

	log.Info("正在停止…")
	stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Stop the scheduler before the pool closes: an in-flight crawl still
	// needs the database. Anything that overruns the deadline is left to the
	// stale-run reaper on the next start.
	sched.Stop(stopCtx)
	return nil
}

func cmdMigrate(ctx context.Context, cfg *config.Config) error {
	if err := cfg.RequireDB(); err != nil {
		return err
	}
	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	results, err := st.Migrate(ctx)
	if err != nil {
		// Report what did land before the failure, so a partial run is visible.
		reportMigrations(results)
		return err
	}
	reportMigrations(results)
	return nil
}

func reportMigrations(results []store.MigrationResult) {
	var applied, skipped, failed int
	for _, r := range results {
		switch {
		case r.Err != nil:
			failed++
			fmt.Fprintf(os.Stderr, "  跳过（可选） %s：%v\n", r.Version, r.Err)
		case r.Applied:
			applied++
			fmt.Printf("  已应用 %s\n", r.Version)
		default:
			skipped++
		}
	}
	fmt.Printf("\n迁移完成：应用 %d，已是最新 %d，可选跳过 %d\n", applied, skipped, failed)
	if failed > 0 {
		fmt.Fprint(os.Stderr,
			"\n可选迁移失败通常是因为当前数据库用户没有 CREATE EXTENSION 权限。\n"+
				"这只影响中文搜索的索引加速，功能不受影响。\n"+
				"如需启用，请让管理员执行：CREATE EXTENSION pg_trgm;\n"+
				"授权后重新运行 exams migrate 即可自动补上。\n")
	}
}

func cmdCollect(ctx context.Context, cfg *config.Config, args []string) error {
	if err := cfg.RequireDB(); err != nil {
		return err
	}
	log := slog.Default()

	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := st.SyncSources(ctx, registrySources()); err != nil {
		return fmt.Errorf("同步数据源失败：%w", err)
	}

	fetcher, err := collector.NewFetcher(cfg.UserAgent, cfg.HTTPProxy, cfg.CrawlTimeout, cfg.Location)
	if err != nil {
		return err
	}
	runner := crawl.New(st, fetcher, log, crawl.Options{
		MaxItems:       cfg.CrawlMaxItems,
		MaxDetails:     cfg.CrawlMaxDetails,
		Timeout:        cfg.CrawlTimeout,
		NotifyOnUpdate: cfg.NotifyOnUpdate,
	}, buildDispatcher(cfg, st, log))

	sched, err := scheduler.New(st, runner, log, scheduler.Options{
		GlobalCron:  cfg.CrawlCron,
		Concurrency: cfg.CrawlConcurrency,
	}, cfg.Location)
	if err != nil {
		return err
	}
	// Reap first so this run is not confused by a previous process's leftovers.
	if n, err := st.ReapStaleRuns(ctx); err == nil && n > 0 {
		log.Warn("清理了上次中断的抓取记录", "count", n)
	}

	runs, err := sched.RunNow(ctx, args)
	if err != nil {
		return err
	}

	fmt.Printf("\n%-12s %-8s %8s %8s %8s %8s  %s\n",
		"数据源", "状态", "抓到", "新增", "更新", "未变", "耗时")
	for _, r := range runs {
		fmt.Printf("%-12s %-8s %8d %8d %8d %8d  %dms\n",
			r.SourceKey, r.Status, r.Fetched, r.Inserted, r.Updated, r.Unchanged, r.DurationMS)
		if r.Error != "" {
			fmt.Printf("%14s错误：%s\n", "", r.Error)
		}
		if r.Note != "" {
			fmt.Printf("%14s说明：%s\n", "", r.Note)
		}
	}
	return nil
}

func cmdProbe(ctx context.Context, cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	dumpDir := fs.String("dump-html", "", "把抓取到的原始页面保存到该目录")
	withDetail := fs.Bool("with-detail", false, "额外抓取第一条的详情页以校验正文选择器")
	limit := fs.Int("limit", 10, "最多显示多少条")

	// Go's flag package stops parsing at the first non-flag argument, so
	// `probe neea --with-detail` would silently ignore the flag. Pull the
	// positional source key out first and parse the flags from what remains,
	// which makes both argument orders work.
	key, flagArgs := splitKeyAndFlags(args)
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	if extra := fs.Args(); len(extra) > 0 {
		return fmt.Errorf("无法识别的参数：%s", strings.Join(extra, " "))
	}
	if key == "" {
		return fmt.Errorf("用法：exams probe <数据源标识> [--dump-html 目录] [--with-detail]\n已注册：%s",
			strings.Join(collector.Keys(), ", "))
	}

	c, ok := collector.Get(key)
	if !ok {
		return fmt.Errorf("没有名为 %q 的采集器\n已注册：%s", key, strings.Join(collector.Keys(), ", "))
	}

	// probe does not need a database: it is the tool you reach for precisely
	// when something is broken.
	fetcher, err := collector.NewFetcher(cfg.UserAgent, cfg.HTTPProxy, cfg.CrawlTimeout, cfg.Location)
	if err != nil {
		return err
	}
	if *dumpDir != "" {
		if err := fetcher.SetDumpDir(*dumpDir); err != nil {
			return err
		}
	}

	fmt.Printf("数据源   %s (%s)\n", c.Name(), c.Key())
	fmt.Printf("基础地址 %s\n", c.BaseURL())
	if *dumpDir != "" {
		fmt.Printf("页面转储 %s\n", *dumpDir)
	}
	fmt.Println(strings.Repeat("-", 72))

	started := time.Now()
	items, err := c.List(ctx, fetcher)
	elapsed := time.Since(started)

	if err != nil {
		fmt.Fprintf(os.Stderr, "\n抓取失败（耗时 %s）：\n\n%v\n", elapsed.Round(time.Millisecond), err)
		fmt.Fprint(os.Stderr, `
排查建议：
  1. 用 --dump-html 保存真实页面：
       exams probe `+key+` --dump-html ./tmp/`+key+`
  2. 打开保存的 HTML，找到列表项的真实标签与 class
  3. 修改 internal/collector/`+key+`.go 中 listPageConfig 的 ItemSelector / LinkSelector
  4. 重新运行本命令确认
`)
		return errors.New("解析失败")
	}

	fmt.Printf("解析到 %d 条，耗时 %s\n\n", len(items), elapsed.Round(time.Millisecond))
	if len(items) == 0 {
		fmt.Fprintln(os.Stderr,
			"警告：解析结果为 0 条。这通常意味着选择器已失效，而不是真的没有公告。\n"+
				"用 --dump-html 保存页面后比对选择器。")
		return errors.New("解析到 0 条")
	}

	printItems(items, *limit)

	if *withDetail {
		d, ok := c.(collector.Detailer)
		if !ok {
			fmt.Printf("\n%s 没有实现 Detailer，正文由列表页直接提供。\n", key)
			return nil
		}
		fmt.Println(strings.Repeat("-", 72))
		fmt.Printf("详情页校验（第 1 条）\n")
		detailed, err := d.Detail(ctx, fetcher, items[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "详情页抓取失败：%v\n\n"+
				"请修改 internal/collector/%s.go 中的正文选择器常量。\n", err, key)
			return errors.New("正文选择器校验失败")
		}
		fmt.Printf("  正文长度 %d 字\n", len([]rune(detailed.Content)))
		fmt.Printf("  正文开头 %s\n", firstRunes(detailed.Content, 200))
	}
	return nil
}

func printItems(items []model.Item, limit int) {
	if limit > 0 && len(items) > limit {
		fmt.Printf("（仅显示前 %d 条，共 %d 条）\n", limit, len(items))
		items = items[:limit]
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "#\t发布日期\t分类\t标题")
	for i, it := range items {
		title := it.Title
		if len([]rune(title)) > 46 {
			title = string([]rune(title)[:46]) + "…"
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\n",
			i+1, model.FormatTime(it.PublishedAt), orDash(it.Category), title)
	}
	_ = w.Flush()

	fmt.Println()
	for i, it := range items {
		if i >= 3 {
			break
		}
		fmt.Printf("[%d] %s\n    %s\n", i+1, it.Title, it.URL)
		if it.PublishedRaw != "" {
			fmt.Printf("    原日期文本：%s\n", it.PublishedRaw)
		}
	}
}

func cmdSources(ctx context.Context, cfg *config.Config) error {
	if err := cfg.RequireDB(); err != nil {
		return fmt.Errorf("%w\n（数据源列表需要读取数据库；"+
			"若只想查看已注册的采集器，可用 exams probe <标识>）", err)
	}
	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	if err := st.SyncSources(ctx, registrySources()); err != nil {
		return err
	}
	sources, err := st.ListSources(ctx)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "标识\t名称\t启用\t已注册\t调度\t上次运行")
	for _, s := range sources {
		_, registered := collector.Get(s.Key)
		interval := s.Interval
		if interval == "" {
			interval = cfg.CrawlCron + "（默认）"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			s.Key, s.Name, yesNo(s.Enabled), yesNo(registered), interval,
			model.FormatTime(s.LastRunAt))
	}
	return w.Flush()
}

// splitKeyAndFlags separates the positional source key from the flags, so
// `probe neea --with-detail` and `probe --with-detail neea` both work.
//
// Go's flag package stops at the first positional argument, which would make
// flags placed after the key silently do nothing — a failure mode that looks
// like the flag is broken rather than ignored.
func splitKeyAndFlags(args []string) (key string, flags []string) {
	// Flags that consume the following token as their value. Without this the
	// value of `--limit 4` would be mistaken for the positional key.
	takesValue := map[string]bool{
		"-limit": true, "--limit": true,
		"-dump-html": true, "--dump-html": true,
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]

		if strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			// "--flag=value" carries its own value; a bare "--flag" takes the
			// next token.
			if takesValue[arg] && !strings.Contains(arg, "=") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}

		if key == "" {
			key = arg
			continue
		}
		flags = append(flags, arg)
	}
	return key, flags
}

// ---------------------------------------------------------------------------

func openStore(ctx context.Context, cfg *config.Config) (*store.Store, error) {
	maxConns := int32(cfg.CrawlConcurrency + 4)
	return store.Open(ctx, cfg.DatabaseURL, maxConns)
}

// loadPlugins compiles the Lua collectors in cfg.PluginsDir and registers
// them.
//
// Registration is skipped for a script whose key collides with an existing
// collector; those conflicts come back as errors and are logged with the file
// that caused them.
func loadPlugins(cfg *config.Config) {
	log := slog.Default()

	res, err := lua.LoadDir(cfg.PluginsDir, cfg.Location, log)
	if err != nil {
		log.Error("could not read the plugins directory; continuing without Lua sources",
			"dir", cfg.PluginsDir, "err", err)
		return
	}
	for _, e := range res.Errors {
		log.Error("lua plugin skipped", "err", e)
	}
	for _, c := range res.Sources {
		collector.Register(c)
		log.Info("lua plugin loaded", "key", c.Key(), "name", c.Name())
	}
}

// registrySources converts the collector registry into source rows.
func registrySources() []model.Source {
	all := collector.All()
	out := make([]model.Source, 0, len(all))
	for _, c := range all {
		out = append(out, model.Source{
			Key:     c.Key(),
			Name:    c.Name(),
			BaseURL: c.BaseURL(),
		})
	}
	return out
}

func buildDispatcher(cfg *config.Config, st *store.Store, log *slog.Logger) *notify.Dispatcher {
	if !cfg.NotifyEnabled {
		return nil
	}
	var notifiers []notify.Notifier
	if cfg.SMTPHost != "" && len(cfg.NotifyTo) > 0 {
		notifiers = append(notifiers, notify.NewEmailNotifier(
			cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPUser, cfg.SMTPPass,
			cfg.SMTPFrom, cfg.NotifyTo, cfg.NotifyTimeout))
	}
	if cfg.WebhookURL != "" {
		notifiers = append(notifiers, notify.NewWebhookNotifier(
			cfg.WebhookURL, cfg.WebhookSecret, notify.ParseFlavor(cfg.WebhookFlavor),
			cfg.NotifyTimeout))
	}
	return notify.NewDispatcher(st, log, notifiers...)
}

// isPubliclyBound reports whether addr listens on more than the loopback
// interface.
func isPubliclyBound(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	host = strings.Trim(host, "[]")
	return host == "" || host == "0.0.0.0" || host == "::"
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "是"
	}
	return "否"
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
