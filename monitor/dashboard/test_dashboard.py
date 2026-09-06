import json
import itertools
from unittest.mock import patch
import manage
from pathlib import Path
import subprocess
import tempfile
import unittest


class DashboardTests(unittest.TestCase):
    def test_prometheus_transport_and_freshness_gates(self):
        dashboard = json.loads(Path(__file__).with_name('dashboard.json').read_text())
        state = next(p for p in dashboard['panels'] if p['title'] == 'ishii1648/codex-issue-loop')
        expression = state['targets'][0]['expr']
        series = [
            {'series': 'agent_loop_monitor_state{repository="ishii1648/codex-issue-loop"}', 'values': '1 2 3 0 1 1'},
            {'series': 'up{job="agent-loop-monitor"}', 'values': '1 1 1 1 0 1'},
            {'series': 'agent_loop_monitor_reference_time_seconds', 'values': '0 15 30 45 60 0'},
        ]
        cases = []
        for seconds, value in [(0, 1), (15, 2), (30, 3), (45, 0), (60, None), (75, None)]:
            cases.append({'expr': expression, 'eval_time': str(seconds) + 's', 'exp_samples': [] if value is None else [{'labels': 'agent_loop_monitor_state{repository="ishii1648/codex-issue-loop"}', 'value': value}]})
        payload = {'rule_files': [], 'evaluation_interval': '15s', 'tests': [{'interval': '15s', 'input_series': series, 'promql_expr_test': cases}]}
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'test.json'
            path.write_text(json.dumps(payload))
            subprocess.run(['promtool', 'test', 'rules', str(path)], check=True)

    def test_all_current_panels_gate_missing_and_stale_transport(self):
        dashboard = json.loads(Path(__file__).with_name('dashboard.json').read_text())
        for panel in dashboard['panels']:
            if panel['type'] == 'stat':
                expression = panel['targets'][0]['expr']
                self.assertIn('up{job="agent-loop-monitor"} == 1', expression)
                self.assertIn('time() - agent_loop_monitor_reference_time_seconds < 45', expression)
                self.assertEqual(panel['fieldConfig']['defaults']['noValue'], 'UNKNOWN / データなし')
            if panel['type'] == 'state-timeline':
                self.assertIn('/api/timeline?', panel['targets'][0]['url'])
                self.assertEqual(panel['targets'][0]['parser'], 'backend')
                order = panel['transformations'][0]['options']['indexByName']
                repo = panel['targets'][0]['columns'][2]['text']
                for fields in itertools.permutations([repo, '終了', '開始']):
                    frame = {repo: ['UNKNOWN', 'DOWN'], '開始': [0, 10], '終了': [10, 24]}
                    ordered = sorted(fields, key=order.__getitem__)
                    times = [frame[name] for name in ordered if name != repo]
                    self.assertEqual(list(zip(*times, frame[repo])), [(0, 10, 'UNKNOWN'), (10, 24, 'DOWN')])
                mappings = panel['fieldConfig']['defaults']['mappings'][0]['options']
                self.assertEqual(len({v['color'] for v in mappings.values()}), 4)
                self.assertTrue(all(k == v['text'] for k, v in mappings.items()))
        self.assertEqual(dashboard['timezone'], 'Asia/Tokyo')
        self.assertIn(dashboard['refresh'], dashboard['timepicker']['refresh_intervals'])

    def test_restart_loaded_services_does_not_bootout(self):
        with patch('sys.argv', ['manage.py', 'restart']), patch('manage.subprocess.run') as run:
            run.return_value.returncode = 0
            manage.main()
        commands = [call.args[0] for call in run.call_args_list]
        self.assertEqual(sum(c[1:3] == ['kickstart', '-k'] for c in commands), 3)
        self.assertFalse(any(c[1] in ['bootstrap', 'bootout'] for c in commands))

    def test_restart_unloaded_services_bootstraps(self):
        with patch('sys.argv', ['manage.py', 'restart']), patch('manage.subprocess.run') as run:
            run.return_value.returncode = 1
            manage.main()
        self.assertEqual(sum(c.args[0][1] == 'bootstrap' for c in run.call_args_list), 3)


if __name__ == '__main__':
    unittest.main()
