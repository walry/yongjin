package calc

import (
	"testing"
	"time"

	"github.com/walry/yongjin/internal/domain"
	"github.com/walry/yongjin/internal/money"
)

// shareFacts 造一笔与设计 §14 金标对齐的订单：合同 500,000 / 收入 450,000，
// 目标 600,000 / 500,000，Q3 成单，产品 A1。
func shareFacts(rels []domain.Relation) domain.Facts {
	f := attainFacts(money.FromYuan(500000, 0), money.FromYuan(450000, 0), money.FromYuan(500000, 0))
	f.Orders[0].ClosedAt = time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	f.Persons["S1"] = domain.Person{ID: "S1", Name: "SS", Dept: "SALES"}
	f.Relations = rels
	return f
}

// splitRules 在 factorRules 基础上给直接佣金规则挂上分账方式。
func splitRules(mode domain.ShareMode, bandBPS int64, extra ...domain.Rule) domain.RuleSet {
	rs := factorRules(bandBPS, extra...)
	for i := range rs.Rules {
		if rs.Rules[i].ID == "R-DIR" {
			rs.Rules[i].Action.ShareMode = mode
		}
	}
	return rs
}

func splitsOfKPI(res Result, kpi domain.KPI) []SplitLine {
	var out []SplitLine
	for _, s := range res.Splits {
		if s.KPI == kpi {
			out = append(out, s)
		}
	}
	return out
}

func personTotal(t *testing.T, res Result, p domain.PersonID) PersonTotal {
	t.Helper()
	for _, x := range res.Persons {
		if x.Person == p {
			return x
		}
	}
	t.Fatalf("未找到 %s 的结算", p)
	return PersonTotal{}
}

// M4 验收：AS 成单与其配对 SS 各 50%。金标 14,580 → 7,290 / 7,290。
func TestSplitPair1to1Halves(t *testing.T) {
	facts := shareFacts([]domain.Relation{{From: "S1", To: "P1", Kind: domain.RelSSPairAS}})
	rs := splitRules(domain.SharePair1to1, 18000,
		timeFactor("R-TIME-Q3", domain.Q3, 12000),
		productFactor("R-PROD-A1", "A", "A1", 15000),
	)
	res, err := Run(facts, rs, Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	d := pickDirect(t, res, domain.KPIRevenue)
	if d.Amount != money.FromYuan(14580, 0) {
		t.Fatalf("金标分账前佣金应为 14580.00, got %s", d.Amount)
	}

	sp := splitsOfKPI(res, domain.KPIRevenue)
	if len(sp) != 1 {
		t.Fatalf("收入口径应恰好一条分账, got %d", len(sp))
	}
	if sp[0].From != "P1" || sp[0].To != "S1" {
		t.Fatalf("分账方向应为 P1→S1, got %s→%s", sp[0].From, sp[0].To)
	}
	if sp[0].Amount != money.FromYuan(7290, 0) {
		t.Fatalf("SS 应得 7290.00, got %s", sp[0].Amount)
	}
	// 成单人保留分账前全额减去转出 —— 守恒的另一半。
	if keep := d.Amount - sp[0].Amount; keep != money.FromYuan(7290, 0) {
		t.Fatalf("AS 应留 7290.00, got %s", keep)
	}

	// 两个口径各转一半：合同 16,200 的一半 8,100，收入 14,580 的一半 7,290。
	// 分账按口径分别算，绝不合计算完再拆 —— 合了就无法分别对账。
	if got := personTotal(t, res, "P1").Split; got != -money.FromYuan(15390, 0) {
		t.Fatalf("P1 分账净额应为 -(8100+7290), got %s", got)
	}
	if got := personTotal(t, res, "S1").Split; got != money.FromYuan(15390, 0) {
		t.Fatalf("S1 分账净额应为 +(8100+7290), got %s", got)
	}
}

// M4 验收：SS 成单与名下 3 个 AS 均分，各 1/4（14,580 → 3,645 × 4）。
// 份额用权重 [1,1,1,1] 直接 Allocate，不先算 2500bps 再乘。
func TestSplitSubordinatesEqualsQuarter(t *testing.T) {
	facts := shareFacts([]domain.Relation{
		{From: "P1", To: "A1", Kind: domain.RelSSPairAS},
		{From: "P1", To: "A2", Kind: domain.RelSSPairAS},
		{From: "P1", To: "A3", Kind: domain.RelSSPairAS},
	})
	rs := splitRules(domain.ShareSplitSubs, 18000,
		timeFactor("R-TIME-Q3", domain.Q3, 12000),
		productFactor("R-PROD-A1", "A", "A1", 15000),
	)
	res, err := Run(facts, rs, Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	sp := splitsOfKPI(res, domain.KPIRevenue)
	if len(sp) != 3 {
		t.Fatalf("应收 3 条分录, got %d", len(sp))
	}
	var sum money.Money
	for i, s := range sp {
		// 收款人按 ID 升序，顺序确定才不会让尾差在两次运行间漂移。
		if want := []domain.PersonID{"A1", "A2", "A3"}[i]; s.To != want {
			t.Fatalf("第 %d 条收款人应为 %s, got %s", i, want, s.To)
		}
		if s.Amount != money.FromYuan(3645, 0) {
			t.Fatalf("%s 应得 3645.00, got %s", s.To, s.Amount)
		}
		sum += s.Amount
	}
	// 成单人保留 1/4，加上 3 条分录恰好是分账前全额（I6）。
	d := pickDirect(t, res, domain.KPIRevenue)
	if keep := d.Amount - sum; keep != money.FromYuan(3645, 0) {
		t.Fatalf("成单人应保留 3645.00, got %s", keep)
	}
}

// 尾差给成单人：1.00 元按 3 人拆，[34,33,33] —— 成单人拿 34 分。
func TestSplitRemainderStaysWithEarner(t *testing.T) {
	facts := shareFacts([]domain.Relation{
		{From: "P1", To: "A1", Kind: domain.RelSSPairAS},
		{From: "P1", To: "A2", Kind: domain.RelSSPairAS},
	})
	facts.Orders[0].Lines[0].Revenue = money.FromYuan(100, 0)
	facts.Orders[0].Lines[0].Contract = money.FromYuan(100, 0)
	facts.Targets[0].Contract = money.FromYuan(100, 0)
	facts.Targets[0].Revenue = money.FromYuan(100, 0)

	res, err := Run(facts, splitRules(domain.ShareSplitSubs, 10000), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	d := pickDirect(t, res, domain.KPIRevenue)
	if d.Amount != 100 {
		t.Fatalf("基数 100.00 元 × 1%% 应为 1.00 元（100 分）, got %s", d.Amount)
	}
	sp := splitsOfKPI(res, domain.KPIRevenue)
	if len(sp) != 2 || sp[0].Amount != 33 || sp[1].Amount != 33 {
		t.Fatalf("两个下属应各 33 分, got %+v", sp)
	}
	if keep := d.Amount - sp[0].Amount - sp[1].Amount; keep != 34 {
		t.Fatalf("尾差应留给成单人（34 分）, got %s", keep)
	}
}

// M4 验收：分账是零和转移 —— 合计不变、达成率不变、管理奖基数仍是分账前全额（I12）。
func TestSplitKeepsTotalAttainmentAndManagerBase(t *testing.T) {
	facts := shareFacts([]domain.Relation{{From: "S1", To: "P1", Kind: domain.RelSSPairAS}})
	facts.Persons["P1"] = domain.Person{ID: "P1", Name: "张三", Manager: "S1", Dept: "SALES"}
	mk := func(mode domain.ShareMode) Result {
		rs := splitRules(mode, 18000)
		rs.Rules = append(rs.Rules, domain.Rule{
			ID: "R-MGR", Phase: domain.PhaseManager,
			Action: domain.Action{Kind: domain.ActManagerBonus, ChainDepth: 1, DecayBPS: []int64{1000}},
		})
		res, err := Run(facts, rs, Options{})
		if err != nil {
			t.Fatalf("run(%s): %v", mode, err)
		}
		return res
	}
	base := mk(domain.ShareNone)
	split := mk(domain.SharePair1to1)

	if base.Total() != split.Total() {
		t.Fatalf("分账不得改变合计: %s vs %s", base.Total(), split.Total())
	}
	if len(base.Attainments) != len(split.Attainments) {
		t.Fatalf("达成率条数不应变化")
	}
	for i := range base.Attainments {
		if base.Attainments[i] != split.Attainments[i] {
			t.Fatalf("分账不得回写达成率（I10）: %+v vs %+v", base.Attainments[i], split.Attainments[i])
		}
	}
	if len(split.ManagerBonuses) == 0 {
		t.Fatal("应产出管理奖金")
	}
	for _, mb := range split.ManagerBonuses {
		want := pickDirect(t, split, mb.KPI).Amount
		if mb.Base != want {
			t.Fatalf("管理奖基数必须是分账前全额（I12）: got %s want %s", mb.Base, want)
		}
	}
}

// 关系缺口按 100% 自留 + 告警，不静默按 0 处理。
func TestSplitMissingRelationRetainsAll(t *testing.T) {
	facts := shareFacts(nil)
	res, err := Run(facts, splitRules(domain.SharePair1to1, 10000), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Splits) != 0 {
		t.Fatalf("无关系时不应有分账, got %d", len(res.Splits))
	}
	if !hasWarning(res, "[share]") {
		t.Fatalf("应告警关系缺口: %v", res.Warnings)
	}
	if got := personTotal(t, res, "P1").Split; got != 0 {
		t.Fatalf("缺口时成单人分账净额应为 0, got %s", got)
	}
}

// 不级联：收款人只拿钱，它的这笔收入不再被继续分出去。
func TestSplitDoesNotCascade(t *testing.T) {
	facts := shareFacts([]domain.Relation{{From: "S1", To: "P1", Kind: domain.RelSSPairAS}})
	res, err := Run(facts, splitRules(domain.SharePair1to1, 18000), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, s := range res.Splits {
		if s.From != "P1" {
			t.Fatalf("分账来源只能是成单人 P1, got %s→%s", s.From, s.To)
		}
	}
	// 收款人 S1 没有任何直接佣金，也就没有可被二次拆分的来源。
	for _, d := range res.Directs {
		if d.Person == "S1" {
			t.Fatalf("S1 不应有直接佣金明细")
		}
	}
	if got := personTotal(t, res, "S1").Direct; got != 0 {
		t.Fatalf("S1 直接佣金应为 0, got %s", got)
	}
}

// I9：分账结果与关系切片录入顺序无关（收款人按 ID 排序后拆份额）。
func TestSplitIgnoresRelationOrder(t *testing.T) {
	rels := []domain.Relation{
		{From: "P1", To: "A2", Kind: domain.RelSSPairAS},
		{From: "P1", To: "A1", Kind: domain.RelSSPairAS},
		{From: "P1", To: "A3", Kind: domain.RelSSPairAS},
	}
	forward, err := Run(shareFacts(rels), splitRules(domain.ShareSplitSubs, 18000), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	back := []domain.Relation{rels[2], rels[1], rels[0]}
	reversed, err := Run(shareFacts(back), splitRules(domain.ShareSplitSubs, 18000), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(forward.Splits) != len(reversed.Splits) {
		t.Fatalf("条数应一致: %d vs %d", len(forward.Splits), len(reversed.Splits))
	}
	for i := range forward.Splits {
		if forward.Splits[i] != reversed.Splits[i] {
			t.Fatalf("第 %d 条分账不一致: %+v vs %+v", i, forward.Splits[i], reversed.Splits[i])
		}
	}
	if forward.CalcRunID != reversed.CalcRunID {
		t.Fatal("关系顺序不应改变 CalcRunID")
	}
}
