"""Exercise shared CLI cleanup using isolated files and mocked systemd."""
import os
import pathlib
import shutil
import subprocess
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parents[1]
GIT_BASH = pathlib.Path(r'C:\Program Files\Git\bin\bash.exe')
BASH = str(GIT_BASH) if os.name == 'nt' and GIT_BASH.exists() else shutil.which('bash')


def bash_path(path):
    path = pathlib.Path(path).as_posix()
    if os.name == 'nt' and path[1:2] == ':':
        return '/' + path[0].lower() + path[2:]
    return path


@unittest.skipUnless(BASH, 'bash is required')
class SharedCliTest(unittest.TestCase):
    def test_preserves_cli_when_another_instance_exists_or_systemd_fails(self):
        for script in ('uninstall-agent.sh', 'uninstall-panel.sh'):
            source = (ROOT / 'scripts' / script).read_text(encoding='utf-8')
            function = 'cleanup_shared_cli() {' + source.split('cleanup_shared_cli() {', 1)[1].split('\n}\n', 1)[0] + '\n}\n'
            for scenario in ('alone', 'stale', 'unit', 'running', 'unit-file', 'binary', 'error'):
                with self.subTest(script=script, scenario=scenario), tempfile.TemporaryDirectory() as temp:
                    root = pathlib.Path(temp)
                    cli = root / 'usr/local/bin/nekopassctl'
                    cli.parent.mkdir(parents=True)
                    cli.write_text('manager')
                    unit_dir = root / 'etc/systemd/system'
                    unit_dir.mkdir(parents=True)
                    opt = root / 'opt'
                    opt.mkdir()
                    if scenario == 'unit-file':
                        (unit_dir / 'nekopass-agent-other.service').write_text('unit')
                    if scenario == 'binary':
                        binary = opt / 'nekopass-agent-other/bin/nekopass-agent'
                        binary.parent.mkdir(parents=True)
                        binary.write_text('agent')
                    shell = root / 'test.sh'
                    shell.write_text('''set -eu
systemctl() {
 if [[ "$SCENARIO" == error ]]; then return 1; fi
 if [[ "$SCENARIO" == unit && "$1" == list-unit-files ]]; then echo 'nekopass-agent-other.service enabled'; fi
 if [[ "$SCENARIO" == running && "$1" == list-units ]]; then echo 'nekopass-panel-other.service loaded active running'; fi
 if [[ "$SCENARIO" == stale && "$1" == list-units ]]; then echo "$SERVICE.service not-found inactive dead"; fi
 return 0
}
''' + function + '\ncleanup_shared_cli\n', encoding='utf-8', newline='\n')
                    env = dict(os.environ, SCENARIO=scenario, CLI_PATH=bash_path(cli),
                               UNIT_DIR=bash_path(unit_dir), OPT_ROOT=bash_path(opt), SERVICE='nekopass-agent', MSYS_NO_PATHCONV='1')
                    result = subprocess.run([BASH, bash_path(shell)], env=env, capture_output=True)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(cli.exists(), scenario not in ('alone', 'stale'))


if __name__ == '__main__':
    unittest.main()
