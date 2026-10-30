package rules

import (
	"testing"

	"github.com/walry/yongjin/internal/domain"
)

func rule(id string, phase domain.Phase, scope domain.Scope, bps int64) domain.Rule {
	return domain.Rule{
		ID: domain.RuleID(id), Phase: phase, Scope: scope,
		Action: domain.Action{Kind: domain.ActPayRole, BaseBPS: bps},
	}
}

var cat = Catalog{
	Lines: []domain.ProductLineID{"A", "B"},
	Roles: []domain.RoleID{domain.RoleOwner, domain.RolePre},
}

func TestSpecificityWins(t *testing.T) {
	rs := domain.RuleSet{Version: "v1", Rules: []domain.Rule{
		rule("generic", domain.PhaseDirect, domain.Scope{Lines: []domain.ProductLineID{"A"}}, 100),
		rule("series", domain.PhaseDirect, domain.Scope{Lines: []domain.ProductLineID{"A"}, Series: []domain.SeriesID{"A1"}}, 200),
		rule("role", domain.PhaseDirect, domain.Scope{Lines: []domain.ProductLineID{"B"}, Roles: []domain.RoleID{domain.RoleOwner}}, 150),
	}}
	ix, err := Compile(rs, cat)
	if err != nil {
		t.Fatal(err)
	}

	if r, ok := ix.Best(domain.PhaseDirect, Ctx{Line: "A", Series: "A1", Role: domain.RoleOwner}, "2026-10"); !ok || r.ID != "series" {
		t.Fatalf("A1 应命中 series 规则, got %v", r.ID)
	}
	if r, ok := ix.Best(domain.PhaseDirect, Ctx{Line: "A", Series: "A2", Role: domain.RoleOwner}, "2026-10"); !ok || r.ID != "generic" {
		t.Fatalf("A2 应回落 generic, got %v", r.ID)
	}
	if r, ok := ix.Best(domain.PhaseDirect, Ctx{Line: "B", Role: domain.RoleOwner}, "2026-10"); !ok || r.ID != "role" {
		t.Fatalf("B/owner 应命中 role, got %v", r.ID)
	}
}

func TestConflictRejected(t *testing.T) {
	same := domain.Scope{Lines: []domain.ProductLineID{"A"}}
	rs := domain.RuleSet{Version: "v-bad", Rules: []domain.Rule{
		rule("r1", domain.PhaseDirect, same, 100),
		rule("r2", domain.PhaseDirect, same, 200),
	}}
	if _, err := Compile(rs, cat); err == nil {
		t.Fatal("同特异性同优先级的两条规则应当被拒绝")
	}
}

func TestPriorityResolvesAmbiguity(t *testing.T) {
	same := domain.Scope{Lines: []domain.ProductLineID{"A"}}
	r1 := rule("r1", domain.PhaseDirect, same, 100)
	r2 := rule("r2", domain.PhaseDirect, same, 200)
	r2.Priority = 10
	if _, err := Compile(domain.RuleSet{Version: "v2", Rules: []domain.Rule{r1, r2}}, cat); err != nil {
		t.Fatalf("显式优先级应能裁决: %v", err)
	}
}

func TestGapDetected(t *testing.T) {
	rs := domain.RuleSet{Version: "v3", Rules: []domain.Rule{
		rule("f", domain.PhaseFund, domain.Scope{Lines: []domain.ProductLineID{"A"}}, 300),
		rule("d", domain.PhaseDirect, domain.Scope{Lines: []domain.ProductLineID{"A"}}, 100),
	}}
	ix, err := Compile(rs, cat)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Gaps) == 0 {
		t.Fatal("产品线 B 未配置规则，应产生缺口告警")
	}
}

func bandRule(id string, bands ...domain.Band) domain.Rule {
	return domain.Rule{
		ID: domain.RuleID(id), Phase: domain.PhaseAttain,
		Action: domain.Action{Kind: domain.ActCredit, Bands: bands},
	}
}

// M2：档位表是乘法因子，取不到档 = 少发钱且不报错，所以任何缺口都必须拒绝发布。
func TestBandTableRejected(t *testing.T) {
	ok := []domain.Band{
		{ID: "LOW", FromBPS: 0, ToBPS: 8000, CoeffBPS: 9000},
		{ID: "HIGH", FromBPS: 8000, ToBPS: 0, CoeffBPS: 15000},
	}
	if _, err := Compile(domain.RuleSet{Version: "ok", Rules: []domain.Rule{bandRule("a", ok...)}}, cat); err != nil {
		t.Fatalf("合法档位表不应被拒: %v", err)
	}

	cases := map[string][]domain.Band{
		"未配置":       nil,
		"最低档不从 0 起": {{ID: "LOW", FromBPS: 100, ToBPS: 0, CoeffBPS: 9000}},
		"最高档有上界": {
			{ID: "LOW", FromBPS: 0, ToBPS: 8000, CoeffBPS: 9000},
			{ID: "HIGH", FromBPS: 8000, ToBPS: 20000, CoeffBPS: 15000},
		},
		"档位有洞": {
			{ID: "LOW", FromBPS: 0, ToBPS: 8000, CoeffBPS: 9000},
			{ID: "HIGH", FromBPS: 9000, ToBPS: 0, CoeffBPS: 15000},
		},
		"档位重叠": {
			{ID: "LOW", FromBPS: 0, ToBPS: 9000, CoeffBPS: 9000},
			{ID: "HIGH", FromBPS: 8000, ToBPS: 0, CoeffBPS: 15000},
		},
		"倍数非单调": {
			{ID: "LOW", FromBPS: 0, ToBPS: 8000, CoeffBPS: 15000},
			{ID: "HIGH", FromBPS: 8000, ToBPS: 0, CoeffBPS: 9000},
		},
		"倍数为 0": {{ID: "LOW", FromBPS: 0, ToBPS: 0, CoeffBPS: 0}},
	}
	for name, bands := range cases {
		rs := domain.RuleSet{Version: "bad", Rules: []domain.Rule{bandRule("a", bands...)}}
		if _, err := Compile(rs, cat); err == nil {
			t.Fatalf("%s 应被拒绝发布", name)
		}
	}
}

// 档位表只能配在 attain 阶段：配在 direct 上等于让人以为"这道单走这个倍数"。
func TestBandsOutsideAttainRejected(t *testing.T) {
	r := rule("d", domain.PhaseDirect, domain.Scope{Lines: []domain.ProductLineID{"A"}}, 100)
	r.Action.Bands = []domain.Band{{ID: "LOW", FromBPS: 0, ToBPS: 0, CoeffBPS: 9000}}
	if _, err := Compile(domain.RuleSet{Version: "bad", Rules: []domain.Rule{r}}, cat); err == nil {
		t.Fatal("direct 阶段配档位表应被拒绝")
	}
}

// 达成率是"人员 × 期间"指标，按产品线/区域配作用域永远命中不了，必须拒发。
func TestAttainScopeMisuseRejected(t *testing.T) {
	r := bandRule("a", domain.Band{ID: "LOW", FromBPS: 0, ToBPS: 0, CoeffBPS: 9000})
	r.Scope = domain.Scope{Lines: []domain.ProductLineID{"A"}}
	if _, err := Compile(domain.RuleSet{Version: "bad", Rules: []domain.Rule{r}}, cat); err == nil {
		t.Fatal("attain 规则按产品线配作用域应被拒绝")
	}
}

// 冲突检测改成作用域交集判定后，通配交叉造成的重叠仍必须被抓到。
func TestConflictDetectedAcrossWildcardDimensions(t *testing.T) {
	a := rule("a", domain.PhaseDirect, domain.Scope{Lines: []domain.ProductLineID{"A"}}, 100)
	b := rule("b", domain.PhaseDirect, domain.Scope{Roles: []domain.RoleID{domain.RoleOwner}}, 200)
	if _, err := Compile(domain.RuleSet{Version: "bad", Rules: []domain.Rule{a, b}}, cat); err == nil {
		t.Fatal("A/owner 上下文同时命中两条规则，应判冲突")
	}
	// 作用域不相交时不算冲突：不同产品线各配各的。
	c := rule("c", domain.PhaseDirect, domain.Scope{Lines: []domain.ProductLineID{"A"}}, 100)
	d := rule("d", domain.PhaseDirect, domain.Scope{Lines: []domain.ProductLineID{"B"}}, 200)
	if _, err := Compile(domain.RuleSet{Version: "v", Rules: []domain.Rule{c, d}}, cat); err != nil {
		t.Fatalf("不相交的作用域不应冲突: %v", err)
	}
}

func TestOverlayExpires(t *testing.T) {
	rs := domain.RuleSet{Version: "v4", Overlays: []domain.Overlay{{
		ID: "ov", Target: domain.OverlayAddRate, Phase: domain.PhaseDirect,
		Scope: domain.Scope{Lines: []domain.ProductLineID{"A"}}, BPS: 30, ExpiresAt: "2026-10",
	}}}
	ix, err := Compile(rs, cat)
	if err != nil {
		t.Fatal(err)
	}
	ctx := Ctx{Line: "A", Role: domain.RoleOwner}
	if got := ix.Overlays(domain.PhaseDirect, ctx, "2026-10"); len(got) != 1 {
		t.Fatalf("有效期内应命中, got %d", len(got))
	}
	if got := ix.Overlays(domain.PhaseDirect, ctx, "2026-11"); len(got) != 0 {
		t.Fatalf("过期后应失效, got %d", len(got))
	}
}

func factorRule(id string, kind domain.ActionKind, scope domain.Scope, bps int64) domain.Rule {
	return domain.Rule{
		ID: domain.RuleID(id), Phase: domain.PhaseFactor, Scope: scope,
		Action: domain.Action{Kind: kind, MultiplierBPS: bps},
	}
}

// M3 验收：区域只能当乘数。配进 direct 等于既是筛选维度又是乘数，
// 同一维度被乘两次，而 1.1×1.1 与直接配 1.21 在结果上完全无法区分。
func TestTerritoryInDirectRejected(t *testing.T) {
	base := rule("d", domain.PhaseDirect, domain.Scope{Lines: []domain.ProductLineID{"A"}}, 100)
	bad := rule("d2", domain.PhaseDirect, domain.Scope{
		Lines: []domain.ProductLineID{"A"}, Territories: []domain.TerritoryID{"EAST"},
	}, 120)
	if _, err := Compile(domain.RuleSet{Version: "bad", Rules: []domain.Rule{base, bad}}, cat); err == nil {
		t.Fatal("direct 阶段配区域作用域应被拒绝")
	}

	// 同样的区域配在 factor 阶段是唯一正确的位置。
	ok := factorRule("f", domain.ActTerritoryFactor, domain.Scope{Territories: []domain.TerritoryID{"EAST"}}, 11000)
	if _, err := Compile(domain.RuleSet{Version: "ok", Rules: []domain.Rule{base, ok}}, cat); err != nil {
		t.Fatalf("factor 阶段配区域应合法: %v", err)
	}
}

// M3：因子规则的配错都属于运行时看不出来的那类 —— 倍数 0 会把钱乘没，
// 只写系列的规则会被产品线规则静默压过。全部拒发。
func TestFactorRuleRejected(t *testing.T) {
	lineA := domain.Scope{Lines: []domain.ProductLineID{"A"}}
	case1 := factorRule("z", domain.ActTimeFactor, domain.Scope{Quarters: []domain.Quarter{domain.Q4}}, 0)
	case2 := factorRule("k", domain.ActPayRole, lineA, 12000)
	case3 := factorRule("s", domain.ActProductFactor, domain.Scope{Series: []domain.SeriesID{"A1"}}, 15000)
	case4 := rule("p", domain.PhaseDirect, lineA, 100)
	case4.Action.Kind = domain.ActTimeFactor
	case4.Action.MultiplierBPS = 12000

	for name, bad := range map[string]domain.Rule{"倍数为 0": case1, "Kind 不是因子": case2, "只写系列不写产品线": case3, "因子动作配在 direct": case4} {
		if _, err := Compile(domain.RuleSet{Version: "bad", Rules: []domain.Rule{bad}}, cat); err == nil {
			t.Fatalf("%s 应被拒绝发布", name)
		}
	}

	// 系列因子同时写明产品线就合法：特异性 8+4=12，压得过产品线级那条 8。
	series, err := Compile(domain.RuleSet{Version: "ok", Rules: []domain.Rule{
		factorRule("f1", domain.ActProductFactor, lineA, 11000),
		factorRule("f2", domain.ActProductFactor, domain.Scope{Lines: []domain.ProductLineID{"A"}, Series: []domain.SeriesID{"A1"}}, 15000),
	}}, cat)
	if err != nil {
		t.Fatalf("系列因子写明产品线应合法: %v", err)
	}
	r, _ := series.BestByKind(domain.PhaseFactor, domain.ActProductFactor, Ctx{Line: "A", Series: "A1"}, "2026-10")
	if r.ID != "f2" {
		t.Fatalf("系列级因子应压过产品线级, got %s", r.ID)
	}
}

// M3：不同 Kind 的因子各自成域 —— 同时命中是相乘，不是冲突。
// 这条守的是冲突检测的裁决域粒度：若按整个 factor 阶段判冲突，
// 任何一套时间×产品×区域的配置都会被自己判成不可发布。
func TestFactorKindsDoNotConflict(t *testing.T) {
	tm := factorRule("t", domain.ActTimeFactor, domain.Scope{Quarters: []domain.Quarter{domain.Q4}}, 12000)
	tr := factorRule("r", domain.ActTerritoryFactor, domain.Scope{Territories: []domain.TerritoryID{"EAST"}}, 11000)
	// 两者特异性都是 4、优先级都是 0，作用域按 Overlaps 也算相交（季度与区域互不约束）。
	if _, err := Compile(domain.RuleSet{Version: "ok", Rules: []domain.Rule{tm, tr}}, cat); err != nil {
		t.Fatalf("不同 Kind 的因子不构成冲突: %v", err)
	}

	// 同一个 Kind 的两条，特异性与优先级都相同且作用域相交，才是真的无法裁决。
	tm2 := factorRule("t2", domain.ActTimeFactor, domain.Scope{Quarters: []domain.Quarter{domain.Q1}}, 13000)
	tm3 := factorRule("t3", domain.ActTimeFactor, domain.Scope{Quarters: []domain.Quarter{domain.Q1, domain.Q2}}, 14000)
	if _, err := Compile(domain.RuleSet{Version: "bad", Rules: []domain.Rule{tm2, tm3}}, cat); err == nil {
		t.Fatal("同 Kind 同特异性重叠应判冲突")
	}
}

// M4：分账方式只允许配在 PhaseDirect，且取值必须已定义。
// 两者都是「运行时既不生效也不报错」的配置错误：配在池阶段永远读不到，
// 写了未知取值会一路传到运行时，最后静默按 100% 自留。
func TestShareModeMisuseRejected(t *testing.T) {
	lineA := domain.Scope{Lines: []domain.ProductLineID{"A"}}

	ok := rule("d1", domain.PhaseDirect, lineA, 100)
	ok.Action.ShareMode = domain.SharePair1to1
	if _, err := Compile(domain.RuleSet{Version: "ok", Rules: []domain.Rule{ok}}, cat); err != nil {
		t.Fatalf("直接佣金配分账方式应合法: %v", err)
	}

	badPhase := rule("d2", domain.PhaseDistribute, domain.Scope{Pools: []domain.PoolID{"P"}}, 100)
	badPhase.Action.ShareMode = domain.ShareSplitSubs

	badValue := rule("d3", domain.PhaseDirect, lineA, 100)
	badValue.Action.ShareMode = domain.ShareMode("one_to_many")

	for name, bad := range map[string]domain.Rule{
		"分账配在池分发阶段": badPhase,
		"分账方式未定义":   badValue,
	} {
		if _, err := Compile(domain.RuleSet{Version: "bad", Rules: []domain.Rule{bad}}, cat); err == nil {
			t.Fatalf("%s 应被拒绝发布", name)
		}
	}
}

// M5：池分发的四类配置错误必须在发布期拒绝，而不是等运行时把钱静默退回结转。
func TestPoolDistModeMisuseRejected(t *testing.T) {
	poolA := domain.Scope{Pools: []domain.PoolID{"POOL-A"}}
	dist := func(id string, a domain.Action) domain.Rule {
		return domain.Rule{ID: domain.RuleID(id), Phase: domain.PhaseDistribute, Scope: poolA, Action: a}
	}

	okEqual := dist("d-equal", domain.Action{Kind: domain.ActDistributePool, DistMode: domain.DistEqual})
	okWeight := dist("d-weight", domain.Action{Kind: domain.ActDistributePool, DistMode: domain.DistWeight,
		WeightTable: map[domain.RoleID]int64{domain.RoleOwner: 100}})
	for name, good := range map[string]domain.Rule{"均分": okEqual, "按权重": okWeight} {
		if _, err := Compile(domain.RuleSet{Version: "ok", Rules: []domain.Rule{good}}, cat); err != nil {
			t.Fatalf("%s 应合法: %v", name, err)
		}
	}

	badValue := dist("d-bad-value", domain.Action{Kind: domain.ActDistributePool, DistMode: domain.DistMode("fifo")})
	badPhase := rule("d-bad-phase", domain.PhaseFund, domain.Scope{Lines: []domain.ProductLineID{"A"}}, 100)
	badPhase.Action.DistMode = domain.DistEqual
	noRole := dist("d-all-norole", domain.Action{Kind: domain.ActDistributePool, DistMode: domain.DistAll})
	noTable := dist("d-weight-notable", domain.Action{Kind: domain.ActDistributePool, DistMode: domain.DistWeight})
	noMode := dist("d-nomode", domain.Action{Kind: domain.ActDistributePool})
	badRetain := dist("d-retain", domain.Action{Kind: domain.ActDistributePool, DistMode: domain.DistEqual, RetainBPS: 12000})

	for name, bad := range map[string]domain.Rule{
		"分发方式未定义":         badValue,
		"分发方式配在领取阶段":      badPhase,
		"all 未指定 AllocTo": noRole,
		"weight 未配权重表":    noTable,
		"分发规则未配 DistMode": noMode,
		"RetainBPS 越界":    badRetain,
	} {
		if _, err := Compile(domain.RuleSet{Version: "bad", Rules: []domain.Rule{bad}}, cat); err == nil {
			t.Fatalf("%s 应被拒绝发布", name)
		}
	}
}

// M5：领取规则指向的池没有分发规则，是「领了发不出去」，发布期必须报缺口。
func TestPoolWithoutDistributeRuleIsGap(t *testing.T) {
	fund := domain.Rule{ID: "f1", Phase: domain.PhaseFund,
		Scope:  domain.Scope{Lines: []domain.ProductLineID{"A"}},
		Action: domain.Action{Kind: domain.ActFundPool, Target: "POOL-A", BaseBPS: 500}}
	ix, err := Compile(domain.RuleSet{Version: "v", Rules: []domain.Rule{fund}}, cat)
	if err != nil {
		t.Fatalf("缺分发规则不该拒发: %v", err)
	}
	var got bool
	for _, g := range ix.Gaps {
		if g == "[pool] 池 POOL-A 无分发规则，领取额全额结转" {
			got = true
		}
	}
	if !got {
		t.Fatalf("应报告池分发缺口, got %v", ix.Gaps)
	}
}

// M5：入池规则按目标池分域 —— 不同池两条同时命中是各领各的，同池两条重叠才是冲突。
func TestFundRulesArbitratePerPool(t *testing.T) {
	lineA := domain.Scope{Lines: []domain.ProductLineID{"A"}}
	fund := func(id string, pool domain.PoolID, sc domain.Scope) domain.Rule {
		return domain.Rule{ID: domain.RuleID(id), Phase: domain.PhaseFund, Scope: sc,
			Action: domain.Action{Kind: domain.ActFundPool, Target: string(pool), BaseBPS: 100}}
	}
	dist := func(id string, pool domain.PoolID) domain.Rule {
		return domain.Rule{ID: domain.RuleID(id), Phase: domain.PhaseDistribute,
			Scope:  domain.Scope{Pools: []domain.PoolID{pool}},
			Action: domain.Action{Kind: domain.ActDistributePool, Target: string(pool), DistMode: domain.DistEqual}}
	}

	// 同一产品线、同一特异性，但目标池不同：合法，运行时各领各的。
	two := []domain.Rule{
		fund("f-line", "POOL-LINE", lineA),
		fund("f-owner", "POOL-OWNER", lineA),
		dist("d-line", "POOL-LINE"),
		dist("d-owner", "POOL-OWNER"),
	}
	ix, err := Compile(domain.RuleSet{Version: "two", Rules: two}, cat)
	if err != nil {
		t.Fatalf("不同目标池的两条入池规则不该冲突: %v", err)
	}
	if got := ix.All(domain.PhaseFund, Ctx{Line: "A", Role: domain.RoleOwner}, "2026-10"); len(got) != 2 {
		t.Fatalf("应同时命中两个池, got %d 条", len(got))
	}

	// 目标池相同、作用域重叠、特异性相同：无法裁决，必须拒发。
	same := []domain.Rule{fund("f1", "POOL-LINE", lineA), fund("f2", "POOL-LINE", lineA)}
	if _, err := Compile(domain.RuleSet{Version: "same", Rules: same}, cat); err == nil {
		t.Fatal("同一目标池的两条重叠入池规则应被拒绝发布")
	}
}
