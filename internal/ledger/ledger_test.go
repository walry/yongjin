package ledger

import (
	"strings"
	"testing"

	"github.com/walry/yongjin/internal/calc"
	"github.com/walry/yongjin/internal/demo"
)

// 本文件只钉台账的状态机（Post/Lock/Recalculate 的准入），金额差额的三种成因见
// recalc_test.go。分开的理由：状态机错了是「账面结构错」，差额错了是「数字错」，
// 排查入口完全不同 —— 前者看分录集合，后者看分项列。

// TestLockedPeriodRequiresAdjustment：锁定后 Post 必须被拒，只能走 Recalculate。
func TestLockedPeriodRequiresAdjustment(t *testing.T) {
	facts := demo.Facts()
	v1 := mustRun(t, facts, demo.RulesV1())
	v2 := mustRun(t, facts, demo.RulesV2())

	book := New()
	if _, err := book.Post(v1); err != nil {
		t.Fatal(err)
	}
	book.Lock(v1.Period)

	if _, err := book.Post(v2); err == nil {
		t.Fatal("已锁定期间不允许直接重写")
	}

	adjs := mustRecalc(t, book, v1, v2)
	if book.PeriodTotal(v1.Period) != v2.Total() {
		t.Fatalf("调整后账面 %s 应等于 v2 合计 %s", book.PeriodTotal(v1.Period), v2.Total())
	}
	if v2.Total() <= v1.Total() {
		t.Fatalf("v2 提高费率后合计应增加: %s -> %s", v1.Total(), v2.Total())
	}
	for _, e := range adjs {
		if !strings.Contains(e.Cause, "rules:v1->v2") {
			t.Fatalf("费率变更的原因应写明版本迁移，got %q", e.Cause)
		}
	}
}

// TestRecalculateRequiresLockedPeriod：未锁定期间走 Recalculate 必须报错。
//
// 放行的后果是静默的：账面会从「只有一批 adjustment、没有 settlement」开始，
// PeriodTotal 从此等于差额而非合计，且之后再怎么调都补不回来。
func TestRecalculateRequiresLockedPeriod(t *testing.T) {
	facts := demo.Facts()
	v1 := mustRun(t, facts, demo.RulesV1())
	v2 := mustRun(t, facts, demo.RulesV2())

	// 未入账（open）：没有什么可重算。
	if _, err := New().Recalculate(v1, v2); err == nil {
		t.Fatal("未入账期间不应允许重算差额")
	}

	// 已入账未锁定（calculated）：直接 Post 就是重算。
	book := New()
	if _, err := book.Post(v1); err != nil {
		t.Fatal(err)
	}
	if _, err := book.Recalculate(v1, v2); err == nil {
		t.Fatal("已结算未锁定期间应提示「直接 Post 即可」，而不是写调整单")
	}
	if got := book.PeriodTotal(v1.Period); got != v1.Total() {
		t.Fatalf("被拒的重算不得入账：账面 %s 仍应为 v1 合计 %s", got, v1.Total())
	}
}

// TestPostOverwritesUnlockedDraft：Calculated 期间再次 Post 是覆盖草稿，不是追加。
//
// 直接追加会让同一期间出现两套 settlement，账面凭空翻倍，且两套指向不同 CalcRunID，
// 事后无从判断哪一套是最终稿。
func TestPostOverwritesUnlockedDraft(t *testing.T) {
	facts := demo.Facts()
	v1 := mustRun(t, facts, demo.RulesV1())
	v2 := mustRun(t, facts, demo.RulesV2())

	book := New()
	if _, err := book.Post(v1); err != nil {
		t.Fatal(err)
	}
	if _, err := book.Post(v2); err != nil {
		t.Fatal(err)
	}

	if got := book.PeriodTotal(v1.Period); got != v2.Total() {
		t.Fatalf("二次入账后账面 %s 应等于 v2 合计 %s（不能被两套分录累加）", got, v2.Total())
	}
	if n, want := len(book.Entries()), len(v2.Persons); n != want {
		t.Fatalf("分录数 %d 应等于人数 %d：旧草稿必须被丢弃", n, want)
	}
	for _, e := range book.Entries() {
		if e.CalcRunID != v2.CalcRunID {
			t.Fatalf("覆盖后不应残留指向旧计算单的分录: %+v", e)
		}
	}
}

// TestRecalculateRejectsPeriodMismatch：跨期重算没有意义，必须拒。
func TestRecalculateRejectsPeriodMismatch(t *testing.T) {
	facts := demo.Facts()
	v1 := mustRun(t, facts, demo.RulesV1())

	other := facts
	other.Period = "2099-12"
	v2 := mustRun(t, other, demo.RulesV1())

	book := lockedBook(t, v1)
	if _, err := book.Recalculate(v1, v2); err == nil {
		t.Fatal("期间不一致的重算必须被拒")
	}
	if got := book.PeriodTotal(v1.Period); got != v1.Total() {
		t.Fatalf("被拒的重算不得入账：账面 %s 仍应为 %s", got, v1.Total())
	}
}

// 编译期确认 Result 仍是 Recalculate 的入参类型（防止签名被悄悄改成接口）。
var _ func(*Book, calc.Result, calc.Result) ([]Entry, error) = (*Book).Recalculate
