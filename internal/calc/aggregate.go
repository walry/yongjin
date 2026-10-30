package calc

import (
	"sort"

	"github.com/walry/yongjin/internal/domain"
)

// aggregatePersonTotals 把四类明细折成按人结算。它是 Run 阶段 8 的**唯一**实现，
// 投影（project.go）也复用它：投影只裁输入明细，绝不另写一套求和规则。
//
// 拆出来不只是为了少写几行——分账净额必须在这里同时记在 From（负）与 To（正），
// 若投影自己再实现一遍，很容易只记转入不记转出，投影出来的 Split 就不再零和。
func aggregatePersonTotals(
	directs []DirectLine,
	splits []SplitLine,
	dists []Distribution,
	bonuses []ManagerBonus,
	name func(domain.PersonID) string,
) []PersonTotal {
	totals := map[domain.PersonID]*PersonTotal{}
	get := func(p domain.PersonID) *PersonTotal {
		t, ok := totals[p]
		if !ok {
			t = &PersonTotal{Person: p, Name: name(p)}
			totals[p] = t
		}
		return t
	}
	for _, d := range directs {
		t := get(d.Person)
		t.Direct += d.Amount
		if d.KPI == domain.KPIRevenue {
			t.DirectRevenue += d.Amount
		} else {
			t.DirectContract += d.Amount
		}
	}
	// 分账净额：转出为负、转入为正。全公司求和为 0 —— 这是零和转移的直接推论，
	// 也是「分账不产生新钱」在结算层的体现。
	for _, s := range splits {
		get(s.From).Split -= s.Amount
		get(s.To).Split += s.Amount
	}
	for _, d := range dists {
		get(d.Person).Pool += d.Amount
	}
	for _, m := range bonuses {
		get(m.Manager).Manager += m.Amount
	}

	ids := make([]string, 0, len(totals))
	for p := range totals {
		ids = append(ids, string(p))
	}
	sort.Strings(ids)
	out := make([]PersonTotal, 0, len(ids))
	for _, sp := range ids {
		t := totals[domain.PersonID(sp)]
		t.Total = t.Direct + t.Split + t.Pool + t.Manager
		out = append(out, *t)
	}
	return out
}
