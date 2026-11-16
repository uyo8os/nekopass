"""Isolated tests for the Agent uninstaller; no host services or files are changed."""
import os
import pathlib
import shutil
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
GIT_BASH = pathlib.Path(r'C:\Program Files\Git\bin\bash.exe')
BASH = (os.environ.get('NEKOPASS_TEST_BASH') or
        (str(GIT_BASH) if os.name == 'nt' and GIT_BASH.exists() else shutil.which('bash')))


def bash_path(path):
    path = pathlib.Path(path).as_posix()
    if os.name == 'nt' and len(path) > 2 and path[1] == ':':
        return '/' + path[0].lower() + path[2:]
    return path


@unittest.skipUnless(BASH, 'bash is required')
class AgentUninstallTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = pathlib.Path(self.temp.name)
        root = bash_path(self.root)
        src = (ROOT / 'scripts/uninstall-agent.sh').read_text(encoding='utf-8')
        for path in ['/opt/nekopass', '/opt/$SERVICE', '/etc/nekopass', '/etc/$SERVICE',
                     '/var/lib/$SERVICE', '/etc/systemd/system', '/run/systemd/system']:
            src = src.replace(path, root + path)
        src = src.replace('[[ $(id -u) -eq 0 ]]', '[[ 0 -eq 0 ]]')
        src = src.replace('set -Eeuo pipefail', '''set -Eeuo pipefail
systemctl() {
 printf '%s\\n' "$*" >> "$AGENT_TEST_LOG"
 if [[ "$1" == show ]]; then
  if [[ "$2" == "$AGENT_BAD_UNIT" ]]; then echo masked; else echo loaded; fi
 fi
 if [[ "$1" == stop && "$2" == "$AGENT_FAIL_UNIT" ]]; then return 1; fi
 if [[ "$1" == disable && "$2" == "$AGENT_FAIL_DISABLE" ]]; then return 1; fi
 return 0
}
''')
        self.script = self.root / 'uninstall.sh'
        self.script.write_text(src, encoding='utf-8', newline='\n')
        self.make('run/systemd/system/.keep')
        self.env = os.environ.copy()
        self.env.update(AGENT_TEST_LOG=bash_path(self.root / 'systemctl.log'),
                        AGENT_FAIL_UNIT='', AGENT_BAD_UNIT='', AGENT_FAIL_DISABLE='', MSYS_NO_PATHCONV='1')

    def make(self, path):
        file = self.root / path
        file.parent.mkdir(parents=True, exist_ok=True)
        file.write_text('keep', encoding='utf-8')
        return file

    def fixture(self, service='nekopass-agent'):
        base = 'nekopass' if service == 'nekopass-agent' else service
        config = 'nekopass' if service == 'nekopass-agent' else service
        return {name: self.make(path) for name, path in {
            'bin': f'opt/{base}/bin/nekopass-agent',
            'updater': f'opt/{base}/bin/nekopass-update',
            'config': f'etc/{config}/agent.env',
            'state': f'var/lib/{service}/state.db',
            'unit': f'etc/systemd/system/{service}.service',
            'path': f'etc/systemd/system/{service}-update.path',
            'update': f'etc/systemd/system/{service}-update.service',
            'panel': 'opt/nekopass/bin/nekopass',
        }.items()}

    def run_script(self, *args):
        return subprocess.run([BASH, bash_path(self.script), *args], env=self.env,
                              text=True, encoding='utf-8', capture_output=True)

    def test_default_preserves_credentials_state_and_shared_files(self):
        files = self.fixture()
        result = self.run_script('--yes')
        self.assertEqual(result.returncode, 0, result.stderr)
        for key in ('bin', 'unit', 'path', 'update'):
            self.assertFalse(files[key].exists(), key)
        for key in ('updater', 'config', 'state', 'panel'):
            self.assertTrue(files[key].exists(), key)
        log = (self.root / 'systemctl.log').read_text()
        self.assertLess(log.index('stop nekopass-agent-update.path'),
                        log.index('stop nekopass-agent-update.service'))
        self.assertLess(log.index('stop nekopass-agent-update.service'),
                        log.index('stop nekopass-agent.service'))

    def test_purge_only_selected_node(self):
        other = self.fixture()
        files = self.fixture('nekopass-agent-test')
        result = self.run_script('--yes', '--purge', '--service-name', 'nekopass-agent-test')
        self.assertEqual(result.returncode, 0, result.stderr)
        for key in ('bin', 'updater', 'config', 'state', 'unit', 'path', 'update'):
            self.assertFalse(files[key].exists(), key)
        for file in other.values():
            self.assertTrue(file.exists(), file)

    def test_stop_failure_does_not_delete_files(self):
        files = self.fixture()
        self.env['AGENT_FAIL_UNIT'] = 'nekopass-agent-update.service'
        result = self.run_script('--yes', '--purge')
        self.assertNotEqual(result.returncode, 0)
        for file in files.values():
            self.assertTrue(file.exists(), file)

    def test_disable_failure_does_not_delete_files(self):
        files = self.fixture()
        self.env['AGENT_FAIL_DISABLE'] = 'nekopass-agent.service'
        result = self.run_script('--yes', '--purge')
        self.assertNotEqual(result.returncode, 0)
        for file in files.values():
            self.assertTrue(file.exists(), file)

    def test_invalid_name_and_dry_run_do_not_change_files(self):
        files = self.fixture()
        self.assertNotEqual(self.run_script('--service-name', '../other', '--yes').returncode, 0)
        self.assertEqual(self.run_script('--dry-run', '--purge').returncode, 0)
        self.assertFalse((self.root / 'systemctl.log').exists())
        for file in files.values():
            self.assertTrue(file.exists(), file)

    def test_unexpected_unit_state_aborts_before_stopping(self):
        files = self.fixture()
        self.env['AGENT_BAD_UNIT'] = 'nekopass-agent.service'
        self.assertNotEqual(self.run_script('--yes').returncode, 0)
        self.assertNotIn('stop ', (self.root / 'systemctl.log').read_text())
        for file in files.values():
            self.assertTrue(file.exists(), file)


if __name__ == '__main__':
    unittest.main()
