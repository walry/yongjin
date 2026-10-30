package calc

import (
	"testing"
	"time"

	"github.com/walry/yongjin/internal/demo"
	"github.com/walry/yongjin/internal/domain"
	"github.com/walry/yongjin/internal/money"
)

// managerFacts 造一笔「AS 成单、与 SS 1:1 分账、两人之上有经理 AD」的最小组织。
//
// 数字刻意与设计文档 §14 的金标用例对齐：合同 1,458,000 × 1% = 14,580，
// 分账后 AS / SS 各 7,290，AD 的管理奖基数必须是 14,580 而不是 7,290（10% 差一倍）。
func managerFacts() domain.Facts {
	return domain.Facts{
		Period:   "2026-10",
		Products: map[string]domain.Product{"SKU-A1": {ID: "SKU-A1", Line: "A", Series: "A1"}},
		Persons: map[domain.PersonID]domain.Person{
			"P-AS": {ID: "P-AS", Name: "销售", Dept: "SALES", Manager: "P-AD"},
			"P-SS": {ID: "P-SS", Name: "主管", Dept: "SALES", Manager: "P-AD"},
			"P-AD": {ID: "P-AD", Name: "经理", Dept: "SALES"},
		},
		Orders: []domain.Order{{
			ID: "O-1", Owner: "P-AS", Territory: "EAST", Class: "NEW",
			ClosedAt: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC),
			Lines: []domain.OrderLine{{
				ID: "L1", Product: "SKU-A1", Contract: money.FromYuan(1458000, 0),
			}},
		}},
		// 方向是 From=上级、To=下属：SS 是 AS 的配对主管。
		Relations: []domain.Relation{{From: "P-SS", To: "P-AS", Kind: domain.RelSSPairAS}},
	}
}

func managerRules() domain.RuleSet {
	return domain.RuleSet{Version: "m", Rules: []domain.Rule{
		{
			ID: "R-DIR-OWNER", Phase: domain.PhaseDirect,
			Scope:  domain.Scope{Lines: []domain.ProductLineID{"A"}, Roles: []domain.RoleID{domain.RoleOwner}},
			Action: domain.Action{Kind: domain.ActPayRole, BaseBPS: 100, ShareMode: domain.SharePair1to1},
		},
		{
			ID: "R-MGR", Phase: domain.PhaseManager,
			Action: domain.Action{Kind: domain.ActManagerBonus, ChainDepth: 1, DecayBPS: []int64{1000}},
		},
	}}
}

func managerSum(res Result) money.Money {
	var s money.Money
	for _, m := range res.ManagerBonuses {
		s += m.Amount
	}
	return s
}

// assertManagerBaseInDirects 校验 I12 的明细层形态：每条管理奖的基数都必须能在
// Directs 里按 (订单行, 来源人, 口径) 找到**金额相等**的那一条。
// 只比金额是不够的——同人同口径多条直接佣金金额可能相同，比 (OrderID, LineID) 才唯一。
func assertManagerBaseInDirects(t *testing.T, res Result) {
	t.Helper()
	for _, m := range res.ManagerBonuses {
		found := false
		for _, d := range res.Directs {
			if d.OrderID == m.OrderID && d.LineID == m.LineID && d.Person == m.Source && d.KPI == m.KPI {
				found = true
				if m.Base != d.Amount {
					t.Fatalf("I12 违反：%s/%s/%s 的管理奖基数 %s != 直接佣金 %s",
						m.OrderID, m.LineID, m.Source, m.Base, d.Amount)
				}
			}
		}
		if !found {
			t.Fatalf("I12 违反：管理奖 %s/%s 来源 %s 口径 %s 在 Directs 里找不到对应明细",
				m.OrderID, m.LineID, m.Source, m.KPI)
		}
	}
}

// M6 验收（设计 §7.1 方案 A）：管理奖基数取分账前全额。
// AD 拿 14,580 × 10% = 1,458，而不是 7,290 × 10% = 729。
func TestManagerBaseIsPreSplit(t *testing.T) {
	res := mustRun(t, managerFacts(), managerRules())

	var direct money.Money
	for _, d := range res.Directs {
		if d.Person == "P-AS" {
			direct = d.Amount
		}
	}
	if direct != money.FromYuan(14580, 0) {
		t.Fatalf("P-AS 直接佣金 = %s, want 14580.00", direct)
	}

	// 分账确实发生了，否则这个用例证明不了「分账前」。
	var splitToSS money.Money
	for _, s := range res.Splits {
		if s.From == "P-AS" && s.To == "P-SS" {
			splitToSS += s.Amount
		}
	}
	if splitToSS != money.FromYuan(7290, 0) {
		t.Fatalf("P-AS → P-SS 分账 = %s, want 7290.00", splitToSS)
	}

	var got *ManagerBonus
	for i := range res.ManagerBonuses {
		if res.ManagerBonuses[i].Source == "P-AS" && res.ManagerBonuses[i].Manager == "P-AD" {
			got = &res.ManagerBonuses[i]
		}
	}
	if got == nil {
		t.Fatal("缺少 P-AD 的管理奖金")
	}
	if got.Base != money.FromYuan(14580, 0) {
		t.Fatalf("管理奖基数 = %s, want 14580.00（分账前全额，不是 7290.00）", got.Base)
	}
	if got.Amount != money.FromYuan(1458, 0) {
		t.Fatalf("管理奖金额 = %s, want 1458.00", got.Amount)
	}
	if got.Amount == money.FromYuan(729, 0) {
		t.Fatal("管理奖按分账后净额算了 729.00 —— 基数被串到 SplitLine 上（I12）")
	}

	// 结算层同样要按分账前给：AD 的管理奖合计就是 1,458。
	var adTotal money.Money
	for _, p := range res.Persons {
		if p.Person == "P-AD" {
			adTotal = p.Manager
		}
	}
	if adTotal != money.FromYuan(1458, 0) {
		t.Fatalf("P-AD 管理奖合计 = %s, want 1458.00", adTotal)
	}

	assertManagerBaseInDirects(t, res)
}

// 池分成不进管理奖上溯（设计 §8.4）：预算怎么改，管理奖一分不变。
func TestManagerBonusIgnoresPools(t *testing.T) {
	// 三档预算：0（一分不发）、基线、缩到 1/10（领不满）。
	// 用缩预算而不是放大预算：demo 的基线预算本来就能覆盖全部领取额，
	// 再放大发放额不会变，「管理奖不变」就成了没有区分度的断言。
	run := func(budget money.Money) Result {
		facts := demo.Facts()
		if budget >= 0 {
			for i := range facts.Pools {
				facts.Pools[i].Budget = budget
			}
		}
		res := mustRun(t, facts, demo.RulesV1())
		assertManagerBaseInDirects(t, res)
		return res
	}

	baseline := run(-1) // 负数 = 保持 demo 原值
	if poolPay(baseline) == 0 {
		t.Fatal("基线池发放不应为 0")
	}
	none := run(0)
	small := run(money.FromYuan(1, 0))

	// 前提：池的发放额确实变了，否则「管理奖不变」是废话。
	if poolPay(none) != 0 {
		t.Fatalf("预算为 0 时不应有任何池发放，got %s", poolPay(none))
	}
	if poolPay(small) >= poolPay(baseline) {
		t.Fatalf("预算缩到 1 元后池发放应减少：基线 %s / 缩后 %s", poolPay(baseline), poolPay(small))
	}

	base := managerSum(baseline)
	if managerSum(none) != base || managerSum(small) != base {
		t.Fatalf("池预算变化不应影响管理奖：基线 %s / 零预算 %s / 极小预算 %s",
			base, managerSum(none), managerSum(small))
	}
}

func poolPay(res Result) money.Money {
	var s money.Money
	for _, p := range res.Pools {
		s += p.Distributed
	}
	return s
}
