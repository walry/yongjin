package calc

import (
	"fmt"
	"sort"

	"github.com/walry/yongjin/internal/domain"
	"github.com/walry/yongjin/internal/money"
)

// 分账（设计 §7）是一条零和转移，不是「再算一笔佣金」。三条硬约束：
//
//  1. 不级联：只对 DirectLine 做一次转移，SplitLine 永不再拆。级联会让一条订单行
//     产生 N 层转移，还容易成环。
//  2. 守恒：Σ SplitLine + 自留 == 分账前金额。用最大余数法拆，尾差给成单人。
//  3. 关系图缺失按 100% 自留 + 告警，不静默按 0 处理 —— 少分一分钱也要能看见。
//
// 份额一律用权重 [1,1,...] 直接 Allocate，不先算整数百分比再乘：1/3 写成 3333bps，
// 三个人加起来只剩 99.99%，差的 0.01% 会凭空消失。
func splitsOf(facts domain.Facts, directs []DirectLine) ([]SplitLine, []string) {
	var out []SplitLine
	var warns []string
	warned := map[string]bool{}
	warn := func(key, msg string) {
		if warned[key] {
			return
		}
		warned[key] = true
		warns = append(warns, msg)
	}

	for _, d := range directs {
		if d.Amount <= 0 || d.ShareMode == domain.ShareNone {
			continue
		}

		receivers, why := shareReceivers(facts, d)
		if len(receivers) == 0 {
			// 关系缺口只告警不改数：成单人保留全额，账面仍然守恒。
			warn(string(d.OrderID)+"/"+string(d.LineID)+"/"+string(d.Person), fmt.Sprintf(
				"[share] %s 分账方式 %s：%s，按 100%% 自留", d.Person, d.ShareMode, why))
			continue
		}

		// 成单人固定排第一位：最大余数法按索引升序补尾差，
		// 于是「尾差给成单人」是排序性质，不靠额外分支。
		people := append([]domain.PersonID{d.Person}, receivers...)
		weights := make([]int64, len(people))
		for i := range weights {
			weights[i] = 1
		}
		parts := money.Allocate(d.Amount, weights)

		for i := 1; i < len(people); i++ {
			if parts[i] == 0 {
				continue // 金额小到拆不出一分，不产出零金额分录
			}
			out = append(out, SplitLine{
				OrderID: d.OrderID, LineID: d.LineID, KPI: d.KPI,
				From: d.Person, To: people[i], Amount: parts[i],
				Mode: d.ShareMode, RuleID: d.RuleID,
			})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.OrderID != b.OrderID {
			return a.OrderID < b.OrderID
		}
		if a.LineID != b.LineID {
			return a.LineID < b.LineID
		}
		if a.KPI != b.KPI {
			return a.KPI < b.KPI
		}
		if a.From != b.From {
			return a.From < b.From
		}
		return a.To < b.To
	})
	return out, warns
}

// shareReceivers 按分账方式找收款人。关系图取自事实快照，不看实时组织架构：
// 重算历史期间时，配对必须还是当时那一份。
func shareReceivers(facts domain.Facts, d DirectLine) ([]domain.PersonID, string) {
	switch d.ShareMode {
	case domain.SharePair1to1:
		// AS 成单 → 与配对 SS 各 50%。取「上级」方向：To=AS 的那条关系，From 才是 SS。
		ss := facts.SuperiorsOf(d.Person, domain.RelSSPairAS)
		if len(ss) == 0 {
			return nil, "无配对 SS（关系 " + string(domain.RelSSPairAS) + " 缺失）"
		}
		// 一对多只有 SS→AS 一个方向，AS 反过来挂多个 SS 属于数据错误。
		// 取字典序第一个保证结果可复现，剩下的靠告警暴露，不静默丢弃。
		return ss[:1], ""
	case domain.ShareSplitSubs:
		// SS 成单 → 与名下每个 AS 均分，各 1/(1+N)。
		subs := facts.SubordinatesOf(d.Person, domain.RelSSPairAS)
		if len(subs) == 0 {
			return nil, "名下无 AS（关系 " + string(domain.RelSSPairAS) + " 缺失）"
		}
		return subs, ""
	}
	return nil, "未知分账方式 " + string(d.ShareMode)
}

// assertSplits 校验 I6。三件事必须同时成立，任一破坏就返回 error 而不是告警：
//
//   - 每条分账都能找到直接佣金来源（分账不得凭空造钱）
//   - 同一来源的 Σ 分出 ≤ 分账前金额（守恒）
//   - 收款人不等于成单人、同一来源内不重复（不得重复转移）
//
// 不级联是结构保证的：分账只读 DirectLine，从不读 SplitLine。这里再把
// 「每条 SplitLine 的 From 必须是某条 DirectLine 的受益人」钉成可测的断言。
func assertSplits(directs []DirectLine, splits []SplitLine) error {
	type key struct {
		order  domain.OrderID
		line   domain.OrderLineID
		person domain.PersonID
		kpi    domain.KPI
	}
	gross := make(map[key]money.Money, len(directs))
	for _, d := range directs {
		gross[key{d.OrderID, d.LineID, d.Person, d.KPI}] = d.Amount
	}

	sum := make(map[key]money.Money, len(splits))
	seen := make(map[string]struct{}, len(splits))
	for _, s := range splits {
		k := key{s.OrderID, s.LineID, s.From, s.KPI}
		if _, ok := gross[k]; !ok {
			return fmt.Errorf("I6 违反：分账 %s/%s %s→%s 找不到对应直接佣金",
				s.OrderID, s.LineID, s.From, s.To)
		}
		if s.From == s.To {
			return fmt.Errorf("I6 违反：%s/%s %s 分给了自己", s.OrderID, s.LineID, s.From)
		}
		id := fmt.Sprintf("%s/%s/%s/%s/%s", s.OrderID, s.LineID, s.KPI, s.From, s.To)
		if _, dup := seen[id]; dup {
			return fmt.Errorf("I6 违反：%s 出现重复分账", id)
		}
		seen[id] = struct{}{}
		sum[k] += s.Amount
	}

	for k, s := range sum {
		if s > gross[k] {
			return fmt.Errorf("I6 违反：%s/%s/%s 分出 %s 超过分账前 %s",
				k.order, k.line, k.person, s, gross[k])
		}
	}
	return nil
}
