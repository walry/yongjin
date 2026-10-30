package calc

import (
	"github.com/walry/yongjin/internal/domain"
	"github.com/walry/yongjin/internal/money"
)

// EngineVersion 必须参与 calcRunID：代码升级后重算结果变化时，幂等键不能相同。
//
// 2.0.0：事实增加双口径（合同/收入）、目标额、关系快照与池预算；Action.BPS 语义收紧为
// BaseBPS（基准费率）；factsDigest 改为规范化编码。以上任一项都会改变计算结果或摘要，
// 若不 bump，幂等缓存会吞掉重算请求，表现为"改了代码但线上数字没动"。
const EngineVersion = "2.0.0"

type Options struct {
	EngineVersion string
}

// Attainment 是 (人, 口径) 的期间达成率。它是期间级函数，必须先归集全期业绩
// 才能逐单算钱——这正是 Run 分两趟的原因（阶段 0 与阶段 3~8）。
type Attainment struct {
	Person  domain.PersonID
	KPI     domain.KPI
	Actual  money.Money
	Target  money.Money
	RateBPS int64 // floor(actual/target)，仅供展示；档位裁决用交叉相乘，不看这个数
}

// DirectLine 是"这一行的这笔佣金给这个人"的最终事实。
//
// 合同口径与收入口径各出一条，靠 KPI 区分，绝不合并：同一条订单行在同一个人身上
// 会出现两条 DirectLine，金额各自按自己的基数与档位算（不变式 I11）。
type DirectLine struct {
	OrderID  domain.OrderID
	LineID   domain.OrderLineID
	Person   domain.PersonID
	Role     domain.RoleID
	Product  string
	KPI      domain.KPI
	Base     money.Money // 该口径的基数
	RateBPS  int64       // 基准费率 BaseBPS
	BandID   domain.BandID
	CoeffBPS int64 // 档位倍数，10000 = 1.0
	// Factors 是时间/产品/区域因子，按 Kind 固定顺序。与档位一起进同一条
	// ApplyChain，全链只舍入一次（设计 §6.1）。
	Factors []FactorHit
	// Weight 是参与份额（100 = 全额）。它是"分钱比例"，不是业绩归属，
	// 所以只作用在金额上，绝不回写达成率（I10）。
	Weight int64
	// OverlayDelta 是叠加/封顶/豁免作用在因子链结果上的净增量，可为负。
	// 特殊规则是加法与截断，不是乘法，所以不进因子链（Trace.Replay 会把它加回来）。
	OverlayDelta money.Money
	Amount       money.Money
	// ShareMode 是这条规则的解析结果（分账方式）。金额 Amount 始终是**分账前**全额：
	// 管理奖金与管理链都读它（I12），分账只产出 SplitLine，不回写这里。
	ShareMode domain.ShareMode
	RuleID    domain.RuleID
	Overlays  []domain.RuleID
}

// SplitLine 是一次分账转移：成单人 From 把 Amount 转给 To。
//
// 它不是"再算一笔佣金"，所以没有 RateBPS / 倍率链：金额只能靠"分账前全额 −
// 这一组的其他分录"得到，守恒关系是 Σ SplitLine + 自留 == 分账前金额（I6）。
// 也正因为它不是倍率链的一环，Trace 里不设分账记录 —— 它自己就是留痕。
type SplitLine struct {
	OrderID domain.OrderID
	LineID  domain.OrderLineID
	KPI     domain.KPI
	From    domain.PersonID // 成单人，即直接佣金原受益人
	To      domain.PersonID // 收款人（配对 SS 或名下 AS）
	Amount  money.Money
	Mode    domain.ShareMode
	RuleID  domain.RuleID
}

// PoolClaim 是「这一条订单行从池里领走了多少」。它是领取，不是分发：
//
// 领取回答「池被谁消耗、消耗到哪一单就没钱了」，分发回答「领到的钱分给了谁」。
// 合并成一条会让「池没发完是因为没订单还是规则没配」无法区分 —— 前者是正常结转，
// 后者是配置缺口。Budget/Remaining 只在这层有意义，Distribution 一概不看。
type PoolClaim struct {
	OrderID   domain.OrderID
	LineID    domain.OrderLineID
	Pool      domain.PoolID
	Budget    money.Money // 期初预算（事实）
	Remaining money.Money // 领取前剩余
	Claimed   money.Money // 本单领取，≤ Remaining
	// Exhausted 表示领取后池已空：后续同池订单会拿到 0，这是预算语义的正确结果，
	// 但必须在明细里看得见，否则「后面几单池没有钱」会被当成 bug 排查。
	Exhausted bool
	RuleID    domain.RuleID
}

// Distribution 是「这一笔领取额分给了谁」。它挂在领取明细下，所以带 OrderID/LineID；
// 按人汇总是结算层的事，不在这一层做（设计 §8.1/§8.2 的分层）。
type Distribution struct {
	OrderID domain.OrderID
	LineID  domain.OrderLineID
	Pool    domain.PoolID
	Person  domain.PersonID
	Role    domain.RoleID
	Weight  int64
	Amount  money.Money
	RuleID  domain.RuleID
}

// ManagerBonus 是某条直接佣金沿组织树上溯产生的一笔管理奖。
//
// Base 恒等于被上溯那条 DirectLine 的 Amount（分账前全额，I12），因此带上
// OrderID/LineID 才能在明细层把两者一一对上——只留 Source 时，同一个人同口径
// 的多条直接佣金金额可能相同，对账会落到"能找到但不知道是哪一条"。
type ManagerBonus struct {
	OrderID domain.OrderID
	LineID  domain.OrderLineID
	Source  domain.PersonID // 产生佣金的原受益人
	Manager domain.PersonID
	KPI     domain.KPI // 合同/收入两条分别上溯，留痕用
	Level   int
	Base    money.Money // == 对应 DirectLine.Amount（分账前）
	RateBPS int64
	Amount  money.Money
	RuleID  domain.RuleID
}

// PoolSettlement 是池的期末对账：Budget == Distributed + CarryOver（I2）。
//
// Claimed 是「离开预算」的部分，Retained 是其中的留存（留在池里下次再用），
// 未领取的部分也进 CarryOver —— 所以 CarryOver = Budget − Distributed 恒成立，
// 且 Retained 是 CarryOver 的子集，不是与它并列的另一份钱。
type PoolSettlement struct {
	Pool        domain.PoolID
	Budget      money.Money // 期初预算（事实）
	Claimed     money.Money // 累计领取
	Retained    money.Money // 领取额中留存未发的部分
	Distributed money.Money // 实际分出去
	CarryOver   money.Money // Budget − Distributed
	// ClaimRule 与 DistRule 必须分开留痕：一层决定「每单领多少」，另一层决定
	// 「领到的钱怎么分」。合成一个字段后，改错层就无从定位。
	ClaimRule domain.RuleID
	DistRule  domain.RuleID
}

type PersonTotal struct {
	Person domain.PersonID
	Name   string
	// Direct 是两个口径之和，等于 DirectContract + DirectRevenue。
	Direct         money.Money
	DirectContract money.Money
	DirectRevenue  money.Money
	// Split 是分账净额：转出为负、转入为正，全公司求和恒为 0（零和转移）。
	// 它单独成列而不是折进 Direct，是为了让"这单是谁促成的"（Direct，管理奖基数）
	// 与"钱最后落在谁账上"（Direct + Split）永远是两个可分别对账的数（I12）。
	Split   money.Money
	Pool    money.Money
	Manager money.Money
	Total   money.Money
}

// Trace 是逐笔留痕。它必须足够完整，能只靠 Trace 复算出 Amount（不变式 I8），
// 复算逻辑见 Trace.Replay。
type Trace struct {
	OrderID  string
	LineID   string
	Phase    string
	RuleID   string
	Pool     string
	Person   string
	KPI      string
	Band     string
	Base     money.Money
	RateBPS  int64
	CoeffBPS int64
	Factors  []FactorHit
	// Weight 是份额；0 表示该阶段不分份额（池注入、管理奖金），复算时跳过。
	Weight       int64
	OverlayDelta money.Money
	Amount       money.Money
	Note         string
}

// Replay 只用 Trace 自身的留痕重算金额：
//
//	Amount == round(Base × RateBPS × CoeffBPS × Π Factors.BPS) × Weight/100 + OverlayDelta
//
// 它存在的意义不是"再算一遍"，而是让"结果可解释"成为可断言的性质：
// 任何一笔金额都必须能被它自己的留痕复现，否则 Trace 就是装饰品。
func (t Trace) Replay() money.Money {
	chain := make([]int64, 0, 2+len(t.Factors))
	chain = append(chain, t.RateBPS, t.CoeffBPS)
	chain = append(chain, factorBPS(t.Factors)...)

	amt := money.ApplyChain(t.Base, chain...)
	if t.Weight != 0 && t.Weight != 100 {
		amt = amt * money.Money(t.Weight) / 100
	}
	return amt + t.OverlayDelta
}

type Result struct {
	CalcRunID      string
	Period         domain.Period
	EngineVersion  string
	RuleSetVersion string
	FactsDigest    string

	Attainments    []Attainment
	Directs        []DirectLine
	Splits         []SplitLine
	Claims         []PoolClaim
	Distributions  []Distribution
	ManagerBonuses []ManagerBonus
	Pools          []PoolSettlement
	Persons        []PersonTotal
	Trace          []Trace
	Warnings       []string
}

// Total 是全公司应发合计，用于守恒断言。
func (r Result) Total() money.Money {
	var s money.Money
	for _, p := range r.Persons {
		s += p.Total
	}
	return s
}
