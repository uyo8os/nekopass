#!/usr/bin/env bash
# Remove one native Agent installation without touching a colocated panel.
set -Eeuo pipefail

SERVICE=nekopass-agent
PURGE=0; YES=0; DRY_RUN=0
usage() {
 cat <<'HELP'
Nekopass Agent 卸载脚本（Linux + systemd，需要 root）

  bash uninstall-agent.sh [--service-name NAME] [--purge] [--yes] [--dry-run]

  --service-name NAME  指定节点服务，默认为 nekopass-agent
  --purge              同时删除该节点的配置和本地状态，无法恢复
  --yes                不再交互确认
  --dry-run            仅显示操作，不修改文件或服务
  -h, --help           显示帮助

默认保留配置（含节点 token）和状态，以便使用原连接信息重新安装。
共享的 nekopassctl、系统用户和依赖不会被删除；面板仍在使用时保留共享更新工具。
卸载不会删除面板中的节点记录，也不会撤销防火墙规则。
HELP
}
die() { printf '[nekopass] 错误：%s\n' "$*" >&2; exit 1; }
while (($#)); do
 case "$1" in
  --service-name) (($# >= 2)) && [[ -n "$2" ]] || die '缺少服务名'; SERVICE=$2; shift 2;;
  --purge) PURGE=1; shift;;
  --yes) YES=1; shift;;
  --dry-run) DRY_RUN=1; shift;;
  -h|--help) usage; exit 0;;
  *) die "未知选项：$1";;
 esac
done
[[ "$SERVICE" =~ ^nekopass-agent(-[a-z0-9]{1,16})?$ ]] || die '无效的节点服务名'
if [[ "$SERVICE" == nekopass-agent ]]; then
 BIN=/opt/nekopass/bin/nekopass-agent; CONFIG_DIR=/etc/nekopass
else
 BIN="/opt/$SERVICE/bin/nekopass-agent"; CONFIG_DIR="/etc/$SERVICE"
fi
ENV_FILE="$CONFIG_DIR/agent.env"; STATE_DIR="/var/lib/$SERVICE"
UNIT_DIR=/etc/systemd/system
printf '[nekopass] 将卸载节点 %s，停止其服务和更新服务并删除程序及服务文件。\n' "$SERVICE"
if ((PURGE)); then
 printf '[nekopass] 将永久删除 %s 和 %s。\n' "$ENV_FILE" "$STATE_DIR"
else
 printf '[nekopass] 将保留配置 %s 和状态 %s。\n' "$ENV_FILE" "$STATE_DIR"
fi
((DRY_RUN)) && exit 0
[[ $(id -u) -eq 0 ]] || die '请以 root 运行'
command -v systemctl >/dev/null && [[ -d /run/systemd/system ]] || die '需要正在运行的 systemd'
# Reject symlinked parents before any change; never recursively follow an unexpected path.
for target in "$(dirname "$BIN")" "$CONFIG_DIR" "$STATE_DIR" "$UNIT_DIR"; do
 [[ "$(realpath -m "$target")" == "$target" ]] || die "拒绝符号链接路径：$target"
done
for target in "$BIN" "$ENV_FILE" "$UNIT_DIR/$SERVICE.service" "$UNIT_DIR/$SERVICE-update.path" "$UNIT_DIR/$SERVICE-update.service"; do
 [[ ! -L "$target" ]] || die "拒绝符号链接：$target"
done
if ((!YES)); then
 [[ -r /dev/tty ]] || die '需要交互确认；无人值守时请显式传入 --yes'
 printf '确认继续？输入 yes：' > /dev/tty
 IFS= read -r answer < /dev/tty || die '未确认，已取消'
 [[ "$answer" == yes ]] || die '未确认，已取消'
fi
# Check every unit before stopping anything. Unexpected load states require attention.
for unit in "$SERVICE-update.path" "$SERVICE-update.service" "$SERVICE.service"; do
 load_state=$(systemctl show "$unit" -p LoadState --value) || die "无法检查 $unit，未删除安装文件"
 case "$load_state" in loaded|not-found) ;; *) die "$unit 状态异常：$load_state";; esac
done
# Stop the watcher first, then any updater already running, then the Agent.
for unit in "$SERVICE-update.path" "$SERVICE-update.service" "$SERVICE.service"; do
 load_state=$(systemctl show "$unit" -p LoadState --value) || die "无法检查 $unit，未删除安装文件"
 case "$load_state" in loaded|not-found) ;; *) die "$unit 状态异常：$load_state";; esac
 if [[ "$load_state" == loaded ]]; then
  systemctl stop "$unit" || die "无法停止 $unit，未删除安装文件"
 fi
done
for unit in "$SERVICE-update.path" "$SERVICE.service"; do
 load_state=$(systemctl show "$unit" -p LoadState --value) || die "无法检查 $unit，未删除安装文件"
 case "$load_state" in loaded|not-found) ;; *) die "$unit 状态异常：$load_state";; esac
 if [[ "$load_state" == loaded ]]; then
  systemctl disable "$unit" || die "无法禁用 $unit，未删除安装文件"
 fi
done
rm -f -- "$UNIT_DIR/$SERVICE.service" "$UNIT_DIR/$SERVICE-update.path" "$UNIT_DIR/$SERVICE-update.service" "$BIN"
if [[ "$SERVICE" != nekopass-agent ]]; then
 # Suffix instances have an isolated updater; the default updater is shared with the panel.
 rm -f -- "$(dirname "$BIN")/nekopass-update"
else
 # Preserve the shared updater if the panel or any of its update units exists,
 # or if systemd cannot confirm that all three units are absent.
 panel_absent=1
 for unit in nekopass.service nekopass-update.path nekopass-update.service; do
  panel_state=$(systemctl show "$unit" -p LoadState --value) || panel_state=unknown
  if [[ "$panel_state" != not-found || -e "$UNIT_DIR/$unit" || -L "$UNIT_DIR/$unit" ]]; then
   panel_absent=0
  fi
 done
 if ((panel_absent)) && [[ ! -e "$(dirname "$BIN")/nekopass" && ! -L "$(dirname "$BIN")/nekopass" ]]; then
  rm -f -- "$(dirname "$BIN")/nekopass-update"
 fi
fi
rmdir -- "$(dirname "$BIN")" "$(dirname "$(dirname "$BIN")")" 2>/dev/null || true
systemctl daemon-reload
if ((PURGE)); then
 rm -f -- "$ENV_FILE"
 rm -rf -- "$STATE_DIR"
fi
printf '[nekopass] 节点 %s 已卸载。\n' "$SERVICE"
