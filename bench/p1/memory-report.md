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
| `TestStoreGetAutoPublishesPromotedOncePerReorder` | 库层自动发 `promoted` | 注入 `Store.WithSink` 后 `Get` 实际重排时发 1 条；再次读取已置顶 key 不再发 |
| `TestStoreInsertAutoPublishesEvictedOncePerCycle` | 库层自动发 `evicted` | 注入 sink 后 `Insert` 立即发 1 条且 `DrainEvictions` 为空；`Save` 与手工 drain 均不重复 |
| `TestEvictedAndDeletedAreBothKept` | `deleted` 与 `evicted` 语义分离 | 同一次淘汰同时保留 `Save` 的 `deleted` diff 与 LRU 的 `evicted` 事件 |
| `TestStoreWithoutSinkKeepsLegacyBehavior` / `TestManualPromotionDoesNotDoublePublish` | 兼容与无 panic | 未注入 sink 时行为不变、淘汰留在待发队列；手工提升不重复发布 |

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
| 事件自动发布 | `TestStoreGetAutoPublishesPromotedOncePerReorder`、`TestStoreInsertAutoPublishesEvictedOncePerCycle`、`TestEvictedAndDeletedAreBothKept`、`TestStoreWithoutSinkKeepsLegacyBehavior`、`TestManualPromotionDoesNotDoublePublish` | 通过（库层；服务层无接点） |
| 事件序列还原最终状态 | `TestIntegrationWriteEventsReplayFinalState`、`TestIntegrationPromotionEventReplayRestoresOrder` | 通过（还原 key 集合/顺序/版本） |

## 4. 必须如实说明的边界

1. **事件只承载结构，不承载正文**：`TopicChange` 只有 `topic_key/action/content_hash/order_rank`，因此事件重放可还原主题集合、LRU 顺序与 `order_version`，正文仍以磁盘文件为准。报告不宣称“仅靠事件即可还原完整文档内容”。
2. **库层已自动发布；服务层无接点**：`Store.WithSink` 注入后，`Store.Get` 在真正改变 LRU 顺序时自动发 `memory.lru_promoted`，`Store.Insert`（以及 `Writer.Save` 对未发布队列的兜底）自动发 `memory.evicted`，不再要求调用方手工调用 `Writer.EmitPromotion` / `Store.DrainEvictions`；手工 API 保留兼容且与自动路径共用同一事件出口。但 `services/memory-service` 只使用 `memoryfile.Parse`、不实例化 `Store`，因此服务层没有可注入点，这两类事件仍不会由该服务写入事件库；运行时是否落库取决于上层是否注入 sink。本报告不能宣称服务层已自动落库。
3. **`deleted` 与 `evicted` 是两种语义，不能互相替代**：`Save` 的 `diffTopics` 会为被淘汰 key 产出 `deleted`，LRU 淘汰另发 `evicted`。同一 key 在同一淘汰周期内 `evicted` 只发一次（注入 sink 时立即发布且不进入待发队列）；消费方按 `(topic_key, event_type)` 幂等，不得为了去重删掉 `deleted`。