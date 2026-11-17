"""Exercise the endpoint validation and comparison embedded in the installer."""
import pathlib
import subprocess
import sys
import unittest


SOURCE = (pathlib.Path(__file__).resolve().parents[1] / 'scripts/install-agent.sh').read_text()
VALIDATE = SOURCE.split("python3 - \"$SERVER\" <<'PY'\n", 1)[1].split('\nPY\n', 1)[0]
COMPARE = SOURCE.split("<<'PY_COMPARE_ENDPOINTS'\n", 1)[1].split('\nPY_COMPARE_ENDPOINTS\n', 1)[0]
CONFIG_ENDPOINT = SOURCE.split("<<'PY_CONFIG_ENDPOINT'\n", 1)[1].split('\nPY_CONFIG_ENDPOINT\n', 1)[0]


def run(source, *args):
    return subprocess.run([sys.executable, '-c', source, *args], capture_output=True, text=True)


class AgentInstallerEndpointTests(unittest.TestCase):
    def test_valid_addresses(self):
        for endpoint in ('https://agent.example.com', 'http://agent.example.com',
                         'https://agent.example.com:9443', 'agent.example.com:9443',
                         'https://[::1]', 'http://[::1]:8080', '[::1]:9443'):
            with self.subTest(endpoint=endpoint):
                self.assertEqual(run(VALIDATE, endpoint).returncode, 0)

    def test_invalid_addresses(self):
        for endpoint in ('agent.example.com', 'https://agent.example.com:',
                         'https://[::1]:', 'https://agent.example.com:0',
                         'https://agent.example.com:65536', 'https://agent.example.com:abc',
                         'https://agent.example.com/path', 'ftp://agent.example.com',
                         'https://user:password@agent.example.com'):
            with self.subTest(endpoint=endpoint):
                self.assertNotEqual(run(VALIDATE, endpoint).returncode, 0)

    def test_equivalent_default_ports_preserve_state(self):
        for first, second in (('https://agent.example.com', 'https://agent.example.com:443'),
                              ('http://agent.example.com', 'http://agent.example.com:80'),
                              ('https://[::1]', '[::1]:443')):
            with self.subTest(first=first, second=second):
                self.assertEqual(run(COMPARE, first, second).returncode, 0)
        self.assertNotEqual(run(COMPARE, 'https://agent.example.com', 'https://agent.example.com:9443').returncode, 0)

    def test_written_endpoints_remain_compatible_with_older_agents(self):
        examples = {
            'https://agent.example.com': 'https://agent.example.com:443',
            'http://agent.example.com': 'http://agent.example.com:80',
            'https://[::1]': 'https://[::1]:443',
            'https://agent.example.com/': 'https://agent.example.com:443/',
            'https://agent.example.com:9443': 'https://agent.example.com:9443',
            '[::1]:9443': '[::1]:9443',
        }
        for supplied, expected in examples.items():
            with self.subTest(endpoint=supplied):
                result = run(CONFIG_ENDPOINT, supplied)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout, expected)
                self.assertEqual(run(COMPARE, supplied, result.stdout).returncode, 0)


if __name__ == '__main__':
    unittest.main()
