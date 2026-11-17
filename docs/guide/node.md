# 安装节点

支持 Linux、amd64 / arm64，要求以 root 执行且 systemd 正在运行。安装使用系统服务，不需要 Docker。

## 一键接入

在后台「节点管理」创建节点，复制该节点的「安装命令」，以 root 在节点服务器执行。

命令使用 `wget … -O nekopass-install-agent.sh && bash nekopass-install-agent.sh …`，参数直接包含「系统设置 → Agent 通信地址」、节点密钥、版本和下载源。节点地址使用 `https://`（公共 CA 可信证书）或测试用 `http://`（明文 HTTP/2），不附带 CA 参数。命令可重复使用，自动判断首次安装或覆盖安装；传入的连接参数会覆盖配置文件。同一节点密钥和主控主机及端口的状态会保留；更换密钥、主机或端口时会归档原状态。

节点显示「在线 / 已同步」后即可使用。端口范围、入口出口和探针设置均在后台修改。

## 安装过程

节点安装脚本会执行以下操作：

1. 检查 `curl` 和 Python 3；缺少时，通过 `apt`、`dnf` 或 `yum` 安装它们、CA 证书和基础工具。
2. 根据服务器架构从配置的 HTTPS 下载源获取 Agent 程序，并检查程序能否运行及其版本。
3. 创建不可登录的系统用户和服务组，安装 Agent 和更新工具，将面板地址与节点密钥写入配置文件。发布版安装脚本还会安装 `nekopassctl`。
4. 注册并启动 Agent systemd 服务，设置开机自启。Agent 连接面板后接收转发规则、限速等运行配置。

默认服务名为 `nekopass-agent`，主要文件如下：

| 内容 | 默认位置 |
| --- | --- |
| Agent 程序和更新工具 | `/opt/nekopass/bin/` |
| 连接地址和节点密钥 | `/etc/nekopass/agent.env` |
| 节点状态 | `/var/lib/nekopass-agent/state.db` |
| 服务管理命令 | `/usr/local/bin/nekopassctl` |
| systemd 服务 | `/etc/systemd/system/nekopass-agent.service` |

节点脚本不安装 PostgreSQL，也不会自动修改防火墙。请根据实际转发和隧道端口配置服务器防火墙及云安全组。更新工具通过 systemd 监听更新请求，供面板或管理命令触发；它不会定时自动升级。

重新安装会停止服务并替换程序及连接配置。覆盖安装失败时脚本会尝试恢复原有安装；状态保留和归档规则见下方的「抢占实例重建」。

## 下载配置

安装文件默认从 GitHub Releases 下载，新仓库需要先发布对应的脚本和 Agent 程序。在「系统设置 → 节点安装」可修改下载配置：

| 设置 | 示例 |
| --- | --- |
| 安装脚本地址 | `https://github.com/uyo8os/nekopass/releases/latest/download/install-agent.sh` |
| 版本下载根地址 | `https://github.com/uyo8os/nekopass/releases/download` |
| Agent 版本 | `latest`（最新正式版，也可填写版本号） |

节点连接信息由安装参数写入 `/etc/nekopass/agent.env`，只需 `NEKOPASS_SERVER` 和 `NEKOPASS_NODE_TOKEN`。规则、限速等运行配置通过 gRPC 下发；节点无需连接网页端口。

## 密钥与连接

节点密钥可在「节点管理 → 编辑」的基本信息中查看、复制和修改，留空保存会生成新密钥。已安装节点更换密钥后，需要更新其连接配置并重启：

```bash
sudo nekopassctl agent configure
sudo nekopassctl agent restart
```

生产环境填写 `https://域名` 或 `https://域名:端口`，证书由公共 CA 签发并覆盖该域名；省略端口时 HTTPS 默认使用 443，HTTP 默认使用 80。测试环境可填写 `http://IP:端口`。`NEKOPASS_SERVER` 未带协议的旧地址仍需指定端口，并按 TLS 连接。旧版安装脚本及 Agent 需要显式填写端口，例如 `https://域名:443`。

## 升级

在「节点管理」检查更新并点击节点的「更新」，或在节点执行：

```bash
sudo nekopassctl agent check-update
sudo nekopassctl agent update
```

老版本先重新运行新版节点安装命令，即可使用在线更新。

目标 IP 限制使用 v0.15.2 或以上节点；raw(udp) 中转使用 v0.17.1 或以上的面板及节点。

连接配置位于 `/etc/nekopass/agent.env`，运行状态位于 `/var/lib/nekopass-agent/state.db`。新增服务器时，在后台创建对应节点。

## 卸载节点

从 GitHub Releases 下载卸载脚本，在节点服务器执行：

```bash
wget https://github.com/uyo8os/nekopass/releases/latest/download/uninstall-agent.sh -O uninstall-agent.sh
sudo bash uninstall-agent.sh
```

确认后，脚本会停止并移除该节点及其更新服务和 Agent 程序，现有转发会中断。默认保留节点配置（包括密钥）和本地状态，以便重新接入；面板中的节点记录不会删除。

如需同时永久删除该节点的配置和状态，先备份，再执行：

```bash
sudo bash uninstall-agent.sh --purge
```

指定实例可加 `--service-name nekopass-agent-xxx`；`--dry-run` 显示计划，`--yes` 跳过确认，包括数据删除确认。脚本保留系统用户、依赖软件、防火墙规则以及同机面板和共享管理命令。默认节点与默认面板共用 `/opt/nekopass`：另一端仍在使用时保留其文件和更新工具；否则删除不再使用的更新工具并移除空目录。目录中其他文件不会被递归删除。卸载脚本作为新 Release 资产发布后才能从上述地址下载；也可从源码目录运行 `scripts/uninstall-agent.sh`。

## 抢占实例重建

同一节点的服务器完全重建后，重新执行原安装命令即可接入。此功能使用 v0.16.1 或以上的面板及节点，每个节点对应一台服务器。

状态文件丢失时，节点使用原密钥获取主控保存的计数并恢复上线。历史已上报流量保留，旧节点尚未结算的有限流量授权按已消耗处理。

重新安装时，传入参数覆盖连接配置。更换密钥或主控主机及端口会将原状态归档为 `state.db.before-reconfigure-*`；仅切换同一主机和端口的 HTTP / HTTPS 会保留状态。

## 主控暂时离线

节点保留最后下发的配置并持续重连。有限额度用户只能使用该节点已获得的剩余授权，耗尽后等待主控恢复；无限额度用户仍受缓存权限和有效期限制。用量保存在状态文件中，恢复连接后继续上报。

## 动态 IP / DDNS

在节点列表点击「DDNS」，配置 Cloudflare、记录域名、IPv4 / IPv6 和同步参数。完整设置见 [节点与节点组](./nodes.md#ddns)。

## 入口、出口与传输协议

在「节点管理 → 编辑」中，点击「传输协议」旁的「编辑传输协议」，打开子窗口。关闭子窗口会返回节点编辑，保留未保存的基础信息。

入口和出口可同时启用。入口设置是否允许作为入口和是否允许直转。出口分别选择传输协议和安全性：raw(tcp)、h2、raw(udp) 均可搭配明文或 TLS；raw(udp) 仅用于 UDP。uTLS 用于 raw(tcp)、h2 的 TLS 模式。Host、Path、Fallback 和流控仅用于 h2；入口统一使用所选出口的配置。UDP 配置见 [UDP 转发](../protocols/udp.md)。

出口配置始终显示。关闭「作为出口节点」时仍可编辑、保存参数和关联入口，但不开放隧道监听、不启动证书申请；启用后才生效。开关切换不会清空已填配置。

节点编辑保留基本信息、端口范围、连接限制和探针设置。已有节点的协议配置独立保存；添加节点时先在子窗口确定协议，再保存节点即可创建。
