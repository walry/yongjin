// Package demo 提供一套示例事实与规则集，供 cmd/demo 与测试共用。
package demo

import (
	"time"

	"github.com/walry/yongjin/internal/domain"
	"github.com/walry/yongjin/internal/money"
)

func ptr(m money.Money) *money.Money { return &m }

// Facts 构造两笔大额订单：产品线 A（系列 A1/A2）与产品线 B，各带独立奖金池。
func Facts() domain.Facts {
	f := domain.Facts{
		AsOf:   time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		Period: "2026-10",
		Products: map[string]domain.Product{
			"SKU-A1": {ID: "SKU-A1", Line: "A", Series: "A1"},
			"SKU-A2": {ID: "SKU-A2", Line: "A", Series: "A2"},
			"SKU-B1": {ID: "SKU-B1", Line: "B", Series: "B1"},
		},
		Persons: map[domain.PersonID]domain.Person{
			"P1": {ID: "P1", Name: "张三", Manager: "P3", Dept: "SALES", Territories: []domain.TerritoryID{"EAST"}},
			"P2": {ID: "P2", Name: "李四", Manager: "P3", Dept: "SALES", Territories: []domain.TerritoryID{"EAST"}},
			"P3": {ID: "P3", Name: "王五", Manager: "P5", Dept: "SALES", Territories: []domain.TerritoryID{"EAST"}},
			"P4": {ID: "P4", Name: "赵六", Manager: "P3", Dept: "SALES", Territories: []domain.TerritoryID{"SOUTH"}},
			"P5": {ID: "P5", Name: "陈七", Manager: "", Dept: "SALES"},
		},
		Territories: map[domain.TerritoryID]domain.Territory{
			"EAST":  {ID: "EAST", Name: "华东", Members: []domain.PersonID{"P1", "P2", "P3"}},
			"SOUTH": {ID: "SOUTH", Name: "华南", Members: []domain.PersonID{"P4"}},
		},
		Orders: []domain.Order{
			{
				ID: "O-1001", Customer: "C1", Tier: "TIER-STD", Territory: "EAST", Class: "NEW",
				ClosedAt: time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC),
				Owner:    "P1",
				Participants: []domain.Participant{
					{Person: "P2", Role: domain.RolePre, Weight: 100},
					{Person: "P4", Role: domain.RoleChan, Weight: 100},
				},
				Lines: []domain.OrderLine{
					{
						ID: "L1", Product: "SKU-A1",
						Contract: money.FromYuan(800000, 0),
						Revenue:  money.FromYuan(720000, 0),
						Cost:     money.FromYuan(600000, 0),
					},
					{
						ID: "L2", Product: "SKU-B1",
						Contract: money.FromYuan(400000, 0),
						Revenue:  money.FromYuan(380000, 0),
						Cost:     money.FromYuan(320000, 0),
					},
				},
			},
			{
				ID: "O-1002", Customer: "C2", Tier: "TIER-VIP", Territory: "EAST", Class: "RENEW",
				ClosedAt: time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC),
				Owner:    "P1",
				Participants: []domain.Participant{
					{Person: "P2", Role: domain.RolePre, Weight: 50},
				},
				Lines: []domain.OrderLine{
					{
						ID: "L1", Product: "SKU-A2",
						Contract: money.FromYuan(600000, 0),
						Discount: money.FromYuan(20000, 0),
						Revenue:  money.FromYuan(520000, 0),
						Cost:     money.FromYuan(420000, 0),
					},
				},
			},
		},
		Targets: []domain.Target{
			{Person: "P1", Period: "2026-10", Contract: money.FromYuan(1500000, 0), Revenue: money.FromYuan(1200000, 0)},
			{Person: "P2", Period: "2026-10", Contract: money.FromYuan(1500000, 0), Revenue: money.FromYuan(1300000, 0)},
			{Person: "P4", Period: "2026-10", Contract: money.FromYuan(1200000, 0), Revenue: money.FromYuan(1000000, 0)},
		},
		Relations: []domain.Relation{
			{From: "P1", To: "P2", Kind: domain.RelSSPairAS},
			{From: "P1", To: "P4", Kind: domain.RelSSPairAS},
			{From: "P3", To: "P1", Kind: domain.RelADPairAS},
		},
		Pools: []domain.PoolDef{
			{ID: "POOL-A", Line: "A", Period: "2026-10", Budget: money.FromYuan(60000, 0)},
			{ID: "POOL-B", Line: "B", Period: "2026-10", Budget: money.FromYuan(20000, 0)},
		},
	}
	return f
}

// RulesV1 是初始规则集。
func RulesV1() domain.RuleSet {
	return domain.RuleSet{
		Version: "v1",
		Rules: []domain.Rule{
			// 池注入：产品线级兜底 + 系列级更具体
			{
				ID: "R-FUND-A", Phase: domain.PhaseFund, Scope: domain.Scope{Lines: []domain.ProductLineID{"A"}},
				Action:      domain.Action{Kind: domain.ActFundPool, Target: "POOL-A", Method: domain.MethodPercent, BaseBPS: 300},
				Description: "产品线A 3% 入池",
			},
			{
				ID: "R-FUND-A1", Phase: domain.PhaseFund,
				Scope:       domain.Scope{Lines: []domain.ProductLineID{"A"}, Series: []domain.SeriesID{"A1"}},
				Action:      domain.Action{Kind: domain.ActFundPool, Target: "POOL-A", Method: domain.MethodPercent, BaseBPS: 400},
				Description: "系列A1 提高至 4% 入池",
			},
			{
				ID: "R-FUND-B", Phase: domain.PhaseFund, Scope: domain.Scope{Lines: []domain.ProductLineID{"B"}},
				Action:      domain.Action{Kind: domain.ActFundPool, Target: "POOL-B", Method: domain.MethodPercent, BaseBPS: 200},
				Description: "产品线B 2% 入池",
			},

			// 直接佣金：产品线 x 角色
			{
				ID: "R-DIR-A-OWNER", Phase: domain.PhaseDirect,
				Scope:  domain.Scope{Lines: []domain.ProductLineID{"A"}, Roles: []domain.RoleID{domain.RoleOwner}},
				Action: domain.Action{Kind: domain.ActPayRole, Target: "sales_owner", Method: domain.MethodPercent, BaseBPS: 250},
			},
			{
				ID: "R-DIR-A-OWNER-VIP", Phase: domain.PhaseDirect,
				Scope:  domain.Scope{Lines: []domain.ProductLineID{"A"}, Roles: []domain.RoleID{domain.RoleOwner}, Tiers: []domain.TierID{"TIER-VIP"}},
				Action: domain.Action{Kind: domain.ActPayRole, Target: "sales_owner", Method: domain.MethodPercent, BaseBPS: 300},
			},
			{
				ID: "R-DIR-A-PRE", Phase: domain.PhaseDirect,
				Scope:  domain.Scope{Lines: []domain.ProductLineID{"A"}, Roles: []domain.RoleID{domain.RolePre}},
				Action: domain.Action{Kind: domain.ActPayRole, Target: "pre_sales", Method: domain.MethodPercent, BaseBPS: 50},
			},
			{
				ID: "R-DIR-A-CHAN", Phase: domain.PhaseDirect,
				Scope:  domain.Scope{Lines: []domain.ProductLineID{"A"}, Roles: []domain.RoleID{domain.RoleChan}},
				Action: domain.Action{Kind: domain.ActPayRole, Target: "channel", Method: domain.MethodPercent, BaseBPS: 80},
			},
			{
				ID: "R-DIR-B-OWNER", Phase: domain.PhaseDirect,
				Scope:  domain.Scope{Lines: []domain.ProductLineID{"B"}, Roles: []domain.RoleID{domain.RoleOwner}},
				Action: domain.Action{Kind: domain.ActPayRole, Target: "sales_owner", Method: domain.MethodPercent, BaseBPS: 180},
			},
			{
				ID: "R-DIR-B-PRE", Phase: domain.PhaseDirect,
				Scope:  domain.Scope{Lines: []domain.ProductLineID{"B"}, Roles: []domain.RoleID{domain.RolePre}},
				Action: domain.Action{Kind: domain.ActPayRole, Target: "pre_sales", Method: domain.MethodPercent, BaseBPS: 40},
			},

			// 池分发：按角色权重表，A 池留存 10% 结转
			{
				ID: "R-DIST-A", Phase: domain.PhaseDistribute, Scope: domain.Scope{Pools: []domain.PoolID{"POOL-A"}},
				Action: domain.Action{
					Kind: domain.ActDistributePool, Target: "POOL-A", DistMode: domain.DistWeight,
					RetainBPS: 1000,
					WeightTable: map[domain.RoleID]int64{
						domain.RoleOwner: 60, domain.RolePre: 30, domain.RoleChan: 10,
					},
				},
			},
			{
				ID: "R-DIST-B", Phase: domain.PhaseDistribute, Scope: domain.Scope{Pools: []domain.PoolID{"POOL-B"}},
				Action: domain.Action{Kind: domain.ActDistributePool, Target: "POOL-B", DistMode: domain.DistEqual},
			},

			// 管理奖金：上溯 3 层，50%/25%/10%
			{
				ID: "R-MGR", Phase: domain.PhaseManager,
				Action: domain.Action{Kind: domain.ActManagerBonus, ChainDepth: 3, DecayBPS: []int64{5000, 2500, 1000}},
			},
		},
		Overlays: []domain.Overlay{
			{
				ID: "OV-A1-VIP", Target: domain.OverlayAddRate, Phase: domain.PhaseDirect,
				Scope:         domain.Scope{Lines: []domain.ProductLineID{"A"}, Series: []domain.SeriesID{"A1"}, Roles: []domain.RoleID{domain.RoleOwner}},
				BPS:           30,
				ExpiresAt:     "2026-12",
				Owner:         "P3",
				Justification: "华东大客户首单加成 / 工单 T-2026-0912",
			},
		},
	}
}

// Bands 是达成率档位表。CoeffBPS 是倍数而不是费率：
// 9000 = 基准的 0.9 倍。区间左闭右开，最高档 ToBPS = 0 表示无上界。
func Bands() []domain.Band {
	return []domain.Band{
		{ID: "LOW", FromBPS: 0, ToBPS: 8000, CoeffBPS: 9000},
		{ID: "NORMAL", FromBPS: 8000, ToBPS: 10000, CoeffBPS: 10000},
		{ID: "HIGH", FromBPS: 10000, ToBPS: 13000, CoeffBPS: 12000},
		{ID: "TOP", FromBPS: 13000, ToBPS: 0, CoeffBPS: 15000},
	}
}

// RulesV1Bands 在 RulesV1 上加一条档位表，用来演示倍率模型。
//
// RulesV1 刻意不带档位：它是"无档位时倍率恒 1.0、结果与旧行为逐条一致"的回归基线。
func RulesV1Bands() domain.RuleSet {
	rs := RulesV1()
	rs.Version = "v1-bands"
	rs.Rules = append(rs.Rules, domain.Rule{
		ID:          "R-ATTAIN-STD",
		Phase:       domain.PhaseAttain,
		Action:      domain.Action{Kind: domain.ActCredit, Bands: Bands()},
		Description: "标准档位：<80% 0.9×，80~100% 1.0×，100~130% 1.2×，>=130% 1.5×",
	})
	return rs
}

// Factors 是三段乘数因子：时间（按成单季度）、产品（产品线/系列）、区域。
// 三者都只配在 PhaseFactor 上，与基准费率、档位倍数一起进同一条 ApplyChain，
// 全链只舍入一次 —— 分步乘会在 800 万级订单上漂出几分钱（设计 §6.1）。
//
// 系列级产品因子同时写明产品线（A1 → {Lines:A, Series:A1}）：特异性 8+4=12 才压得过
// 产品线级那条 8；只写系列的规则会被静默压过，Compile 直接拒收。
func Factors() []domain.Rule {
	return []domain.Rule{
		{
			ID: "R-TIME-Q1", Phase: domain.PhaseFactor,
			Scope:       domain.Scope{Quarters: []domain.Quarter{domain.Q1}},
			Action:      domain.Action{Kind: domain.ActTimeFactor, MultiplierBPS: 10000},
			Description: "Q1 无加成 1.0 倍",
		},
		{
			ID: "R-TIME-Q2", Phase: domain.PhaseFactor,
			Scope:       domain.Scope{Quarters: []domain.Quarter{domain.Q2}},
			Action:      domain.Action{Kind: domain.ActTimeFactor, MultiplierBPS: 11000},
			Description: "Q2 1.1 倍",
		},
		{
			ID: "R-TIME-Q3", Phase: domain.PhaseFactor,
			Scope:       domain.Scope{Quarters: []domain.Quarter{domain.Q3}},
			Action:      domain.Action{Kind: domain.ActTimeFactor, MultiplierBPS: 12000},
			Description: "Q3 1.2 倍",
		},
		{
			ID: "R-TIME-Q4", Phase: domain.PhaseFactor,
			Scope:       domain.Scope{Quarters: []domain.Quarter{domain.Q4}},
			Action:      domain.Action{Kind: domain.ActTimeFactor, MultiplierBPS: 13000},
			Description: "Q4 旺季 1.3 倍",
		},
		{
			ID: "R-PROD-A", Phase: domain.PhaseFactor,
			Scope:       domain.Scope{Lines: []domain.ProductLineID{"A"}},
			Action:      domain.Action{Kind: domain.ActProductFactor, MultiplierBPS: 11000},
			Description: "产品线A 1.1 倍",
		},
		{
			ID: "R-PROD-A1", Phase: domain.PhaseFactor,
			Scope:       domain.Scope{Lines: []domain.ProductLineID{"A"}, Series: []domain.SeriesID{"A1"}},
			Action:      domain.Action{Kind: domain.ActProductFactor, MultiplierBPS: 15000},
			Description: "系列A1 1.5 倍（覆盖产品线）",
		},
		{
			ID: "R-PROD-B", Phase: domain.PhaseFactor,
			Scope:       domain.Scope{Lines: []domain.ProductLineID{"B"}},
			Action:      domain.Action{Kind: domain.ActProductFactor, MultiplierBPS: 10000},
			Description: "产品线B 无加成",
		},
		{
			ID: "R-TERR-EAST", Phase: domain.PhaseFactor,
			Scope:       domain.Scope{Territories: []domain.TerritoryID{"EAST"}},
			Action:      domain.Action{Kind: domain.ActTerritoryFactor, MultiplierBPS: 11000},
			Description: "华东 1.1 倍（区域只是乘数，不参与选费率）",
		},
	}
}

// RulesV1Full 是倍率模型的完整语义（M2 档位 + M3 因子），用于演示
// "档位给倍数、因子再乘一次"的全链。
func RulesV1Full() domain.RuleSet {
	rs := RulesV1Bands()
	rs.Version = "v1-full"
	rs.Rules = append(rs.Rules, Factors()...)
	return rs
}

// RulesV2 是变更后的规则集：产品线 A 负责人佣金 250 -> 280 bps，用于演示历史重算与差额调整。
func RulesV2() domain.RuleSet {
	rs := RulesV1()
	rs.Version = "v2"
	for i := range rs.Rules {
		if rs.Rules[i].ID == "R-DIR-A-OWNER" {
			rs.Rules[i].Action.BaseBPS = 280
			rs.Rules[i].Description = "产品线A 负责人 2.8%（v2 调整）"
		}
	}
	return rs
}

// Shares 把"渠道角色成单要与配对 SS 各分一半"挂到直接佣金规则上。
//
// 分账是零和转移：只改钱落在谁账上，不改合计，也不改管理奖基数（基数读分账前全额）。
// 这里只给 channel（AS 侧）配 pair1to1 —— 同一阶段的其他规则保持全额自留，
// 分账方式是按规则配的，不是全局开关。
func Shares() domain.RuleSet {
	rs := RulesV1Full()
	rs.Version = "v1-split"
	for i := range rs.Rules {
		if rs.Rules[i].ID == "R-DIR-A-CHAN" {
			rs.Rules[i].Action.ShareMode = domain.SharePair1to1
		}
	}
	return rs
}
