package golden

import (
	"github.com/walry/yongjin/internal/demo"
	"github.com/walry/yongjin/internal/domain"
	"github.com/walry/yongjin/internal/money"
)

// Cases 是金标语料。选取标准只有一条：**每一份压一个具体语义**，
// 且这个语义一旦回退，除了它没有别的地方会红。
//
// 全部从 demo 的构造器出发（同一套订单、同一批人），再显式改一到两处。
// 好处是差异可读：「相对基线只把 POOL-A 预算改成 30000」，而不是另一套
// 面目全非的虚构数据 —— 后者出问题时没人能判断哪边才是对的。
func Cases() []Case {
	return []Case{
		baselineCase(),
		bandsCase(),
		factorsCase(),
		splitCase(),
		poolStarvedCase(),
		poolNoDistributeCase(),
		edgeCase(),
	}
}

// baselineCase 是金额锚点：无档位、无因子、无分账。
//
// RulesV1 刻意不带档位表，所以倍率恒 1.0，金额与引入倍率模型之前逐分一致。
// 它守的是「不该动的别动」——任何人不小心把默认倍率从 1.0 改成别的、
// 或让缺配档位按 0 处理，只有这条会红。
func baselineCase() Case {
	return Case{
		Name:  "baseline",
		Note:  "demo 事实 + RulesV1：无档位 / 无因子 / 无分账。倍率恒 1.0，金额是回归锚点。",
		Tags:  []string{TagClaim, TagDistribute, TagManager, TagDualKPI, TagCarryOver, TagRetained},
		Facts: demo.Facts(),
		Rules: demo.RulesV1(),
	}
}

// bandsCase 压档位：达成率定倍数，且倍数与基准费率同进一条链。
//
// 陷阱在边界：148.33% 与 130% 之间只差一个档位表写法。若把区间判成
// 闭区间或写成 <=，边界订单会落到低一档，合计只差几个百分点 —— 守恒断言
// 完全看不出来。
func bandsCase() Case {
	return Case{
		Name:  "bands",
		Note:  "demo 事实 + RulesV1Bands：达成率命中 LOW/NORMAL/HIGH/TOP 四档，倍率 0.9~1.5。",
		Tags:  []string{TagBand, TagDualKPI, TagManager, TagClaim, TagDistribute},
		Facts: demo.Facts(),
		Rules: demo.RulesV1Bands(),
	}
}

// factorsCase 压因子链：时间 × 产品 × 区域三段乘数，全链只舍入一次。
//
// 若有人把 chain 拆成三次 ApplyRate，这里会差几分到几毛。金额很小，
// 但「几分钱对不上」正是业务最先发现、工程最难复现的那类问题。
func factorsCase() Case {
	return Case{
		Name:  "factors",
		Note:  "demo 事实 + RulesV1Full：档位 + 时间/产品/区域因子，全链单次舍入。",
		Tags:  []string{TagBand, TagFactor, TagDualKPI, TagManager, TagClaim, TagDistribute},
		Facts: demo.Facts(),
		Rules: demo.RulesV1Full(),
	}
}

// splitCase 压分账：零和转移，且管理奖基数仍是分账前全额。
//
// 分账最容易出的错不是「少转」(守恒会抓)，而是「转完还按净额上溯管理奖」——
// 合计不变、分账净额也是 0，只有 P3/P5 的 manager 列会变。这正是金标集
// 按列冻结的理由：只看 total 的回归会放过它。
func splitCase() Case {
	return Case{
		Name:  "split",
		Note:  "demo 事实 + Shares：channel 成单与配对 SS 对半，分账零和、管理奖基数不动。",
		Tags:  []string{TagSplit, TagBand, TagFactor, TagDualKPI, TagManager, TagClaim, TagDistribute},
		Facts: demo.Facts(),
		Rules: demo.Shares(),
	}
}

// poolStarvedCase 压预算饥饿：POOL-A 只有 30000，而按费率应领 60000 以上。
//
// 三个语义一起验：FIFO 谁先领满、领取封顶后 Trace 仍能复算、留存与结转
// 的关系（CarryOver 含 Retained，不是并列）。预算充裕时这三条全被掩盖 ——
// 看起来和「按费率注资」没区别，所以必须有一个被打饿的用例。
func poolStarvedCase() Case {
	f := demo.Facts()
	setPoolBudget(f, "POOL-A", money.FromYuan(30000, 0))
	return Case{
		Name:  "pool-starved",
		Note:  "demo 事实（POOL-A 预算 60000 → 30000）+ RulesV1Full：池按成单全序领满即止。",
		Tags:  []string{TagClaim, TagDistribute, TagCarryOver, TagRetained, TagBand, TagFactor, TagManager},
		Facts: f,
		Rules: demo.RulesV1Full(),
	}
}

// poolNoDistributeCase 压「领了但分不出去」：删掉 POOL-A 的分发规则。
//
// 这是配置缺口而非算法缺陷，正确的行为是钱留在池里结转 + 发布期报缺口，
// 绝不能默认发给负责人 —— 默认值会把配置错误变成一笔无声的、金额正确的错发。
func poolNoDistributeCase() Case {
	f := demo.Facts()
	setPoolBudget(f, "POOL-A", money.FromYuan(30000, 0))
	return Case{
		Name:  "pool-no-distribute",
		Note:  "demo 事实（POOL-A 预算 30000）+ RulesV1Full 去掉 R-DIST-A：领到的钱全额留存结转并报缺口。",
		Tags:  []string{TagClaim, TagCarryOver, TagRetained, TagWarning, TagBand, TagFactor},
		Facts: f,
		Rules: dropRule(demo.RulesV1Full(), "R-DIST-A"),
	}
}

// edgeCase 压边界输入：目标额为 0、收入口径无基数。
//
// 目标 <= 0 按 Q8 取最低档并告警；收入为 0 的行只出合同口径一条明细，
// 池也不为它领钱。两个都是「不报错、但金额静默偏小」的场景。
func edgeCase() Case {
	f := demo.Facts()
	// P2 本期不背指标：达成率无意义 → 最低档 + 告警。
	for i := range f.Targets {
		if f.Targets[i].Person == "P2" {
			f.Targets[i].Contract = 0
			f.Targets[i].Revenue = 0
		}
	}
	// O-1002/L1 只有合同额、没有实际收入：收入口径整条不出明细，池也不领。
	for i := range f.Orders {
		if f.Orders[i].ID == "O-1002" {
			for j := range f.Orders[i].Lines {
				if f.Orders[i].Lines[j].ID == "L1" {
					f.Orders[i].Lines[j].Revenue = 0
				}
			}
		}
	}
	return Case{
		Name:  "edge",
		Note:  "demo 事实 + P2 目标清零 + O-1002/L1 收入清零：最低档回落、单口径出行、告警不静默。",
		Tags:  []string{TagWarning, TagBand, TagFactor, TagClaim, TagDistribute, TagCarryOver},
		Facts: f,
		Rules: demo.RulesV1Full(),
	}
}

func setPoolBudget(f domain.Facts, id domain.PoolID, budget money.Money) {
	for i := range f.Pools {
		if f.Pools[i].ID == id {
			f.Pools[i].Budget = budget
		}
	}
}

func dropRule(rs domain.RuleSet, id domain.RuleID) domain.RuleSet {
	kept := rs.Rules[:0]
	for _, r := range rs.Rules {
		if r.ID != id {
			kept = append(kept, r)
		}
	}
	rs.Rules = kept
	rs.Version = rs.Version + "-nodist"
	return rs
}
