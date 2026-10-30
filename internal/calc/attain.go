package calc

import (
	"fmt"
	"sort"

	"github.com/walry/yongjin/internal/domain"
	"github.com/walry/yongjin/internal/money"
	"github.com/walry/yongjin/internal/rules"
)

// dualKPI 是必须同时算的两个口径，顺序固定：合同在前、收入在后。
// 顺序决定 Directs / Attainments 的稳定输出，不决定金额。
var dualKPI = []domain.KPI{domain.KPIContract, domain.KPIRevenue}

type attKey struct {
	Person domain.PersonID
	KPI    domain.KPI
}

// attainmentsOf 是流水线阶段 0：扫全期订单，按业绩归属累加实际，除以目标额。
//
// 业绩归属口径（M2 默认，见设计 §2.2 / 开放问题 Q10）：订单的每个获利角色都按**全额**
// 记业绩，Participant.Weight 只影响佣金份额，不影响业绩归属。业绩归属图与佣金分账图
// 必须分开——否则 AS 分账给 SS 之后，SS 的达成率会跟着跳档（不变式 I10）。
func attainmentsOf(facts domain.Facts, ix *rules.Index) ([]Attainment, []string) {
	var warnings []string

	actual := map[attKey]money.Money{}
	people := map[domain.PersonID]struct{}{}
	for _, o := range facts.Orders {
		credited := o.Persons()
		for _, pp := range credited {
			people[pp.Person] = struct{}{}
		}
		for _, ln := range o.Lines {
			for _, kpi := range dualKPI {
				base := ln.BaseOf(kpi)
				if base <= 0 {
					continue
				}
				for _, pp := range credited {
					actual[attKey{Person: pp.Person, KPI: kpi}] += base
				}
			}
		}
	}
	for _, t := range facts.Targets {
		if t.Period == "" || t.Period == facts.Period {
			people[t.Person] = struct{}{}
		}
	}

	ids := make([]string, 0, len(people))
	for p := range people {
		ids = append(ids, string(p))
	}
	sort.Strings(ids)

	out := make([]Attainment, 0, len(ids)*len(dualKPI))
	for _, sp := range ids {
		pid := domain.PersonID(sp)
		t := facts.TargetOf(pid)
		for _, kpi := range dualKPI {
			target := t.Contract
			if kpi == domain.KPIRevenue {
				target = t.Revenue
			}
			got := actual[attKey{Person: pid, KPI: kpi}]
			if got == 0 && target == 0 {
				continue // 既不背指标也没业绩，不产出记录
			}
			a := Attainment{Person: pid, KPI: kpi, Actual: got, Target: target}
			if target > 0 {
				a.RateBPS = int64(got) * money.BPS / int64(target)
			} else {
				// Q8 默认：目标额 <= 0 视为 0%，取最低档，并告警。
				warnings = append(warnings, fmt.Sprintf(
					"[attain] %s 口径 %s 目标额 %s <= 0，按 0%% 处理（取最低档）", pid, kpi, target))
			}
			out = append(out, a)
		}
	}
	return out, warnings
}

// bandHit 是一次档位裁决的结果。
type bandHit struct {
	Band     domain.Band
	RuleID   domain.RuleID
	CoeffBPS int64
}

// pickBand 用「人员达成率 + (角色 × 口径) 档位表」定倍数。
//
// 两个维度分开是刻意的：达成率是人员级（同一人在本期只有一条），档位表是角色级
// （业务口径：佣金系数按角色和达成率设置）。若把档位表也做成人员级，就得先知道
// 这个人在本期算哪个角色，而角色本来就随订单变——那才是真的循环依赖。
func pickBand(ix *rules.Index, facts domain.Facts, att map[attKey]Attainment,
	person domain.PersonID, role domain.RoleID, kpi domain.KPI) (bandHit, bool) {
	ctx := domain.MatchCtx{KPI: kpi, Role: role, Dept: facts.Persons[person].Dept}
	r, ok := ix.Best(domain.PhaseAttain, ctx, facts.Period)
	if !ok || len(r.Action.Bands) == 0 {
		return bandHit{RuleID: r.ID, CoeffBPS: money.BPS}, false
	}
	bands := append([]domain.Band(nil), r.Action.Bands...)
	sort.SliceStable(bands, func(i, j int) bool { return bands[i].FromBPS < bands[j].FromBPS })

	// 目标额 <= 0 时达成率无意义，按 Q8 取最低档（attainmentsOf 已告警）。
	a, has := att[attKey{Person: person, KPI: kpi}]
	if !has || a.Target <= 0 {
		return bandHit{Band: bands[0], RuleID: r.ID, CoeffBPS: bands[0].CoeffBPS}, true
	}
	for _, b := range bands {
		if b.Hit(a.Actual, a.Target) {
			return bandHit{Band: b, RuleID: r.ID, CoeffBPS: b.CoeffBPS}, true
		}
	}
	// 档位表已在 Compile 校验为覆盖 [0,+∞)，走到这里说明表被运行时改过。
	return bandHit{RuleID: r.ID, CoeffBPS: money.BPS}, false
}
