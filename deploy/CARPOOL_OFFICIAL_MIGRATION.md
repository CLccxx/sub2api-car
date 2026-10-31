# 官方 Docker 0.2.18 → 拼车版（基于 0.2.19）迁移

本文件准备后续服务器迁移；本次代码同步没有连接生产服务器、执行迁移或发布镜像。
目标是让新应用继续使用原安装的 PostgreSQL、Redis、应用文件与配置，只替换应用容器。
生产 Compose 布局尚未核实，下面的变量与示例必须根据现场记录填写后才能执行。

`init-car.sh` 和 `docker-compose.car.yml` 用于新安装，不能套用到已有官方安装。
`upgrade_car.py`、网页更新安装器及宿主机更新服务依赖已有拼车版目录和 Compose
约定，当前没有提供官方安装导入功能。不要直接用它们完成这次迁移；网页更新接入另行规划。

## 1. 记录现有安装与恢复材料

先记录实际应用容器、Compose project 名称、工作目录、所有 `-f` 文件及顺序、
`--env-file`、profiles、运行镜像 ID/digest、平台架构、入口代理与域名。
可用以下只读命令辅助定位；先从列表确认容器，再设置 `APP_CONTAINER`、`PG_CONTAINER` 等变量。

```bash
docker ps --format 'table {{.Names}}\t{{.Image}}\t{{.Status}}'
docker inspect --format '{{json .Config.Labels}}' "$APP_CONTAINER"
docker inspect --format '{{json .Mounts}}' "$APP_CONTAINER"
docker inspect --format '{{json .Mounts}}' "$PG_CONTAINER"
docker inspect --format '{{json .Mounts}}' "$REDIS_CONTAINER"
```

在仅管理员可读的备份目录保存原始 Compose 文件、实际使用的 env 文件、启动参数、
容器有效配置和镜像身份。`docker inspect` 完整输出及 `docker compose config` 展开结果
可能含密码，只写入权限 `0600` 的本地文件，不贴到聊天或提交到 Git。
不要只依据仓库里的 `.env.example` 或新版本默认值重建环境。

逐项核实并保留：

| 对象 | 必须保持的内容 |
| --- | --- |
| PostgreSQL | 原容器/服务、版本、数据库、用户、认证方式、连接参数和所有真实数据挂载；包含 `schema_migrations`、用户、余额、API Key、账号凭据、订阅、设置、日志、支付等整库数据 |
| Redis | 原实例、ACL 用户/密码、TLS、逻辑 DB 编号及实时状态；refresh token、用户 token 集合、token family 都在 Redis 中，不能清库或换空实例 |
| 应用数据 | 实际 `DATA_DIR`（通常 `/app/data`）的完整内容、`config.yaml`、`.installed`、上传文件、其他挂载文件与可写资源；保留权限/属主 |
| 有效配置 | 容器环境变量、实际配置文件路径、命令行、外部 secrets 及它们的优先级；继续使用原数据库/Redis/代理/OAuth 客户端配置 |
| 登录与 2FA | 原 JWT 签名密钥、数据库 `security_secrets`、原 TOTP 加密密钥、用户 email/password_hash；不重置密码、不生成新密钥、不撤销会话 |
| 公网入口 | 原协议、域名、端口、TLS、反向代理与可信代理配置，保持同一浏览器 origin；保留用户浏览器本地存储和 cookies |

JWT 启动逻辑优先沿用数据库中已经持久化的 `jwt_secret`；`.env` 中留空不代表当前没有密钥。
TOTP 必须保留当前实际使用的加密密钥，不能用新安装脚本生成替代值。
如果这些配置的来源无法确认，先补齐记录并在副本验证，再安排切换。

### 特别检查 PostgreSQL 的真实数据位置

在已确认容器、数据库用户和库名后检查实际路径：

```bash
docker exec "$PG_CONTAINER" sh -c 'printf "%s\n" "$PGDATA"'
docker exec "$PG_CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" -Atqc 'SHOW data_directory'
```

将 `SHOW data_directory` 结果与容器 `.Mounts` 对照，确认覆盖该路径的真实 bind mount
或 Docker volume（包括匿名卷）。PostgreSQL 18 镜像的默认路径可能在
`/var/lib/postgresql/18/docker`，而某些旧 Compose 只挂载了 `/var/lib/postgresql/data`；
真正的数据可能位于另一个匿名卷。现场的 `PGDATA`、SQL 查询与挂载结果才是依据。
本次保留原 PostgreSQL 容器与挂载，不调整目录、不升级数据库大版本、不重建基础设施。

## 2. 准备固定镜像并隔离演练

1. 从完成合并与验证的固定提交准备匹配服务器架构的拼车版镜像，保存版本、提交号、
   镜像 ID/digest 和 SQL 校验清单。使用预编译包时先校验归档与包内 `SHA256SUMS`，
   在独立目录构建 `Dockerfile.goreleaser`；不要解压覆盖生产目录。
   不使用官方 `latest`、不复用已发布标签。本次准备不意味着已经发布可下载版本。
2. 提前下载/导入并验证镜像；检查 `/app/sub2api -version` 输出和平台。版本检查应使用
   `--network none`、无生产挂载、无生产环境变量的临时容器，覆盖 entrypoint 只运行版本命令。
3. 导出生产备份用于演练，把 PostgreSQL、Redis 和应用文件恢复到独立实例、独立卷、独立
   Compose project；克隆配置必须指向副本。Redis 副本也不能连接生产 Redis。
4. 在启动任何副本应用前，从容器网络/防火墙层阻断所有外发访问，仅允许其连接副本
   PostgreSQL、Redis 和本地测试浏览器。禁止连接上游 API、OAuth token endpoint、
   代理、邮件、支付回调或其他生产服务。仅把账号改成“不调度”不足以阻止后台刷新。
   **不能让副本使用真实 refresh token 联系上游**，否则可能轮换令牌并使生产授权失效。
5. 使用副本启动目标应用，确认迁移完成；再次启动确认幂等。比较迁移前后用户、余额、
   API Key、上游 credentials、设置、订阅额度/已用量/窗口、历史账单与使用记录。
   原分组的 `carpool_members` 应为 `0`，新按张计费开关默认关闭，迁移不会自动开启拼车。
6. 在隔离环境验证登录、TOTP、已有 access token 和 refresh token 的兼容性。
   换域名的副本不能直接证明生产浏览器同源存储会继续可用；正式验收仍要在原 origin
   使用迁移前保留的浏览器会话。任何用于测试的生产 token/凭据只在本地私密处理。

迁移增量是以下六个文件，历史官方迁移不可改写或手工覆盖 checksum：

- `237_custom_group_carpool.sql`
- `238_custom_carpool_reset_monitor.sql`
- `241_custom_quota_reset_operations.sql`
- `242_custom_carpool_reset_voting.sql`
- `247_scoped_admin_api_keys.sql`
- `248_group_image_count_billing.sql`

仓库回归测试 `TestOfficial018ToCar019PostgresPreservesData` 从过滤后的原始 SQL 建立
官方 0.2.18 基线，填充虚构数据，运行真实迁移两次并比较整行数据及历史校验记录。
可在 `backend` 目录使用具备 `CREATEDB` 权限的**隔离测试服务器**运行：

```bash
go test ./internal/repository -run '^TestOfficial018ToCar019PostgresPreservesData$' -count=1 -v
```

需预先设置私密的 `CARPOOL_TEST_DATABASE_URL`。测试创建并清理独立临时数据库，
因为历史 SQL 显式使用 `public`；不会启动应用或访问上游。它不能代替真实备份演练、
Redis/session 验收或具体服务器的部署检查。

## 3. 停写、最终备份与只切换应用

演练通过后安排维护窗口。暂停入口新请求、Webhook/定时作业等写入，等待流式请求、
支付处理和后台写入结束，再停止**所有旧应用实例/worker**。保留 PostgreSQL、Redis
和入口代理运行，避免旧新应用同时操作同一上游 refresh token。

停写后重新做最终备份，不能用演练前的旧备份替代：

- 用与服务端兼容的 `pg_dump -Fc` 导出完整数据库；备份所需角色/权限和扩展清单。
  用 `pg_restore --list` 检查归档可读，并提前在演练中验证实际恢复。
- 保存 Redis 已完成的持久化快照和实际持久化配置；使用其已有安全认证方式触发并等待
  快照完成，核实路径及成功状态。AOF 多文件格式需完整一致备份，不能随意复制写入中的
  单个文件。切换时继续使用现有在线 Redis，不能 `FLUSHDB`/`FLUSHALL`。
- 保存最终应用数据、有效配置、代理配置、旧镜像、全部迁移记录（filename/checksum/applied_at）
  和基线计数。备份保存在权限 `0700` 的目录，校验后保留离机副本。

只有现场已确认 `PG_CONTAINER`、`DB_USER`、`DB_NAME`，并建立私密的 `BACKUP_DIR` 后，
才可用以下示例。认证沿用容器现有安全方式；不要把密码写入命令行：

```bash
umask 077
docker exec "$PG_CONTAINER" pg_dump -U "$DB_USER" -d "$DB_NAME" -Fc > "$BACKUP_DIR/database.dump"
docker exec -i "$PG_CONTAINER" pg_restore --list < "$BACKUP_DIR/database.dump" > "$BACKUP_DIR/database.contents"
```

任一备份或验证失败即停止切换。不要对运行中的 PostgreSQL 数据目录直接打包当作有效备份。

在**原 Compose 工作目录与原 project**内增加仅覆盖应用 image 的附加文件。
下面仅适用于已确认服务名为 `sub2api` 的布局；其他服务名按实际修改。

```yaml
# compose.car-image.yml — 使用已验证的固定镜像 ID 或仓库 digest
services:
  sub2api:
    image: ${CAR_MIGRATION_IMAGE:?set the verified immutable image reference}
```

先在本机私密地比较合并后配置，确认唯一预期变更为应用镜像，所有挂载、环境、网络、端口、
服务名和基础设施保持原值；有宿主机覆盖应用二进制/静态文件的挂载时必须单独审查。
不能盲目保留一个会覆盖新镜像内容的旧程序挂载。

以下 Bash 示例仅在已核实 `PROJECT`、`DEPLOY_DIR`、`ENV_FILE`、`BASE_COMPOSE` 和
`APP_SERVICE` 后使用；存在更多原始 `-f` 文件时，必须按原顺序加入 `compose` 数组。
不要把新的拼车版全套 Compose 放进这个数组。

```bash
compose=(docker compose --project-directory "$DEPLOY_DIR" -p "$PROJECT" --env-file "$ENV_FILE" -f "$BASE_COMPOSE")
"${compose[@]}" -f "$DEPLOY_DIR/compose.car-image.yml" config --quiet
# 停写、排空请求后：
"${compose[@]}" stop "$APP_SERVICE"
# 此时完成并验证上面的最终备份，再继续：
"${compose[@]}" -f "$DEPLOY_DIR/compose.car-image.yml" up -d --no-deps --no-build --pull never "$APP_SERVICE"
```

`CAR_MIGRATION_IMAGE` 应由原启动方式安全传入，例如在当前 shell 导出已核验镜像引用，
不覆盖原 `.env`。全程不执行 `down -v`、volume prune、初始化脚本或全栈 `up`。
若实际部署不是 Compose，则先写出针对该部署方式的等价应用替换步骤，再操作生产。

## 4. 验收与恢复边界

入口仍处于维护状态时检查应用健康、实际版本/提交、迁移记录和启动错误；比较保留的
数据计数与关键字段，确认未进入首次安装页面、未创建替代管理员、未重新授权上游账号。
先在原域名用迁移前已有浏览器会话确认用户身份与管理权限，再验证 refresh 路径、设置、
余额、订阅和历史记录。访问令牌自然到期时应通过保留的 Redis refresh 状态续期；
已经过期、注销或撤销的会话不在保留承诺内，切换期间的长连接也不能保证不断开。

检查通过后逐步恢复入口流量，按需进行一次受控真实请求并核对账单和账号状态；
不要在验收期间批量重授权、重置订阅窗口或自动开启车分组。

启动失败时保留现场并停止新应用。**已经执行了新 SQL 时，不能仅换回 0.2.18 镜像就宣称安全。**
先比较切换前后完整迁移记录和实际 schema，结合旧应用的读写行为审核兼容性。
仅在证明没有迁移/其他写入变化，或已验证旧应用兼容新 schema 后，才考虑只回退应用。
不要删除迁移记录来强迫旧程序启动。

需要恢复数据库时，在维护窗口内停止所有写入，按已演练的方案协调恢复 PostgreSQL、
Redis、应用文件与对应旧镜像。恢复切换前快照会丢弃此后新增的用户、充值、账单与操作；
必须先保存失败现场和增量并确认处理方式。若期间上游 OAuth 已轮换令牌，旧备份中的
refresh token 可能失效，不能假定恢复数据库等于恢复上游授权。此类恢复需要单独审核，
不由本次代码同步或现有拼车版更新脚本自动执行。
