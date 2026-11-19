# 日常管理

## 服务命令

交互菜单：`sudo nekopassctl`。`panel` 可替换为 `agent`：

```bash
sudo nekopassctl panel start       # 启动
sudo nekopassctl panel stop        # 停止
sudo nekopassctl panel restart     # 重启
sudo nekopassctl panel status      # 状态
sudo nekopassctl panel enable      # 开机自启
sudo nekopassctl panel disable     # 关闭自启
sudo nekopassctl panel logs        # 最近日志
sudo nekopassctl panel follow      # 实时日志
sudo nekopassctl panel edit        # 编辑配置
sudo nekopassctl agent configure   # 节点连接配置
sudo nekopassctl panel check-update # 检查更新
sudo nekopassctl panel update       # 更新主控
```

修改配置后执行 `restart`。`enable` / `disable` 只更改自启。

## 重置管理员密码

```bash
sudo nekopassctl panel reset-password
```

命令显示新的随机密码，并注销该管理员的旧会话。

## 更新面板

使用 CLI 检查并更新；会自动下载 GitHub 发布包、升级数据库并重启服务：

```bash
sudo nekopassctl panel check-update
sudo nekopassctl panel update
```

节点升级见 [安装节点](./node.md#升级)。

面板可在「管理后台 → 系统设置 → 软件更新 → 立即更新」升级，页面会显示进度。后台与命令行共用 `nekopassctl panel update`；更新组件由安装和升级流程自动部署，无需额外配置。

更新中断后，再次执行同版本或更新版本的更新命令即可继续。

## 更新 PostgreSQL

面板更新会迁移面板的数据库结构，但不会更新 PostgreSQL 服务端。以下步骤适用于安装脚本在 Debian / Ubuntu 上配置的本机 PostgreSQL；远程、容器或托管数据库请参考相应平台的升级说明。

### 查看实际版本

在面板服务器执行：

```bash
systemctl cat nekopass
pg_lsclusters
```

安装器生成的服务文件中，`Requires=postgresql@17-main.service` 这样的行表示使用 PostgreSQL 17 的 `main` 集群。在 `pg_lsclusters` 输出中找到对应的 `Ver` 和 `Cluster`，记下 `Port`。如果安装时指定了其他面板服务名，将上面的 `nekopass` 换成实际服务名。如果服务文件没有明确列出集群，或数据库连接配置已手动修改，请先确认实际连接的数据库，不要猜测端口。

例如端口为 `5432`，查询正在运行的服务端版本：

```bash
sudo -u postgres psql -h /var/run/postgresql -p 5432 -d postgres -XAtc 'SHOW server_version;'
```

请将 `5432` 换成实际端口。`psql --version` 显示的是客户端版本，可能与服务端不同。不要公开分享 `/etc/nekopass/control.env` 中的数据库连接字符串，其中可能包含密码。

### 更新同一主版本

以下以 PostgreSQL **17**、集群 **main**、端口 **5432** 和默认面板数据库 **nekopass** 为例。执行前将它们替换为实际值；自定义服务名安装的数据库默认与服务同名。

1. 安排维护时间并备份。软件包更新可能重启同一主版本的多个集群，影响使用这些集群的面板和其他服务。确保有可恢复的备份，并在独立环境验证恢复。默认面板数据库的备份示例：

   ```bash
   umask 077
   sudo -u postgres pg_dump -h /var/run/postgresql -p 5432 -Fc -d nekopass > nekopass-backup.dump
   ```

   备份文件包含敏感数据，请妥善保存。此命令只备份指定数据库，不包含其他数据库或全局角色；共享集群应按实际情况备份其他数据和配置。恢复方法参见 [PostgreSQL 官方备份文档](https://www.postgresql.org/docs/current/backup-dump.html)。

2. 刷新现有软件源，查看已安装版本和可用版本：

   ```bash
   sudo apt-get update
   apt-cache policy postgresql-17
   ```

   `Installed` 是安装的包版本，`Candidate` 是当前软件源准备安装的版本。可对照 [PostgreSQL 安全公告](https://www.postgresql.org/support/security/)和发行版安全公告判断修复情况；发行版可能将修复回移植到自己的软件包。软件源没有所需更新时，先检查发行版及软件源的支持情况，不要直接更换源。

3. 确认更新计划后，更新该主版本的服务端软件包：

   ```bash
   sudo apt-get install --only-upgrade postgresql-17
   ```

   根据提示检查将要更新的软件包。这条命令更新服务端包及必要依赖，其他客户端和扩展包需根据使用情况检查。更新失败时先处理错误，再进行后续操作。

4. 软件包更新后，需要重启对应集群才能确保加载新程序。更新过程可能已经自动重启；如果无法确认，可在维护时间手动重启：

   ```bash
   sudo pg_ctlcluster 17 main restart
   ```

   重启会中断数据库连接。之后用 `pg_lsclusters` 确认集群在线，再次查询服务端版本，并检查面板状态和访问情况：

   ```bash
   sudo systemctl status nekopass
   ```

   如果数据库重启后面板未运行，确认数据库已正常启动后执行 `sudo systemctl start nekopass`，再检查状态和日志。发行版回移植修复时，上游版本号可能不变；还应使用 `apt-cache policy postgresql-17` 核对已安装包版本及发行版安全公告。

同一主版本的小版本更新通常不需要导出后重新导入数据，但应阅读目标版本以及跳过版本的[发行说明](https://www.postgresql.org/docs/release/)，确认是否需要重建索引或执行其他更新后操作。跨主版本（例如 17 升到 18）需要单独规划数据迁移，检查扩展兼容性、面板连接端口和 systemd 集群依赖；不能仅安装新版本软件包。参见 [PostgreSQL 官方升级文档](https://www.postgresql.org/docs/current/upgrading.html)和[版本支持策略](https://www.postgresql.org/support/versioning/)。

不要使用面板安装脚本的 `--reinstall` 更新数据库：该选项会清空面板数据库。

## 功能入口

| 功能 | 文档 |
| --- | --- |
| 注册、SMTP、条款和邀请返利 | [注册与邀请](./account.md) |
| 充值、购买和续费 | [商城与余额](./shop.md) |
| 用户、个人资源和套餐 | [用户与套餐](./users.md) |
| 节点、节点组、探针和 DDNS | [节点与节点组](./nodes.md) |
| 工单和客服对话 | [工单](./tickets.md) |
| 首页多公告 | [站点公告](./announcements.md) |
| 用户与管理员订单 | [订单查询](./orders.md) |
| Telegram 管理与 IP / DDNS 通知 | [Telegram Bot](./telegram.md) |
| 目标 IP 限制、PROXY 来源和管理密钥 | [转发安全](./security.md) |
