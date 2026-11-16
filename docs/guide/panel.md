# 安装面板

支持 Debian / Ubuntu、amd64 / arm64，要求以 root 执行且 systemd 正在运行。安装使用系统服务，不需要 Docker。

## 本地安装包

将 `install-panel.sh` 与对应架构的安装包放在同一目录：

```bash
sudo bash install-panel.sh --package ./nekopass-panel-linux-amd64.tar.gz
```

ARM64 使用 `nekopass-panel-linux-arm64.tar.gz`。指定本地安装包只省去面板包下载；安装系统依赖仍可能需要访问软件源。

## 网络安装

从 GitHub Releases 下载：

```bash
wget https://github.com/uyo8os/nekopass/releases/latest/download/install-panel.sh -O install-panel.sh && sudo bash install-panel.sh
```

## 安装过程

安装脚本会交互询问面板域名或 IP、端口、管理员账号、站点名称、数据库来源和节点连接方式。自动检测的地址可能是内网 IP，请确认用户和节点能访问；也可通过参数指定。脚本默认执行以下操作：

1. 更新软件包列表，安装 `curl`、CA 证书、Python 3、OpenSSL 和 PostgreSQL 管理工具。选择系统源或 PostgreSQL 官方源后，安装或使用已有的 PostgreSQL 14 及以上版本；选择官方源时会添加对应软件源。
2. 下载匹配架构的面板发布包，或使用指定的本地包；创建或复用数据库集群，以及面板所需的系统用户、数据库用户和数据库。
3. 安装面板程序、网页文件、管理命令和更新工具，迁移数据库并初始化面板设置。如果没有已有管理员，则创建管理员并随机生成密码，安装完成时显示。
4. 注册并启动面板 systemd 服务，设置面板和数据库开机自启，并检查面板是否就绪。
5. 默认在正在运行的 UFW 或 firewalld 中放行网页端口，以及非代理模式下的节点控制端口。云服务器安全组仍需自行放行。

发布安装脚本使用打包时指定的版本下载面板包；仓库源码脚本的默认版本可通过 `--help` 查看，也可用 `--version` 指定。新仓库需要先发布对应版本的安装脚本和程序包。

默认服务名为 `nekopass`，主要文件如下：

| 内容 | 默认位置 |
| --- | --- |
| 面板程序、网页和更新工具 | `/opt/nekopass/` |
| 面板配置 | `/etc/nekopass/control.env` |
| 面板运行状态 | `/var/lib/nekopass/` |
| 服务管理命令 | `/usr/local/bin/nekopassctl` |
| systemd 服务 | `/etc/systemd/system/nekopass.service` |

更新工具通过 systemd 监听更新请求，供面板或管理命令触发；它不会定时自动升级。

| 默认入口 | 端口 |
| --- | --- |
| 网页 HTTP | `8080` |
| 节点控制连接 | `9443` |

打开 `http://服务器地址:8080`。云服务器还需放行安全组端口。

节点连接可选 `plain`（测试明文 HTTP/2）、`existing`（已有公共 CA 证书）或 `proxy`（已有代理终止 TLS）；默认 `plain`，不生成自签 CA。脚本不安装 Nginx；网页 HTTPS 需自行配置 [反向代理](./proxy.md)。

常用参数：

| 参数 | 用途 |
| --- | --- |
| `--host` | 用户和节点能访问的域名 / IP。 |
| `--http-port` / `--grpc-port` | 网页和节点控制监听端口。 |
| `--agent-host` / `--agent-port` | 节点连接的外部地址 / 端口，适用于代理或 NAT。 |
| `--postgres-source` / `--postgres-version` | 数据库来源和主版本。 |
| `--admin` / `--site-name` | 初始管理员和站点名称。 |
| `--firewall skip` | 跳过自动防火墙放行。 |
| `--skip-dependencies` | 已具备所需依赖时跳过软件包安装。 |
| `--reinstall` | 清空该服务的面板数据库并全新安装。 |
| `--yes` | 使用参数与默认值，不交互询问。 |
| `--dry-run` | 只验证参数，不安装。 |

全部参数：`bash install-panel.sh --help`。忘记密码见 [日常管理](./manage.md#重置管理员密码)。

## 重新安装

默认以 `control.env` 是否存在判断已有安装；配置不存在时允许重跑，复用同名数据库并保留已有管理员。

清空数据库并全新安装：

```bash
sudo bash install-panel.sh --reinstall
```

**此操作会删除所选 PostgreSQL 实例中该服务的全部面板数据，包括用户、余额、订单、套餐、节点和规则，并重新生成管理员密码。** 执行前自行备份。交互时输入服务名确认；加 `--yes` 会直接执行，不再确认。

只处理 `--service-name` 指定的面板，默认 `nekopass`；同机节点端的配置与状态保留。原节点密钥会随面板数据库清空，节点需在新面板重新添加并配置密钥。

## 卸载面板

从 GitHub Releases 下载卸载脚本，在面板服务器执行：

```bash
wget https://github.com/uyo8os/nekopass/releases/latest/download/uninstall-panel.sh -O uninstall-panel.sh
sudo bash uninstall-panel.sh
```

脚本会要求输入服务名确认，然后停止并移除面板及其更新服务、面板程序和网页。默认保留面板配置、状态和数据库；因此默认配置文件仍存在时，安装脚本会拒绝直接覆盖，重新部署前请先备份并处理原配置。卸载会中断面板服务。

如需同时永久删除面板配置、状态和同名数据库，先备份，再执行：

```bash
sudo bash uninstall-panel.sh --purge
```

`--purge` 仅在配置仍使用安装器生成的本地 PostgreSQL 连接、同名数据库归属同名角色时处理数据库；无法安全确认时会停止并提示手动处理。数据库可能是安装时复用的，清除前请确认其内容。指定实例可加 `--service-name nekopass-panel-xxx`；`--dry-run` 显示计划，`--yes` 跳过确认，包括数据删除确认。

脚本保留 PostgreSQL 集群、数据库角色、系统用户、依赖软件、防火墙规则以及同机节点和共享管理命令。默认面板与默认节点共用 `/opt/nekopass`：另一端仍在使用时保留其文件和更新工具；否则删除不再使用的更新工具并移除空目录。目录中其他文件不会被递归删除。卸载脚本作为新 Release 资产发布后才能从上述地址下载；也可从源码目录运行 `scripts/uninstall-panel.sh`。

如果旧版脚本已清除配置但留下更新工具，可运行新版脚本的普通卸载（不加 `--purge`）重试清理。普通卸载不会检查或删除数据库；数据库是否已清除需另行确认。
