package calc

import (
	"testing"

	"github.com/walry/yongjin/internal/demo"
	"github.com/walry/yongjin/internal/domain"
	"github.com/walry/yongjin/internal/money"
)

// demoResult 用 demo.Shares()（RulesV1Full + 渠道成单 1:1 分账）：投影需要一份
// 同时含直接佣金、分账、池领取与池分发的完整结果，只用 RulesV1 测不到分账那一路。
func demoResult(t *testing.T) Result {
	t.Helper()
	return mustRun(t, demo.Facts(), demo.Shares())
}

func findPerson(t *testing.T, res Result, id domain.PersonID) PersonTotal {
	t.Helper()
	for _, p := range res.Persons {
		if p.Person == id {
			return p
		}
	}
	t.Fatalf("投影结果里没有 %s 的结算行", id)
	return PersonTotal{}
}

// 按人投影：明细只剩这个人的，但结算行原样保留（它本来就是这个人的期间合计）。
func TestProjectByPersonKeepsOwnRowsAndSplitBothSides(t *testing.T) {
	all := demoResult(t)
	one := all.ProjectByPerson("P1")

	if one.CalcRunID != all.CalcRunID {
		t.Fatal("投影不得改变 CalcRunID：它不是另一次计算")
	}
	if len(one.Persons) != 1 || one.Persons[0].Person != "P1" {
		t.Fatalf("按人投影应只剩 P1，got %d 行", len(one.Persons))
	}
	want := findPerson(t, all, "P1")
	if one.Persons[0] != want {
		t.Fatalf("P1 的结算行被改写了：%+v != %+v", one.Persons[0], want)
	}

	for _, d := range one.Directs {
		if d.Person != "P1" {
			t.Fatalf("投影里混入了非 P1 的直接佣金：%s/%s %s", d.OrderID, d.LineID, d.Person)
		}
	}
	for _, a := range one.Attainments {
		if a.Person != "P1" {
			t.Fatalf("投影里混入了非 P1 的达成率")
		}
	}
	for _, d := range one.Distributions {
		if d.Person != "P1" {
			t.Fatalf("投影里混入了非 P1 的池分发")
		}
	}
	for _, tr := range one.Trace {
		if tr.Person != "P1" {
			t.Fatalf("投影里混入了非 P1 的 Trace 行（%s）", tr.Person)
		}
	}

	// 分账两方都留：只留一侧会让净额对不上。逐向比对——全量里有几个方向，
	// 投影里就必须有几个方向（demo 里 P1 只收不出，所以不能假设两向都存在）。
	count := func(res Result) (out, in int) {
		for _, s := range res.Splits {
			if s.From != "P1" && s.To != "P1" {
				t.Fatalf("投影里混入了与 P1 无关的分账：%s → %s", s.From, s.To)
			}
			if s.From == "P1" {
				out++
			}
			if s.To == "P1" {
				in++
			}
		}
		return out, in
	}
	wantOut, wantIn := count(all)
	gotOut, gotIn := count(one)
	if wantOut+wantIn == 0 {
		t.Fatal("demo 里 P1 没参与任何分账，本用例失去意义")
	}
	if gotOut != wantOut || gotIn != wantIn {
		t.Fatalf("分账两个方向都要保留：转出 %d/%d 转入 %d/%d", gotOut, wantOut, gotIn, wantIn)
	}

	// 明细自洽：P1 的四个分项必须能由裁剪后的明细重新加出来。
	var direct, pool, manager, splitNet money.Money
	for _, d := range one.Directs {
		direct += d.Amount
	}
	for _, d := range one.Distributions {
		pool += d.Amount
	}
	for _, m := range one.ManagerBonuses {
		if m.Manager == "P1" {
			manager += m.Amount
		}
	}
	for _, s := range one.Splits {
		if s.From == "P1" {
			splitNet -= s.Amount
		}
		if s.To == "P1" {
			splitNet += s.Amount
		}
	}
	if direct != want.Direct || pool != want.Pool || manager != want.Manager || splitNet != want.Split {
		t.Fatalf("明细与结算行不一致：直接 %s/%s 池 %s/%s 管理 %s/%s 分账 %s/%s",
			direct, want.Direct, pool, want.Pool, manager, want.Manager, splitNet, want.Split)
	}

	// 投影不重算：重复投影得到完全相同的结果，且原结果未被改动。
	again := all.ProjectByPerson("P1")
	if len(again.Directs) != len(one.Directs) || len(again.Splits) != len(one.Splits) {
		t.Fatal("投影不幂等")
	}
	if len(all.Directs) == len(one.Directs) {
		t.Fatal("投影没有裁掉任何明细")
	}
	if len(all.Persons) != 5 {
		t.Fatalf("原结果被投影改动了：Persons 只剩 %d 行", len(all.Persons))
	}
}

// 未知 ID 只裁空，不报错、不 panic：投影是视图，不是查询。
func TestProjectByPersonUnknownIDIsEmpty(t *testing.T) {
	all := demoResult(t)
	empty := all.ProjectByPerson("NOPE")

	if len(empty.Persons) != 0 || len(empty.Directs) != 0 || len(empty.Trace) != 0 {
		t.Fatalf("未知人员应裁出空视图，got persons=%d directs=%d trace=%d",
			len(empty.Persons), len(empty.Directs), len(empty.Trace))
	}
	if empty.CalcRunID != all.CalcRunID {
		t.Fatal("空视图仍应保留 CalcRunID")
	}
	if empty.Total() != 0 {
		t.Fatalf("空视图合计应为 0，got %s", empty.Total())
	}
}

// 按订单投影：人对小计必须按保留明细重新求和（期间合计放进单笔订单是错的数）。
func TestProjectByOrderReaggregates(t *testing.T) {
	all := demoResult(t)
	one := all.ProjectByOrder("O-1001")

	if len(one.Directs) == 0 {
		t.Fatal("O-1001 应有多条直接佣金")
	}
	for _, d := range one.Directs {
		if d.OrderID != "O-1001" {
			t.Fatalf("投影里混入了其它订单：%s", d.OrderID)
		}
	}
	for _, s := range one.Splits {
		if s.OrderID != "O-1001" {
			t.Fatalf("投影里混入了其它订单的分账：%s", s.OrderID)
		}
	}
	for _, tr := range one.Trace {
		if tr.OrderID != "O-1001" {
			t.Fatalf("投影里混入了其它订单的 Trace：%s", tr.OrderID)
		}
	}

	// 守恒：这一单发出去的钱 == 直接 + 池分发 + 管理奖（分账零和，不出现在式子里）。
	var direct, pool, manager money.Money
	for _, d := range one.Directs {
		direct += d.Amount
	}
	for _, d := range one.Distributions {
		pool += d.Amount
	}
	for _, m := range one.ManagerBonuses {
		manager += m.Amount
	}
	if one.Total() != direct+pool+manager {
		t.Fatalf("投影守恒失败：合计 %s != %s + %s + %s", one.Total(), direct, pool, manager)
	}
	var splitNet money.Money
	for _, p := range one.Persons {
		splitNet += p.Split
	}
	if splitNet != 0 {
		t.Fatalf("单笔订单的分账净额应逐组零和，got %s", splitNet)
	}

	// 与整期口径区分开：单笔订单的合计必须小于整期合计（否则说明没裁也没重算）。
	if one.Total() >= all.Total() {
		t.Fatalf("单笔订单合计 %s 不应 ≥ 整期合计 %s", one.Total(), all.Total())
	}

	// 投影覆盖全部订单时，合计必须回到整期值——这是「投影只裁不重算」的最强形式。
	ids := []domain.OrderID{}
	for _, o := range demo.Facts().Orders {
		ids = append(ids, o.ID)
	}
	full := all.ProjectByOrder(ids...)
	if full.Total() != all.Total() {
		t.Fatalf("全量投影合计 %s != 整期合计 %s", full.Total(), all.Total())
	}
	for i := range all.Persons {
		if full.Persons[i] != all.Persons[i] {
			t.Fatalf("全量投影后 %s 的结算行不一致：%+v != %+v",
				all.Persons[i].Person, full.Persons[i], all.Persons[i])
		}
	}
}
