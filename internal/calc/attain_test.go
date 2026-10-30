package calc

import (
	"strings"
	"testing"

	"github.com/walry/yongjin/internal/demo"
	"github.com/walry/yongjin/internal/domain"
	"github.com/walry/yongjin/internal/money"
)

// attainFacts 造一份最小事实：一个人、一条订单行、一个目标额。
// 合同额同时被当作收入额，除非另有说明。
func attainFacts(contract, revenue, target money.Money) domain.Facts {
	return domain.Facts{
		Period:   "2026-10",
		Products: map[string]domain.Product{"SKU": {ID: "SKU", Line: "A", Series: "A1"}},
		Persons:  map[domain.PersonID]domain.Person{"P1": {ID: "P1", Name: "张三", Dept: "SALES"}},
		Orders: []domain.Order{{
			ID: "O-1", Owner: "P1", Territory: "EAST", Class: "NEW",
			Lines: []domain.OrderLine{{ID: "L1", Product: "SKU", Contract: contract, Revenue: revenue}},
		}},
		Targets: []domain.Target{{Person: "P1", Period: "2026-10", Contract: target, Revenue: target}},
	}
}

// attainRules 只要两条规则：一条基准费率，一条档位表。
func attainRules(bands []domain.Band) domain.RuleSet {
	rules := []domain.Rule{{
		ID: "R-DIR", Phase: domain.PhaseDirect,
		Scope:  domain.Scope{Lines: []domain.ProductLineID{"A"}, Roles: []domain.RoleID{domain.RoleOwner}},
		Action: domain.Action{Kind: domain.ActPayRole, BaseBPS: 100},
	}}
	if len(bands) > 0 {
		// 空的 attain 规则会在 Compile 被拒（未配置档位表），所以传 nil 表示"不配档位"。
		rules = append(rules, domain.Rule{
			ID: "R-ATTAIN", Phase: domain.PhaseAttain,
			Action: domain.Action{Kind: domain.ActCredit, Bands: bands},
		})
	}
	return domain.RuleSet{Version: "t", Rules: rules}
}

// M2 验收：跨档边界必须由交叉相乘裁决，69.99 / 70.00 / 70.01 分别落在不同档。
func TestBandBoundaryUsesCrossMultiplication(t *testing.T) {
	bands := []domain.Band{
		{ID: "LOW", FromBPS: 0, ToBPS: 7000, CoeffBPS: 5000},
		{ID: "HIGH", FromBPS: 7000, ToBPS: 0, CoeffBPS: 18000},
	}
	target := money.FromYuan(1000000, 0)
	// 比率先取整就会把 69.996% 舍成 70.00% 而跳档，这里逐分卡边界。
	cases := []struct {
		contract money.Money
		want     domain.BandID
	}{
		{money.FromYuan(699900, 0), "LOW"},  // 69.99%
		{money.FromYuan(700000, 0), "HIGH"}, // 70.00%（含下界）
		{money.FromYuan(700100, 0), "HIGH"}, // 70.01%
		{money.FromYuan(699996, 0), "LOW"},  // 69.9996%，仍不足 70%
	}
	for _, c := range cases {
		res, err := Run(attainFacts(c.contract, c.contract, target), attainRules(bands), Options{})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		d := pickDirect(t, res, domain.KPIContract)
		if d.BandID != c.want {
			t.Fatalf("基数 %s 应命中 %s, got %s", c.contract, c.want, d.BandID)
		}
	}
}

// M2 验收：目标额为 0 时按 Q8 取最低档并告警，不能靠整数除法除出 NaN/panic。
func TestAttainTargetZeroTakesLowestBand(t *testing.T) {
	bands := []domain.Band{
		{ID: "LOW", FromBPS: 0, ToBPS: 7000, CoeffBPS: 5000},
		{ID: "HIGH", FromBPS: 7000, ToBPS: 0, CoeffBPS: 18000},
	}
	facts := attainFacts(money.FromYuan(100000, 0), money.FromYuan(100000, 0), 0)
	res, err := Run(facts, attainRules(bands), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if d := pickDirect(t, res, domain.KPIContract); d.BandID != "LOW" {
		t.Fatalf("目标为 0 应取最低档, got %s", d.BandID)
	}
	if !hasWarning(res, "目标额") {
		t.Fatal("目标额为 0 必须告警，不能静默取最低档")
	}
}

// M2 验收：双口径各出一笔，基数各不相同，绝不合并且不互相顶替。
func TestDualKPIEmitsTwoDirectLines(t *testing.T) {
	facts := attainFacts(money.FromYuan(1000, 0), money.FromYuan(800, 0), money.FromYuan(2000, 0))
	res, err := Run(facts, attainRules(nil), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Directs) != 2 {
		t.Fatalf("同一行同一人应产出两条明细（合同/收入）, got %d", len(res.Directs))
	}
	contract := pickDirect(t, res, domain.KPIContract)
	revenue := pickDirect(t, res, domain.KPIRevenue)
	if contract.Base != money.FromYuan(1000, 0) || contract.Amount != money.FromYuan(10, 0) {
		t.Fatalf("合同口径明细错误: %+v", contract)
	}
	if revenue.Base != money.FromYuan(800, 0) || revenue.Amount != money.FromYuan(8, 0) {
		t.Fatalf("收入口径明细错误: %+v", revenue)
	}
	if res.Persons[0].DirectContract != money.FromYuan(10, 0) ||
		res.Persons[0].DirectRevenue != money.FromYuan(8, 0) ||
		res.Persons[0].Direct != money.FromYuan(18, 0) {
		t.Fatalf("结算未按口径拆开: %+v", res.Persons[0])
	}
}

// M2 验收：没有档位表时倍率恒 1.0，逐条等于旧行为的 ApplyRate(base, BaseBPS)。
func TestNoBandsMeansUnitMultiplier(t *testing.T) {
	res, err := Run(demo.Facts(), demo.RulesV1(), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var contractTotal money.Money
	for _, d := range res.Directs {
		if d.CoeffBPS != money.BPS {
			t.Fatalf("无档位时倍数必须为 10000: %+v", d)
		}
		if d.BandID != "" {
			t.Fatalf("无档位时不应有档位标识: %+v", d)
		}
		if d.KPI == domain.KPIContract {
			contractTotal += d.Amount
		}
	}
	if contractTotal == 0 {
		t.Fatal("合同口径合计不应为 0")
	}
}

// M2：档位倍数改变金额，且提升档位必须让金额严格变大（单调性）。
func TestBandMultiplierScalesAmount(t *testing.T) {
	bands := demo.Bands()
	base := money.FromYuan(1000000, 0)
	// 目标 100 万，实际 138 万 = 138% -> TOP(1.5x)
	facts := attainFacts(base, base, money.FromYuan(1000000, 0))
	facts.Orders[0].Lines[0].Contract = money.FromYuan(1380000, 0)
	res, err := Run(facts, attainRules(bands), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	d := pickDirect(t, res, domain.KPIContract)
	if d.BandID != "TOP" || d.CoeffBPS != 15000 {
		t.Fatalf("138%% 应命中 TOP 1.5x, got %s/%d", d.BandID, d.CoeffBPS)
	}
	// 1,380,000 × 1% × 1.5 = 20,700.00，一次舍入
	if want := money.FromYuan(20700, 0); d.Amount != want {
		t.Fatalf("金额 = %s, want %s", d.Amount, want)
	}
}

func pickDirect(t *testing.T, res Result, kpi domain.KPI) DirectLine {
	t.Helper()
	for _, d := range res.Directs {
		if d.KPI == kpi && d.Person == "P1" {
			return d
		}
	}
	t.Fatalf("未找到口径 %s 的明细", kpi)
	return DirectLine{}
}

func hasWarning(res Result, sub string) bool {
	for _, w := range res.Warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}
