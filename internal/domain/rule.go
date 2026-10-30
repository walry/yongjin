package domain

import (
	"time"

	"github.com/walry/yongjin/internal/money"
)

type Phase string

const (
	PhaseAttain     Phase = "attain"     // 业绩归集：定达成率、选档位表
	PhaseFund       Phase = "fund"       // 池注入
	PhaseFactor     Phase = "factor"     // 因子乘数：时间 / 产品 / 区域
	PhaseDirect     Phase = "direct"     // 直接佣金
	PhaseSplit      Phase = "split"      // 分账（SS/AS 零和转移，只是 PhaseDirect 的后续动作）
	PhaseDistribute Phase = "distribute" // 池分发
	PhaseManager    Phase = "manager"    // 管理奖金
)

type ActionKind string

const (
	ActCredit          ActionKind = "credit" // 业绩归属 + 档位表
	ActTimeFactor      ActionKind = "time_factor"
	ActProductFactor   ActionKind = "product_factor"
	ActTerritoryFactor ActionKind = "territory_factor"
	ActFundPool        ActionKind = "fund_pool"
	ActPayRole         ActionKind = "pay_role"
	ActDistributePool  ActionKind = "distribute_pool"
	ActManagerBonus    ActionKind = "manager_bonus"
)

// ShareMode 是直接佣金的分账方式（设计 §7）。它挂在 PhaseDirect 规则上：
// 规则决定"这笔佣金给谁"，分账方式决定"到手后要不要转一部分给搭档"。
//
// 分账是**零和转移**，不是再算一笔：金额拆完之和恒等于分账前金额，且只拆一次不级联。
type ShareMode string

const (
	// ShareNone 是默认值：全额自留。
	ShareNone ShareMode = ""
	// SharePair1to1 用于 AS 成单：与配对 SS 各 50%。
	SharePair1to1 ShareMode = "pair1to1"
	// ShareSplitSubs 用于 SS 成单：与名下每个 AS 均分，各 1/(1+N)。
	ShareSplitSubs ShareMode = "splitSubordinates"
)

type Method string

const (
	MethodPercent Method = "percent"
	MethodFixed   Method = "fixed"
	MethodTier    Method = "tier"
)

// DistMode 是池领取额在参与人之间的分发方式（设计 §8.2）。
//
// 「按成单顺序分」不在这里：它不是分发方式，而是领取顺序本身 ——
// 预算有限，谁先成单谁先领满，这条全序由 calc 用 (ClosedAt, OrderID, LineID)
// 保证，跟「这单领到的钱在参与人之间怎么分」是两个正交的问题。
type DistMode string

const (
	DistEqual  DistMode = "equal"  // 本单领取额在参与人之间均分
	DistAll    DistMode = "all"    // 全给指定角色（AllocTo）
	DistWeight DistMode = "weight" // 按角色权重表（WeightTable）
)

// Tier 是阶梯档位，UpTo == 0 表示无上限。档位按 UpTo 升序排列。
type Tier struct {
	UpTo money.Money
	BPS  int64
}

// Band 是达成率区间。FromBPS 含、ToBPS 不含；ToBPS == 0 表示无上界。
//
// CoeffBPS 是**倍数**（18000 = 基准的 1.8 倍），不是费率：佣金 = 基数 × BaseBPS
// × CoeffBPS × 各因子，全链只舍入一次（见 money.ApplyChain）。
type Band struct {
	ID       BandID
	FromBPS  int64
	ToBPS    int64
	CoeffBPS int64
}

// Hit 判断达成率 actual/target 是否落在本档。用交叉相乘比较，不先算比率：
// actual*10000/target 一旦取整，69.996% 会被舍成 70.00% 而跳档。
func (b Band) Hit(actual, target money.Money) bool {
	if target <= 0 {
		return false
	}
	lhs := int64(actual) * money.BPS
	if lhs < b.FromBPS*int64(target) {
		return false
	}
	return b.ToBPS == 0 || lhs < b.ToBPS*int64(target)
}

// Quarter 是成单季度。只到季度不到年份：年份由规则的生效期与期间切片限定，
// 这样"每年 Q4 都是旺季 1.2 倍"只需一条规则，不必每年复制一遍。
type Quarter string

const (
	Q1 Quarter = "Q1"
	Q2 Quarter = "Q2"
	Q3 Quarter = "Q3"
	Q4 Quarter = "Q4"
)

// QuarterOf 返回成单时间所属季度（设计 §11 Q4 默认：时间因子按成单季度取，
// 不按回款季度）。ClosedAt 为空时返回空季度，按季度配的时间因子不会命中。
func QuarterOf(t time.Time) Quarter {
	if t.IsZero() {
		return ""
	}
	switch m := t.Month(); {
	case m <= 3:
		return Q1
	case m <= 6:
		return Q2
	case m <= 9:
		return Q3
	default:
		return Q4
	}
}

// MatchCtx 是一次具体查找的上下文，空字段表示"不适用"。
//
// 没有 Person 维度：人级例外一律走 Overlay，否则规则矩阵会从几十格炸到几千格。
type MatchCtx struct {
	Line       ProductLineID
	Series     SeriesID
	Role       RoleID
	Pool       PoolID
	Territory  TerritoryID
	Quarter    Quarter
	Tier       TierID
	Band       BandID
	OrderClass OrderClass
	Dept       DeptID
	KPI        KPI
}

// Scope 是规则的作用域。任一维度为空表示该维度通配。
type Scope struct {
	Lines        []ProductLineID
	Series       []SeriesID
	Roles        []RoleID
	Pools        []PoolID
	Territories  []TerritoryID
	Quarters     []Quarter
	Tiers        []TierID
	Bands        []BandID
	OrderClasses []OrderClass
	Depts        []DeptID
	KPIs         []KPI

	// Specificity 由 Compile 计算，用于"更具体的规则优先"。
	Specificity int
}

var specificityWeight = map[string]int{
	"line": 8, "pool": 8, "role": 8, "band": 8, "dept": 8, "kpi": 8,
	"series": 4, "territory": 4, "order_class": 4, "quarter": 4, "tier": 2,
}

func (s *Scope) ComputeSpecificity() int {
	n := 0
	if len(s.Lines) > 0 {
		n += specificityWeight["line"]
	}
	if len(s.Pools) > 0 {
		n += specificityWeight["pool"]
	}
	if len(s.Roles) > 0 {
		n += specificityWeight["role"]
	}
	if len(s.Bands) > 0 {
		n += specificityWeight["band"]
	}
	if len(s.Depts) > 0 {
		n += specificityWeight["dept"]
	}
	if len(s.KPIs) > 0 {
		n += specificityWeight["kpi"]
	}
	if len(s.Series) > 0 {
		n += specificityWeight["series"]
	}
	if len(s.Territories) > 0 {
		n += specificityWeight["territory"]
	}
	if len(s.Quarters) > 0 {
		n += specificityWeight["quarter"]
	}
	if len(s.OrderClasses) > 0 {
		n += specificityWeight["order_class"]
	}
	if len(s.Tiers) > 0 {
		n += specificityWeight["tier"]
	}
	s.Specificity = n
	return n
}

func (s Scope) Matches(c MatchCtx) bool {
	if len(s.Lines) > 0 && !contains(string(c.Line), lineSet(s.Lines)) {
		return false
	}
	if len(s.Series) > 0 && !contains(string(c.Series), seriesSet(s.Series)) {
		return false
	}
	if len(s.Roles) > 0 && !contains(string(c.Role), roleSet(s.Roles)) {
		return false
	}
	if len(s.Pools) > 0 && !contains(string(c.Pool), poolSet(s.Pools)) {
		return false
	}
	if len(s.Territories) > 0 && !contains(string(c.Territory), terrSet(s.Territories)) {
		return false
	}
	if len(s.Quarters) > 0 && !contains(string(c.Quarter), quarterSet(s.Quarters)) {
		return false
	}
	if len(s.Tiers) > 0 && !contains(string(c.Tier), tierSet(s.Tiers)) {
		return false
	}
	if len(s.Bands) > 0 && !contains(string(c.Band), bandSet(s.Bands)) {
		return false
	}
	if len(s.OrderClasses) > 0 && !contains(string(c.OrderClass), classSet(s.OrderClasses)) {
		return false
	}
	if len(s.Depts) > 0 && !contains(string(c.Dept), deptSet(s.Depts)) {
		return false
	}
	if len(s.KPIs) > 0 && !contains(string(c.KPI), kpiSet(s.KPIs)) {
		return false
	}
	return true
}

// Overlaps 判断两个作用域是否可能命中同一个上下文（任一维度为空即通配）。
// 冲突检测用它做精确的两两比较，不需要按目录枚举上下文——枚举既会漏配维度，
// 也会在没有商品的期间把冲突判成"无冲突"。
func (s Scope) Overlaps(o Scope) bool {
	return overlapStr(s.Lines, o.Lines) &&
		overlapStr(s.Series, o.Series) &&
		overlapStr(s.Roles, o.Roles) &&
		overlapStr(s.Pools, o.Pools) &&
		overlapStr(s.Territories, o.Territories) &&
		overlapStr(s.Quarters, o.Quarters) &&
		overlapStr(s.Tiers, o.Tiers) &&
		overlapStr(s.Bands, o.Bands) &&
		overlapStr(s.OrderClasses, o.OrderClasses) &&
		overlapStr(s.Depts, o.Depts) &&
		overlapStr(s.KPIs, o.KPIs)
}

func overlapStr[T ~string](a, b []T) bool {
	if len(a) == 0 || len(b) == 0 {
		return true
	}
	set := make(map[T]struct{}, len(a))
	for _, x := range a {
		set[x] = struct{}{}
	}
	for _, y := range b {
		if _, ok := set[y]; ok {
			return true
		}
	}
	return false
}

func contains(v string, set map[string]struct{}) bool {
	_, ok := set[v]
	return ok
}

func lineSet(v []ProductLineID) map[string]struct{} {
	m := make(map[string]struct{}, len(v))
	for _, x := range v {
		m[string(x)] = struct{}{}
	}
	return m
}

func seriesSet(v []SeriesID) map[string]struct{} {
	m := make(map[string]struct{}, len(v))
	for _, x := range v {
		m[string(x)] = struct{}{}
	}
	return m
}

func roleSet(v []RoleID) map[string]struct{} {
	m := make(map[string]struct{}, len(v))
	for _, x := range v {
		m[string(x)] = struct{}{}
	}
	return m
}

func poolSet(v []PoolID) map[string]struct{} {
	m := make(map[string]struct{}, len(v))
	for _, x := range v {
		m[string(x)] = struct{}{}
	}
	return m
}

func terrSet(v []TerritoryID) map[string]struct{} {
	m := make(map[string]struct{}, len(v))
	for _, x := range v {
		m[string(x)] = struct{}{}
	}
	return m
}

func quarterSet(v []Quarter) map[string]struct{} {
	m := make(map[string]struct{}, len(v))
	for _, x := range v {
		m[string(x)] = struct{}{}
	}
	return m
}

func tierSet(v []TierID) map[string]struct{} {
	m := make(map[string]struct{}, len(v))
	for _, x := range v {
		m[string(x)] = struct{}{}
	}
	return m
}

func bandSet(v []BandID) map[string]struct{} {
	m := make(map[string]struct{}, len(v))
	for _, x := range v {
		m[string(x)] = struct{}{}
	}
	return m
}

func classSet(v []OrderClass) map[string]struct{} {
	m := make(map[string]struct{}, len(v))
	for _, x := range v {
		m[string(x)] = struct{}{}
	}
	return m
}

func deptSet(v []DeptID) map[string]struct{} {
	m := make(map[string]struct{}, len(v))
	for _, x := range v {
		m[string(x)] = struct{}{}
	}
	return m
}

func kpiSet(v []KPI) map[string]struct{} {
	m := make(map[string]struct{}, len(v))
	for _, x := range v {
		m[string(x)] = struct{}{}
	}
	return m
}

// Action 是规则的动作。同一结构承载四种阶段，具体字段按 Kind 解释。
type Action struct {
	Kind   ActionKind
	Target string // 池 ID 或角色 ID

	Method Method
	// BaseBPS 是基准费率（万分比）。倍率模型下它是整条因子链的起点：
	// 应得 = 基数 × BaseBPS × 档位倍数 × 时间/产品/区域倍数，全链只舍入一次。
	// 字段原名 BPS（语义为"最终费率"），M1 起语义收紧为基准，改名以杜绝误用。
	BaseBPS int64
	// MultiplierBPS 是纯倍数（10000 = 1.0），用于时间/产品/区域因子。
	MultiplierBPS int64
	Tiers         []Tier
	// Bands 是达成率档位表，只允许出现在 PhaseAttain 规则上。
	// 每条 attain 规则必须自带完整档位表（覆盖 [0,+∞)），否则改档会留洞。
	Bands []Band

	Cap   *money.Money
	Floor *money.Money

	// ShareMode 是直接佣金的分账方式，只允许配在 PhaseDirect。
	ShareMode ShareMode

	// DistMode 是池领取额的分发方式，只允许配在 PhaseDistribute。
	DistMode DistMode
	// AllocTo 是 DistMode=all 时的收款角色（如负责人）。
	AllocTo RoleID
	// RetainBPS 是领取额中留在池里的比例，与未领取部分一起进 CarryOver。
	RetainBPS   int64
	WeightTable map[RoleID]int64

	ChainDepth int     // 管理链深度
	DecayBPS   []int64 // 逐层衰减比例
}

type Rule struct {
	ID            RuleID
	Phase         Phase
	Scope         Scope
	Priority      int
	EffectiveFrom Period
	EffectiveTo   Period
	Action        Action
	Description   string
}

func (r Rule) Effective(p Period) bool {
	if r.EffectiveFrom != "" && p < r.EffectiveFrom {
		return false
	}
	if r.EffectiveTo != "" && p > r.EffectiveTo {
		return false
	}
	return true
}

type OverlayTarget string

const (
	OverlayAddRate OverlayTarget = "add_rate" // 叠加费率
	OverlayCap     OverlayTarget = "cap"      // 封顶
	OverlayExempt  OverlayTarget = "exempt"   // 豁免
)

// Overlay 是特殊规则。强制要求：显式声明覆盖对象、到期日、责任人、理由。
type Overlay struct {
	ID            RuleID
	Target        OverlayTarget
	Phase         Phase
	Scope         Scope
	Priority      int
	BPS           int64
	Cap           *money.Money
	Replaces      []RuleID
	ExpiresAt     Period
	Owner         PersonID
	Justification string
}

func (o Overlay) Effective(p Period) bool {
	return o.ExpiresAt == "" || p <= o.ExpiresAt
}

type RuleSet struct {
	Version  string
	Rules    []Rule
	Overlays []Overlay
}
