package calc

import "github.com/walry/yongjin/internal/domain"

// lineKey 是「订单行」的复合键。领取/分发都是行级明细，按人裁剪时只能通过
// 保留下来的行反查它们，不能按人直接筛（池明细里的人和直接佣金的人不是一一对应）。
type lineKey struct {
	order domain.OrderID
	line  domain.OrderLineID
}

// 投影：唯一算钱入口是 Run（全量）。「按人看」「按订单看」只是在已经算完的结果上
// 裁输出，不得触发重算——达成率是期间函数、池是全局序列函数，局部重算出来的数字
// 每一次都是错的（设计 §9）。因此两个投影函数都保持 CalcRunID / FactsDigest /
// RuleSetVersion 原样不变，调用方一眼就能确认「这不是另一次计算」。

// ProjectByPerson 裁出这些人的视图。
//
// 保留什么：
//   - Persons：这些人的结算行，**原样保留**。它本来就是这个人的期间合计，
//     裁剪不会让它变成半个数。
//   - 明细：Directs / Distributions / ManagerBonuses / Trace 按「这个人是其中一方」筛；
//     分账两方都留（From 转出、To 转入），只留一侧会让净额对不上，Σ Split 也不再是 0。
//
// 不裁什么：Pools 与 Warnings。池结算是对**整期预算**的对账（`Budget` 是全公司预算），
// 按人切开会得到一个分子属于个人、分母属于全体的假数。要看「这个人参与了哪些领取行」，
// 读裁好的 Claims。
func (r Result) ProjectByPerson(ids ...domain.PersonID) Result {
	want := make(map[domain.PersonID]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}

	out := r
	out.Persons = nil
	out.Attainments = nil
	out.Directs = nil
	out.Splits = nil
	out.Claims = nil
	out.Distributions = nil
	out.ManagerBonuses = nil
	out.Trace = nil

	for _, p := range r.Persons {
		if want[p.Person] {
			out.Persons = append(out.Persons, p)
		}
	}
	for _, a := range r.Attainments {
		if want[a.Person] {
			out.Attainments = append(out.Attainments, a)
		}
	}
	for _, d := range r.Directs {
		if want[d.Person] {
			out.Directs = append(out.Directs, d)
		}
	}
	for _, s := range r.Splits {
		if want[s.From] || want[s.To] {
			out.Splits = append(out.Splits, s)
		}
	}
	for _, d := range r.Distributions {
		if want[d.Person] {
			out.Distributions = append(out.Distributions, d)
		}
	}
	for _, m := range r.ManagerBonuses {
		if want[m.Source] || want[m.Manager] {
			out.ManagerBonuses = append(out.ManagerBonuses, m)
		}
	}
	// 领取是行级事实：人只通过「参与过这条行」与它相关，所以按保留下来的
	// Directs 反查行，而不是按 Distributions（领了但没参与分发的行同样发生过）。
	lines := make(map[lineKey]bool, len(out.Directs))
	for _, d := range out.Directs {
		lines[lineKey{d.OrderID, d.LineID}] = true
	}
	for _, c := range r.Claims {
		if lines[lineKey{c.OrderID, c.LineID}] {
			out.Claims = append(out.Claims, c)
		}
	}
	for _, t := range r.Trace {
		if want[domain.PersonID(t.Person)] {
			out.Trace = append(out.Trace, t)
		}
	}
	return out
}

// ProjectByOrder 裁出这些订单的视图，并按保留明细**重新求和**人对小计。
//
// 这里必须重算 Persons：期间合计放进「这一单」的视图里是错的数（某人这个月
// 拿了 10 万，不代表这单发了 10 万）。重算只是把 aggregate.go 的求和跑在裁剪后的
// 明细上，金额一分未动，也没有再查任何规则——它不是「局部重算引擎」。
//
// 分母类切片（Attainments / Pools / Warnings）保持整期口径：达成率与池预算都不是
// 单笔订单的属性，按订单切只会得到误导性的残缺数。
func (r Result) ProjectByOrder(ids ...domain.OrderID) Result {
	want := make(map[domain.OrderID]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}

	out := r
	out.Directs = nil
	out.Splits = nil
	out.Claims = nil
	out.Distributions = nil
	out.ManagerBonuses = nil
	out.Trace = nil

	for _, d := range r.Directs {
		if want[d.OrderID] {
			out.Directs = append(out.Directs, d)
		}
	}
	for _, s := range r.Splits {
		if want[s.OrderID] {
			out.Splits = append(out.Splits, s)
		}
	}
	for _, c := range r.Claims {
		if want[c.OrderID] {
			out.Claims = append(out.Claims, c)
		}
	}
	for _, d := range r.Distributions {
		if want[d.OrderID] {
			out.Distributions = append(out.Distributions, d)
		}
	}
	for _, m := range r.ManagerBonuses {
		if want[m.OrderID] {
			out.ManagerBonuses = append(out.ManagerBonuses, m)
		}
	}
	for _, t := range r.Trace {
		if want[domain.OrderID(t.OrderID)] {
			out.Trace = append(out.Trace, t)
		}
	}

	names := make(map[domain.PersonID]string, len(r.Persons))
	for _, p := range r.Persons {
		names[p.Person] = p.Name
	}
	out.Persons = aggregatePersonTotals(out.Directs, out.Splits, out.Distributions, out.ManagerBonuses,
		func(p domain.PersonID) string { return names[p] })
	return out
}
