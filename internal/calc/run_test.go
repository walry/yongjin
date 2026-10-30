package calc

import (
	"testing"

	"github.com/walry/yongjin/internal/demo"
	"github.com/walry/yongjin/internal/domain"
	"github.com/walry/yongjin/internal/money"
)

func TestRunEndToEnd(t *testing.T) {
	facts := demo.Facts()
	res, err := Run(facts, demo.RulesV1(), Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// 池守恒（I2）：预算 = 分出 + 结转，一分不差。
	var budget, distributed, carry money.Money
	for _, p := range res.Pools {
		budget += p.Budget
		distributed += p.Distributed
		carry += p.CarryOver
		if p.Distributed+p.CarryOver != p.Budget {
			t.Fatalf("池 %s 失衡", p.Pool)
		}
	}
	if budget != distributed+carry {
		t.Fatalf("总池失衡: %s != %s + %s", budget, distributed, carry)
	}

	// A 池按**收入口径**领取（设计 §5.2）：
	// A1 4% × 720000 + A2 3% × 520000 = 28800 + 15600 = 44400，未超 60000 预算。
	aClaim := money.FromYuan(44400, 0)
	var gotA money.Money
	for _, p := range res.Pools {
		if p.Pool == "POOL-A" {
			gotA = p.Claimed
		}
	}
	if gotA != aClaim {
		t.Fatalf("POOL-A 领取 = %s, want %s", gotA, aClaim)
	}

	// 系列特异性：R-FUND-A1 覆盖 R-FUND-A。
	var usedSeriesRule bool
	for _, tr := range res.Trace {
		if tr.RuleID == "R-FUND-A1" {
			usedSeriesRule = true
		}
	}
	if !usedSeriesRule {
		t.Fatal("A1 系列应命中更具体的入池规则")
	}

	// 合计守恒：人员合计 == 直接佣金 + 已分出池金额 + 管理奖金。
	// 注意用 distributed 而非 funded：留存/结转部分不发给个人。
	var paid money.Money
	for _, p := range res.Pools {
		paid += p.Distributed
	}
	if res.Total() != paid+sumDirect(res)+sumManager(res) {
		t.Fatalf("合计不守恒: total=%s paid=%s", res.Total(), paid)
	}

	// 特殊规则生效并留痕：A1 负责人 +30bps。
	var overlayHit bool
	for _, d := range res.Directs {
		if d.OrderID == "O-1001" && d.LineID == "L1" && d.Person == "P1" && d.KPI == domain.KPIContract {
			// 2.5% + 0.3% = 2.8% of 800000
			want := money.FromYuan(22400, 0)
			if d.Amount != want {
				t.Fatalf("负责人 A1 佣金 = %s, want %s", d.Amount, want)
			}
			if len(d.Overlays) != 1 || d.Overlays[0] != "OV-A1-VIP" {
				t.Fatalf("overlay 未留痕: %v", d.Overlays)
			}
			overlayHit = true
		}
	}
	if !overlayHit {
		t.Fatal("未找到 A1 负责人佣金明细")
	}

	// VIP 客户等级命中更具体规则 3.0%。
	for _, d := range res.Directs {
		if d.OrderID == "O-1002" && d.Person == "P1" && d.KPI == domain.KPIContract {
			if d.RuleID != "R-DIR-A-OWNER-VIP" || d.Amount != money.FromYuan(17400, 0) {
				t.Fatalf("VIP 规则未命中: rule=%s amount=%s", d.RuleID, d.Amount)
			}
		}
	}

	// 缺口必须有告警：产品线 B 未配置 channel 角色。
	if len(res.Warnings) == 0 {
		t.Fatal("应产生缺口告警")
	}
}

func TestRunIsDeterministicAndVersioned(t *testing.T) {
	facts := demo.Facts()
	a, err := Run(facts, demo.RulesV1(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Run(facts, demo.RulesV1(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if a.CalcRunID != b.CalcRunID {
		t.Fatalf("同输入必须同输出: %s vs %s", a.CalcRunID, b.CalcRunID)
	}

	c, err := Run(facts, demo.RulesV2(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c.CalcRunID == a.CalcRunID {
		t.Fatal("规则版本变化后 calcRunID 必须变化")
	}

	d, err := Run(facts, demo.RulesV1(), Options{EngineVersion: "1.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if d.CalcRunID == a.CalcRunID {
		t.Fatal("引擎版本变化后 calcRunID 必须变化")
	}
}

func TestCalcRunIDIgnoresMapOrder(t *testing.T) {
	f1 := demo.Facts()
	f2 := demo.Facts()
	// 重新构造 person map，插入顺序不影响摘要。
	f2.Persons = map[domain.PersonID]domain.Person{}
	for _, k := range []domain.PersonID{"P5", "P4", "P3", "P2", "P1"} {
		f2.Persons[k] = f1.Persons[k]
	}
	r1, err := Run(f1, demo.RulesV1(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Run(f2, demo.RulesV1(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r1.FactsDigest != r2.FactsDigest {
		t.Fatal("事实摘要必须与 map 插入顺序无关")
	}
}

// shuffledFacts 返回一份深拷贝，把事实里的所有切片逆序，模拟"录入顺序不同"。
// 深拷贝是必须的：浅拷贝后原地逆序会连带改掉原事实。
func shuffledFacts(f domain.Facts) domain.Facts {
	out := f
	out.Orders = make([]domain.Order, len(f.Orders))
	for i, o := range f.Orders {
		o.Lines = append([]domain.OrderLine(nil), o.Lines...)
		for a, b := 0, len(o.Lines)-1; a < b; a, b = a+1, b-1 {
			o.Lines[a], o.Lines[b] = o.Lines[b], o.Lines[a]
		}
		o.Participants = append([]domain.Participant(nil), o.Participants...)
		for a, b := 0, len(o.Participants)-1; a < b; a, b = a+1, b-1 {
			o.Participants[a], o.Participants[b] = o.Participants[b], o.Participants[a]
		}
		out.Orders[len(f.Orders)-1-i] = o
	}
	reverse := func(n int, swap func(i, j int)) {
		for a, b := 0, n-1; a < b; a, b = a+1, b-1 {
			swap(a, b)
		}
	}
	out.Relations = append([]domain.Relation(nil), f.Relations...)
	reverse(len(out.Relations), func(i, j int) { out.Relations[i], out.Relations[j] = out.Relations[j], out.Relations[i] })
	out.Targets = append([]domain.Target(nil), f.Targets...)
	reverse(len(out.Targets), func(i, j int) { out.Targets[i], out.Targets[j] = out.Targets[j], out.Targets[i] })
	out.Pools = append([]domain.PoolDef(nil), f.Pools...)
	reverse(len(out.Pools), func(i, j int) { out.Pools[i], out.Pools[j] = out.Pools[j], out.Pools[i] })
	return out
}

// M1：digest 不能依赖切片顺序，否则同事实换个录入次序就被判成"需要重算"，幂等失效。
func TestDigestIgnoresSliceOrder(t *testing.T) {
	f1, f2 := demo.Facts(), shuffledFacts(demo.Facts())
	d1, err := Digest(f1)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := Digest(f2)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatalf("摘要必须与切片顺序无关: %s vs %s", d1[:12], d2[:12])
	}

	r1, err := Run(f1, demo.RulesV1(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Run(f2, demo.RulesV1(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r1.CalcRunID != r2.CalcRunID {
		t.Fatalf("同事实同规则必须同 CalcRunID: %s vs %s", r1.CalcRunID, r2.CalcRunID)
	}
}

// M1：结果本身也要与输入顺序无关——金额逐条对齐，而不只是幂等键相同。
func TestRunIgnoresSliceOrder(t *testing.T) {
	r1, err := Run(demo.Facts(), demo.RulesV1(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	r2, err := Run(shuffledFacts(demo.Facts()), demo.RulesV1(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r1.Directs) != len(r2.Directs) {
		t.Fatalf("明细条数不一致: %d vs %d", len(r1.Directs), len(r2.Directs))
	}
	for i := range r1.Directs {
		a, b := r1.Directs[i], r2.Directs[i]
		if a.OrderID != b.OrderID || a.LineID != b.LineID || a.Person != b.Person || a.Amount != b.Amount {
			t.Fatalf("第 %d 条明细不一致: %+v vs %+v", i, a, b)
		}
	}
	if r1.Total() != r2.Total() {
		t.Fatalf("合计不一致: %s vs %s", r1.Total(), r2.Total())
	}
	for i := range r1.Persons {
		if r1.Persons[i] != r2.Persons[i] {
			t.Fatalf("人员结算不一致: %+v vs %+v", r1.Persons[i], r2.Persons[i])
		}
	}
}

// M1：合同口径与收入口径是两条独立基数，不许互相顶替。
func TestBaseOfDualKPI(t *testing.T) {
	ln := domain.OrderLine{
		Contract: money.FromYuan(600000, 0),
		Discount: money.FromYuan(20000, 0),
		Revenue:  money.FromYuan(520000, 0),
		Return:   money.FromYuan(1000, 0),
	}
	if got, want := ln.BaseOf(domain.KPIContract), money.FromYuan(579000, 0); got != want {
		t.Fatalf("合同口径基数 = %s, want %s", got, want)
	}
	if got, want := ln.BaseOf(domain.KPIRevenue), money.FromYuan(519000, 0); got != want {
		t.Fatalf("收入口径基数 = %s, want %s", got, want)
	}
	if ln.Base() != ln.BaseOf(domain.KPIContract) {
		t.Fatal("Base() 必须等价于 BaseOf(KPIContract)")
	}
}

func sumDirect(r Result) money.Money {
	var s money.Money
	for _, d := range r.Directs {
		s += d.Amount
	}
	return s
}

func sumManager(r Result) money.Money {
	var s money.Money
	for _, m := range r.ManagerBonuses {
		s += m.Amount
	}
	return s
}
