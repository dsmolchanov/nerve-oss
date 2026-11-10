#!/usr/bin/env python3
"""Deterministic guards for the active-image synthetic send observation."""

from pathlib import Path
import json
import tempfile
import unittest
import threading
import urllib.request
import urllib.error
from http.server import ThreadingHTTPServer

from hybrid_active_image_smoke import complete_sends, fixture_key_state, fixture_handler, observe_single_send, write_admission


class FixtureKeyStateTest(unittest.TestCase):
    def test_admission_does_not_imply_owner_switch(self):
        state = {'kid': 'old', 'admitted_kids': ['old', 'new'], 'active_kid': 'old'}
        self.assertEqual(fixture_key_state(state, 'old'), (True, True))
        self.assertEqual(fixture_key_state(state, 'new'), (True, False))
        self.assertEqual(fixture_key_state(state, 'unknown'), (False, False))
        self.assertEqual(fixture_key_state(state, ''), (False, False))
        state['active_kid'] = 'new'
        self.assertEqual(fixture_key_state(state, 'old'), (True, False))
        self.assertEqual(fixture_key_state(state, 'new'), (True, True))

    def test_initial_pairing_defaults_to_single_key(self):
        self.assertEqual(fixture_key_state({'kid': 'first'}, 'first'), (True, True))
        self.assertEqual(fixture_key_state({'kid': 'first'}, 'other'), (False, False))

    def test_fixture_revision_replaces_one_complete_file(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            write_admission(root, {'kid': 'old'})
            write_admission(root, {'kid': 'old', 'admitted_kids': ['old', 'new']})
            self.assertEqual(json.loads((root / 'admission.json').read_text()),
                             {'kid': 'old', 'admitted_kids': ['old', 'new']})
            self.assertFalse((root / 'admission.next.json').exists())


class FixtureMessageIdentityTest(unittest.TestCase):
    def test_actual_poll_and_reply_keep_provider_and_internet_identifiers_distinct(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            admission = {key: key + '-fixture' for key in
                         ('installation_id', 'org_id', 'inbox_id', 'delivery_id', 'lease_token')}
            admission['kid'] = 'active-fixture-key'
            write_admission(root, admission)
            server = ThreadingHTTPServer(('127.0.0.1', 0), fixture_handler(root))
            worker = threading.Thread(target=server.serve_forever, daemon=True)
            worker.start()
            try:
                def post(action, fields):
                    body = {key: admission[key] for key in ('installation_id', 'inbox_id')}
                    body.update(fields)
                    request = urllib.request.Request(
                        f'http://127.0.0.1:{server.server_port}/v1/hybrid/{action}',
                        data=json.dumps(body).encode(),
                        headers={'Authorization': 'Bearer fixture-token.' + admission['kid']})
                    with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(request, timeout=2) as response:
                        return json.load(response)

                delivery = post('poll', {})['delivery']
                self.assertEqual(delivery['internet_message_id'], '<fixture-inbound@example.invalid>')
                self.assertNotEqual(delivery['provider_message_id'], delivery['internet_message_id'])
                self.assertNotEqual(delivery['provider_message_id'], delivery['id'])
                request = {'kind': 'reply', 'to': ['sender@example.invalid'],
                           'operation_key': 'fixture-reply', 'body': 'Synthetic hybrid reply',
                           'in_reply_to': delivery['internet_message_id'],
                           'references': delivery['internet_message_id']}
                for field in ('in_reply_to', 'references'):
                    wrong = request.copy()
                    wrong[field] = delivery['provider_message_id']
                    with self.assertRaises(urllib.error.HTTPError) as refused:
                        post('send', wrong)
                    self.assertEqual(refused.exception.code, 400)
                    self.assertFalse((root / 'sends.jsonl').exists())
                receipt = post('send', request)
                self.assertEqual(receipt['status'], 'sent')
                self.assertEqual(complete_sends(root / 'sends.jsonl')[0]['in_reply_to'],
                                 delivery['internet_message_id'])
                post('ack', {'delivery_id': delivery['id'], 'lease_token': delivery['lease_token']})
                self.assertIsNone(post('poll', {})['delivery'])
            finally:
                server.shutdown()
                server.server_close()
                worker.join(timeout=2)
                self.assertFalse(worker.is_alive())


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
