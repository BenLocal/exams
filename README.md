# exams

自托管的考试信息聚合服务。后台按计划抓取多个官网的考试公告，落到 PostgreSQL，
用一个 Web 页面统一浏览、搜索、筛选，并在有新公告时推送通知。

解决的问题：各类考试（公务员、考研、四六级、教师资格、职业资格…）的公告散落在
不同官网，没有统一入口，容易错过报名窗口。

```
浏览器 ──► Hertz (SSR 页面 + JSON API)
                │
        ┌───────┴────────┐
        │                │
   PostgreSQL       调度器 ──► 采集器 ──┬── Go 插件（内置的四个）
   exams/crawl_runs/                    └── Lua 插件（plugins/*.lua）
   exam_changes/notifications                  │
                                    共用 Fetcher：重试 / GBK 转码 /
                                    限速 / 字符集嗅探 / 超时
```

## 快速开始

```bash
cp .env.example .env
# 编辑 .env，至少填 DATABASE_URL

make build
./exams migrate          # 建表（只需一次）
./exams probe demo       # 先看解析结果，不写库
./exams serve            # 打开 http://127.0.0.1:8080
```

`demo` 数据源默认启用且完全离线，所以 clone 下来立刻就能看到一个有数据的页面，
不必先等真实站点抓成功。

## 数据源

内置四个，选择器都已对着线上页面核实过：

| 标识 | 站点 | 类型 |
|---|---|---|
| `demo` | 本地生成，零网络请求 | Go |
| `neea` | 中国教育考试网 · 公示公告 | Go |
| `moe` | 教育部 · 公告公示 | Go |
| `cpta` | 中国人事考试网 · 通知公告 | Go |
| `gd_eea` | 广东省教育考试院 · 通知公告 | **Lua**（`plugins/guangdong.lua`） |

```bash
./exams sources              # 看状态
./exams probe cpta           # 校验解析（不写库）
./exams collect cpta         # 抓一次并入库
```

## 用 Lua 写数据源

不需要重新编译二进制。`plugins/` 下放一个 `.lua` 文件，重启即可。

```lua
local source = {
  key      = "gzrsks",              -- 数据库外键，定了别改
  name     = "广州市人事考试中心",
  base_url = "https://example.gov.cn",
  region   = "广东",                 -- 可选，套用到每一条
}

function source.list(ctx)
  local doc = ctx:get("/notice/index.html")

  -- each_checked 在匹配不到任何节点时会报错并带上选择器名字。
  -- 用 each 的话，改版后会静默返回空列表、抓取记录显示"成功"，
  -- 而数据已经断了——这是最值得防的一种故障。
  return doc:each_checked("ul.news li", function(row)
    local a = row:find("a")
    local url = ctx:abs(a:attr("href"))
    if url == "" then return end   -- 返回 nil 即跳过该行

    return {
      title     = a:text(),
      url       = url,
      published = row:find("span.time"):text(),
    }
  end)
end

function source.detail(ctx, item)          -- 可选；定义了才有正文抓取
  return ctx:get(item.url):find("div.article"):body()
end

return source
```

改完用 `./exams probe gzrsks --with-detail` 验证，不用起服务。

**可用的 API**

| | |
|---|---|
| `ctx:get(url)` | 抓取并解析，返回 selection（支持相对路径） |
| `ctx:get_text(url)` | 抓取，返回解码后的原始文本 |
| `ctx:abs(href)` | 相对链接转绝对；无法解析时返回 `""` |
| `ctx:parse_date(s)` | 中文日期 → `"2026-09-04"`，解析不了返回 nil |
| `ctx:fail(msg)` | 中止并报错 |
| `ctx:log(msg)` | 写日志 |
| `sel:find / first / children / parent` | 返回 selection |
| `sel:text()` | 文本，空白折叠（适合标题、日期） |
| `sel:body()` | 文本，**保留段落换行**（适合正文） |
| `sel:attr(name)` | 属性值，不存在返回 `""` |
| `sel:has(sel)` / `sel:len()` | 布尔 / 匹配数量 |
| `sel:each(sel, fn)` | 遍历；`fn` 返回非 nil 则收集 |
| `sel:each_checked(sel, fn)` | 同上，但 0 匹配时报错 |

条目字段：`title` 和 `url`（或 `external_id`）必填；其余可选 `summary`、`content`、
`category`、`region`、`published`、`deadline`、`published_raw`。

**几个不用自己处理的事**

`published` / `deadline` 直接填页面上印的日期原文即可——中文日期解析（`2026年9月4日`、
`2026-09-04`、`2026.09.04`）由 Go 侧统一处理，和内置采集器走的是同一套规则，包括
09:00 本地时间的锚定。解析不了的原文会存进 `published_raw`，不会丢。

HTTP 的重试、限速、GBK 转码、字符集嗅探也都在 `ctx:get` 里面，脚本碰不到。

**隔离与限制**

每次调用都会新建一个 Lua 解释器，脚本之间、轮次之间不会串状态。没有提供
`os` / `io` / `package` / `debug`，`dofile` / `loadfile` / `require` 也移除了。
抓取的 context 带着超时，脚本里的死循环会被打断。

但这只是防误操作，**不是安全边界**——Lua 脚本就是任意代码，别跑别人给的脚本。

### 站点改版了怎么办

`probe` 会在解析失败时直接告诉你是哪个选择器挂了、改哪个文件：

```
$ ./exams probe neea

抓取失败（耗时 58ms）：
neea: selector "div.conlistn ul li:has(span#ReportIDname)" matched 0 nodes on https://...
  The site markup has probably changed. Inspect the page and update
  listPageConfig in internal/collector/neea.go, then re-run:
      exams probe neea
```

改选择器不用靠猜，把真实页面 dump 下来对照：

```bash
./exams probe neea --dump-html ./tmp/neea   # 保存页面（已转成 UTF-8）
./exams probe neea --with-detail            # 顺便校验正文选择器
```

### 自己加一个源

在内建源里加是加一个 Go 文件；`demo.go` 是最简单的模板。核心就是实现四个方法：

```go
type Collector interface {
	Key() string                       // 数据库里的稳定标识，定了就别改
	Name() string
	BaseURL() string
	List(ctx context.Context, f *Fetcher) ([]model.Item, error)
}
```

再实现一个 `Detail(ctx, f, item) (model.Item, error)` 就自动获得详情页抓取能力
（只在条目新增或列表字段变化时才调用，见下文）。

**为什么要区分 List 和 Detail**：列表页只拿标题/链接/日期。如果每轮都把每条公告的
详情页重抓一遍，请求量是必要量的几十倍，也是最快被封 IP 的方式。分开之后，稳定状态
下一轮抓取基本只有每个源几次列表请求。

## 命令行

```
exams serve              启动 Web 服务与定时抓取（默认）
exams migrate            执行 migrations/ 下的迁移
exams collect [标识...]   立即抓取一次并入库；不带标识则抓全部已启用源
exams probe <标识>        抓取并解析单个源，不写数据库
exams sources            列出数据源及其状态
```

`probe` 参数：`--dump-html <目录>`、`--with-detail`、`--limit <n>`。

## 设计要点

这一节记录几个不明显但重要的决定，改代码前值得先看一眼。

**变更检测用两个哈希，不是一个。** `list_hash` 只覆盖列表页能看到的字段，用来决定
"要不要去抓详情页"；`body_hash` 只覆盖正文，用来判断"正文是不是真的变了"。合成一个
哈希的话，要么每轮都得抓所有详情页，要么没法在不加载正文的前提下判断正文变化。
两者都是对**规范化后的文本**取哈希——对原始 HTML 取哈希会让访问计数器、侧栏日期这类
每请求都变的东西变成永久的假"更新"。

**空正文必须是空哈希。** `sha256("")` 是个完全合法的 64 位十六进制串，所以如果用
"body_hash 非空"来判断"正文是否已抓到"，这个判断永远为真。正文缺失时记空串，
抓取器才能靠它决定下一轮要不要重试。

**"抓到 0 条"不等于成功。** 政府站点最常见故障就是 200 响应里装着一个 WAF 拦截页或
改版后的空壳。如果记成 `success`，页面一片绿，数据却静静停了。所以状态里
`empty` 和 `success` 是分开的，`probe` 也会把失败的选择器名字报出来。

**详情抓取有每轮上限，且可跨轮补齐。** 新加一个源时首轮会看到它全部历史公告（可能
几百条），逐条抓详情就是几百次连续请求。`CRAWL_MAX_DETAILS` 限制每轮抓多少条，
超出的部分因为 `body_hash` 还是空的，下一轮会继续补，不会丢。

**调度是一个 tick 轮询，不是每源一个 cron 任务。** 数据源可以在页面上随时启停、
改周期。每源一个 cron entry 意味着每次改动都要重建任务表，是一整类"开关点了没生效"
的 bug。现在是每分钟一个 tick，读数据库判断哪些源到期了。

**首次抓取不发通知。** 新加一个源的首轮会灌进几百条，那一刻推几百条消息毫无意义。
`sources.bootstrap_done` 保证只有已经完成过一轮的源才会触发通知。

**通知失败可重试。** `notifications` 表既是去重键也是投递台账：只有 `status='sent'`
才算送达。一次 SMTP 抖动不会永久压掉那条通知。

**单实例靠咨询锁，不只是进程内互斥。** 你手动跑 `exams collect` 时如果服务也在跑，
进程内的 mutex 拦不住。用 PostgreSQL 咨询锁，进程挂掉锁自动释放——这正是
"`status='running'` 唯一索引"方案会变成永久堵死的地方。

## 配置

见 `.env.example`，每一项都有注释。需要特别注意的两条：

**`APP_ADDR` 默认是 `127.0.0.1:8080`。** 这个服务没有鉴权。改成 `:8080` 会监听所有
网卡，请不要直接暴露到公网。

**`HTTP_USER_AGENT` 默认是一个完整的浏览器 UA，这是必要的而不是随手填的。**
中国人事考试网对 `Go-http-client/1.1` 和诚实的 `Mozilla/5.0 (compatible; exams-bot/...)`
都返回 405，只给完整浏览器 UA 内容；教育部对非浏览器 UA 会返回一个不含数据的精简
页面。想以真实身份抓取、并接受这几个源失败的话，改掉它即可。

## 安全

- 没有用户体系，靠监听地址和反向代理控制访问
- 状态变更全部走 POST，并校验 `Origin`/`Referer` 与 `Host` 一致（防跨站表单提交）
- 抓到的正文按**纯文本**存储和渲染，从不经过 `template.HTML`：把抓来的 HTML 当可信
  标记渲染，等于让每个被采集的站点都成为一个存储型 XSS 载体
- 搜索用 `ILIKE` + `pg_trgm` GIN 索引，参数化查询，`%`/`_`/`\` 都做了转义

## 数据库

```bash
./exams migrate                      # 或手动 psql -f migrations/0001_init.sql
```

`0002_search_index.optional.sql` 是可选迁移：它需要 `CREATE EXTENSION pg_trgm` 权限，
托管数据库未必给。缺了它功能不受影响，只是中文搜索退化成顺序扫描。迁移器会记住它
失败过并在每次 `exams migrate` 时重试，所以事后授权再跑一次就会自动补上。

时间列一律 `timestamptz`。公告都是中国本地日期且不带时区，`TZ` 必须保持
`Asia/Shanghai`，否则所有发布日期偏移 8 小时。

## 开发

```bash
make test              # 全部测试；store 层没有数据库时自动跳过
make vet
make probe SOURCE=neea
```

采集器的测试用 `httptest` 喂固定 HTML，不依赖外网。

**store 层的测试需要真数据库**，因为值得测的东西——`ON CONFLICT` 的语义、索引谓词、
咨询锁、事务回滚——用一个假实现来测，测的就是那个假实现本身。

```bash
make test-db           # 起一个临时 postgres（端口 55433）
make test-store
make test-db-stop
```

这些测试**会 truncate 所有表**，所以库名必须含有 `test`，否则直接拒绝运行——
指错数据库就是删数据。真要对着别的库跑，设 `TEST_DATABASE_URL_ALLOW_ANY=1`。

**一次调用一个新解释器。** 复用 LState 会让脚本状态在轮次之间泄漏，而且 Lua VM 不是
并发安全的，不同数据源并发抓取会撞上。每次 `list` / `detail` 都新建一个 state，
但脚本只在加载时编译一次，保留的是 `FunctionProto`。

**Sandbox 的边界要说清楚。** 没开 `os` / `io` / `package` / `debug`，并且显式移除了
`dofile` / `loadfile` / `require`——最后这个是 `OpenBase` 悄悄注册进来的，
是写测试时才发现的。但这只是防手滑：**Lua 脚本就是任意代码，不是安全沙箱**，
只该跑自己写的脚本。

## 尚未支持

- **JS 渲染的列表页**：目前靠直接调站点自己的后端接口绕开（`moe` 就是这么做的）。
  真需要执行 JS 的站点要引 chromedp，会明显抬高部署复杂度。
- **纯 JSON 接口的源**：现在都走 HTML 解析。Lua 侧可以用 `ctx:get_text` 拿到原始
  响应自己处理，但还没有结构化的 JSON 支持。
- **每个源独立的抓取周期**：`sources.interval` 字段和调度器都支持，但还没有编辑入口。
