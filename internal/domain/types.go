// Package domain 只放不可变的业务实体与值对象，不依赖任何基础设施。
package domain

import (
	"sort"
	"time"

	"github.com/walry/yongjin/internal/money"
)

type (
	ProductLineID string
	SeriesID      string
	RoleID        string
	PoolID        string
	PersonID      string
	TerritoryID   string
	OrderID       string
	OrderLineID   string
	CustomerID    string
	TierID        string
	RuleID        string
	// Period 形如 "2026-10"，字典序即时间序。
	Period string

	// KPI 是计佣口径。合同额与收入额各自独立算一笔，不合并。
	KPI string
	// OrderClass 是订单分类，PS 分账公式的维度之一。
	OrderClass string
	// DeptID 是部门标识，用于"部门 A 独立规则"这类场景。
	DeptID string
	// BandID 是达成率档位标识。
	BandID string
	// RelationKind 是汇报/配对关系类型。
	RelationKind string
)

const (
	// RoleOwner 是订单负责人的固定角色。
	RoleOwner RoleID = "sales_owner"
	RolePre   RoleID = "pre_sales"
	RoleChan  RoleID = "channel"
)

const (
	KPIContract KPI = "contract" // 合同额口径
	KPIRevenue  KPI = "revenue"  // 实际收入额口径
)

const (
	// RelASPairSS 是 AD/SS 与 AS 的一对多配对关系（SS 侧）。
	RelSSPairAS RelationKind = "ss_as"
	// RelADPairAS 是 AD 与 AS 的一对多关系。
	RelADPairAS RelationKind = "ad_as"
)

// Product 是产品快照。产品线/系列可能随时间调整，因此以快照形式冻结在 Facts 中。
type Product struct {
	ID     string
	Line   ProductLineID
	Series SeriesID
}

// OrderLine 是订单行。合同额与实际收入额是两条独立口径，各自算一笔佣金。
type OrderLine struct {
	ID       OrderLineID
	Product  string
	Contract money.Money // 合同额
	Discount money.Money
	Return   money.Money
	Revenue  money.Money // 实际收入额（财务确认口径）
	Cost     money.Money
}

// BaseOf 返回指定口径的可计佣基数。合同口径扣折扣，收入口径只扣退货。
func (l OrderLine) BaseOf(k KPI) money.Money {
	if k == KPIRevenue {
		return l.Revenue - l.Return
	}
	return l.Contract - l.Discount - l.Return
}

// Base 是合同口径基数，等价于 BaseOf(KPIContract)。
func (l OrderLine) Base() money.Money { return l.BaseOf(KPIContract) }

func (l OrderLine) GrossProfit() money.Money { return l.Base() - l.Cost }

// Participant 是订单的协作人。负责人不放在这里，避免重复计算。
type Participant struct {
	Person PersonID
	Role   RoleID
	Weight int64 // 100 = 全额份额
}

type Order struct {
	ID        OrderID
	Customer  CustomerID
	Tier      TierID
	Territory TerritoryID
	Class     OrderClass
	// ClosedAt 是成单时间（业务时间，非录入时间），决定季度因子与池领取顺序。
	ClosedAt     time.Time
	Owner        PersonID
	Participants []Participant
	Lines        []OrderLine
}

// Persons 返回该订单全部获利角色：负责人 + 协作人。
func (o Order) Persons() []Participant {
	out := make([]Participant, 0, len(o.Participants)+1)
	out = append(out, Participant{Person: o.Owner, Role: RoleOwner, Weight: 100})
	out = append(out, o.Participants...)
	return out
}

type Person struct {
	ID          PersonID
	Name        string
	Manager     PersonID // 空表示顶层
	Dept        DeptID
	Territories []TerritoryID
}

type Territory struct {
	ID      TerritoryID
	Name    string
	Parent  TerritoryID
	Members []PersonID
}

// Target 是某人在某期的业绩目标额，两个口径各自独立。
type Target struct {
	Person   PersonID
	Period   Period
	Contract money.Money
	Revenue  money.Money
}

// Relation 是人员关系快照。必须随事实冻结：组织调整后重算历史，配对关系仍应是当时的。
type Relation struct {
	From PersonID
	To   PersonID
	Kind RelationKind
}

// PoolDef 是奖金池定义。Budget 属于事实（期初预算），发放条件属于规则。
type PoolDef struct {
	ID     PoolID
	Line   ProductLineID
	Role   RoleID // 空 = 产品线级池
	Period Period
	Budget money.Money
}

// Facts 是某一期次的事实快照。计算过程中只读，绝不回查实时库。
type Facts struct {
	AsOf        time.Time
	Period      Period
	Orders      []Order
	Products    map[string]Product
	Persons     map[PersonID]Person
	Territories map[TerritoryID]Territory

	// 以下为 M1 补齐的事实维度，供达成率、分账与池预算使用。
	Targets   []Target
	Relations []Relation
	Pools     []PoolDef
}

func (f Facts) PersonName(id PersonID) string {
	if p, ok := f.Persons[id]; ok && p.Name != "" {
		return p.Name
	}
	return string(id)
}

func (f Facts) ManagerOf(id PersonID) PersonID {
	if p, ok := f.Persons[id]; ok {
		return p.Manager
	}
	return ""
}

// TargetOf 返回某人在本期的目标额。缺失时返回零值，由调用方按"目标为 0"处理。
func (f Facts) TargetOf(id PersonID) Target {
	for _, t := range f.Targets {
		if t.Person == id && (t.Period == "" || t.Period == f.Period) {
			return t
		}
	}
	return Target{Person: id, Period: f.Period}
}

// SuperiorsOf 返回 id 的上级：关系里 From 是上级、To 是下属。
//
// 分账只认方向，不认"对称的对方"：AS 成单要找的是它的 SS（To=AS 那条的 From），
// 若用对称语义，SS 成单时会把它的上级当成"搭档"，一个 AS 会被拆两次。
// 返回按 ID 升序去重 —— 份额用最大余数法拆，顺序决定尾差给谁，必须确定。
func (f Facts) SuperiorsOf(id PersonID, kind RelationKind) []PersonID {
	return dedupPersons(f.Relations, id, kind, func(r Relation) (PersonID, PersonID, bool) {
		return r.From, r.To, r.To == id
	})
}

// SubordinatesOf 返回 id 的下属：关系里 From 是上级、To 是下属。
func (f Facts) SubordinatesOf(id PersonID, kind RelationKind) []PersonID {
	return dedupPersons(f.Relations, id, kind, func(r Relation) (PersonID, PersonID, bool) {
		return r.To, r.From, r.From == id
	})
}

// dedupPersons 按方向取对方，去重后按 ID 升序 —— 不能用 map 迭代顺序，
// 否则同一份事实换个录入次序就得到不同的分账尾差（I9）。
func dedupPersons(rels []Relation, id PersonID, kind RelationKind,
	pick func(Relation) (PersonID, PersonID, bool)) []PersonID {
	seen := map[PersonID]struct{}{}
	var out []PersonID
	for _, r := range rels {
		if r.Kind != kind {
			continue
		}
		other, self, ok := pick(r)
		if !ok || self != id || other == "" || other == id {
			continue
		}
		if _, dup := seen[other]; dup {
			continue
		}
		seen[other] = struct{}{}
		out = append(out, other)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// PeersOf 返回与 id 存在指定关系的对方集合，按输入顺序去重。
func (f Facts) PeersOf(id PersonID, kind RelationKind) []PersonID {
	seen := map[PersonID]struct{}{}
	var out []PersonID
	add := func(p PersonID) {
		if p == "" || p == id {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	for _, r := range f.Relations {
		if r.Kind != kind {
			continue
		}
		if r.From == id {
			add(r.To)
		}
		if r.To == id {
			add(r.From)
		}
	}
	return out
}
