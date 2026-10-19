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

1. 在本地合并上游正式版本，保留定制功能，解决冲突并运行测试和构建。
2. 推送本仓库，并从同一提交构建新的部署包。通过 `BUILD_COMMIT` 追踪来源。
3. 更新前停应用，保留数据库运行，导出数据库并备份配置和应用数据：

   ```bash
   umask 077
   sudo docker compose -f docker-compose.car.yml stop sub2api
   sudo docker compose -f docker-compose.car.yml exec -T postgres pg_dump -U sub2api -d sub2api -Fc > ../database-before-upgrade.dump
   sudo tar -czf ../config-before-upgrade.tar.gz .env car-data/app Caddyfile.car
   sudo chmod 600 ../database-before-upgrade.dump ../config-before-upgrade.tar.gz
   ```

   备份包含凭据与业务数据，请限制读取权限并保存在服务器之外。更新包只替换
   二进制、资源和部署文件，必须保留 `.env` 与 `car-data/`。将 `.env` 中的
   `SUB2API_CAR_VERSION` 改成新包的 `BUILD_VERSION`，再运行启动命令。
4. 检查 `/health`、版本号、登录、额度显示和投票状态。数据库迁移在启动时执行。
   如需回滚，使用上个部署包；若迁移已改变数据库结构，还须恢复对应数据库备份。

不要用管理后台的上游“一键更新”、上游安装脚本或官方 `latest` 镜像更新定制版，
这些渠道不包含本仓库的拼车代码。不要通过删除重置事件/投票表来解锁操作；
这些记录承载防重复消费与防重复清空额度的保护。

## 拼车周额度计算

北京时间每两小时的整点（00:00、02:00、04:00……22:00），根据共享账号的有效用量估算总额度，
先向下取整到 10 美元的整数倍，再按人数均分并向下保留两位小数。
例如总额估算为 1374.65 美元，四人车每人周额度为 1370 / 4 = 342.50 美元。

上游使用比例为 0%、估算数据无效或总额不足 10 美元时，保留原来的额度上限。
调整上限不会清零已用额度；整十取整仍有差额，也不构成额度下调保护。

## 常用检查

```bash
sudo docker compose -f docker-compose.car.yml ps
curl -fsS http://127.0.0.1:8080/health
sudo docker compose -f docker-compose.car.yml logs --tail=80 caddy
sudo docker stats --no-stream
```

若调整 Docker 子网，同时修改 `caddy.networks.car.ipv4_address` 和应用的
`SERVER_TRUSTED_PROXIES`，确保客户端 IP 只从可信 Caddy 容器传入。
