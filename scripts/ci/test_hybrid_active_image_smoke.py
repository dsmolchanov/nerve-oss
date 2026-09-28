#!/usr/bin/env python3
"""Deterministic guards for the active-image synthetic send observation."""

from pathlib import Path
import tempfile
import unittest

from hybrid_active_image_smoke import complete_sends, observe_single_send


class CompleteSendsTest(unittest.TestCase):
    def test_refuses_file_creation_and_partial_jsonl(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'sends.jsonl'
            self.assertIsNone(complete_sends(path))
            path.touch()
            self.assertIsNone(complete_sends(path))
            path.write_text('{"body":"synthetic"')
            self.assertIsNone(complete_sends(path))
            path.write_text('{"body":"synthetic"}\n')
            self.assertEqual(complete_sends(path), [{'body': 'synthetic'}])


class ObserveSingleSendTest(unittest.TestCase):
    def test_waits_for_complete_event_and_terminal_outbox(self):
        clock = [0.0]

        def events():
            return None if clock[0] < 0.2 else [{'body': 'synthetic'}]

        def pause(duration):
            clock[0] += duration

        result = observe_single_send(events, lambda: clock[0] >= 0.4,
                                     quiet_seconds=0.3, timeout=2,
                                     now=lambda: clock[0], pause=pause)
        self.assertEqual(result['body'], 'synthetic')
        self.assertGreaterEqual(clock[0], 0.7)

    def test_rejects_later_duplicate_before_quiet_window_ends(self):
        clock = [0.0]

        def events():
            if clock[0] < 0.1:
                return None
            if clock[0] < 0.3:
                return [{'body': 'synthetic'}]
            return [{'body': 'synthetic'}, {'body': 'duplicate'}]

        def pause(duration):
            clock[0] += duration

        with self.assertRaisesRegex(AssertionError, 'more than once'):
            observe_single_send(events, lambda: True, quiet_seconds=0.5,
                                timeout=2, now=lambda: clock[0], pause=pause)

    def test_restart_quiet_window_starts_only_after_a_new_poll(self):
        clock = [0.0]

        def pause(duration):
            clock[0] += duration

        observe_single_send(lambda: [{'body': 'synthetic'}], lambda: True,
                            quiet_seconds=0.3, timeout=2,
                            polled=lambda: clock[0] >= 0.4,
                            inbound_count=lambda: 1,
                            now=lambda: clock[0], pause=pause)
        self.assertGreaterEqual(clock[0], 0.7)

    def test_rejects_inbound_replay_during_post_poll_quiet_window(self):
        clock = [0.0]

        def pause(duration):
            clock[0] += duration

        with self.assertRaisesRegex(AssertionError, 'inbound was replayed'):
            observe_single_send(lambda: [{'body': 'synthetic'}], lambda: True,
                                quiet_seconds=0.5, timeout=2,
                                polled=lambda: clock[0] >= 0.1,
                                inbound_count=lambda: 1 if clock[0] < 0.4 else 2,
                                now=lambda: clock[0], pause=pause)


if __name__ == '__main__':
    unittest.main()
