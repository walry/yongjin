package calc

import (
	"testing"
	"time"

	"github.com/walry/yongjin/internal/demo"
	"github.com/walry/yongjin/internal/domain"
	"github.com/walry/yongjin/internal/money"
)

// factorRules 造一条直接佣金（基准 100bps）+ 一条单档位表（覆盖 [0,+∞)）。
// 单档位表是合法的：最低档 FromBPS=0、最高档 ToBPS=0，即全区间只有一档 ——
// 用它把档位倍数钉成常量，测试就只需要观察因子。
func factorRules(bandBPS int64, extra ...domain.Rule) domain.RuleSet {
	rules := []domain.Rule{
		{
			ID: "R-DIR", Phase: domain.PhaseDirect,
			Scope:  domain.Scope{Lines: []domain.ProductLineID{"A"}, Roles: []domain.RoleID{domain.RoleOwner}},
			Action: domain.Action{Kind: domain.ActPayRole, BaseBPS: 100},
		},
		{
			ID: "R-ATTAIN", Phase: domain.PhaseAttain,
			Action: domain.Action{Kind: domain.ActCredit, Bands: []domain.Band{
				{ID: "ONLY", FromBPS: 0, ToBPS: 0, CoeffBPS: bandBPS},
			}},
		},
	}
	rules = append(rules, extra...)
	return domain.RuleSet{Version: "t", Rules: rules}
}

// timeFactor / productFactor / territoryFactor 是构造因子规则的简写。
func timeFactor(id string, q domain.Quarter, bps int64) domain.Rule {
	return domain.Rule{
		ID: domain.RuleID(id), Phase: domain.PhaseFactor,
		Scope:  domain.Scope{Quarters: []domain.Quarter{q}},
		Action: domain.Action{Kind: domain.ActTimeFactor, MultiplierBPS: bps},
	}
}

func productFactor(id string, line domain.ProductLineID, series domain.SeriesID, bps int64) domain.Rule {
	s := domain.Scope{Lines: []domain.ProductLineID{line}}
	if series != "" {
		s.Series = []domain.SeriesID{series}
	}
	return domain.Rule{
		ID: domain.RuleID(id), Phase: domain.PhaseFactor,
		Scope:  s,
		Action: domain.Action{Kind: domain.ActProductFactor, MultiplierBPS: bps},
	}
}

func territoryFactor(id string, t domain.TerritoryID, bps int64) domain.Rule {
	return domain.Rule{
		ID: domain.RuleID(id), Phase: domain.PhaseFactor,
		Scope:  domain.Scope{Territories: []domain.TerritoryID{t}},
		Action: domain.Action{Kind: domain.ActTerritoryFactor, MultiplierBPS: bps},
	}
}

// M3 验收：因子链只舍入一次。逐步 ApplyRate 在同一个基数上会少算一分钱，
// 800 万级订单上会漂出几毛 —— 这正是 ApplyChain 存在的理由。
func TestFactorChainSingleRounding(t *testing.T) {
	// 4500.16 元：逐步乘得 14580 分，一次乘得 14581 分，只差一分，正好卡在舍入边界。
	base := money.FromYuan(4500, 16)
	facts := attainFacts(base, base, money.FromYuan(10000, 0))
	facts.Orders[0].ClosedAt = time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)

	rs := factorRules(18000,
		timeFactor("R-TIME-Q4", domain.Q4, 12000),
		productFactor("R-PROD-A", "A", "", 15000),
	)
	res, err := Run(facts, rs, Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	d := pickDirect(t, res, domain.KPIContract)

	want := money.ApplyChain(base, 100, 18000, 12000, 15000)
	if d.Amount != want {
		t.Fatalf("金额 = %s, want %s（一次舍入）", d.Amount, want)
	}
	// 逐步乘：先乘费率、再乘档位、再乘每个因子，每步都舍入。
	step := money.ApplyRate(base, 100)
	step = money.ApplyRate(step, 18000)
	step = money.ApplyRate(step, 12000)
	step = money.ApplyRate(step, 15000)
	if step == d.Amount {
		t.Fatalf("这组数应当能区分一次舍入与逐步舍入，两者都是 %s", step)
	}
	if d.Amount != money.FromYuan(145, 81) {
		t.Fatalf("链式结果 = %s, want 145.81", d.Amount)
	}
}

// M3：单个 Kind 只取一条（按 better 裁决），不同 Kind 相乘；倍数为 0 的因子被跳过。
func TestFactorPerKindAndMultipliedAcrossKinds(t *testing.T) {
	base := money.FromYuan(100000, 0)
	facts := attainFacts(base, base, money.FromYuan(10000, 0))
	facts.Orders[0].ClosedAt = time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)

	rs := factorRules(10000,
		// 时间：通配一条 + Q4 一条。Q4 更具体（特异性 4 > 0），必须赢。
		domain.Rule{ID: "R-TIME-ANY", Phase: domain.PhaseFactor,
			Action: domain.Action{Kind: domain.ActTimeFactor, MultiplierBPS: 20000}},
		timeFactor("R-TIME-Q4", domain.Q4, 12000),
		productFactor("R-PROD-A", "A", "", 15000),
	)
	res, err := Run(facts, rs, Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	d := pickDirect(t, res, domain.KPIContract)

	// 只配了时间与产品，区域没有规则 —— 它是 1.0，不是"没算"。
	if len(d.Factors) != 2 {
		t.Fatalf("应只有时间与产品两个因子，got %+v", d.Factors)
	}
	// 固定顺序：时间在前、产品在后。顺序不影响金额，只影响可比对性。
	if d.Factors[0].Kind != domain.ActTimeFactor || d.Factors[0].RuleID != "R-TIME-Q4" {
		t.Fatalf("时间因子应取更具体的 Q4 那条: %+v", d.Factors[0])
	}
	if d.Factors[1].Kind != domain.ActProductFactor {
		t.Fatalf("产品因子缺失: %+v", d.Factors)
	}
	// 100000 × 1% × 1.0 × 1.2 × 1.5 = 1800.00
	if want := money.FromYuan(1800, 0); d.Amount != want {
		t.Fatalf("金额 = %s, want %s（时间 1.2 × 产品 1.5）", d.Amount, want)
	}
}

// M3：没配因子时倍数恒 1.0，金额与配之前完全一致 —— 因子是乘上去的，漏配不会清零。
func TestNoFactorsMeansUnitMultipliers(t *testing.T) {
	base := money.FromYuan(100000, 0)
	facts := attainFacts(base, base, money.FromYuan(10000, 0))
	facts.Orders[0].ClosedAt = time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)

	only, err := Run(facts, factorRules(10000), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	with, err := Run(facts, factorRules(10000, timeFactor("R-TIME-Q4", domain.Q4, 15000)), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	d0 := pickDirect(t, only, domain.KPIContract)
	d1 := pickDirect(t, with, domain.KPIContract)
	if len(d0.Factors) != 0 {
		t.Fatalf("未配因子时不应有 FactorHit: %+v", d0.Factors)
	}
	if want := money.FromYuan(1000, 0); d0.Amount != want {
		t.Fatalf("基准金额 = %s, want %s", d0.Amount, want)
	}
	if want := money.FromYuan(1500, 0); d1.Amount != want {
		t.Fatalf("乘 1.5 后 = %s, want %s", d1.Amount, want)
	}
}

// M3 验收：设计 §14 的金标用例。
// 450,000 × 1.0%（R-BASE-OWNER）× 1.8（NORMAL，达成 90%）× 1.2(Q3) × 1.5(A1) = 14,580.00
func TestGoldExampleRevenueChain(t *testing.T) {
	facts := attainFacts(money.FromYuan(500000, 0), money.FromYuan(450000, 0), money.FromYuan(500000, 0))
	facts.Orders[0].ClosedAt = time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC) // Q3
	facts.Orders[0].Territory = "SOUTH"                                     // 华南无加成
	facts.Targets[0].Contract = money.FromYuan(600000, 0)

	rs := domain.RuleSet{Version: "gold", Rules: []domain.Rule{
		{
			ID: "R-BASE-OWNER", Phase: domain.PhaseDirect,
			Scope:  domain.Scope{Lines: []domain.ProductLineID{"A"}, Roles: []domain.RoleID{domain.RoleOwner}},
			Action: domain.Action{Kind: domain.ActPayRole, BaseBPS: 100},
		},
		{
			ID: "R-BAND-OWNER", Phase: domain.PhaseAttain,
			Action: domain.Action{Kind: domain.ActCredit, Bands: []domain.Band{
				{ID: "LOW", FromBPS: 0, ToBPS: 7000, CoeffBPS: 10000},
				{ID: "NORMAL", FromBPS: 7000, ToBPS: 0, CoeffBPS: 18000},
			}},
		},
		timeFactor("R-TIME-Q3", domain.Q3, 12000),
		productFactor("R-PROD-A1", "A", "A1", 15000),
		territoryFactor("R-TERR-EAST", "EAST", 11000),
	}}
	res, err := Run(facts, rs, Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	d := pickDirect(t, res, domain.KPIRevenue)
	if want := money.FromYuan(14580, 0); d.Amount != want {
		t.Fatalf("收入口径佣金 = %s, want %s", d.Amount, want)
	}
	if d.BandID != "NORMAL" || d.CoeffBPS != 18000 {
		t.Fatalf("90%% 应命中 NORMAL 1.8x, got %s/%d", d.BandID, d.CoeffBPS)
	}
}

// M3 验收（I8）：任何一笔金额都必须能被它自己的 Trace 复算出来。
func TestTraceReplaysAmount(t *testing.T) {
	res, err := Run(demo.Facts(), demo.RulesV1Full(), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Trace) == 0 {
		t.Fatal("Trace 不应为空")
	}
	withFactors := 0
	for _, tr := range res.Trace {
		if tr.Replay() != tr.Amount {
			t.Fatalf("Trace 复算不一致: 阶段 %s 规则 %s 金额 %s 复算 %s（基数 %s × %d × %d，因子 %+v，份额 %d，特殊规则增量 %s）",
				tr.Phase, tr.RuleID, tr.Amount, tr.Replay(), tr.Base, tr.RateBPS, tr.CoeffBPS,
				tr.Factors, tr.Weight, tr.OverlayDelta)
		}
		if len(tr.Factors) > 0 {
			withFactors++
		}
	}
	if withFactors == 0 {
		t.Fatal("带因子的规则集应当产出带 FactorHit 的 Trace")
	}
}
