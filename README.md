# yongjin — 佣金计算内核

订单行 → 因子（季度 / 产品 / 区域）→ 直接佣金 → 分账（SS / AS）→ 池领取（FIFO）→ 池分发 → 管理奖金 → 结算，全流程为纯函数，可回放、可追溯。

**设计文档：[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)** —— 分层、流水线、规则裁决、精度、幂等、历史重算与不变式。

**业务落地方案：[docs/设计-佣金与奖金分成.md](docs/设计-佣金与奖金分成.md)** —— 达成率定系数、SS/AS 分账、预算池按成单顺序、八段流水线、开放问题与落地顺序。

## 运行

```bash
go run ./cmd/demo        # 端到端演示：发布体检 → 计算 → 入账 → 锁期 → 重算 → 差额调整 → 金标闸门
go test ./...            # 全部单测
go test ./internal/calc -run TestRunEndToEnd -v   # 只跑某个用例
go test ./internal/golden -v                     # 金标回归集（发布闸门）
go test ./internal/golden -update                # 重定金标基准（必须先解释清楚为什么变）
go vet ./...
```

## 结构

| 包 | 职责 |
|---|---|
| `internal/money` | 定点金额、万分比、最大余数法分配（池必分到零） |
| `internal/domain` | 事实实体与规则模型，零依赖 |
| `internal/rules` | 规则编译：特异性裁决、冲突拦截、缺口报告、矩阵视图 |
| `internal/calc` | 纯函数内核（归集 + 八段流水线 + 因子链 + 分账 + 池两段）+ 守恒断言 + trace |
| `internal/ledger` | 追加式台账、期间锁定、历史重算差额调整 |
| `internal/golden` | 金标回归集：冻结语料 + 三层期望 + 文本表比对 |
| `internal/demo` | 示例事实与 v1/v2 规则集 |

## 不可妥协的约束

1. **金额只用 `money.Money`（int64 分）**，永不使用 float64；池均分用最大余数法，结算保证 `预算 == 分出 + 结转`（结转是差出来的，`Retained` 只是其中一部分）。
2. **计算内核不读时钟、不碰数据库**：`calc.Run(facts, rules, opts)` 是纯函数，同输入同 `CalcRunID`。
3. **已锁定期间只能追加调整单**：`ledger.Recalculate` 只准入 `Locked` 期间，断言 `Σdelta == Σnew - Σold` 后才入账，并给每条分录写上由 `CalcRunID` 四要素推出的 `Cause`（哪一维变了）；未锁定期间 `Post` 是覆盖草稿，不是追加。
4. **倍率全链只舍入一次**：`基数 × 基准费率 × 档位倍数 × Π 因子倍数` 走同一次 `money.ApplyChain`，`Trace.Replay()` 必须逐分复算回原金额。
5. **分账是零和转移**：`Σ 分出 + 自留 == 分账前金额`，全公司 `Σ Split == 0`，只拆一次不级联。
6. **池是预算，不是费率**：预算属事实，规则只给领取比例；按成单全序 FIFO 领，领完为止，不做按比例缩放。领取与分发明细分两层留痕（`PoolClaim` / `Distribution`），否则"池没发完是因为没订单还是规则没配"无法区分。
7. **金标集是发布闸门**：任何规则或引擎改动都必须让 `internal/golden` 全绿。金标文件里同时冻结 `facts_digest` / `ruleset` / `engine` / `run_id`，「输入变了」与「算法变了」是两种不同的失败；`-update` 只在能解释到具体业务口径时使用——它是唯一能让金标集失去意义的操作。

## 范围外

多币种与多组织不在本项目范围内：金额就是 `int64` 分、单币种、单法人，不引入汇率与换算时点。跨法人按法人分别装载 `Facts` 各跑一次 `calc.Run`，而不是把币种/法人做成 `Facts` 的维度。
