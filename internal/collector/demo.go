package collector

import (
	"context"
	"fmt"
	"time"

	"github.com/BenLocal/exams/internal/model"
)

func init() { Register(demoSource{}) }

// demoSource generates announcements locally so the whole pipeline — crawl,
// change detection, storage, notifications, web UI — can be exercised with no
// network access at all.
//
// It is enabled by default for exactly that reason: a fresh clone should show
// a working page immediately, before any real selector has been verified.
type demoSource struct{}

func (demoSource) Key() string     { return "demo" }
func (demoSource) Name() string    { return "演示数据源（本地生成，不发起网络请求）" }
func (demoSource) BaseURL() string { return "https://example.invalid/exams" }

type demoExam struct {
	Title    string
	Category string
	Region   string
	Body     string
	// DeadlineAfterDays is how long after publication registration closes.
	// Zero means the announcement has no deadline.
	DeadlineAfterDays int
}

var demoExams = []demoExam{
	{
		Title: "全国大学英语四、六级考试报名工作通知", Category: "四六级", Region: "全国",
		Body:              "本次考试报名采用网上报名方式，考生须登录全国大学英语四、六级考试报名网完成注册、信息核对与缴费。报名成功后请于考前一周登录系统打印准考证。",
		DeadlineAfterDays: 21,
	},
	{
		Title: "全国计算机等级考试（NCRE）报名公告", Category: "计算机等级考试", Region: "全国",
		Body:              "本次考试开考一至四级共若干个科目，考生可根据自身情况选报。报名时须上传本人近期免冠正面证件照，照片不符合要求将无法通过审核。",
		DeadlineAfterDays: 30,
	},
	{
		Title: "中小学教师资格考试（笔试）报名公告", Category: "教师资格", Region: "全国",
		Body:              "报名分为网上注册、网上审核和网上缴费三个阶段。考生须本人通过中小学教师资格考试网进行报名，禁止他人代为报名。",
		DeadlineAfterDays: 14,
	},
	{
		Title: "中央机关及其直属机构考试录用公务员公告", Category: "公务员", Region: "全国",
		Body:              "本次招考坚持德才兼备、以德为先的用人标准，采取考试与考察相结合的办法进行。报考人员须仔细阅读招考简章，确认符合拟报考职位的资格条件。",
		DeadlineAfterDays: 10,
	},
	{
		Title: "全国硕士研究生招生考试网上报名公告", Category: "考研", Region: "全国",
		Body:              "网上报名期间，考生可自行修改网上报名信息或重新填报报名信息，但一位考生只能保留一条有效报名信息。逾期不再补报，也不得修改报名信息。",
		DeadlineAfterDays: 25,
	},
	{
		Title: "注册会计师全国统一考试报名简章", Category: "注册会计师", Region: "全国",
		Body:              "报名人员应当具有完全民事行为能力，并具有高等专科以上学校毕业学历，或者具有会计或者相关专业中级以上技术职称。",
		DeadlineAfterDays: 28,
	},
	{
		Title: "一级建造师资格考试考务工作通知", Category: "建造师", Region: "广东",
		Body:              "考试设《建设工程经济》《建设工程法规及相关知识》《建设工程项目管理》和《专业工程管理与实务》4个科目。成绩实行滚动管理。",
		DeadlineAfterDays: 18,
	},
	{
		Title: "护士执业资格考试报名工作的通知", Category: "护士执业资格", Region: "北京",
		Body:              "考生须在规定时间内完成网上预报名，并按要求到指定地点进行现场确认。现场确认时须提交报名申请表、本人身份证及毕业证书原件。",
		DeadlineAfterDays: 12,
	},
	{
		Title: "计算机技术与软件专业技术资格考试通知", Category: "软考", Region: "江苏",
		Body:              "本次考试设初级、中级、高级三个级别。考试合格者将获得相应级别的计算机技术与软件专业技术资格证书，该证书在全国范围内有效。",
		DeadlineAfterDays: 20,
	},
	{
		Title: "国家统一法律职业资格考试公告", Category: "法考", Region: "全国",
		Body:              "客观题考试实行闭卷、计算机化考试方式，试题、答题要求和答题界面均在计算机显示屏上显示，应试人员应当使用计算机鼠标或键盘直接作答。",
		DeadlineAfterDays: 0,
	},
}

// List synthesises a backlog spread over roughly two years, plus one item that
// changes daily so the change-detection and notification paths are observable
// without waiting for a real source to publish something.
func (demoSource) List(ctx context.Context, f *Fetcher) ([]model.Item, error) {
	loc := f.Loc()
	now := time.Now().In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, loc)

	terms := []struct {
		Label string
		// DaysBack shifts this term's block of announcements into the past.
		DaysBack int
	}{
		{"2026年下半年", 0},
		{"2026年上半年", 150},
		{"2025年下半年", 320},
	}

	items := make([]model.Item, 0, len(demoExams)*len(terms)+1)

	// One item whose body tracks the current date, so running the crawler on a
	// later day produces a genuine "updated" event with a changed_fields entry.
	items = append(items, model.Item{
		ExternalID:   "demo-daily-digest",
		Title:        "每日考务动态汇总（演示用，内容随日期变化）",
		URL:          "https://example.invalid/exams/daily-digest",
		Summary:      "此条目由演示数据源生成，正文包含生成日期，用于演示变更检测。",
		Category:     "综合",
		Region:       "全国",
		PublishedAt:  &today,
		PublishedRaw: today.Format("2006年1月2日"),
		Content: fmt.Sprintf(
			"本条内容生成于 %s。\n\n重新抓取时，如果日期发生变化，本次抓取会被记录为一条 updated 变更，"+
				"并在考试详情页的变更历史中显示 content 字段发生了变化。\n\n"+
				"这用于在不依赖外部网站的情况下验证变更检测链路是否工作。",
			today.Format("2006-01-02")),
	})

	for ti, term := range terms {
		for ei, e := range demoExams {
			// Stable identity: re-running the crawler must map each entry onto
			// the same stored row rather than creating duplicates.
			externalID := fmt.Sprintf("demo-%d-%02d", ti, ei)

			published := today.AddDate(0, 0, -(term.DaysBack + ei*6 + ti))
			item := model.Item{
				ExternalID:   externalID,
				Title:        fmt.Sprintf("%s%s", term.Label, e.Title),
				URL:          fmt.Sprintf("https://example.invalid/exams/%s", externalID),
				Summary:      e.Body,
				Category:     e.Category,
				Region:       e.Region,
				PublishedAt:  &published,
				PublishedRaw: published.Format("2006年1月2日"),
				Content:      fmt.Sprintf("%s\n\n%s\n\n发布日期：%s", e.Body, "本条目为演示数据，用于在没有可用外部数据源时验证抓取、存储与展示链路。", published.Format("2006-01-02")),
			}
			if e.DeadlineAfterDays > 0 {
				deadline := published.AddDate(0, 0, e.DeadlineAfterDays)
				item.DeadlineAt = &deadline
			}
			items = append(items, item)
		}
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	return items, nil
}
