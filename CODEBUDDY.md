# CODEBUDDY.md

This file provides guidance to CodeBuddy Code when working with code in this repository.

## Commands

```bash
go build ./...                # 编译
go test ./...                 # 全部测试
go test ./internal/money -v   # 单个包
go test ./internal/calc -run TestRunEndToEnd -v   # 单个用例
go test ./internal/golden -v                     # 金标回归集（发布闸门）
go test ./internal/golden -update                # 重定金标基准（先解释清楚为什么变）
go vet ./...
gofmt -l .                    # 格式检查（应为空）
go run ./cmd/demo             # 端到端演示
```

无外部依赖，仅标准库。修改后必须保证 `go build ./... && go test ./... && gofmt -l .` 全绿。

## Architecture

佣金计算是**纯函数内核 + 不可变事实快照 + 追加式台账**三段式，不引入规则引擎或 DSL。

数据流：`domain.Facts` + `domain.RuleSet` → `rules.Compile` → `calc.Run` → `calc.Result` → `ledger.Book`

| 包 | 职责 | 关键约束 |
|---|---|---|
| `internal/money` | 定点金额、万分比、最大余数法拆分 | 只有 `int64` 分；`Allocate` 保证 `Sum(out) == total` |
| `internal/domain` | 事实实体 + 规则/覆盖模型 | 零依赖，不得导入其他内部包（`money` 除外） |
| `internal/rules` | 编译成索引：特异性裁决、冲突拦截、缺口报告 | 冲突返回 error 而非告警 |
| `internal/calc` | 归集 + 八段流水线（见下）；池两段在 `pool.go`，投影在 `project.go`，按人求和在 `aggregate.go` | 不读时钟、不碰 DB、无随机 |
| `internal/golden` | 金标回归集：冻结语料 + 三层期望 + 文本表比对 | 语料按 Tag 声明压什么语义并自检；期望按人/池/单三层冻结，不只冻合计 |
| `internal/ledger` | 期间状态、入账、历史重算差额调整 | 锁定期间拒绝改写；未锁定 `Post` 覆盖草稿；`Recalculate` 只准入 `Locked`，只追加 adjustment 且带 `Cause` |
| `internal/demo` | 示例事实与 v1/v2 规则集 | 被 `cmd/demo` 与测试共用 |

### 事实层（M1 起）

`OrderLine` 有两条独立口径：`Contract`（合同额）与 `Revenue`（实际收入额），用 `BaseOf(KPI)` 取基数，`Base()` 等价于合同口径。`Facts` 另带 `Targets`（目标额）、`Relations`（SS/AS 配对，必须快照冻结）、`Pools`（池预算）三条切片，以及订单上的 `ClosedAt`（业务时间）与 `Class`（订单分类）。

规则动作里 `Action.BaseBPS` 是**基准费率**，`Action.MultiplierBPS` 是**倍数**；档位给出的也是倍数，全链只舍入一次（见 `docs/设计-佣金与奖金分成.md` §4.3.1）。

### 计算流程（`internal/calc/run.go` + `attain.go`）

**两趟，不能合并**：阶段 0 先整期跑完（`attainmentsOf`，每人每口径一条 `Attainment`），再逐单算钱——单笔佣金依赖档位倍数，档位倍数依赖「这个人本期成了多少单」。

第一趟之后，一个订单行依次走：`PhaseFactor`（季度 × 产品 × 区域 → `FactorHit`，**只出倍数不出钱**）、`PhaseDirect`（产品线 × 角色 × 档位 × 因子 → 人，**合同/收入各一条 `DirectLine`**），订单全部处理完后是 `PhaseSplit`（按 `Action.ShareMode` 与关系图做一次零和转移 → `SplitLine`）、`PhaseFund` + `PhaseDistribute`（同一个 FIFO 遍历产出 `PoolClaim` 与 `Distribution` 两层，见 `calc/pool.go`）与 `PhaseManager`（组织树上溯，合同/收入分别上溯），最后按人聚合。

因子段的三类 `Kind`（`time_factor` / `product_factor` / `territory_factor`）各自成裁决域（`arbitrationKey` 拼 `Phase/Kind`）：同 `Kind` 至多一条，不同 `Kind` 相乘，缺配按 1.0。它们的倍数**必须**与 `BaseBPS`、`BandBPS` 拼成同一条 `money.ApplyChain`，不能先落金额再逐次乘。

业绩归属与佣金分账是两张图：`Attainment.Actual` 只由事实与订单参与者决定，`SplitLine` / `Distribution` 一律不得回写达成率。

分账只读 `DirectLine`、从不读 `SplitLine`（不级联），`DirectLine.Amount` 永远是**分账前**全额（管理奖金基数读它，I12）；分账净额单列在 `PersonTotal.Split`，`Σ Split == 0`。

### 规则裁决

唯一顺序，运行时与编译期共用 `rules.better`：**Specificity（作用域非空维度加权）> Priority > EffectiveFrom 更晚 > ID 字典序**。若两条规则 specificity 与 priority 相同且作用域重叠、生效期相交，`Compile` 直接返回错误——不允许靠数组顺序裁决。

裁决域粒度见 `arbitrationKey`：普通阶段按 `Phase` 成域，`PhaseFactor` 按 `Phase/Kind` 成域（时间 / 产品 / 区域各自至多一条，跨 `Kind` 相乘），`PhaseFund` 按目标池成域（`fund/<池ID>`，一行可同时喂产品线池与负责人池，取规则用 `Index.All` 而非 `Best`）。

### 必须保持的不变式

1. 金额一律 `money.Money`，禁止 float64。
2. 每个池：`Budget == Distributed + CarryOver`，且 `CarryOver := Budget − Distributed`（`Run` 内有断言，违反即返回 error）。`Retained`（领到但分不出去）是 `CarryOver` 的子集，不是并列的一份钱。
3. `PersonTotal.Total` 之和 == 直接佣金（分账前 `DirectLine.Amount`）+ 已分出池金额 + 管理奖金（注意是 `Distributed`，不是 `Budget`）。`Σ PersonTotal.Split == 0`，所以分账不出现在这个式子里。
4. `CalcRunID = hash(period, factsDigest, ruleSetVersion, engineVersion)`，四者任一变化都必须改变 ID，否则幂等会吞掉重算。`factsDigest` 走 `calc/digest.go` 的 `canonical`：摘要前排序所有切片，否则换个录入顺序就会凭空多出一次"重算"。
5. 结果与事实切片的录入顺序无关：`Directs` 在 `Run` 内按 `(OrderID, LineID, Person, Role, KPI)` 排序，`TestRunIgnoresSliceOrder` 逐条比对。
6. `ledger.Recalculate` 必须先断言 `Σdelta == Σnew − Σold` 才入账。
7. 同一 `(OrderID, LineID, Person, KPI)` 至多一条 `DirectLine`：两个口径不得合并，`Run` 内有断言。
8. 每条 `PhaseAttain` 规则的档位表必须覆盖 `[0,+∞)`、不重叠、倍数单调不减——`Compile` 直接 error，不允许靠运行时回落。
9. 倍率全链只舍入一次（`BaseBPS` × `BandBPS` × Π 因子 `BPS` 同进一条 `ApplyChain`）；`Trace.Replay()` 必须逐分复算回 `Amount`，`TestTraceReplaysAmount` 守住。
10. 分账守恒且零和：`Σ SplitLine + 自留 == 分账前金额`，`Σ PersonTotal.Split == 0`，每条 `SplitLine.From` 必须是某条 `DirectLine` 的受益人（`assertSplits`）。
11. 池领取按行去重：同一 `(OrderID, LineID, Pool)` 至多一条 `PoolClaim`（否则总量随参与人数翻倍，且是静默翻倍）。领取顺序是 `(ClosedAt, OrderID, LineID)` 全序 FIFO，与阶段 1~4 共用同一个序。
12. 池封顶不进倍率链：`Trace` 记"应领"，截断差额落 `OverlayDelta`，`Trace.Replay()` 必须逐分复算回实领（`TestPoolClaimTraceReplays`）。`ApplyChain` 只乘不截断。
13. 管理奖基数恒为分账前 `DirectLine.Amount`，且 `ManagerBonus` 必须带 `OrderID/LineID` 才能逐条对上账（`TestManagerBaseIsPreSplit`）；池分成不进管理奖上溯（`TestManagerBonusIgnoresPools`）。
14. 投影不改 `CalcRunID`，也不得另写求和：`ProjectByPerson` 原样保留本人的结算行，`ProjectByOrder` 把裁剪后的明细喂回 `aggregatePersonTotals`。全部订单的投影合计必须等于整期合计（`TestProjectByOrderReaggregates`）。
15. 台账只追加：`Post` 在 `Locked` 期间拒写，对 `Calculated` 期间是**覆盖**草稿（不覆盖会让账面翻倍）；`Recalculate` 只准入 `Locked`，按人 ID 排序出分录，`Σdelta == Σnew − Σold` 不过即拒。`Entry.Cause` 必须由 `causeOf` 从 `CalcRunID` 四要素推导，不接受调用方自报；差额非零而 `Cause` 为空等于「没换输入却换了结果」，返回 error（`TestPostOverwritesUnlockedDraft`、`TestRecalculateRequiresLockedPeriod`、`TestRecalculateRateChangeOnlyMovesDirectAndManager`）。
16. 金标集是发布闸门：`internal/golden` 必须全绿，任何规则或引擎改动都不得让既有用例变红。语料自带 `Tags` 声明它压什么语义，测试断言该语义在结果中**真实出现**（`TestGoldenCasesTrapWhatTheyClaim`）——少了这条，一次「顺手删掉分账规则」会让 `split` 悄悄退化成 `baseline` 的复制品，而金标集仍然全绿地替它作证。`-update` 是唯一能让金标集失去意义的操作，只能在能解释到具体业务口径时使用。

### 范围外（不要实现）

多币种、多组织。金额就是 `money.Money`（`int64` 分）、单币种、单法人：不加 `Currency` 字段、不加汇率/换算时点、不做分币种台账。跨法人按法人分别装载 `Facts` 各跑一次 `calc.Run`。若有人要求「顺手加个币种」，先确认守恒式（池平账、`Σ Split == 0`、台账 `Σdelta`）要不要按分组改写——那才是真实代价。

### 新增规则类型的步骤

1. `internal/domain/rule.go` 加 `Phase`/`ActionKind` 常量与必要字段。
2. `internal/rules/index.go`：新维度要同时登记到 `domain.Scope`、`domain.MatchCtx`、`specificityWeight` 与 `Matches`/`Overlaps`（漏一处只静默失配，不报错）；冲突检测走 `Scope.Overlaps` 两两判定，不按目录枚举上下文。若新阶段的规则本就该同时命中并相乘（如 `PhaseFactor`），裁决域要按 `arbitrationKey` 细分，否则合法配置会被判成冲突。
3. `internal/calc/run.go` 插入对应阶段处理，并追加 `Trace`。
4. `internal/calc/run_test.go` 补守恒与命中断言；`internal/demo/data.go` 补示例规则。
5. `internal/golden/cases.go` 补一条压新语义的用例（声明 `Tags` 并在 `golden_test.go` 的覆盖度列表里登记）；金额有意变化时跑 `go test ./internal/golden -update`，并逐条核对差异——金标变更必须能解释到具体业务口径。
