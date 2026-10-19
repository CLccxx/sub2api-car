# 定制版部署与升级

这套配置适用于单台 2 核 / 2 GB Ubuntu 服务器。应用、PostgreSQL、Redis
和 Caddy 的总容器内存上限约 1.47 GiB；应用连接池已缩小。应保留约 2 GB
Swap，并在上线后观察实际内存和并发。数据库和 Redis 只在 Docker 网络内访问，
应用调试端口只绑定 `127.0.0.1:8080`，公网入口只有 Caddy 的 80/443。

## 首次安装

使用从本仓库已验证提交构建的 Linux amd64 部署包。包内包含预编译的
`sub2api`、`BUILD_VERSION`、`BUILD_COMMIT`、运行资源以及部署文件；
`Dockerfile.goreleaser` 在服务器上只打包运行环境，不编译 Go 或前端。

1. 安装 Docker Engine 和 Compose，将域名 A 记录指向服务器，放行 TCP 80/443。
2. 将部署包解压到固定目录，例如 `/opt/sub2api-car`，进入其 `deploy` 目录。
3. 执行 `sh init-car.sh your.domain.example`。该命令只为首次安装生成密钥和
   管理员凭据，已有 `.env` 时会拒绝覆盖。
4. 执行 `sudo docker compose -f docker-compose.car.yml config --quiet` 检查配置。
5. 执行 `sudo docker compose -f docker-compose.car.yml up -d --build --wait --wait-timeout 300`。
6. 用 `sudo docker compose -f docker-compose.car.yml ps` 检查健康状态，访问
   `https://your.domain.example`。Caddy 自动申请和续期证书，HTTP 自动跳转到 HTTPS。
7. 在服务器本地读取 `.env` 中的 `ADMIN_EMAIL` 和 `ADMIN_PASSWORD` 登录。
   不要把这两个值、整个 `.env` 或首次启动的完整日志发到聊天或提交到 Git。

所有持久数据位于 `deploy/car-data/`。`.env` 保存固定的 JWT/TOTP 密钥及
数据库凭据，必须随备份保管；不要重新运行初始化来解决启动错误。

## 升级流程

日常更新使用两个 Python 3 标准库脚本，不需要配置镜像仓库、访问令牌或定时任务。
合并上游和验证业务改动仍在本地完成；发布时再执行下面的打包和升级命令。

### 本地：一条命令打包

需要 Python 3.9 或更高版本、Node.js 20、pnpm 9 或 10、Git，以及已经启动的 Docker。
先完成相关业务测试、提交代码并推送，再在仓库根目录执行：

```bash
python3 deploy/package_car.py 0.2.17-car.6
```

每次发布递增 `car.N`，不要重复使用已发布的版本号。脚本拒绝未提交的修改、
未跟踪文件和已有输出；从固定提交提取临时源码快照，排除本地未跟踪配置及前端
环境变量覆盖，按锁文件安装前端依赖，运行更新工具测试和前端构建，
再通过 Docker 交叉编译嵌入前端的 Linux amd64 程序，实际运行程序核对版本及提交号。
Go 工具链取自 `backend/go.mod`，构建缓存可重复利用。本机已有其他 Go 镜像时可以
指定 `--go-image golang:1.27.0`；实际编译工具链仍由 `go.mod` 决定。

默认打包会执行前端类型与语言完整性检查，不代表完整业务测试已经通过。
`--test-app` 会额外运行前端 lint/单元测试与后端单元测试；需要 PostgreSQL 等外部
依赖的回归和完整集成检查应在发布前由开发流程或 CI 执行。

生成的文件位于 `output/`：

- `sub2api-car-0.2.17-car.6-linux-amd64.tar.gz`：部署包。
- 同名文件再加 `.sha256`：外层校验文件。
- 同名目录：完整包内容，包含可单独上传的 `deploy/upgrade_car.py`。

包内 `SHA256SUMS` 覆盖全部文件，`BUILD_COMMIT` 记录完整提交号，
`MIGRATIONS.json` 记录与程序一致的 SQL 校验值。打包结束会调用服务器端校验器
再次验证完整包。发布包不包含 `.env`、数据库、应用数据或登录凭据。

### 服务器：上传后执行一条升级命令

将 `.tar.gz` 和对应 `.tar.gz.sha256` 两个文件上传到 `/home/ubuntu/`。
如果服务器尚未安装升级脚本（例如从 `car.5` 首次使用自动升级），再将本地的
`deploy/upgrade_car.py` 单独上传到 `/home/ubuntu/`，首次执行：

```bash
sudo python3 /home/ubuntu/upgrade_car.py /home/ubuntu/sub2api-car-0.2.17-car.6-linux-amd64.tar.gz
```

首次成功后，脚本会随程序安装到固定目录。后续更新只需上传两个发布文件，执行：

```bash
sudo python3 /opt/sub2api-car/deploy/upgrade_car.py /home/ubuntu/sub2api-car-0.2.17-car.7-linux-amd64.tar.gz
```

脚本自动完成以下步骤：

1. 验证外层校验值、包内每个文件、版本和迁移清单，并创建私有暂存目录。
2. 构建新镜像并核对实际程序版本；此时原服务继续运行。
3. 在线导出 PostgreSQL 并验证备份可读，备份私有配置、应用数据和旧程序。
4. 保留现有 Compose/Caddy 配置，仅修改 `.env` 中的 `SUB2API_CAR_VERSION`，
   更新程序资源并切换 `sub2api` 容器。PostgreSQL、Redis、Caddy 保持运行。
5. 检查新容器健康状态和实际版本，输出升级结果和备份位置。

切换单实例应用会有短暂中断，已有长请求可能断开；建议低流量时执行。
看到成功提示后，再打开网站核对登录、用户、订阅及额度显示。
重复运行同一版本且运行中的程序提交号一致、健康时会直接结束；同一版本号对应
不同提交会被拒绝。升级脚本不会运行初始化、删除旧镜像或清理历史备份。

只检查上传包、不操作安装或 Docker，可执行：

```bash
python3 /home/ubuntu/upgrade_car.py /home/ubuntu/sub2api-car-0.2.17-car.6-linux-amd64.tar.gz --check-only
```

缺少相邻校验文件时可用 `--sha256 完整校验值`。脚本也支持 `--install-dir`、
`--updates-dir`、`--backups-dir` 指定自定义目录，默认分别为 `/opt/sub2api-car`、
`/opt/sub2api-car-updates`、`/opt/sub2api-car-backups`。

### 失败与恢复

镜像构建、版本检查或备份失败会在切换前停止，原应用继续运行。切换后的失败只有
在新包迁移清单证明没有待执行或改动过的 SQL，且失败后数据库迁移记录完全不变时，
才自动恢复旧应用及原配置。存在新迁移、迁移记录变化或无法确认状态时，只停止
失败的新应用，保留备份和现场并给出恢复提示，不自动恢复数据库。

旧包缺少迁移清单时仍可校验和升级，但不能自动回退。不要在失败后直接重跑初始化
或手工恢复数据库；应先核对错误阶段、运行版本和迁移状态。需要调整 Compose、
Caddy、数据库大版本或其他基础设施配置的升级，应单独审查配置变更。

备份目录包含凭据及业务数据，目录权限为 `0700`，私有文件为 `0600`。
数据库与应用文件在运行中分别备份，适合升级前保护；它们不是跨存储的同一时刻快照。
请自行保留必要历史并将备份安全复制到服务器之外，不要把备份或 `.env` 发到聊天。

不要用管理后台的上游“一键更新”、上游安装脚本或官方 `latest` 镜像更新定制版，
这些渠道不包含本仓库的拼车代码。不要通过删除重置事件/投票表来解锁操作；
这些记录承载防重复消费与防重复清空额度的保护。

## 拼车周额度计算

北京时间每 30 分钟（每小时的 00 分、30 分），根据共享账号的有效用量估算总额度，
先向上取整到 50 美元的整数倍，再按人数均分并向下保留两位小数。
例如 520 取整为 550，560 取整为 600；550、600 保持不变，550.5 取整为 600。
总额估算为 520 美元时，四人车每人周额度为 550 / 4 = 137.50 美元。

上游使用比例为 0%、估算数据无效或总额不足 10 美元时，保留原来的额度上限。
调整上限不会清零已用额度；向上取整不增加上游实际额度，也不构成额度下调保护。

## 常用检查

```bash
sudo docker compose -f docker-compose.car.yml ps
curl -fsS http://127.0.0.1:8080/health
sudo docker compose -f docker-compose.car.yml logs --tail=80 caddy
sudo docker stats --no-stream
```

若调整 Docker 子网，同时修改 `caddy.networks.car.ipv4_address` 和应用的
`SERVER_TRUSTED_PROXIES`，确保客户端 IP 只从可信 Caddy 容器传入。
