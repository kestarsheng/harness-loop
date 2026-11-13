# T-P1-T1 记忆 LRU 与原子写测试报告

- 基线：`develop` = `2aec72b`
- 范围：仅新增 `pkg/memoryfile/memory_integration_test.go`，未改实现代码
- 日期：2026-11-13

## 1. 验证命令与结果

| 场景 | 命令 | 结果 |
|---|---|---|
| 本地单测 | `go test ./pkg/memoryfile/... -count=1` | `ok agent-platform/pkg/memoryfile 3.036s` |
| 容器竞态检测 | `docker run --rm -v <repo>:/src -v <gomodcache>:/go/pkg/mod -w /src -e CGO_ENABLED=1 golang:1.22-bookworm go test -race ./pkg/memoryfile/... -count=1` | `ok agent-platform/pkg/memoryfile 1.584s` |
| 两包合并竞态 | 同上，追加 `./pkg/harnessloop/compaction/...` | `ok memoryfile 1.369s` / `ok compaction 2.193s` |

本机 `CGO_ENABLED=0` 且无 gcc，无法原生执行 `-race`；已使用宿主现有镜像 `golang:1.22-bookworm`（自带 gcc）在容器内完成，未拉取新镜像。

## 2. 新增用例（只补缺口）

| 用例 | 覆盖点 | 关键断言 |
|---|---|---|
| `TestIntegrationEvictionBoundaryAt31` | 31 主题淘汰边界 | 30 个不淘汰且无事件；第 31 个淘汰最旧 `t01`；第 32 个继续淘汰 `t02`；`Order` 与 `Topics` 等长；淘汰原因为 `lru` |
| `TestIntegrationEvictionAfterReadPromotion` | 读取提升影响淘汰次序 | `Get(t01)` 后 `t01` 置顶，第 31 个淘汰 `t02` 而非 `t01` |
| `TestIntegrationPinnedTailTopicsAreExempt` | 多 pin 豁免（pin 位于 LRU 尾部） | 3 个 pinned 连续处于尾部时连续插入仍只淘汰普通主题，`p1/p2/p3` 全部存活 |
| `TestIntegrationRecoverInvalidFrontmatterKeepsValidTopics` | 非法 markdown 恢复（5 个子用例） | 非法 frontmatter 键 / 非法 scope / 非 JSON order 均保留全部合法主题；重复 key 去重；非法 meta 仅丢弃该块；备份字节与原文一致；修复后可解析且二次恢复不再报损坏 |
| `TestIntegrationWriteEventsReplayFinalState` | 写路径事件可还原最终状态 | 按 `created/updated/deleted/evicted` 事件重放得到的 key 顺序与最终 `Document.Order`、磁盘文件顺序一致；最后一条 `EventWritten.order_version` 与最终文档一致 |
| `TestIntegrationPromotionEventReplayRestoresOrder` | 读提升事件可还原顺序 | `EmitPromotion(a, 0)` 后重放顺序等于最终顺序，且 promoted 事件恰好 1 条 |

已有 `lru_test.go` / `lru_global_test.go` / `atomic_test.go` / `recover_test.go` 已覆盖：原子写、重复写哈希不变、并发写序列化、外部修改拒绝、中断写保留旧文件、全局内存软限、全域读不重排、单 pin 豁免、全 pin 拒绝、部分损坏恢复与幂等。本轮未重复实现这些用例。

## 3. 覆盖矩阵（T-P1-T1 实施要点）

| 要点 | 新增/已有用例 | 结论 |
|---|---|---|
| 31 主题淘汰 | `TestIntegrationEvictionBoundaryAt31`、`TestIntegrationEvictionAfterReadPromotion` | 通过 |
| 读取提升 | `TestIntegrationEvictionAfterReadPromotion`、已有 `TestReadPromotesToFrontAndPersists` | 通过 |
| pin 豁免 | `TestIntegrationPinnedTailTopicsAreExempt`、已有 `TestPinnedTopicsAreNotEvicted` / `TestAllPinnedRejectsInsert` | 通过 |
| 写入中断后文件完整 | 已有 `TestInterruptedWriteKeepsOriginalReadable` | 通过 |
| 重复写哈希不变 | 已有 `TestWriterIsIdempotentForIdenticalContent` | 通过 |
| 非法 markdown 恢复 | `TestIntegrationRecoverInvalidFrontmatterKeepsValidTopics`、已有 `TestRecoverPartialKeepsValidTopics` | 通过 |
| 事件序列还原最终状态 | `TestIntegrationWriteEventsReplayFinalState`、`TestIntegrationPromotionEventReplayRestoresOrder` | 通过（还原 key 集合/顺序/版本） |

## 4. 必须如实说明的边界

1. **事件只承载结构，不承载正文**：`TopicChange` 只有 `topic_key/action/content_hash/order_rank`，因此事件重放可还原主题集合、LRU 顺序与 `order_version`，正文仍以磁盘文件为准。报告不宣称“仅靠事件即可还原完整文档内容”。
2. **读提升与淘汰事件当前无生产调用方**：`grep` 结果显示 `Writer.EmitPromotion` 与 `Store.DrainEvictions` 只在测试中被调用。`Store.Get` 不会自动发 `promoted`，`Store.Insert` 不会自动发 `evicted`；要让事件序列可还原，调用方必须显式转发这两类事件。本测试验证的是该契约，不能据此宣称运行时已自动发出这些事件。
3. **同一次淘汰会产生两条删除类事件**：`Save` 的 `diffTopics` 会为被淘汰 key 产出 `deleted`，调用方再发 `EmitEvictions` 会产出 `evicted`。重放逻辑按幂等删除处理；若后续做事件消费方，需要明确去重口径。