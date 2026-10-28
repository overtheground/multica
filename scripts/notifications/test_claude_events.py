import tempfile
import unittest
from unittest.mock import patch
import json
from pathlib import Path
from claude_events import events, publish


class EventsTest(unittest.TestCase):
    def setUp(self):
        self.snapshot = {"account": "max", "observed_at": "2026-10-28T01:00:00Z", "fresh_until": "2026-10-28T01:05:00Z",
                         "five_hour": {"remaining_percent": 0, "reset_at": "2026-10-28T02:00:00Z"},
                         "seven_day": {"remaining_percent": 20, "reset_at": "2026-11-01T02:00:00Z"}}

    def test_independent_window_and_period(self):
        first = list(events(self.snapshot))[0]
        self.snapshot["seven_day"]["remaining_percent"] = 0
        self.assertEqual(len(list(events(self.snapshot))), 2)
        self.snapshot["five_hour"]["reset_at"] = "2026-10-28T03:00:00Z"
        self.assertNotEqual(list(events(self.snapshot))[0]["event_key"], first["event_key"])

    def test_retry_payload_survives_restart_and_benign_numbers(self):
        with tempfile.TemporaryDirectory() as directory:
            ledger = Path(directory) / "ledger.json"
            first = publish(self.snapshot, ledger)
            self.snapshot["observed_at"] = "2026-10-28T01:01:00Z"
            self.snapshot["seven_day"]["remaining_percent"] = 19
            self.assertEqual(publish(self.snapshot, ledger), first)

    def test_unknown_and_unconfirmed_reset_are_not_known(self):
        self.snapshot["five_hour"]["remaining_percent"] = None
        event = list(events(self.snapshot))[0]
        self.assertEqual(event["facts"]["urgency"], "unknown")
        self.snapshot["observed_at"] = "2026-10-28T02:01:00Z"
        self.assertEqual(list(events(self.snapshot))[0]["facts"]["action"], "reset_not_confirmed")

    def test_publish_uses_cli_and_retries_identical_payload(self):
        calls = []
        def invoke(command, check):
            self.assertTrue(check)
            self.assertEqual(command[:3], ["multica", "notification", "publish"])
            self.assertEqual(command[-2:], ["--agent-id", "agent"])
            calls.append(json.loads(Path(command[4]).read_text()))
        with tempfile.TemporaryDirectory() as directory, patch("claude_events.subprocess.run", side_effect=invoke):
            ledger = Path(directory) / "ledger.json"
            publish(self.snapshot, ledger, execute=True, agent_id="agent")
            self.snapshot["observed_at"] = "2026-10-28T01:01:00Z"
            publish(self.snapshot, ledger, execute=True, agent_id="agent")
        self.assertEqual(calls[0], calls[1])

    def test_no_event_for_benign_remaining_change(self):
        self.snapshot["five_hour"]["remaining_percent"] = 5
        self.assertEqual(list(events(self.snapshot)), [])


if __name__ == "__main__":
    unittest.main()
