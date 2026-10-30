package calc

import (
	"testing"
	"time"

	"github.com/walry/yongjin/internal/demo"
	"github.com/walry/yongjin/internal/domain"
	"github.com/walry/yongjin/internal/money"
)

// poolFacts 造两笔订单：O-1（10-05 成单，收入 100,000）与 O-2（10-20 成单，收入 60,000）。
// 每单的获利角色都是负责人 P1 + 协作人 P2(pre) / P3(chan)。
// 两单成单时间不同，正是 FIFO 的必要条件：预算不够时先成的先领满。
func poolFacts(budget money.Money) domain.Facts {
	return domain.Facts{
		Period:   "2026-10",
		Products: map[string]domain.Product{"SKU-A1": {ID: "SKU-A1", Line: "A", Series: "A1"}},
		Persons: map[domain.PersonID]domain.Person{
			"P1": {ID: "P1", Name: "张三", Dept: "SALES"},
			"P2": {ID: "P2", Name: "李四", Dept: "SALES"},
			"P3": {ID: "P3", Name: "王五", Dept: "SALES"},
		},
		Orders: []domain.Order{
			{
				ID: "O-1", Owner: "P1", Territory: "EAST", Class: "NEW",
				ClosedAt: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC),
				Participants: []domain.Participant{
					{Person: "P2", Role: domain.RolePre, Weight: 100},
					{Person: "P3", Role: domain.RoleChan, Weight: 100},
				},
				Lines: []domain.OrderLine{{ID: "L1", Product: "SKU-A1", Contract: money.FromYuan(120000, 0), Revenue: money.FromYuan(100000, 0)}},
			},
			{
				ID: "O-2", Owner: "P1", Territory: "EAST", Class: "NEW",
				ClosedAt: time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC),
				Participants: []domain.Participant{
					{Person: "P2", Role: domain.RolePre, Weight: 100},
					{Person: "P3", Role: domain.RoleChan, Weight: 100},
				},
				Lines: []domain.OrderLine{{ID: "L1", Product: "SKU-A1", Contract: money.FromYuan(75000, 0), Revenue: money.FromYuan(60000, 0)}},
			},
		},
		Pools: []domain.PoolDef{{ID: "POOL-A", Line: "A", Period: "2026-10", Budget: budget}},
	}
}

// poolRules 只配池的两层：领取 10%、分发按角色权重（负责人 60 / 售前 40）。
// 不配直接佣金，让池的收支可以单独立起来看。
func poolRules(dist domain.Action) domain.RuleSet {
	if dist.Kind == "" {
		dist = domain.Action{Kind: domain.ActDistributePool, Target: "POOL-A", DistMode: domain.DistWeight,
			WeightTable: map[domain.RoleID]int64{domain.RoleOwner: 60, domain.RolePre: 40}}
	}
	return domain.RuleSet{Version: "p", Rules: []domain.Rule{
		{
			ID: "R-FUND-A", Phase: domain.PhaseFund, Scope: domain.Scope{Lines: []domain.ProductLineID{"A"}},
			Action: domain.Action{Kind: domain.ActFundPool, Target: "POOL-A", Method: domain.MethodPercent, BaseBPS: 1000},
		},
		{
			ID: "R-DIST-A", Phase: domain.PhaseDistribute, Scope: domain.Scope{Pools: []domain.PoolID{"POOL-A"}},
			Action: dist,
		},
	}}
}

func mustRun(t *testing.T, facts domain.Facts, rs domain.RuleSet) Result {
	t.Helper()
	res, err := Run(facts, rs, Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res
}

func poolOf(t *testing.T, res Result, pid domain.PoolID) PoolSettlement {
	t.Helper()
	for _, p := range res.Pools {
		if p.Pool == pid {
			return p
		}
	}
	t.Fatalf("未找到池 %s 的对账", pid)
	return PoolSettlement{}
}

// M5 验收：领取只到预算为止，后面的单领 0，且这件事在明细里看得见。
// 领取口径是收入口径（§5.2）：100,000 × 10% = 10,000，60,000 × 10% = 6,000。
func TestPoolClaimStopsAtBudget(t *testing.T) {
	facts := poolFacts(money.FromYuan(12000, 0))
	res := mustRun(t, facts, poolRules(domain.Action{}))

	if len(res.Claims) != 2 {
		t.Fatalf("应有 2 条领取明细, got %d", len(res.Claims))
	}
	first, second := res.Claims[0], res.Claims[1]

	// 全序：先成单的先领满。
	if first.OrderID != "O-1" || second.OrderID != "O-2" {
		t.Fatalf("领取顺序应为 O-1 → O-2, got %s → %s", first.OrderID, second.OrderID)
	}
	if first.Claimed != money.FromYuan(10000, 0) || first.Exhausted {
		t.Fatalf("O-1 应领满 10000.00 且未耗尽, got %s exhausted=%v", first.Claimed, first.Exhausted)
	}
	if first.Remaining != money.FromYuan(12000, 0) {
		t.Fatalf("O-1 领取前剩余应为 12000.00, got %s", first.Remaining)
	}
	// 第二单只剩 2,000：领不满才是预算语义的正确结果，不能按 6,000 全额发。
	if second.Claimed != money.FromYuan(2000, 0) || !second.Exhausted {
		t.Fatalf("O-2 应领 2000.00 并标记耗尽, got %s exhausted=%v", second.Claimed, second.Exhausted)
	}
	if second.Remaining != money.FromYuan(2000, 0) {
		t.Fatalf("O-2 领取前剩余应为 2000.00, got %s", second.Remaining)
	}

	// I2：预算 = 分出 + 结转，一分不多一分不少。
	p := poolOf(t, res, "POOL-A")
	if p.Budget != money.FromYuan(12000, 0) || p.Claimed != money.FromYuan(12000, 0) {
		t.Fatalf("池对账 budget=%s claimed=%s", p.Budget, p.Claimed)
	}
	if p.Distributed+p.CarryOver != p.Budget {
		t.Fatalf("I2 违反: %s + %s != %s", p.Distributed, p.CarryOver, p.Budget)
	}
	// 没领满必须告警，否则「后面几单池没有钱」会被当成 bug 排查。
	if !hasWarning(res, "预算不足") {
		t.Fatal("领取额被封顶必须告警")
	}
}

// M5 验收：发放明细之和 == 领取额，且按角色权重拆（负责人 60 / 售前 40）。
func TestPoolDistributionSumsToClaim(t *testing.T) {
	res := mustRun(t, poolFacts(money.FromYuan(100000, 0)), poolRules(domain.Action{}))

	var claimed money.Money
	for _, c := range res.Claims {
		claimed += c.Claimed
	}
	var paid money.Money
	byPerson := map[domain.PersonID]money.Money{}
	for _, d := range res.Distributions {
		paid += d.Amount
		byPerson[d.Person] += d.Amount
		if d.OrderID == "" || d.LineID == "" {
			t.Fatalf("分发必须挂在领取明细下: %+v", d)
		}
	}
	p := poolOf(t, res, "POOL-A")
	if paid != claimed || p.Distributed != claimed {
		t.Fatalf("分发额 %s 应等于领取额 %s（池对账 %s）", paid, claimed, p.Distributed)
	}

	// O-1 领 10,000：60/40 → 6,000 / 4,000；O-2 领 6,000 → 3,600 / 2,400。
	if got := byPerson["P1"]; got != money.FromYuan(9600, 0) {
		t.Fatalf("P1 池分成应为 9600.00, got %s", got)
	}
	if got := byPerson["P2"]; got != money.FromYuan(6400, 0) {
		t.Fatalf("P2 池分成应为 6400.00, got %s", got)
	}
	// 权重表里没有 channel，不参与，不做「按 1 兜底」的静默错发。
	if got, ok := byPerson["P3"]; ok {
		t.Fatalf("P3 不应拿到池分成, got %s", got)
	}
}

// M5 验收：三种分发方式各自成立，均分用最大余数法保证恰好分完。
func TestPoolDistModes(t *testing.T) {
	a := money.FromYuan(10000, 0) // O-1 的领取额

	t.Run("equal", func(t *testing.T) {
		res := mustRun(t, poolFacts(money.FromYuan(100000, 0)),
			poolRules(domain.Action{Kind: domain.ActDistributePool, Target: "POOL-A", DistMode: domain.DistEqual}))
		var got []money.Money
		for _, d := range res.Distributions {
			if d.OrderID == "O-1" {
				got = append(got, d.Amount)
			}
		}
		// 三个人均分 10,000：余数给排在最前的人，合计必须正好 10,000。
		want := []money.Money{money.FromYuan(3333, 34), money.FromYuan(3333, 33), money.FromYuan(3333, 33)}
		if len(got) != 3 {
			t.Fatalf("均分应产生 3 条分录, got %d", len(got))
		}
		var sum money.Money
		for i := range got {
			sum += got[i]
			if got[i] != want[i] {
				t.Fatalf("第 %d 份应为 %s, got %s", i, want[i], got[i])
			}
		}
		if sum != a {
			t.Fatalf("均分合计 %s != 领取额 %s", sum, a)
		}
	})

	t.Run("all", func(t *testing.T) {
		res := mustRun(t, poolFacts(money.FromYuan(100000, 0)),
			poolRules(domain.Action{Kind: domain.ActDistributePool, Target: "POOL-A",
				DistMode: domain.DistAll, AllocTo: domain.RolePre}))
		for _, d := range res.Distributions {
			if d.OrderID == "O-1" && (d.Person != "P2" || d.Amount != a) {
				t.Fatalf("all 模式应全给指定角色 pre_sales, got %s %s", d.Person, d.Amount)
			}
		}
	})
}

// M5 验收：没有分发规则时，领取额全额结转，而不是凭空留在某个人账上。
func TestPoolWithoutDistributeRuleCarriesOver(t *testing.T) {
	rs := poolRules(domain.Action{})
	// 删掉分发规则，保留领取规则。
	rs.Rules = rs.Rules[:1]
	res := mustRun(t, poolFacts(money.FromYuan(100000, 0)), rs)

	p := poolOf(t, res, "POOL-A")
	if p.Claimed != money.FromYuan(16000, 0) {
		t.Fatalf("领取仍应发生 16000.00, got %s", p.Claimed)
	}
	if p.Distributed != 0 || p.CarryOver != p.Budget {
		t.Fatalf("无分发规则应全额结转, got distributed=%s carry=%s", p.Distributed, p.CarryOver)
	}
	if len(res.Distributions) != 0 {
		t.Fatalf("无分发规则不应产生分录, got %d", len(res.Distributions))
	}
	if !hasWarning(res, "无分发规则") {
		t.Fatal("缺分发规则必须告警")
	}
}

// M5 验收：领取顺序由 (ClosedAt, OrderID, LineID) 决定，与事实录入顺序无关（I9）。
func TestPoolFIFOIgnoresFactOrder(t *testing.T) {
	facts := poolFacts(money.FromYuan(12000, 0))
	shuffled := facts
	shuffled.Orders = []domain.Order{facts.Orders[1], facts.Orders[0]}

	a := mustRun(t, facts, poolRules(domain.Action{}))
	b := mustRun(t, shuffled, poolRules(domain.Action{}))

	if len(a.Claims) != len(b.Claims) {
		t.Fatalf("领取条数不一致: %d vs %d", len(a.Claims), len(b.Claims))
	}
	for i := range a.Claims {
		x, y := a.Claims[i], b.Claims[i]
		if x.OrderID != y.OrderID || x.Claimed != y.Claimed || x.Remaining != y.Remaining {
			t.Fatalf("第 %d 条领取不一致: %+v vs %+v", i, x, y)
		}
	}
	if a.CalcRunID != b.CalcRunID {
		t.Fatalf("同事实必须同 CalcRunID: %s vs %s", a.CalcRunID, b.CalcRunID)
	}
}

// M5 验收：池分成不参与管理奖上溯（§8.4）——预算放大十倍，管理奖总额分毫不动。
func TestPoolDoesNotFeedManagerBonus(t *testing.T) {
	// 小预算会把领取额卡住，大预算放得下全部领取额：池分成因此必然不同。
	small := demo.Facts()
	small.Pools = []domain.PoolDef{
		{ID: "POOL-A", Line: "A", Period: "2026-10", Budget: money.FromYuan(20000, 0)},
		{ID: "POOL-B", Line: "B", Period: "2026-10", Budget: money.FromYuan(5000, 0)},
	}
	big := demo.Facts()
	big.Pools = []domain.PoolDef{
		{ID: "POOL-A", Line: "A", Period: "2026-10", Budget: money.FromYuan(600000, 0)},
		{ID: "POOL-B", Line: "B", Period: "2026-10", Budget: money.FromYuan(200000, 0)},
	}
	rs := demo.RulesV1()

	a := mustRun(t, small, rs)
	b := mustRun(t, big, rs)

	if sumManager(a) != sumManager(b) {
		t.Fatalf("池预算变化不该改变管理奖: %s vs %s", sumManager(a), sumManager(b))
	}
	if sumManager(a) == 0 {
		t.Fatal("用例应真的产生管理奖，否则这条断言没有意义")
	}
	// 每条管理奖基数必须能在分账前明细里找到同额记录，且不来自池。
	for _, m := range a.ManagerBonuses {
		var hit bool
		for _, d := range a.Directs {
			if d.Person == m.Source && d.KPI == m.KPI && d.Amount == m.Base {
				hit = true
				break
			}
		}
		if !hit {
			t.Fatalf("管理奖基数 %s 应来自分账前直接佣金: %+v", m.Base, m)
		}
	}
	// 池分成变了，但管理奖没变——这条差额必须完全由 Pool 列承担。
	if a.Total()-sumManager(a) == b.Total()-sumManager(b) {
		t.Fatal("预算放大后池分成应变化，否则用例没测到池")
	}
}

// M5 验收：领取留痕可逐分复算（I8）——封顶靠 OverlayDelta 表达，不破坏倍率链。
func TestPoolClaimTraceReplays(t *testing.T) {
	res := mustRun(t, poolFacts(money.FromYuan(12000, 0)), poolRules(domain.Action{}))
	var seen int
	for _, tr := range res.Trace {
		if tr.Phase != string(domain.PhaseFund) {
			continue
		}
		seen++
		if got := tr.Replay(); got != tr.Amount {
			t.Fatalf("领取留痕复算不一致: replay=%s amount=%s (%s)", got, tr.Amount, tr.Note)
		}
	}
	if seen != 2 {
		t.Fatalf("应有 2 条领取留痕, got %d", seen)
	}
}

// M5 验收：同一行可以同时喂多个池（设计 §8.3 的产品线池 + 负责人池），各领各的。
// 若把 fund 阶段当成"整段一个裁决域"，两条规则会互相压过对方，其中一个是静默 0。
func TestPoolMultiplePoolsPerLine(t *testing.T) {
	facts := poolFacts(money.FromYuan(100000, 0))
	facts.Pools = append(facts.Pools,
		domain.PoolDef{ID: "POOL-OWNER", Line: "A", Period: "2026-10", Budget: money.FromYuan(5000, 0)})

	rs := poolRules(domain.Action{})
	rs.Rules = append(rs.Rules,
		domain.Rule{
			ID: "R-FUND-OWNER", Phase: domain.PhaseFund,
			Scope:  domain.Scope{Lines: []domain.ProductLineID{"A"}, Roles: []domain.RoleID{domain.RoleOwner}},
			Action: domain.Action{Kind: domain.ActFundPool, Target: "POOL-OWNER", Method: domain.MethodPercent, BaseBPS: 200},
		},
		domain.Rule{
			ID: "R-DIST-OWNER", Phase: domain.PhaseDistribute,
			Scope: domain.Scope{Pools: []domain.PoolID{"POOL-OWNER"}},
			Action: domain.Action{Kind: domain.ActDistributePool, Target: "POOL-OWNER",
				DistMode: domain.DistAll, AllocTo: domain.RoleOwner},
		},
	)

	res := mustRun(t, facts, rs)

	// 产品线池仍按 10% 领（10000 + 6000），负责人池按 2% 领（2000 + 1200），互不干扰。
	if got := poolOf(t, res, "POOL-A").Claimed; got != money.FromYuan(16000, 0) {
		t.Fatalf("POOL-A 领取应为 16000.00, got %s", got)
	}
	owner := poolOf(t, res, "POOL-OWNER")
	if owner.Claimed != money.FromYuan(3200, 0) {
		t.Fatalf("POOL-OWNER 领取应为 3200.00, got %s", owner.Claimed)
	}
	// 全给负责人：钱不留给协作人。
	if owner.Distributed != owner.Claimed {
		t.Fatalf("POOL-OWNER 应全部分出, got %s / %s", owner.Distributed, owner.Claimed)
	}
	var toOwner money.Money
	for _, d := range res.Distributions {
		if d.Pool == "POOL-OWNER" {
			if d.Person != "P1" {
				t.Fatalf("POOL-OWNER 只应分给负责人, got %s", d.Person)
			}
			toOwner += d.Amount
		}
	}
	if toOwner != owner.Claimed {
		t.Fatalf("发给负责人的合计 %s 应等于领取额 %s", toOwner, owner.Claimed)
	}
}
