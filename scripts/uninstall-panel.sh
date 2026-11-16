#!/usr/bin/env bash
# Remove only the selected panel. PostgreSQL and shared Agent tools are retained.
set -Eeuo pipefail
SERVICE=nekopass; PURGE=0; YES=0; DRY_RUN=0
die() { printf '[nekopass] %s\n' "$*" >&2; exit 1; }
usage() {
 cat <<'HELP'
Nekopass 面板卸载（以 root 执行）
  bash uninstall-panel.sh [--service-name NAME] [--purge] [--yes] [--dry-run]
  --service-name NAME  服务名：nekopass 或 nekopass-panel-xxx
  --purge              同时删除面板配置、状态和符合条件的同名数据库
  --yes                不询问确认（包括 --purge 的数据删除）
  --dry-run            显示计划，不修改系统

默认保留配置、状态和数据库。--purge 要求 control.env 中的数据库地址
仍为安装器生成的本地地址，且数据库归属同名角色。PostgreSQL 集群、角色、
系统用户、节点、共享工具及防火墙规则均保留。卸载不会自动删除依赖软件。
HELP
}
while (($#)); do
 case "$1" in
  -h|--help) usage; exit 0;;
  --service-name) (($# >= 2)) && [[ -n "$2" ]] || die '服务名缺少值'; SERVICE=$2; shift 2;;
  --purge) PURGE=1; shift;;
  --yes) YES=1; shift;;
  --dry-run) DRY_RUN=1; shift;;
  *) die "未知参数：$1";;
 esac
done
[[ "$SERVICE" == nekopass || "$SERVICE" =~ ^nekopass-panel-[a-z0-9]{1,16}$ ]] || die '服务名不正确'
BASE="/opt/$SERVICE"; CONFIG="/etc/$SERVICE"; STATE="/var/lib/$SERVICE"
UNIT_DIR=/etc/systemd/system
printf '[nekopass] 卸载服务 %s；清除面板配置、状态及数据库：%s\n' "$SERVICE" "$([[ $PURGE == 1 ]] && printf 是 || printf 否)"
if ((DRY_RUN)); then exit 0; fi
[[ $(id -u) -eq 0 ]] || die '请以 root 执行'
[[ -d /run/systemd/system ]] && command -v systemctl >/dev/null || die '需要正在运行的 systemd'
# Reject symlinks, including parent components, before touching any installation path.
for target in "$BASE" "$BASE/bin" "$BASE/bin/nekopass" "$BASE/web" "$CONFIG" "$CONFIG/control.env" "$CONFIG/tls" "$CONFIG/tls/server.crt" "$CONFIG/tls/server.key" "$STATE" "$UNIT_DIR/$SERVICE.service" "$UNIT_DIR/$SERVICE.service.d" "$UNIT_DIR/$SERVICE.service.d/update.conf" "$UNIT_DIR/$SERVICE-update.service" "$UNIT_DIR/$SERVICE-update.path"; do
 [[ ! -L "$target" && "$(realpath -m -- "$target")" == "$target" ]] || die "目标路径包含符号链接：$target"
done
if ((PURGE)); then
 [[ -f "$CONFIG/control.env" ]] || die '缺少面板配置，无法安全确定数据库；请手动核实并清理'
 command -v python3 >/dev/null && command -v runuser >/dev/null && command -v psql >/dev/null && command -v dropdb >/dev/null || die '清除数据库需要 python3、runuser 和 PostgreSQL 客户端工具'
 DB_PORT=$(python3 - "$CONFIG/control.env" "$SERVICE" <<'PY'
import pathlib,sys,urllib.parse
try:
 lines=pathlib.Path(sys.argv[1]).read_text().splitlines()
 values=[line.split('=',1)[1] for line in lines if line.startswith('NEKOPASS_DATABASE_URL=')]
 if len(values)!=1: raise ValueError()
 u=urllib.parse.urlsplit(values[0]); q=urllib.parse.parse_qs(u.query,strict_parsing=True)
 if (u.scheme!='postgres' or u.username!=sys.argv[2] or u.password is not None or
     u.hostname is not None or u.path!='/'+sys.argv[2] or u.fragment or
     set(q)!={'host','port','sslmode'} or q['host']!=['/var/run/postgresql'] or q['sslmode']!=['disable'] or
     len(q['port'])!=1 or not q['port'][0].isascii() or not q['port'][0].isdigit() or not 1<=int(q['port'][0])<=65535): raise ValueError()
 print(int(q['port'][0]))
except (ValueError,OSError,UnicodeError):
 sys.exit('数据库地址不是安装器生成的本机地址；请手动核实并清理')
PY
 ) || die '无法安全确定数据库'
 owner=$(cd / && runuser -u postgres -- env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin psql -h /var/run/postgresql -p "$DB_PORT" -U postgres -d postgres -XAtqc "SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname='$SERVICE'") || die '无法连接 PostgreSQL；未卸载或删除数据'
 [[ -z "$owner" || "$owner" == "$SERVICE" ]] || die '同名数据库不属于该服务；未卸载或删除数据'
fi
if ((!YES)); then
 answer=''
 if [[ -t 0 ]]; then read -r -p "输入服务名 $SERVICE 确认卸载：" answer || die '未确认卸载'
 else read -r -p "输入服务名 $SERVICE 确认卸载：" answer </dev/tty || die '无法交互确认，请使用 --yes'; fi
 [[ "$answer" == "$SERVICE" ]] || die '已取消卸载'
fi
# Verify systemd is reachable before modifying any unit. A communication error
# must not be mistaken for an inactive or absent service.
units=("$SERVICE-update.path" "$SERVICE-update.service" "$SERVICE.service")
states=()
for unit in "${units[@]}"; do
 state=$(systemctl show "$unit" --property=LoadState --value) || die "无法查询 $unit"
 [[ "$state" == loaded || "$state" == not-found ]] || die "$unit 状态异常：$state"
 states+=("$state")
done
# Stop the watcher first so no update starts during removal.
for i in "${!units[@]}"; do
 unit=${units[$i]}; [[ "${states[$i]}" == loaded ]] || continue
 systemctl stop "$unit" || die "无法停止 $unit"
 if [[ "$unit" != "$SERVICE-update.service" ]]; then
  systemctl disable "$unit" || die "无法禁用 $unit"
 fi
done
if ((PURGE)) && [[ -n "$owner" ]]; then
 (cd / && runuser -u postgres -- env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin dropdb -h /var/run/postgresql -p "$DB_PORT" -U postgres --maintenance-db=postgres --force -- "$SERVICE") || die '删除数据库失败；面板服务已停止，文件和配置仍保留'
fi
rm -f -- "$UNIT_DIR/$SERVICE.service" "$UNIT_DIR/$SERVICE-update.service" "$UNIT_DIR/$SERVICE-update.path" "$UNIT_DIR/$SERVICE.service.d/update.conf"
rmdir -- "$UNIT_DIR/$SERVICE.service.d" 2>/dev/null || true
systemctl daemon-reload
rm -f -- "$BASE/bin/nekopass"
rm -rf -- "$BASE/web"
if ((PURGE)); then
 rm -f -- "$CONFIG/control.env" "$CONFIG/tls/server.crt" "$CONFIG/tls/server.key"
 rmdir -- "$CONFIG/tls" "$CONFIG" 2>/dev/null || true
 rm -rf -- "$STATE"
fi
printf '[nekopass] 面板已卸载。PostgreSQL、节点、共享管理和更新工具及防火墙规则已保留。\n'
