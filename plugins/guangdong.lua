-- 广东省教育考试院 · 通知公告
--
-- 这个文件既是一个能直接用的采集器，也是一份可以照抄的模板。
-- 改 key 和 base_url、换掉选择器，就是一个新数据源，不需要重新编译。
--
-- 验证方式：
--     ./exams probe gd_eea --with-detail
--
-- 站点改版后如果解析失败，把真实页面存下来对照：
--     ./exams probe gd_eea --dump-html ./tmp/gd

local source = {
  -- key 是数据库里的外键，所有已抓数据都挂在它下面。定了之后不要再改，
  -- 改了等于换了一个数据源，旧数据会变成孤儿。
  key      = "gd_eea",
  name     = "广东省教育考试院 · 通知公告",
  base_url = "https://eea.gd.gov.cn",

  -- 以下两项会套用到本脚本返回的每一条上，列表里没有地区/分类字段时很有用。
  region   = "广东",
}

-- 列表页。该栏目分页是 index.html, index_2.html … index_17.html。
-- 只取前几页：对一个"看有什么新公告"的工具来说足够了，
-- 而且稳态下每轮就只有这几次请求。
local PAGES = {
  "/tzgg/index.html",
  "/tzgg/index_2.html",
  "/tzgg/index_3.html",
}

function source.list(ctx)
  local items = {}

  for _, path in ipairs(PAGES) do
    local doc = ctx:get(path)

    -- 这里用 each_checked 而不是 each，是整份脚本里最要紧的一行。
    --
    -- 站点改版后选择器会匹配不到任何节点。如果只是静默返回空表，抓取会被
    -- 记为"成功"、页面上一切正常，而数据已经断了——这种故障可能要几周后
    -- 才被发现。each_checked 会直接抛错并带上选择器的名字。
    local rows = doc:each_checked("ul.list li", function(row)
      local a = row:find("a")

      -- 相对链接交给 ctx:abs 解析（内部按 base_url 补全）。
      local url = ctx:abs(a:attr("href"))
      if url == "" then
        return -- 回调返回 nil 表示跳过这一行，用来过滤导航/分页等杂项
      end

      return {
        -- 该站列表里没有稳定 id，用规范化后的详情页 URL 作为身份标识。
        external_id = url,
        title       = a:text(),
        url         = url,

        -- 直接把页面上印的日期原样交给 Go 侧解析：
        -- 中文日期（2026年9月4日 / 2026-09-04 / 2026.09.04）由 Go 统一处理，
        -- 脚本不需要自己写正则。解析不了的会留在 published_raw 里不丢。
        published   = row:find("span.time"):text(),
      }
    end)

    for _, it in ipairs(rows) do
      items[#items + 1] = it
    end
  end

  if #items == 0 then
    ctx:fail("三个列表页一条都没解析出来，站点结构可能已变")
  end
  return items
end

-- 定义了 detail 就自动获得按需抓取正文的能力：
-- 只有新增的、或列表字段发生变化的条目才会走到这里，
-- 所以稳态下一轮抓取基本只有列表页请求。
--
-- 返回一个字符串表示"这就是正文"；也可以返回表来顺带更新其它字段。
function source.detail(ctx, item)
  -- 用 :body() 而不是 :text()：前者保留段落换行，后者会把整篇压成一行。
  return ctx:get(item.url):find("div.article"):body()
end

return source
