# 数据服务（PostgreSQL / MongoDB）

`docker-compose.data.yaml` 只负责常驻数据容器，业务服务（gateway、agent-service、harnessloop-service 等）仍按原方式在本机启动。

容器名与端口刻意与历史手工容器保持一致，避免所有 DSN 失效：

| 容器 | 镜像 | 端口 | 数据卷 |
|---|---|---|---|
| `hlp-pg-test` | `pgvector/pgvector:pg16` | `55432 -> 5432` | `pgdata -> /var/lib/postgresql/data` |
| `hlp-mongo` | `mongo:6` | `27017 -> 27017` | `mongodata -> /data/db`、`mongoconfig -> /data/configdb` |

## 常用命令

```bash
# 启动 / 重启
docker compose -f docker/docker-compose.data.yaml up -d
docker compose -f docker/docker-compose.data.yaml restart

# 停止（保留容器与卷）
docker compose -f docker/docker-compose.data.yaml stop

# 停止并移除容器（卷为 external，不会被删除，数据仍在）
docker compose -f docker/docker-compose.data.yaml down

# 查看状态
docker compose -f docker/docker-compose.data.yaml ps
docker inspect hlp-pg-test --format '{{json .Mounts}}'
```

## 运维铁律

1. **禁止执行 `docker volume prune`**。`pgdata` / `mongodata` / `mongoconfig` 是数据唯一载体；prune 会清掉未被容器挂载的卷，且不可恢复。
2. **重建容器必须复用同名卷**。用本 compose 重建即可；若手工 `docker run`，必须显式挂载：
   ```bash
   docker run -d --name hlp-pg-test -p 55432:5432 \
     -v pgdata:/var/lib/postgresql/data \
     -v "$PWD/docker/postgres/init.sql:/docker-entrypoint-initdb.d/00-init.sql:ro" \
     -e POSTGRES_USER=agent -e POSTGRES_PASSWORD=agent_dev_password \
     -e POSTGRES_DB=agent_platform pgvector/pgvector:pg16
   ```
3. **不要 `docker rm -f` 后直接 `docker run` 新容器**。不带 `-v pgdata:...` 会新建空卷，现象上就是「数据没了」。
4. 迁移或批量操作前先 `stop`，避免写入过程中复制。

## 历史迁移记录（2026-11-16）

此前两个容器由手工 `docker run` 创建，数据在**匿名卷**中：

| 数据 | 原匿名卷 |
|---|---|
| PostgreSQL | `1204ef876f2c…`（296.7 MB） |
| Mongo `/data/db` | `693ac2ac8f28…`（325.2 MB） |
| Mongo `/data/configdb` | `242d46785135…`（0 B） |

已用 `cp -a` 将内容复制到具名卷 `pgdata` / `mongodata` / `mongoconfig`，并用本 compose 启动。迁移后核对：

- `task_runs` 1391、`subtasks` 57、`model_selections` 92、`events` 333、`schema_migrations` 11
- Mongo `ping` 正常，`agent_platform` 库存在

**三个匿名卷暂未删除**，作为回滚兜底。确认不再需要后可手动删除：

```bash
docker volume rm 1204ef876f2cbe6e8346636923e227aac36f94160462eb50d1a28f4d89b86ea8 \
                 693ac2ac8f28f95a4053da943559b2f0e648f091dcb26add9eabd27872966a3e \
                 242d46785135ab0e46adc91b40855c791ddde2febde6bebc351876816cdd8cad
```

## 备份建议

当前没有自动备份链路。需要备份时：

```bash
# PostgreSQL
docker exec hlp-pg-test pg_dump -U agent -Fc agent_platform > agent_platform.dump

# MongoDB
docker exec hlp-mongo mongodump --archive=/tmp/agent_platform.archive --db agent_platform
docker cp hlp-mongo:/tmp/agent_platform.archive ./agent_platform.archive
```

备份文件不要提交到仓库。