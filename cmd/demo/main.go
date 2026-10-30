// demo 跑通端到端流程：发布体检 -> 计算 -> 入台账 -> 锁期 -> 规则变更 -> 重算 -> 差额调整。
package main

import (
	"fmt"
	"log"

	"github.com/walry/yongjin/internal/calc"
	"github.com/walry/yongjin/internal/demo"
	"github.com/walry/yongjin/internal/domain"
	"github.com/walry/yongjin/internal/ledger"
	"github.com/walry/yongjin/internal/money"
	"github.com/walry/yongjin/internal/rules"
)

func main() {
	facts := demo.Facts()
	v1 := demo.RulesV1()

	// 1. 发布闸门：编译规则集，输出矩阵、缺口、冲突。
	idx, err := rules.Compile(v1, calc.CatalogOf(facts))
	if err != nil {
		log.Fatalf("规则集不可发布: %v", err)
	}
	fmt.Println("=== 发布前体检：产品线 x 角色 矩阵 ===")
	for _, line := range idx.MatrixLines(calc.CatalogOf(facts)) {
		fmt.Println(" ", line)
	}
	if len(idx.Gaps) > 0 {
		fmt.Println("--- 缺口（会导致少发钱，必须确认）---")
		for _, g := range idx.Gaps {
			fmt.Println("  !", g)
		}
	}

	// 2. 计算 v1。
	res1, err := calc.Run(facts, v1, calc.Options{})
	if err != nil {
		log.Fatalf("计算失败: %v", err)
	}
	printResult("v1 计算结果", facts, res1)

	// 3. 入账并锁定期间。
	book := ledger.New()
	if _, err := book.Post(res1); err != nil {
		log.Fatalf("入账失败: %v", err)
	}
	book.Lock(res1.Period)
	fmt.Printf("\n已入账并锁定期间 %s，账面合计 %s\n", res1.Period, book.PeriodTotal(res1.Period))

	// 4. 业务改规则 -> 历史重算 -> 差额调整单。
	v2 := demo.RulesV2()
	res2, err := calc.Run(facts, v2, calc.Options{})
	if err != nil {
		log.Fatalf("v2 计算失败: %v", err)
	}
	fmt.Printf("\nv1 runID=%s  total=%s\n", res1.CalcRunID, res1.Total())
	fmt.Printf("v2 runID=%s  total=%s  (id 必须变化，否则幂等会吞掉重算)\n", res2.CalcRunID, res2.Total())

	adjs, err := book.Recalculate(res1, res2)
	if err != nil {
		log.Fatalf("重算调整失败: %v", err)
	}
	fmt.Printf("\n=== 差额调整单（期间已锁定，只能冲正不可改写）===\n")
	for _, e := range adjs {
		fmt.Printf("  %s %-6s %s → 调整 %s (adjustOf=%s)\n",
			e.ID, e.Person, e.Kind, e.Amount, e.AdjustOf)
	}
	// Cause 由四要素推导，不是调用方自报：事后不问人也能复核「为什么重算」。
	if len(adjs) > 0 {
		fmt.Printf("  原因 %q（由 CalcRunID 四要素推出：rules/facts/engine 哪一维变了）\n", adjs[0].Cause)
	}
	fmt.Printf("调整后账面合计 %s（应与 v2 合计 %s 一致）\n",
		book.PeriodTotal(res1.Period), res2.Total())

	// 5. 抽一条 trace，演示可解释性。
	if len(res1.Trace) > 0 {
		t := res1.Trace[0]
		fmt.Printf("\n=== 追溯样例 ===\n  订单 %s 行 %s 阶段 %s 规则 %s 口径 %s 基数 %s × %dbps × %dbps = %s %s\n",
			t.OrderID, t.LineID, t.Phase, t.RuleID, t.KPI, t.Base, t.RateBPS, t.CoeffBPS, t.Amount, t.Note)
	}

	// 6. 倍率模型：同一份事实 + 一条档位表，看达成率如何改金额。
	// 注意 v1 没有档位表（倍率恒 1.0），这里换成带宽档位表的版本做对比。
	res3, err := calc.Run(facts, demo.RulesV1Bands(), calc.Options{})
	if err != nil {
		log.Fatalf("带档位计算失败: %v", err)
	}
	fmt.Printf("\n=== 倍率模型：达成率定倍数 ===\n")
	fmt.Printf("  无档位合计 %s -> 有档位合计 %s\n", res1.Total(), res3.Total())
	fmt.Println("  -- 达成率（阶段 0 先跑完，才轮到逐单算钱）--")
	for _, a := range res3.Attainments {
		fmt.Printf("    %-4s %-9s 实际 %-12s 目标 %-12s 达成 %s\n",
			a.Person, a.KPI, a.Actual, a.Target, pct(a.RateBPS))
	}
	fmt.Println("  -- 明细链路（基数 × 基准费率 × 档位倍数）--")
	for _, d := range res3.Directs {
		if d.OrderID != "O-1001" || d.Person != "P1" {
			continue
		}
		fmt.Printf("    %s/%s %-9s 基数 %-12s × %dbps × %dbps(%s) = %s\n",
			d.OrderID, d.LineID, d.KPI, d.Base, d.RateBPS, d.CoeffBPS, d.BandID, d.Amount)
	}

	printFactors(facts, res3)
	printSplit(facts)
	printPools()
}

// pct 把"比率的万分比"渲染成百分数。整数运算，不走 float64。
func pct(bps int64) string {
	return fmt.Sprintf("%d.%02d%%", bps/100, bps%100)
}

// printFactors 跑一遍带因子的规则集，对比"只有档位"与"档位 + 因子"的差别。
// 因子按 Kind 各取一条再相乘，缺的 Kind 视作 1.0 —— 而不是把整条链乘成 0。
func printFactors(facts domain.Facts, res3 calc.Result) {
	res4, err := calc.Run(facts, demo.RulesV1Full(), calc.Options{})
	if err != nil {
		log.Fatalf("带因子计算失败: %v", err)
	}
	fmt.Printf("\n=== 因子链：档位之后再乘时间/产品/区域 ===\n")
	fmt.Printf("  只有档位 %s -> 档位+因子 %s\n", res3.Total(), res4.Total())
	fmt.Println("  -- 明细链路（每段因子都留痕，全链只舍入一次）--")
	for _, d := range res4.Directs {
		if d.OrderID != "O-1001" || d.LineID != "L1" || d.Person != "P1" {
			continue
		}
		fmt.Printf("    %s/%s %-9s 基数 %s × %dbps × %dbps(%s)%s = %s\n",
			d.OrderID, d.LineID, d.KPI, d.Base, d.RateBPS, d.CoeffBPS, d.BandID,
			factorNote(d.Factors), d.Amount)
	}
	// 留痕自证：金额必须能被自己的 Trace 复算出来，否则留痕只是装饰（I8）。
	bad := 0
	for _, t := range res4.Trace {
		if t.Replay() != t.Amount {
			bad++
		}
	}
	fmt.Printf("  -- 留痕自证：%d 条 Trace，复算不一致 %d 条\n", len(res4.Trace), bad)
}

// factorNote 把因子链渲染成 " × 11000bps(R-TERR-EAST)" 这样的片段。
func factorNote(hits []calc.FactorHit) string {
	if len(hits) == 0 {
		return ""
	}
	s := ""
	for _, h := range hits {
		s += fmt.Sprintf(" × %dbps(%s)", h.BPS, h.RuleID)
	}
	return s
}

func printResult(title string, facts domain.Facts, res calc.Result) {
	fmt.Printf("\n=== %s ===\n", title)
	fmt.Printf("calcRunID=%s  period=%s  ruleSet=%s\n", res.CalcRunID, res.Period, res.RuleSetVersion)

	fmt.Println("-- 奖金池（budget == distributed + carryover）--")
	for _, p := range res.Pools {
		fmt.Printf("  %-8s 预算 %-14s 领取 %-14s 留存 %-12s 分出 %-14s 结转 %-12s\n",
			p.Pool, p.Budget, p.Claimed, p.Retained, p.Distributed, p.CarryOver)
	}

	if len(res.Attainments) > 0 {
		fmt.Println("-- 达成率（合同/收入各一条，各自定档）--")
		for _, a := range res.Attainments {
			fmt.Printf("  %-4s %-9s 实际 %-12s 目标 %-12s 达成 %s\n",
				a.Person, a.KPI, a.Actual, a.Target, pct(a.RateBPS))
		}
	}

	fmt.Println("-- 人员结算 --")
	for _, p := range res.Persons {
		fmt.Printf("  %-4s %-6s 直接 %-12s（合同 %-10s / 收入 %-10s）分账 %-12s 池 %-12s 管理 %-12s 合计 %-14s\n",
			p.Person, facts.PersonName(p.Person), p.Direct,
			p.DirectContract, p.DirectRevenue, p.Split, p.Pool, p.Manager, p.Total)
	}
	fmt.Printf("  合计 %s\n", res.Total())

	if len(res.Warnings) > 0 {
		fmt.Println("-- 告警 --")
		for _, w := range res.Warnings {
			fmt.Println("  !", w)
		}
	}
}

// printSplit 演示分账：AS 成单与配对 SS 各一半，一次零和转移。
// 三件事要同时看见：合计不变（零和）、管理奖基数仍是分账前全额（I12）、
// 每组分出不超过分账前金额（I6）。
func printSplit(facts domain.Facts) {
	res, err := calc.Run(facts, demo.Shares(), calc.Options{})
	if err != nil {
		log.Fatalf("带分账计算失败: %v", err)
	}
	plain, err := calc.Run(facts, demo.RulesV1Full(), calc.Options{})
	if err != nil {
		log.Fatalf("对照计算失败: %v", err)
	}

	fmt.Printf("\n=== 分账：AS 成单与配对 SS 各一半 ===\n")
	fmt.Printf("  不分账合计 %s -> 分账后合计 %s（必须相等：零和转移）\n", plain.Total(), res.Total())
	fmt.Println("  -- 明细（只转一次，不级联）--")
	for _, s := range res.Splits {
		if s.OrderID != "O-1001" || s.LineID != "L1" {
			continue
		}
		fmt.Printf("    %s/%s %-9s %s -> %s %-10s（%s）\n",
			s.OrderID, s.LineID, s.KPI, s.From, s.To, s.Amount, s.Mode)
	}

	fmt.Println("  -- 分账前后：直接佣金是「谁促成的」，分账净额是「钱落在谁账上」--")
	for _, p := range res.Persons {
		var before money.Money
		for _, q := range plain.Persons {
			if q.Person == p.Person {
				before = q.Total
			}
		}
		fmt.Printf("    %-4s %-6s 直接 %-12s 分账净额 %-12s 合计 %-12s（分账前 %s）\n",
			p.Person, facts.PersonName(p.Person), p.Direct, p.Split, p.Total, before)
	}

	// I6：每一组分出的部分加上成单人自留，必须恰好等于分账前金额。
	bad, groups := 0, 0
	for _, d := range res.Directs {
		var out money.Money
		n := 0
		for _, s := range res.Splits {
			if s.OrderID == d.OrderID && s.LineID == d.LineID && s.KPI == d.KPI && s.From == d.Person {
				out += s.Amount
				n++
			}
		}
		if n == 0 {
			continue // 未配分账方式的规则，全额自留
		}
		groups++
		if out > d.Amount {
			bad++
		}
	}
	fmt.Printf("  -- 守恒自证：%d 组分账，超出分账前金额 %d 组；分录毛额合计 %s（净值恒为 0）\n",
		groups, bad, sumSplit(res.Splits))
}

// sumSplit 求分账分录之和。它不是净额（净额要区分 From/To 正负），只用于对账展示。
func sumSplit(splits []calc.SplitLine) money.Money {
	var s money.Money
	for _, x := range splits {
		s += x.Amount
	}
	return s
}

// printPools 演示池的两层：领取受预算约束、按成单全序 FIFO；分发决定这笔钱给谁。
// 把 POOL-A 的预算调小，才能看见「前面几单领满、后面的单领不到」——
// 预算充裕时这个语义被掩盖，看起来和「按费率注资」没区别。
func printPools() {
	facts := demo.Facts()
	for i := range facts.Pools {
		if facts.Pools[i].ID == "POOL-A" {
			facts.Pools[i].Budget = money.FromYuan(30000, 0)
		}
	}
	res, err := calc.Run(facts, demo.RulesV1Full(), calc.Options{})
	if err != nil {
		log.Fatalf("池计算失败: %v", err)
	}

	fmt.Printf("\n=== 奖金池：预算有限，按成单顺序领取 ===\n")
	fmt.Println("  -- 领取（阶段 5）：领取额 ≤ 剩余额，全序由 ClosedAt 决定 --")
	for _, c := range res.Claims {
		state := ""
		if c.Exhausted {
			state = "，耗尽"
		}
		fmt.Printf("    %s/%s %-8s 剩余 %-12s 领  %-12s（余 %s%s）\n",
			c.OrderID, c.LineID, c.Pool, c.Remaining, c.Claimed, c.Remaining-c.Claimed, state)
	}
	fmt.Println("  -- 分发（阶段 6）：领到的钱按规则分给这一单的参与人 --")
	for _, d := range res.Distributions {
		fmt.Printf("    %s/%s %-8s %-4s %-9s 权重 %-4d 得 %s\n",
			d.OrderID, d.LineID, d.Pool, d.Person, d.Role, d.Weight, d.Amount)
	}
	fmt.Println("  -- 对账（I2）：budget == distributed + carryover --")
	for _, p := range res.Pools {
		fmt.Printf("    %-8s 预算 %-12s 领取 %-12s 留存 %-10s 分出 %-12s 结转 %s\n",
			p.Pool, p.Budget, p.Claimed, p.Retained, p.Distributed, p.CarryOver)
	}
}
