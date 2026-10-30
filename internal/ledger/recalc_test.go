package ledger

import (
	"strings"
	"testing"

	"github.com/walry/yongjin/internal/calc"
	"github.com/walry/yongjin/internal/demo"
	"github.com/walry/yongjin/internal/domain"
	"github.com/walry/yongjin/internal/money"
)

func mustRun(t *testing.T, facts domain.Facts, rs domain.RuleSet) calc.Result {
	t.Helper()
	res, err := calc.Run(facts, rs, calc.Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return res
}

// lockedBook 入账并锁定：重算差额只在这种情况下存在。
func lockedBook(t *testing.T, res calc.Result) *Book {
	t.Helper()
	b := New()
	if _, err := b.Post(res); err != nil {
		t.Fatalf("post: %v", err)
	}
	b.Lock(res.Period)
	return b
}

func mustRecalc(t *testing.T, b *Book, oldRes, newRes calc.Result) []Entry {
	t.Helper()
	adjs, err := b.Recalculate(oldRes, newRes)
	if err != nil {
		t.Fatalf("recalculate: %v", err)
	}
	if len(adjs) == 0 {
		t.Fatal("重算应当产生至少一条调整分录")
	}
	for _, e := range adjs {
		if e.Kind != "adjustment" {
			t.Fatalf("重算只能写 adjustment，got %s", e.Kind)
		}
		if e.AdjustOf != oldRes.CalcRunID || e.CalcRunID != newRes.CalcRunID {
			t.Fatalf("调整单必须与新老计算单双向关联: %+v", e)
		}
		if e.Cause == "" {
			t.Fatalf("调整单必须记下重算原因: %+v", e)
		}
	}
	return adjs
}

// comp 是人对小计的四个分项。台账差额必须能按它们拆开：
// 只对得上「合计」而对不上「哪一列动了」，事后仍然无法归因。
type comp struct {
	direct, split, pool, manager money.Money
}

func components(res calc.Result) map[domain.PersonID]comp {
	out := make(map[domain.PersonID]comp, len(res.Persons))
	for _, p := range res.Persons {
		out[p.Person] = comp{direct: p.Direct, split: p.Split, pool: p.Pool, manager: p.Manager}
	}
	return out
}

func compDelta(oldC, newC comp) comp {
	return comp{
		direct:  newC.direct - oldC.direct,
		split:   newC.split - oldC.split,
		pool:    newC.pool - oldC.pool,
		manager: newC.manager - oldC.manager,
	}
}

func (c comp) sum() money.Money { return c.direct + c.split + c.pool + c.manager }

// assertBookTiesTo 核对台账与新结果逐人对齐。这是重算用例的共同底线：
// 只要有一处对不上，账面就是错的，无论差额本身算得多漂亮。
func assertBookTiesTo(t *testing.T, b *Book, res calc.Result) {
	t.Helper()
	if got := b.PeriodTotal(res.Period); got != res.Total() {
		t.Fatalf("账面合计 %s 应等于重算合计 %s", got, res.Total())
	}
	for _, p := range res.Persons {
		if got := b.Balance(res.Period, p.Person); got != p.Total {
			t.Fatalf("%s 账面 %s 应等于重算合计 %s", p.Person, got, p.Total)
		}
	}
}

// 费率变更（规则维度）：只有 ruleSetVersion 变，factsDigest 不变。
// 差额必须落在直接佣金与管理奖上，池与分账一分不动。
func TestRecalculateRateChangeOnlyMovesDirectAndManager(t *testing.T) {
	facts := demo.Facts()
	old := mustRun(t, facts, demo.RulesV1())
	new := mustRun(t, facts, demo.RulesV2())

	if old.FactsDigest != new.FactsDigest {
		t.Fatal("费率变更不应改动 factsDigest：预算/目标/订单都没变")
	}
	if old.CalcRunID == new.CalcRunID {
		t.Fatal("规则版本变了，CalcRunID 必须变（否则幂等会吞掉重算）")
	}

	book := lockedBook(t, old)
	adjs := mustRecalc(t, book, old, new)
	for _, e := range adjs {
		if !strings.Contains(e.Cause, "rules:v1->v2") {
			t.Fatalf("原因应指向规则版本变化，got %q", e.Cause)
		}
		if strings.Contains(e.Cause, "facts:") {
			t.Fatalf("事实未变，原因里不应出现 facts:，got %q", e.Cause)
		}
	}

	oldC, newC := components(old), components(new)
	var sawDirect, sawManager bool
	for p, oc := range oldC {
		d := compDelta(oc, newC[p])
		if d.pool != 0 {
			t.Fatalf("%s 的池金额不应受费率变更影响: %s", p, d.pool)
		}
		if d.split != 0 {
			t.Fatalf("%s 的分账净额不应受费率变更影响: %s", p, d.split)
		}
		if d.direct != 0 {
			sawDirect = true
		}
		if d.manager != 0 {
			sawManager = true
		}
		// 差额必须等于分项差额之和：漏掉任何一列都会在这里暴露。
		var ledgerDelta money.Money
		for _, e := range adjs {
			if e.Person == p {
				ledgerDelta = e.Amount
			}
		}
		if ledgerDelta != d.sum() {
			t.Fatalf("%s 的台账差额 %s != 分项差额之和 %s", p, ledgerDelta, d.sum())
		}
	}
	if !sawDirect || !sawManager {
		t.Fatalf("费率上调应同时带动直接佣金与管理奖（连带效应必须由系统算）：direct=%v manager=%v",
			sawDirect, sawManager)
	}
	assertBookTiesTo(t, book, new)
}

// 预算变更（事实维度）：ruleSetVersion 不变，factsDigest 变。
// 差额必须只落在池那一列 —— 这正是「预算属于事实」的价值：
// 若把预算写进规则，它就会和费率变更混成同一种原因，无法归因（设计 §2.3）。
func TestRecalculateBudgetChangeOnlyMovesPoolColumn(t *testing.T) {
	base := demo.Facts()
	old := mustRun(t, base, demo.RulesV1())

	shrunk := demo.Facts()
	shrunk.Pools[0].Budget = money.FromYuan(30000, 0) // POOL-A：60000 -> 30000
	new := mustRun(t, shrunk, demo.RulesV1())

	if old.RuleSetVersion != new.RuleSetVersion {
		t.Fatal("预算变更不该改规则版本")
	}
	if old.FactsDigest == new.FactsDigest {
		t.Fatal("预算属于事实，factsDigest 必须变")
	}

	book := lockedBook(t, old)
	adjs := mustRecalc(t, book, old, new)
	for _, e := range adjs {
		if !strings.Contains(e.Cause, "facts:") || strings.Contains(e.Cause, "rules:") {
			t.Fatalf("原因应只指向事实快照变化，got %q", e.Cause)
		}
	}

	oldC, newC := components(old), components(new)
	var sawPoolCut bool
	for p, oc := range oldC {
		d := compDelta(oc, newC[p])
		if d.direct != 0 || d.split != 0 || d.manager != 0 {
			t.Fatalf("%s 的非池分项不应受预算影响：直接 %s 分账 %s 管理 %s", p, d.direct, d.split, d.manager)
		}
		if d.pool < 0 {
			sawPoolCut = true
		}
	}
	if !sawPoolCut {
		t.Fatal("预算减半后必须有人少拿池分成")
	}
	if new.Total() >= old.Total() {
		t.Fatalf("预算减半后合计应下降：%s -> %s", old.Total(), new.Total())
	}
	assertBookTiesTo(t, book, new)
}

// 目标变更（事实维度）：改 P1 的合同目标额，达成率跨档，档位倍数变化，
// 直接佣金与管理奖跟着变；池领取口径是收入，不受合同目标影响。
func TestRecalculateTargetChangeMovesBandAndManagerChain(t *testing.T) {
	rs := demo.RulesV1Bands()
	base := demo.Facts()
	old := mustRun(t, base, rs)

	raised := demo.Facts()
	// P1 合同目标 1,500,000 -> 1,200,000：达成率 118.66% (HIGH 1.2×) -> 148.33% (TOP 1.5×)
	for i := range raised.Targets {
		if raised.Targets[i].Person == "P1" {
			raised.Targets[i].Contract = money.FromYuan(1200000, 0)
		}
	}
	new := mustRun(t, raised, rs)

	if old.FactsDigest == new.FactsDigest {
		t.Fatal("目标额属于事实，factsDigest 必须变")
	}

	// 前提：P1 的合同档位确实跨了一档，否则本用例没有区分度。
	if oldBand := bandOf(t, old, "P1", domain.KPIContract); oldBand != "HIGH" {
		t.Fatalf("P1 合同原档位应为 HIGH，got %q", oldBand)
	}
	if newBand := bandOf(t, new, "P1", domain.KPIContract); newBand != "TOP" {
		t.Fatalf("P1 合同达成率提高后应命中 TOP 档，got %q", newBand)
	}

	book := lockedBook(t, old)
	adjs := mustRecalc(t, book, old, new)
	for _, e := range adjs {
		if !strings.Contains(e.Cause, "facts:") || strings.Contains(e.Cause, "rules:") {
			t.Fatalf("原因应只指向事实快照变化，got %q", e.Cause)
		}
	}

	oldC, newC := components(old), components(new)
	d := compDelta(oldC["P1"], newC["P1"])
	if d.direct <= 0 {
		t.Fatalf("P1 档位从 HIGH 升到 TOP，直接佣金应增加，got %s", d.direct)
	}
	if d.pool != 0 {
		t.Fatalf("池领取按收入口径，不应受合同目标影响，got %s", d.pool)
	}
	// 管理奖由 P1 的直接佣金上溯：连带上调，不得靠人补。
	var mgrDelta money.Money
	for p, oc := range oldC {
		if p == "P1" {
			continue
		}
		mgrDelta += compDelta(oc, newC[p]).manager
	}
	if mgrDelta <= 0 {
		t.Fatalf("红利/管理奖链应随 P1 佣金同步上调，got %s", mgrDelta)
	}
	assertBookTiesTo(t, book, new)
}

// bandOf 从直接佣金的留痕里取档位，避免用例自己重算一遍达成率。
func bandOf(t *testing.T, res calc.Result, person domain.PersonID, kpi domain.KPI) string {
	t.Helper()
	for _, d := range res.Directs {
		if d.Person == person && d.KPI == kpi && d.BandID != "" {
			return string(d.BandID)
		}
	}
	return ""
}
