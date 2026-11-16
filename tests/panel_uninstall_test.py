"""Exercise uninstall argument and database safety checks without system changes."""
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / 'scripts' / 'uninstall-panel.sh'
GIT_BASH = pathlib.Path(r'C:\Program Files\Git\bin\bash.exe')
BASH = str(GIT_BASH) if sys.platform == 'win32' and GIT_BASH.exists() else shutil.which('bash')


def bash_path(path):
 path = pathlib.Path(path).as_posix()
 if os.name == 'nt' and len(path) > 2 and path[1] == ':':
  return '/' + path[0].lower() + path[2:]
 return path


@unittest.skipUnless(BASH, 'Bash is required')
class PanelUninstallTest(unittest.TestCase):
 def shell(self, *args):
  return subprocess.run([BASH, str(SCRIPT), *args], capture_output=True, text=True, encoding='utf-8', errors='replace')

 def test_syntax_and_dry_run(self):
  subprocess.run([BASH, '-n', str(SCRIPT)], check=True)
  for args in [(), ('--purge',), ('--service-name', 'nekopass-panel-test', '--purge', '--yes')]:
   self.assertEqual(self.shell(*args, '--dry-run').returncode, 0)

 def test_rejects_other_services_even_in_dry_run(self):
  for service in ['nekopass-agent', '../nekopass', 'nekopass-panel-a.service', 'nekopass-panel-A']:
   self.assertNotEqual(self.shell('--service-name', service, '--dry-run').returncode, 0)

 def test_purge_only_accepts_installer_database_address(self):
  # Execute the actual embedded parser, never the system-modifying shell code.
  source = SCRIPT.read_text(encoding='utf-8')
  parser = source.split("<<'PY'\n", 1)[1].split('\nPY\n', 1)[0]
  valid = 'postgres://nekopass@/nekopass?host=/var/run/postgresql&port=5432&sslmode=disable'
  with tempfile.TemporaryDirectory() as temp:
   config = pathlib.Path(temp) / 'control.env'
   for dsn, accepted in [
    (valid, True),
    (valid.replace('5432', '65536'), False),
    (valid.replace('nekopass@', 'other@'), False),
    (valid.replace('/nekopass?', '/other?'), False),
    (valid.replace('@/', '@remote/'), False),
    (valid.replace('/var/run/postgresql', '/tmp'), False),
    (valid + '&port=5433', False),
    (valid + '&other=x', False),
   ]:
    config.write_text('NEKOPASS_DATABASE_URL=' + dsn + '\n', encoding='utf-8')
    result = subprocess.run([sys.executable, '-c', parser, str(config), 'nekopass'], capture_output=True)
    self.assertEqual(result.returncode == 0, accepted, dsn)
   config.write_text('NEKOPASS_DATABASE_URL=' + valid + '\nNEKOPASS_DATABASE_URL=' + valid + '\n', encoding='utf-8')
   self.assertNotEqual(subprocess.run([sys.executable, '-c', parser, str(config), 'nekopass'], capture_output=True).returncode, 0)

 def test_service_removal_preserves_agent_and_validates_systemd(self):
  with tempfile.TemporaryDirectory() as temp:
   root = pathlib.Path(temp)
   for directory in ['opt/nekopass/bin', 'opt/nekopass/web', 'etc/nekopass',
                     'etc/systemd/system', 'var/lib/nekopass', 'mock']:
    (root / directory).mkdir(parents=True, exist_ok=True)
   for name in ['opt/nekopass/bin/nekopass', 'opt/nekopass/bin/nekopass-agent',
                'opt/nekopass/bin/nekopass-update', 'opt/nekopass/web/index.html',
                'etc/nekopass/control.env', 'etc/nekopass/agent.env',
                'var/lib/nekopass/panel-update-ready', 'etc/systemd/system/nekopass.service']:
    (root / name).write_text('fixture')
   posix_root = bash_path(root)
   source = SCRIPT.read_text(encoding='utf-8')
   source = source.replace('BASE="/opt/$SERVICE"; CONFIG="/etc/$SERVICE"; STATE="/var/lib/$SERVICE"',
                           f'BASE="{posix_root}/opt/$SERVICE"; CONFIG="{posix_root}/etc/$SERVICE"; STATE="{posix_root}/var/lib/$SERVICE"')
   source = source.replace('UNIT_DIR=/etc/systemd/system', f'UNIT_DIR={posix_root}/etc/systemd/system')
   source = source.replace('[[ $(id -u) -eq 0 ]]', '[[ 0 -eq 0 ]]')
   source = source.replace('[[ -d /run/systemd/system ]]', '[[ -d / ]]')
   source = source.replace('set -Eeuo pipefail', '''set -Eeuo pipefail
systemctl() {
 if [[ "$1" == show ]]; then
  [[ "${FAIL_SHOW:-0}" == 0 ]] || return 1
  echo loaded
 fi
 if [[ "$1" == stop && "${FAIL_STOP:-0}" == 1 ]]; then return 1; fi
 return 0
}
runuser() {
 printf '%s\\n' "$*" >> "$DB_LOG"
 for arg in "$@"; do
  if [[ "$arg" == psql ]]; then echo nekopass; return 0; fi
  if [[ "$arg" == dropdb ]]; then touch "$DROPPED"; return 0; fi
 done
 return 1
}
psql() { :; }
dropdb() { :; }
''')
   if os.name == 'nt':
    source = source.replace('set -Eeuo pipefail', '''set -Eeuo pipefail
python3() { command python "$1" "$(cygpath -w "$2")" "$3"; }
''', 1)
   isolated = root / 'uninstall.sh'
   isolated.write_text(source, encoding='utf-8', newline='\n')
   env = dict(os.environ, FAIL_SHOW='1', MSYS_NO_PATHCONV='1')
   result = subprocess.run([BASH, bash_path(isolated), '--yes'], env=env, capture_output=True)
   self.assertNotEqual(result.returncode, 0)
   self.assertTrue((root / 'opt/nekopass/bin/nekopass').exists())
   env['FAIL_SHOW'] = '0'
   result = subprocess.run([BASH, bash_path(isolated), '--yes'], env=env, capture_output=True)
   self.assertEqual(result.returncode, 0, result.stderr)
   self.assertFalse((root / 'opt/nekopass/bin/nekopass').exists())
   self.assertFalse((root / 'opt/nekopass/web').exists())
   for name in ['opt/nekopass/bin/nekopass-agent', 'opt/nekopass/bin/nekopass-update',
                'etc/nekopass/control.env', 'etc/nekopass/agent.env', 'var/lib/nekopass/panel-update-ready']:
    self.assertTrue((root / name).exists(), name)

   panel = root / 'opt/nekopass/bin/nekopass'
   panel.write_text('fixture')
   config = root / 'etc/nekopass/control.env'
   env['DROPPED'] = bash_path(root / 'dropped')
   env['DB_LOG'] = bash_path(root / 'database.log')
   config.unlink()
   result = subprocess.run([BASH, bash_path(isolated), '--purge', '--yes'], env=env, capture_output=True)
   self.assertNotEqual(result.returncode, 0)
   self.assertTrue(panel.exists())
   config.write_text('NEKOPASS_DATABASE_URL=postgres://nekopass@remote/nekopass\n')
   result = subprocess.run([BASH, bash_path(isolated), '--purge', '--yes'], env=env, capture_output=True)
   self.assertNotEqual(result.returncode, 0)
   self.assertTrue(config.exists())
   self.assertFalse((root / 'dropped').exists())
   config.write_text('NEKOPASS_DATABASE_URL=postgres://nekopass@/nekopass?host=/var/run/postgresql&port=5432&sslmode=disable\n')
   env['FAIL_STOP'] = '1'
   result = subprocess.run([BASH, bash_path(isolated), '--purge', '--yes'], env=env, capture_output=True)
   self.assertNotEqual(result.returncode, 0)
   self.assertTrue(config.exists())
   self.assertTrue(panel.exists())
   self.assertFalse((root / 'dropped').exists())
   env['FAIL_STOP'] = '0'
   result = subprocess.run([BASH, bash_path(isolated), '--purge', '--yes'], env=env, capture_output=True)
   self.assertEqual(result.returncode, 0, result.stderr)
   self.assertTrue((root / 'dropped').exists())
   db_calls = (root / 'database.log').read_text()
   self.assertIn('env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin psql -h /var/run/postgresql -p 5432 -U postgres -d postgres', db_calls)
   self.assertIn('env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin dropdb -h /var/run/postgresql -p 5432 -U postgres --maintenance-db=postgres', db_calls)
   self.assertFalse(config.exists())
   self.assertFalse((root / 'var/lib/nekopass').exists())
   self.assertTrue((root / 'opt/nekopass/bin/nekopass-agent').exists())
   self.assertTrue((root / 'etc/nekopass/agent.env').exists())


if __name__ == '__main__':
 unittest.main()
