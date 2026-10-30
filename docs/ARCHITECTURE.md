# 佣金计算架构

> 本文描述 `yongjin` 的计算内核设计。代码即文档的部分以 `file:line` 标注，其余为设计约定。
> 结论先行：**纯函数内核 + 不可变事实快照 + 追加式台账**，不引入规则引擎或 DSL。

---

## 1. 设计前提

三个业务事实决定了架构取舍，它们比任何技术偏好都重要：

| 业务事实 | 架构决策 | 因此放弃的东西 |
|---|---|---|
| 规则由业务在后台配置 | 结构化模板参数化，不做通用脚本 DSL；发布走「校验 → 预演 → 生效」 | 规则编译器、表达式沙箱 |
| 需要历史重算（改规则回溯已发佣金） | 双时间轴 + 输入快照冻结 + 差额调整单 | 可重放查询、原地更新 |
| 订单量少、单笔金额大 | 精度与可追溯优先，允许全量重算 | 增量计算引擎、流式计算、对账补偿链路 |

第三条尤其关键：订单稀疏意味着**全量重算的成本可以忽略**，而单笔金额大意味着**任何一分钱误差都会变成财务争议**。所以复杂度应当全部投入到正确性和可解释性，而不是吞吐。

---

## 2. 分层

```
┌──────────────────────────────────────────────────────┐
│ 事实层  domain.Facts                                  │
│   订单 · 产品映射 · 组织树 · 区域 · 客户等级（快照冻结） │
└───────────────────────┬──────────────────────────────┘
                        │ 纯数据，无 IO
┌───────────────────────▼──────────────────────────────┐
│ 规则层  rules.Index（发布期编译产物）                   │
│   冲突拦截 · 特异性裁决 · 缺口报告 · 矩阵视图            │
└───────────────────────┬──────────────────────────────┘
                        │ 只读索引
┌───────────────────────▼──────────────────────────────┐
│ 计算层  calc.Run（八段纯函数）                          │
│   不读时钟 · 不碰 DB · 无随机 · 同输入必同输出           │
└───────────────────────┬──────────────────────────────┘
                        │ 不可变结果
┌───────────────────────▼──────────────────────────────┐
│ 结果层  ledger.Book（追加式台账）                       │
│   期间状态机 · 锁定拒绝改写 · 重算只产调整单             │
└──────────────────────────────────────────────────────┘
```

**为什么事实必须冻结**：组织架构、产品线归属、区域划分都在变。若计算时回查实时库，三个月后业务问「张三 8 月为什么少了两百块」，你将无法复现当时的判断依据。快照让每次计算都可回放、可复核。

---

## 3. 流水线（八段）

主流程实现于 `internal/calc/run.go`，归集实现在 `internal/calc/attain.go`，池两段在 `internal/calc/pool.go`。阶段 0 必须先整期跑完，之后订单行才进入阶段 1~4；阶段 5~6 复用同一个全序再走一遍 FIFO。

| 段 | 函数段落 | 输入维度 | 输出 | 关键约束 |
|---|---|---|---|---|
| 0 归集 | `PhaseAttain` · `attainmentsOf` | 全部订单行 + `Targets` | `Attainment`（每人每口径一条） | 达成率是**期间级**函数，必须先跑完；业绩归属不回写分账 |
| 1 可计佣基数 | `OrderLine.BaseOf(kpi)` | 行项目 × 口径 | `Contract − Discount − Return` / `Revenue − Return` | 基数 ≤ 0 跳过；两个口径各算一笔 |
| 2 因子 | `PhaseFactor` · `factorsOf` | 季度 · 产品线/系列 · 区域 | `FactorHit`（倍数，不落钱） | 同 `Kind` 至多一条（`BestByKind`），不同 `Kind` 相乘；缺配按 1.0；不单独舍入 |
| 3 直接佣金 | `PhaseDirect` | 产品线 × 角色 × 档位 | 人 × 行 × 口径明细 | 产品线定**基准费率**、角色定**归属**、档位给**倍数**；全链只舍入一次 |
| 4 分账 | `PhaseSplit` · `splitsOf` | 关系图（快照，有向） + `Action.ShareMode` | `SplitLine`（From → To 转移） | 一次零和转移：`Σ 分出 + 自留 == 分账前`，不级联；关系缺口按 100% 自留 + 告警 |
| 5 池领取 | `PhaseFund` · `runPools` | 行（收入口径）× 池预算（事实） | `PoolClaim`（每行每池至多一条） | 全序 `(ClosedAt, OrderID, LineID)` 上 FIFO，领完为止；领到 0 也留痕（`Exhausted`）；裁决域按目标池切分，一行可同时喂多个池 |
| 6 池分发 | `PhaseDistribute` · `distributeClaim` | 领取额 + 参与者 × 角色 | `Distribution`（池 → 人，带 `OrderID/LineID`） | `Budget == Distributed + CarryOver`；分不出去的退回留存；不上溯管理奖 |
| 7 管理奖金 | `PhaseManager` | 组织树 | 逐层衰减明细（带 `OrderID/LineID`，可与 `Directs` 逐条对账） | 基数为**分账前**直接佣金全额，非分账后、非池（I12） |
| 8 结算 | 聚合段 | 人 | `PersonTotal` | 按口径分开留存，便于对账 |

阶段 0 与阶段 3 是全流程唯一的前后依赖：单笔佣金依赖档位倍数，档位倍数依赖「这个人本期一共成了多少单」。因此流水线天然分两趟，不能边算边累积——顺序累积会让结果依赖订单遍历顺序，也无法解释「为什么第 3 单和第 10 单比例不同」。

阶段 2 不是独立的出钱阶段：它只产出倍数，必须与阶段 3 的基准费率、档位倍数拼成**一条** `money.ApplyChain`（`base × BaseBPS × BandBPS × Π FactorBPS`），否则每一步各舍入一次，链越长偏差越大。`Trace.Replay()` 就是这条链的无损留痕：拿 Trace 里的 `Base/RateBPS/CoeffBPS/Factors/Weight/OverlayDelta` 复算，必须逐分等于 `Amount`。

阶段 4（分账）的四条实现约束：

- **零和**：`Σ PersonTotal.Split == 0`。分账从不产生新钱，也不进 Trace —— 它自己的留痕是 `SplitLine`（`From → To → Amount`），`Replay` 那套倍率链对它无意义。
- **不级联**：只读 `DirectLine`，从不读 `SplitLine`，所以"分出来的钱再分一次"在结构上不可能发生。
- **`Direct` 保持分账前全额**，分账净额单列 `Split`。把两者合进一列，管理奖基数（读分账前）与对账（读收款方）必有一处串味。
- **关系只认方向**：快照记 `From`=上级、`To`=下属。用对称的"对方"语义会让 SS 成单时把上级当搭档。

阶段 5~6（池）的五条实现约束：

- **预算来自事实，比例来自规则**。`PoolDef.Budget` 进 `factsDigest`，规则只给 `ClaimBPS`（即 `Action.BaseBPS`）。预算写进规则的话，改版就会改预算，重算差额无法归因。
- **领取按全序 FIFO，不做按比例缩放**。缩放会让早单金额随后续补录订单变化，每次都重算全部历史单（设计文档 §2.3）。
- **两层产物都要留**。`PoolClaim` 答"这单从池里领了多少、还剩多少"，`Distribution` 答"这笔钱给了谁"。合成一层后，"池没发完是因为没订单还是规则没配"就无从区分。
- **封顶不进倍率链**。`Trace` 记"应领"，截断差额落 `OverlayDelta`，`Replay()` 仍逐分复算回实领；`ApplyChain` 只乘不截断。
- **池钱不上溯管理奖**。池本身已是对成单的二次奖励，再沿组织链衰减一次会让同一笔钱奖励两次管理者，且直接打破 `Budget == Distributed + CarryOver`。

「按人 / 按订单」不是第三、第四个入口，而是对同一个 `Result` 的裁剪（`project.go`）：`ProjectByPerson` / `ProjectByOrder` 都不改 `CalcRunID`，因为它们没有重新算钱——只是把明细切片，再决定要不要把求和重跑一遍。按人保留原结算行（那是这个人的期间合计），按订单则把裁剪后的明细喂回同一个 `aggregatePersonTotals` 重新求和（期间合计放进单笔订单是错的数）。求和因此只有一份实现，存在 `aggregate.go`。

业务口径与建模取舍见 [设计-佣金与奖金分成.md](设计-佣金与奖金分成.md)。

### 3.1 为什么按阶段拆而不是按规则统一处理

四个业务维度看起来「都在竞争同一笔钱」，实际落在不同阶段：

- 池领取只关心**领多少**（产品线/系列 × 参与者角色）和**还剩多少**（预算序列），与佣金怎么算无关
- 直接佣金关心**产品 × 角色**，产品定费率、角色定份额
- 池分发只关心**池与团队构成**
- 管理奖金只关心**组织树**

因此绝大多数所谓「复杂规则」其实是维度错位造成的误解。真正的规则冲突只存在于**同阶段、同维度、多条命中**的窄缝里，裁决机制只需覆盖这一小片。

---

## 4. 规则模型

### 4.1 作用域与特异性

```go
type Scope struct {
    Lines, Series, Roles, Pools, Territories, Tiers []string
    Specificity int  // 编译期由非空维度加权得出
}
```

空维度 = 通配。加权规则（`domain.specificityWeight`）：产品线 / 池 / 角色各 8 分，系列 / 区域各 4 分，客户等级 2 分。含义是「产品线和角色是最硬的分辨维度」。

### 4.2 裁决顺序（唯一且有确定性）

运行时与编译期共用同一个 `rules.better`，避免两套逻辑漂移：

```
Specificity 降序 → Priority 降序 → EffectiveFrom 更晚 → ID 字典序（仅保证稳定输出）
```

**硬约束**：若两条规则 specificity 与 priority 相同、作用域重叠、生效期相交，`rules.Compile` 直接返回 error，规则集不可发布。绝不允许靠数组出现顺序决定谁生效——这是佣金系统里最难排查的一类事故。

### 4.3 特殊规则（Overlay）

业务永远会给「华东区大客户首单加成」这类需求开口子。治理方式不是拒绝，而是四条强制约束：

```go
type Overlay struct {
    Target        OverlayTarget  // add_rate / cap / exempt
    Replaces      []RuleID       // 铁律 1：必须显式声明覆盖对象
    ExpiresAt     Period         // 铁律 2：必须有到期日
    Owner         PersonID       // 铁律 3：必须有责任人
    Justification string         // 铁律 4：必须有理由/工单号
}
```

- 只有 `add_rate`（叠加）与 `cap`（封顶）允许不写 `Replaces`；改变基础规则结果必须显式声明。
- 到期自动失效，防止特殊规则永久化。
- 每个 overlay 的 ID 写入 `DirectLine.Overlays`，`trace` 中可见「30% 基础 + 5% 特批」的完整来源。

### 4.4 发布期体检

`rules.Compile` 产出三类结果，共同构成发布闸门：

| 产物 | 含义 | 处理 |
|---|---|---|
| `Conflicts` | 无法裁决的规则对 | **返回 error，拒绝发布** |
| `Gaps` | 某产品线 × 角色无规则 | 告警，但允许发布（可能是业务有意留空） |
| `MatrixLines` | 产品线 × 角色矩阵 | 人工复核视图 |

缺口是最隐蔽的资损来源：订单进来少发钱时**系统不会报错、当事人可能也不报**。演示中 `B × channel` 的缺口即在发布前被标红。

矩阵同时是运行时索引的自然形态——组合数很小（产品线 × 角色通常几十到几百格），可以全量枚举，所以「发布时体检的全集」与「运行时实际命中的规则」不会出现口径不一致。

---

## 5. 金额与精度

### 5.1 表示

只有 `money.Money`（`int64` 分）。禁止 `float64`，禁用浮点聚合与比较。

万分比（bps）为整数，`10000 bps = 100%`。`money.ApplyRate` 采用「大额乘法拆解」避免 int64 溢出：

```
a*b/d = (a/d)*b + (a%d)*b/d
```

并在最后一步做远离零的四舍五入，负数对称。

### 5.2 池分配必须分到零

`money.Allocate` 用**最大余数法**拆分，保证 `Sum(out) == total` 恒成立（含负数、含全零权重、含权重量级远大于总额的情形）。

实现要点：把 `amt*w/sum` 拆成 `q*w + (r*w)/sum`，余数项 `(r*w)%sum` 作为排序键分配尾差。若简化为先算 `q = amt/sum`，在「权重以分为单位」时（池分发的常态：权重和 123 万 ≫ 总额 4.4 万）`q` 会归零，整数部分整体丢失，结果是池子被均分——金额看着分完了，比例全错，且对账对不出来。

### 5.3 舍入的层级

- 个人级：`ApplyRate` 四舍五入
- 池级：最大余数法，尾差在池内消化
- 汇总级：只做加法，**不再舍入**

跨层重复舍入会造成系统性偏差，必须在设计上禁止。

---

## 6. 幂等与版本

```
CalcRunID = sha256(period | factsDigest | ruleSetVersion | engineVersion)[:16]
```

四要素缺一不可：

| 要素 | 变化意味着 |
|---|---|
| `period` | 不同结算期 |
| `factsDigest` | 事实变了（订单补录、组织调整） |
| `ruleSetVersion` | 规则变了 |
| `engineVersion` | **代码逻辑变了** |

最后一项最容易漏。若引擎升级导致重算结果不同而 ID 不变，幂等缓存会直接吞掉重算请求，表现为「改了代码但线上数字没动」。

`factsDigest` 用 `encoding/json`（对 map 键排序）而非 Go 原生 map 遍历，否则同输入会得到不同摘要。测试 `TestCalcRunIDIgnoresMapOrder` 专门守这条。

但只靠 json 的 map 排序还不够：事实里有 `Orders` / `Lines` / `Participants` / `Relations` / `Targets` / `Pools` 六处切片，其顺序只反映录入次序，不携带业务含义。因此 `Digest` 先 canonical 规范化（按业务标识排序全部切片）再摘要，见 `internal/calc/digest.go`。`TestDigestIgnoresSliceOrder` 与 `TestRunIgnoresSliceOrder` 守这条，后者还要求 `Directs` / `Persons` 逐条对齐——只保证幂等键相同、金额却依赖顺序，问题只是被藏起来而非解决。

---

## 7. 期间与历史重算

### 7.1 双时间轴

必须彻底分开，混在一起就永远算不清：

- **业务时间**：成交期 / 佣金归属期 / 规则生效期
- **系统时间**：录入时间 / 计算时间

事实快照与规则集都带生效期，计算必须显式携带 `asOf` / `period`。用「今天的组织架构」算去年的佣金是最常见的线上事故。

### 7.2 期间状态机

```
Open ──► Calculated ──► Locked
  │            │            │
  │            │            └─ 只允许追加 adjustment
  │            └─ 可重算覆盖草稿
  └─ 可自由重算
```

`ledger.Book.Post` 在 `Locked` 期间直接返回 error；对 `Calculated` 期间是**覆盖草稿**（先丢弃该期间已有的 settlement 再写），不是追加——追加会让同一期间出现两套结算分录，账面凭空翻倍且各指不同 `CalcRunID`。

`ledger.Book.Recalculate` 只对 `Locked` 期间开放。未结算时直接 `Post` 就是重算，未锁定时 `Post` 覆盖草稿，这两个入口本就更简单；若让 `Recalculate` 也在未锁定期间放行，账面会从「只有一批 adjustment、没有 settlement」开始，`PeriodTotal` 从此等于差额而非合计。

### 7.3 重算流程

```
0. 断言期间状态为 Locked            ← 否则提示「直接 Post 即可」
1. 用 snapshot 的 facts + 新 ruleSetVersion 重跑 calc.Run
2. delta = newTotal − oldTotal（按人，按人 ID 排序后逐条出分录）
3. 断言 Σdelta == Σnew − Σold      ← 不过则拒绝入账
4. Cause = causeOf(old, new)：四要素里哪一维变了
   若 Cause 为空而差额非零 → 拒绝（引擎不确定，不可复现的差异不得入账）
5. 生成 adjustment 分录，双向关联新旧 CalcRunID
```

调整单不可合并、不可覆盖；原单与调整单通过 `AdjustOf` 互链，任何时候都能回答「这笔钱最初怎么算的、后来为什么改」。

`Cause` 必须**推导**而非由调用方传入：判据与 `CalcRunID` 的输入完全一致（period 已校验相同），所以「`Cause` 非空」与「`CalcRunID` 变化」是同一件事。它只回答「哪一维变了」（`rules:v1->v2` / `facts:3f2a91c0->8b7d4e12` / `engine:...`），更细的归因（改了哪条规则、哪个池预算）拿这两个版本号回快照上查——台账不该变成变更日志。

演示中 v1→v2 费率上调后，差额自动落到 P1 (+4560.00)、P3 (+2280.00)、P5 (+1140.00)——管理链按 50%/25% 连带变动，`Cause` 记为 `rules:v1->v2`。这类连带效应必须由系统算，不能靠人补。

---

## 8. 必须保持的不变式

`Run` 与 `Recalculate` 内均为**断言而非常量注释**，违反即返回 error：

1. 金额一律 `money.Money`，禁止 `float64`。
2. 每个池：`Budget == Distributed + CarryOver`，且 `CarryOver := Budget − Distributed`（由构造保证，断言实守 `Distributed ≤ Budget`）。
3. `Σ PersonTotal.Total == Σ Direct + Σ Distributed + Σ ManagerBonus`
   —— 注意是 `Distributed`，不是 `Budget`；留存与结转不发给个人，`Retained` 是 `CarryOver` 的子集而非并列的一份钱。
4. `CalcRunID` 四要素任一变化必须改变 ID。
5. `Recalculate` 先断言 `Σdelta == Σnew − Σold` 再入账；且只准入 `Locked` 期间，每条分录的 `Cause` 由四要素推导（差额非零而 `Cause` 为空即拒绝）。

第 3 条在开发期实际踩过一次（测试把池的"预算"当成"已发出"代入导致守恒断言失败），说明「守恒等式写错方向」比「算错金额」更隐蔽——等式必须由业务口径推导，不能凭直觉写。同类错误在 M5 换口径时又出现一次：`CarryOver` 若用「领了没发 + 没领」两个累加器相加，等于把 `Retained` 数了两遍；写成差式后该错误在结构上不可能发生。

---

## 9. 可解释性

每个结果都留 `Trace`：

```go
type Trace struct {
    OrderID, LineID, Phase, RuleID, Pool, Person string
    Base money.Money
    RateBPS int64
    Amount money.Money
    Note string   // 产品线/系列、overlay 列表、管理层级
}
```

配合 `DirectLine.Overlays`、`Distribution.Weight`、`ManagerBonus.Level`，任何一笔金额都能沿链路回答：

> 这 22400 元 = 基数 800000 × 2.5%（规则 R-DIR-A-OWNER）+ 800000 × 0.3%（特殊规则 OV-A1-VIP）

这是争议处理的核心能力，其价值高于任何性能优化。

---

## 10. 扩展指引

### 10.1 新增规则类型

1. `internal/domain/rule.go`：加 `Phase` / `ActionKind` 常量与 `Action` 字段。
2. `internal/rules/index.go`：新维度必须**同时**登记到 `domain.Scope`、`domain.MatchCtx`、`specificityWeight`，以及 `Matches` / `Overlaps` 两条判定——漏一处不会报错，只会静默失配（作用域永远命中或永远不命中）。冲突检测对作用域做两两交集判定（`Scope.Overlaps`），不按目录枚举上下文，所以不会出现「漏配枚举导致冲突静默通过」。
   裁决域粒度由 `arbitrationKey` 决定：普通阶段按 `Phase` 成域，`PhaseFactor` 按 `Phase/Kind` 成域——时间、产品、区域因子本就该同时命中并相乘，按整个阶段判冲突会把任何一套合法因子配置判成不可发布。
3. `internal/calc/run.go`：插入阶段处理，**必须**追加 `Trace` 与守恒断言。
4. 测试：`run_test.go` 补守恒与命中断言；`demo/data.go` 补示例规则。

### 10.2 新增分配方式

在 `domain.DistMode` 加枚举，`internal/calc/pool.go` 的 `distribute` 内实现，并在 `rules.detectPoolRules` 的取值白名单里登记——漏登记的后果是发布期就报 error（未知取值），不是运行时静默留存。`DistMode` 表达的是「一份领取额怎么分」，不要往里塞「何时领」（那是阶段 5 的全序）。任何新分配方式都必须满足「金额不增不减」——权重表里没有的人不参与，分不出去的退回 `Retained`（→ `CarryOver`），不允许残留未归集金额。

### 10.3 接入外部存储

计算内核不应感知存储。推荐形态：

```
配置中心 / DB ──► Facts 装载器 ──► domain.Facts ──► calc.Run（纯函数）
                    │
                    └─ 快照整包落对象存储，路径写入 calc_snapshot.payload_uri
```

因为订单量少，**整包冻结**比「可重放查询」更稳：不依赖历史版本的查询代码还能正确执行。

---

## 11. 演进路线

| 阶段 | 内容 | 状态 |
|---|---|---|
| P0 | 事实快照 · 纯度内核（六段，M2 起扩到八段）· 精度/幂等/守恒断言 | **已完成** |
| P1 | 规则模板化后台配置 · 发布校验 · 金标回归集 | **金标回归集已完成**（`internal/golden`，7 例）；模板化后台配置待做 |
| P2 | 期间锁定 · 历史重算 · 差额调整单 | **已有骨架**（内存台账） |
| P3 | 影响分析报告 · 审批流 · 单订单秒级重算 | 待做 |

P1 的关键不是规则编辑器，而是**金标回归集**：一套历史典型订单 + 期望结果，每次规则发布自动跑通。业务自助配置唯一有效的护栏是「改 A 规则把 B 弄坏」能被自动发现。

金标集已按此落地（M8）：语料在 `internal/golden/cases.go`，期望在 `testdata/*.golden` 文本表，闸门是 `go test ./internal/golden`。三个设计点值得记：期望按「结算表 / 池对账 / 单笔投影」三层冻结（只冻合计会放过「合计对但归属错」）；文件头同时冻结 `engine` / `ruleset` / `facts_digest` / `run_id`，让「输入漂移」和「算法回退」成为两种不同的失败（否则为了消红而 `-update`，会把回退固化成基准）；用例各自声明压什么语义，测试断言该语义真的出现（否则用例会退化成基线副本，仍然全绿）。

---

## 12. 与代码的对应

| 概念 | 实现位置 |
|---|---|
| 事实快照 | `internal/domain/types.go` — `Facts` |
| 规则 / 特殊规则 | `internal/domain/rule.go` — `Rule` / `Overlay` / `Scope` |
| 裁决顺序 | `internal/rules/index.go` — `better` |
| 冲突与缺口 | `internal/rules/index.go` — `detectConflicts` / `detectGaps` |
| 矩阵视图 | `internal/rules/index.go` — `MatrixLines` |
| 八段流水线 | `internal/calc/run.go` — `Run`；归集在 `internal/calc/attain.go`；池两段在 `internal/calc/pool.go` |
| 池领取 / 分发 / 结算 | `internal/calc/pool.go` — `runPools` / `distributeClaim` / `distribute`；`types.go` — `PoolClaim` / `Distribution` / `PoolSettlement` |
| 分账 | `internal/calc/split.go` — `splitsOf` / `assertSplits` |
| 按人求和 / 投影 | `internal/calc/aggregate.go` — `aggregatePersonTotals`；`project.go` — `ProjectByPerson` / `ProjectByOrder` |
| 因子链 | `internal/money/money.go` — `ApplyChain` |
| 幂等键与摘要规范化 | `internal/calc/digest.go` — `Digest` / `runID` / `canonical` |
| 定点与分配 | `internal/money/money.go` — `ApplyRate` / `Allocate` |
| 台账与重算 | `internal/ledger/ledger.go` — `Post` / `Recalculate` / `causeOf` / `Entry.Cause`；用例在 `recalc_test.go`（三种成因）与 `ledger_test.go`（状态机准入） |
| 金标回归集 | `internal/golden/cases.go`（语料）· `golden.go`（格式/比对）· `testdata/*.golden`（期望） |
| 端到端示例 | `cmd/demo/main.go` |
