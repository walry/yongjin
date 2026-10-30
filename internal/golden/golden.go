// Package golden 是金标回归集：一组冻结的「事实 + 规则 → 期望结果」，
// 在每次规则发布前后逐分比对，回答一个问题——
//
//	这次改动到底动了谁的钱，动了哪一列。
//
// 它与按不变式分布的单点断言互不替代：不变式证明「结构没坏」，金标集证明
// 「这一批具体订单的每一个数字没变」。两者都要，因为「守恒但仍然发错人」
// 是真实存在的一类缺陷（守恒只约束合计，不约束归属）。
//
// 语料（Cases）是代码，期望（testdata/*.golden）是文本表：
//   - 输入用代码写，才能复用 demo 的构造器并显式写出「相对基线改了哪一处」；
//   - 期望用文本写，才能在 git diff 里一眼看出「P3 的池分成少了 1200 元」，
//     而不是在几百行 JSON 里找一个小数点。
//
// 期望文件里同时冻结 facts_digest / ruleset / engine 三个身份字段。这样
// 「输入变了」与「算法变了」是两种不同的失败，报错文案不同 —— 前者要人确认
// 数据集改动是否有意，后者才是回归。混在一起时最危险：为了消红而 -update，
// 会把一次真实的算法回退永久固化成正确结果。
package golden

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/walry/yongjin/internal/calc"
	"github.com/walry/yongjin/internal/domain"
	"github.com/walry/yongjin/internal/money"
)

// Case 是一份金标用例：冻结的事实快照、规则集，以及它刻意要压住的语义（Tags）。
type Case struct {
	Name  string
	Note  string
	Tags  []string
	Facts domain.Facts
	Rules domain.RuleSet
}

// PersonRow 是结算表的一个人：金额按列拆开，不只看合计。
// 只对得上 Total 而对不上「哪一列动了」，等于没有归因能力。
type PersonRow struct {
	Person   domain.PersonID
	Direct   money.Money
	Contract money.Money
	Revenue  money.Money
	Split    money.Money
	Pool     money.Money
	Manager  money.Money
	Total    money.Money
}

// PoolRow 是池的期末对账，逐列冻结：Budget == Distributed + CarryOver 是结构断言，
// 但「预算 60000 却分出 54000」这种数值回退只有金标集拦得住。
type PoolRow struct {
	Pool        domain.PoolID
	Budget      money.Money
	Claimed     money.Money
	Retained    money.Money
	Distributed money.Money
	CarryOver   money.Money
}

// OrderRow 是单笔订单的投影合计。它是第三层：期间合计对得上、但「钱在订单之间挪了」
// 只会在这里显形 —— 靠 ProjectByOrder 复用同一套求和，不另算一遍。
type OrderRow struct {
	Order domain.OrderID
	Total money.Money
}

// Expect 是一份完整的金标期望。
//
// 前四个字段是身份（谁算的、用什么算的），后面是三个层级的钱。
// 身份字段单独成组，是为了让「输入漂移」这种失败能在第一行就被识别出来。
type Expect struct {
	Engine      string
	RuleSet     string
	FactsDigest string
	RunID       string

	Warnings int
	Total    money.Money
	Persons  []PersonRow
	Pools    []PoolRow
	Orders   []OrderRow
}

// Features 是所有 Case.Tags 的取值。用常量而不是字符串字面量，
// 是为了让「用例声明的语义」和「校验器认识的语义」不会各写各的。
const (
	TagBand       = "band"       // 达成率档位命中
	TagFactor     = "factor"     // 因子链（时间/产品/区域）
	TagSplit      = "split"      // 分账转移
	TagClaim      = "claim"      // 池领取
	TagDistribute = "distribute" // 池分发
	TagManager    = "manager"    // 管理奖上溯
	TagWarning    = "warning"    // 告警（缺口/目标为 0/产品缺失）
	TagDualKPI    = "dual-kpi"   // 同一订单行两个口径各出一笔
	TagCarryOver  = "carryover"  // 预算没发完而结转
	TagRetained   = "retained"   // 领到但分不出去，留存
)

// Features 观察一份结果实际用到了哪些语义。它不看用例声明了什么，
// 只看结果的形态 —— 这样「声明要压分账、实际压根没产生分账」能被抓住。
func Features(res calc.Result) map[string]bool {
	got := map[string]bool{}
	kpis := map[domain.KPI]bool{}
	for _, d := range res.Directs {
		if d.BandID != "" {
			got[TagBand] = true
		}
		if len(d.Factors) > 0 {
			got[TagFactor] = true
		}
		kpis[d.KPI] = true
	}
	if len(res.Splits) > 0 {
		got[TagSplit] = true
	}
	if len(res.Claims) > 0 {
		got[TagClaim] = true
	}
	if len(res.Distributions) > 0 {
		got[TagDistribute] = true
	}
	if len(res.ManagerBonuses) > 0 {
		got[TagManager] = true
	}
	if len(res.Warnings) > 0 {
		got[TagWarning] = true
	}
	if len(kpis) > 1 {
		got[TagDualKPI] = true
	}
	for _, p := range res.Pools {
		if p.CarryOver > 0 {
			got[TagCarryOver] = true
		}
		if p.Retained > 0 {
			got[TagRetained] = true
		}
	}
	return got
}

// Of 从结果里提取金标期望。
//
// 提取时顺手做两条自检：三个层级的合计必须彼此相等。金标集里放一组自己就不平的
// 数字，比没有金标集更坏 —— 它会把一次真实的失衡固化成「正确」。
func Of(facts domain.Facts, res calc.Result) (Expect, error) {
	e := Expect{
		Engine:      res.EngineVersion,
		RuleSet:     res.RuleSetVersion,
		FactsDigest: res.FactsDigest,
		RunID:       res.CalcRunID,
		Warnings:    len(res.Warnings),
		Total:       res.Total(),
	}
	for _, p := range res.Persons {
		e.Persons = append(e.Persons, PersonRow{
			Person: p.Person, Direct: p.Direct,
			Contract: p.DirectContract, Revenue: p.DirectRevenue,
			Split: p.Split, Pool: p.Pool, Manager: p.Manager, Total: p.Total,
		})
	}

	ids := make([]string, 0, len(facts.Orders))
	for _, o := range facts.Orders {
		ids = append(ids, string(o.ID))
	}
	sort.Strings(ids)

	var orderSum money.Money
	for _, id := range ids {
		t := res.ProjectByOrder(domain.OrderID(id)).Total()
		e.Orders = append(e.Orders, OrderRow{Order: domain.OrderID(id), Total: t})
		orderSum += t
	}
	if orderSum != e.Total {
		return Expect{}, fmt.Errorf("金标自检失败：全部订单合计 %s 不等于整期合计 %s", orderSum, e.Total)
	}

	for _, p := range res.Pools {
		e.Pools = append(e.Pools, PoolRow{
			Pool: p.Pool, Budget: p.Budget, Claimed: p.Claimed,
			Retained: p.Retained, Distributed: p.Distributed, CarryOver: p.CarryOver,
		})
	}
	sort.Slice(e.Persons, func(i, j int) bool { return e.Persons[i].Person < e.Persons[j].Person })
	sort.Slice(e.Pools, func(i, j int) bool { return e.Pools[i].Pool < e.Pools[j].Pool })
	return e, nil
}

// Encode 把期望渲染成文本表。字段顺序固定、记录排序固定，
// 因此同一份期望永远得到同一段文本 —— 否则 git diff 会全是噪声。
func Encode(e Expect) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	line("engine %s", e.Engine)
	line("ruleset %s", e.RuleSet)
	line("facts_digest %s", e.FactsDigest)
	line("run_id %s", e.RunID)
	line("warnings %d", e.Warnings)
	line("total %s", e.Total)
	for _, p := range e.Persons {
		line("person %s direct=%s contract=%s revenue=%s split=%s pool=%s manager=%s total=%s",
			p.Person, p.Direct, p.Contract, p.Revenue, p.Split, p.Pool, p.Manager, p.Total)
	}
	for _, p := range e.Pools {
		line("pool %s budget=%s claimed=%s retained=%s distributed=%s carryover=%s",
			p.Pool, p.Budget, p.Claimed, p.Retained, p.Distributed, p.CarryOver)
	}
	for _, o := range e.Orders {
		line("order %s total=%s", o.Order, o.Total)
	}
	return b.String()
}

// Decode 解析文本表。未知标签直接报错：静默忽略会让「写错字段名」变成
// 「这一列永远不比」，而那正是金标集该守的地方。
func Decode(s string) (Expect, error) {
	var e Expect
	for i, raw := range strings.Split(s, "\n") {
		text := strings.TrimSpace(raw)
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		tok := strings.Fields(text)
		at := fmt.Sprintf("第 %d 行 %q", i+1, text)
		switch tok[0] {
		case "engine", "ruleset", "facts_digest", "run_id":
			if len(tok) != 2 {
				return Expect{}, fmt.Errorf("%s：%s 需要一个值", at, tok[0])
			}
			switch tok[0] {
			case "engine":
				e.Engine = tok[1]
			case "ruleset":
				e.RuleSet = tok[1]
			case "facts_digest":
				e.FactsDigest = tok[1]
			case "run_id":
				e.RunID = tok[1]
			}
		case "warnings", "total":
			if len(tok) != 2 {
				return Expect{}, fmt.Errorf("%s：%s 需要一个值", at, tok[0])
			}
			if tok[0] == "warnings" {
				n, err := strconv.Atoi(tok[1])
				if err != nil {
					return Expect{}, fmt.Errorf("%s：warnings 不是整数: %w", at, err)
				}
				e.Warnings = n
				continue
			}
			m, err := parseMoney(tok[1])
			if err != nil {
				return Expect{}, fmt.Errorf("%s：%w", at, err)
			}
			e.Total = m
		case "person", "pool", "order":
			if len(tok) < 3 {
				return Expect{}, fmt.Errorf("%s：%s 记录缺少字段", at, tok[0])
			}
			kv, err := parseKV(tok[2:])
			if err != nil {
				return Expect{}, fmt.Errorf("%s：%w", at, err)
			}
			switch tok[0] {
			case "person":
				var r PersonRow
				r.Person = domain.PersonID(tok[1])
				fields := []struct {
					name string
					dst  *money.Money
				}{
					{"direct", &r.Direct}, {"contract", &r.Contract}, {"revenue", &r.Revenue},
					{"split", &r.Split}, {"pool", &r.Pool}, {"manager", &r.Manager}, {"total", &r.Total},
				}
				if err := fill(kv, at, fields); err != nil {
					return Expect{}, err
				}
				e.Persons = append(e.Persons, r)
			case "pool":
				var r PoolRow
				r.Pool = domain.PoolID(tok[1])
				fields := []struct {
					name string
					dst  *money.Money
				}{
					{"budget", &r.Budget}, {"claimed", &r.Claimed}, {"retained", &r.Retained},
					{"distributed", &r.Distributed}, {"carryover", &r.CarryOver},
				}
				if err := fill(kv, at, fields); err != nil {
					return Expect{}, err
				}
				e.Pools = append(e.Pools, r)
			case "order":
				var r OrderRow
				r.Order = domain.OrderID(tok[1])
				fields := []struct {
					name string
					dst  *money.Money
				}{{"total", &r.Total}}
				if err := fill(kv, at, fields); err != nil {
					return Expect{}, err
				}
				e.Orders = append(e.Orders, r)
			}
		default:
			return Expect{}, fmt.Errorf("%s：未知记录类型 %q", at, tok[0])
		}
	}
	return e, nil
}

func fill(kv map[string]string, at string, fields []struct {
	name string
	dst  *money.Money
}) error {
	for _, f := range fields {
		raw, ok := kv[f.name]
		if !ok {
			return fmt.Errorf("%s：缺少字段 %s", at, f.name)
		}
		m, err := parseMoney(raw)
		if err != nil {
			return fmt.Errorf("%s：字段 %s %w", at, f.name, err)
		}
		*f.dst = m
	}
	return nil
}

func parseKV(tok []string) (map[string]string, error) {
	out := make(map[string]string, len(tok))
	for _, t := range tok {
		k, v, ok := strings.Cut(t, "=")
		if !ok {
			return nil, fmt.Errorf("字段 %q 不是 k=v 形式", t)
		}
		out[k] = v
	}
	return out, nil
}

// parseMoney 解析 money.Money.String() 的输出（「-15649.92」）。必须两位小数：
// 允许「1.5」这类写法，就等于允许期望文件与渲染结果不是同一种表示。
func parseMoney(s string) (money.Money, error) {
	neg := strings.HasPrefix(s, "-")
	body := strings.TrimPrefix(s, "-")
	yuan, cents, ok := strings.Cut(body, ".")
	if !ok || len(cents) != 2 {
		return 0, fmt.Errorf("金额 %q 必须是两位小数的元", s)
	}
	y, err := strconv.ParseInt(yuan, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("金额 %q 的整数部分不是数字: %w", s, err)
	}
	c, err := strconv.ParseInt(cents, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("金额 %q 的小数部分不是数字: %w", s, err)
	}
	v := y*100 + c
	if neg {
		v = -v
	}
	return money.Money(v), nil
}

// Mismatch 是一处不一致。Layer 让读者先知道「错在哪一层」，
// 因为三层对应三种完全不同的排查方向：身份 → 输入/版本，结算 → 分配，池 → 预算。
type Mismatch struct {
	Layer   string
	Key     string // 人 / 池 / 订单 ID；身份类为空
	Field   string
	Want    string
	Got     string
	Delta   money.Money
	IsMoney bool
}

func (m Mismatch) String() string {
	head := m.Field
	if m.Key != "" {
		head = fmt.Sprintf("%s.%s", m.Key, m.Field)
	}
	if m.IsMoney {
		sign := ""
		if m.Delta > 0 {
			sign = "+"
		}
		return fmt.Sprintf("[%s] %-24s 金标 %-14s 实得 %-14s 差 %s%s",
			m.Layer, head, m.Want, m.Got, sign, m.Delta)
	}
	return fmt.Sprintf("[%s] %-24s 金标 %-14s 实得 %s", m.Layer, head, m.Want, m.Got)
}

// Diff 比对两份期望，按「身份 → 合计 → 人 → 池 → 单」的顺序返回全部差异。
// 顺序即排查顺序：先确认输入没漂，再看钱。
func Diff(want, got Expect) []Mismatch {
	var out []Mismatch
	idField := func(field, w, g string) {
		if w != g {
			out = append(out, Mismatch{Layer: "身份", Field: field, Want: w, Got: g})
		}
	}
	idField("engine", want.Engine, got.Engine)
	idField("ruleset", want.RuleSet, got.RuleSet)
	idField("facts_digest", want.FactsDigest, got.FactsDigest)
	idField("run_id", want.RunID, got.RunID)

	if want.Warnings != got.Warnings {
		out = append(out, Mismatch{
			Layer: "告警", Field: "warnings",
			Want: strconv.Itoa(want.Warnings), Got: strconv.Itoa(got.Warnings),
		})
	}
	if want.Total != got.Total {
		out = append(out, Mismatch{
			Layer: "合计", Field: "total", Want: want.Total.String(), Got: got.Total.String(),
			Delta: got.Total - want.Total, IsMoney: true,
		})
	}

	out = append(out, diffPersons(want.Persons, got.Persons)...)
	out = append(out, diffPools(want.Pools, got.Pools)...)
	out = append(out, diffOrders(want.Orders, got.Orders)...)
	return out
}

func diffPersons(want, got []PersonRow) []Mismatch {
	w := map[domain.PersonID]PersonRow{}
	g := map[domain.PersonID]PersonRow{}
	for _, r := range want {
		w[r.Person] = r
	}
	for _, r := range got {
		g[r.Person] = r
	}
	var out []Mismatch
	for _, id := range unionPersons(w, g) {
		wr, wok := w[id]
		gr, gok := g[id]
		key := string(id)
		switch {
		case !gok:
			out = append(out, Mismatch{Layer: "结算", Key: key, Field: "整行", Want: "存在", Got: "缺失"})
			continue
		case !wok:
			out = append(out, Mismatch{Layer: "结算", Key: key, Field: "整行", Want: "缺失", Got: "多出"})
			continue
		}
		cols := []struct {
			name string
			w, g money.Money
		}{
			{"direct", wr.Direct, gr.Direct},
			{"contract", wr.Contract, gr.Contract},
			{"revenue", wr.Revenue, gr.Revenue},
			{"split", wr.Split, gr.Split},
			{"pool", wr.Pool, gr.Pool},
			{"manager", wr.Manager, gr.Manager},
			{"total", wr.Total, gr.Total},
		}
		for _, c := range cols {
			if c.w != c.g {
				out = append(out, Mismatch{
					Layer: "结算", Key: key, Field: c.name,
					Want: c.w.String(), Got: c.g.String(),
					Delta: c.g - c.w, IsMoney: true,
				})
			}
		}
	}
	return out
}

func diffPools(want, got []PoolRow) []Mismatch {
	w := map[domain.PoolID]PoolRow{}
	g := map[domain.PoolID]PoolRow{}
	for _, r := range want {
		w[r.Pool] = r
	}
	for _, r := range got {
		g[r.Pool] = r
	}
	var out []Mismatch
	for _, id := range unionPools(w, g) {
		wr, wok := w[id]
		gr, gok := g[id]
		key := string(id)
		switch {
		case !gok:
			out = append(out, Mismatch{Layer: "池", Key: key, Field: "整行", Want: "存在", Got: "缺失"})
			continue
		case !wok:
			out = append(out, Mismatch{Layer: "池", Key: key, Field: "整行", Want: "缺失", Got: "多出"})
			continue
		}
		cols := []struct {
			name string
			w, g money.Money
		}{
			{"budget", wr.Budget, gr.Budget},
			{"claimed", wr.Claimed, gr.Claimed},
			{"retained", wr.Retained, gr.Retained},
			{"distributed", wr.Distributed, gr.Distributed},
			{"carryover", wr.CarryOver, gr.CarryOver},
		}
		for _, c := range cols {
			if c.w != c.g {
				out = append(out, Mismatch{
					Layer: "池", Key: key, Field: c.name,
					Want: c.w.String(), Got: c.g.String(),
					Delta: c.g - c.w, IsMoney: true,
				})
			}
		}
	}
	return out
}

func diffOrders(want, got []OrderRow) []Mismatch {
	w := map[domain.OrderID]money.Money{}
	g := map[domain.OrderID]money.Money{}
	for _, r := range want {
		w[r.Order] = r.Total
	}
	for _, r := range got {
		g[r.Order] = r.Total
	}
	var out []Mismatch
	for _, id := range unionOrders(w, g) {
		wv, wok := w[id]
		gv, gok := g[id]
		key := string(id)
		switch {
		case !gok:
			out = append(out, Mismatch{Layer: "单", Key: key, Field: "total", Want: wv.String(), Got: "缺失"})
		case !wok:
			out = append(out, Mismatch{Layer: "单", Key: key, Field: "total", Want: "缺失", Got: gv.String()})
		case wv != gv:
			out = append(out, Mismatch{
				Layer: "单", Key: key, Field: "total",
				Want: wv.String(), Got: gv.String(), Delta: gv - wv, IsMoney: true,
			})
		}
	}
	return out
}

func unionPersons(a, b map[domain.PersonID]PersonRow) []domain.PersonID {
	seen := map[domain.PersonID]bool{}
	var out []domain.PersonID
	for k := range a {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range b {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func unionPools(a, b map[domain.PoolID]PoolRow) []domain.PoolID {
	seen := map[domain.PoolID]bool{}
	var out []domain.PoolID
	for k := range a {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range b {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func unionOrders(a, b map[domain.OrderID]money.Money) []domain.OrderID {
	seen := map[domain.OrderID]bool{}
	var out []domain.OrderID
	for k := range a {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range b {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Run 跑一个用例并提取期望。它是发布闸门的入口：
// 目录里放什么不重要，重要的是同一份 Case 永远得到同一份 Expect。
func Run(c Case) (Expect, error) {
	res, err := calc.Run(c.Facts, c.Rules, calc.Options{})
	if err != nil {
		return Expect{}, fmt.Errorf("%s: %w", c.Name, err)
	}
	return Of(c.Facts, res)
}

// Load 读取一份金标。缺失时返回的错误里带 os.ErrNotExist，
// 调用方据此给出「跑 -update 生成」的提示，而不是当成内容不一致。
func Load(dir, name string) (Expect, error) {
	b, err := os.ReadFile(filepath.Join(dir, name+".golden"))
	if err != nil {
		return Expect{}, err
	}
	return Decode(string(b))
}

// Save 写回一份金标，文件头带上用例说明 —— 期望文件必须自解释，
// 否则三个月后没人知道这条用例在守什么。
func Save(dir string, c Case, e Expect) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# golden: %s\n", c.Name)
	for _, l := range strings.Split(c.Note, "\n") {
		fmt.Fprintf(&b, "# %s\n", l)
	}
	if len(c.Tags) > 0 {
		fmt.Fprintf(&b, "# traps: %s\n", strings.Join(c.Tags, ", "))
	}
	b.WriteString(Encode(e))
	return os.WriteFile(filepath.Join(dir, c.Name+".golden"), []byte(b.String()), 0o644)
}

// FormatDiff 把差异渲染成可直接贴进 issue 的说明。
// 结尾的提示是刻意的：-update 是唯一能让金标集失去意义的操作，
// 所以它必须伴随一句「先解释清楚为什么变」。
func FormatDiff(name string, ms []Mismatch) string {
	var b strings.Builder
	fmt.Fprintf(&b, "金标用例 %s 不一致（%d 处）\n", name, len(ms))
	for _, m := range ms {
		fmt.Fprintf(&b, "  %s\n", m)
	}
	if len(ms) > 0 && ms[0].Layer == "身份" {
		// 身份差异还要再分一层：三个分量变了是输入/版本变了；
		// 只有 run_id 变（分量全同）说明幂等键的构成被改坏了 —— 前者要人确认数据，
		// 后者是线上所有缓存集体失效，排查方向完全不同。
		inputChanged := false
		for _, m := range ms {
			if m.Layer == "身份" && m.Field != "run_id" {
				inputChanged = true
			}
		}
		if inputChanged {
			b.WriteString("\n第一条是身份差异：输入快照或版本变了，不是算法变了。\n" +
				"先确认这处改动是不是有意的；确认后才允许 go test ./internal/golden -update。\n")
		} else {
			b.WriteString("\n只有 run_id 变了，facts_digest / ruleset / engine 都没变：\n" +
				"幂等键的构成被改坏了（runID 的哈希输入、顺序或截断长度）。\n" +
				"这会让线上所有缓存集体失效，必须查回 digest.go 而不是改金标。\n")
		}
	} else {
		b.WriteString("\n数字变了。先定位是哪一层（结算 / 池 / 单），再看它是被上游哪一处改动带偏的；\n" +
			"直接 -update 会把回退固化成基准。\n")
	}
	return b.String()
}

// Walk 返回目录下所有金标用例名（去掉 .golden 后缀）。
// 用来发现「用例改名后遗留的孤儿金标」—— 孤儿文件永远不会参与比对，
// 却会让人误以为那条语义还有人守着。
func Walk(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".golden") {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".golden"))
	}
	sort.Strings(out)
	return out, nil
}
