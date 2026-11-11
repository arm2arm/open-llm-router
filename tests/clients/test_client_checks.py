import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from client_checks import CancellationProbe, comparison, long_limits, vllm_snapshot


METRICS = '''# TYPE vllm:request_success_total counter
vllm:num_requests_running{model_name="served",engine="0"} 0
vllm:num_requests_running{engine="1",model_name="served"} 1
vllm:num_requests_waiting{model_name="served"} 2
vllm:request_success_total{finished_reason="abort",model_name="served"} 3
vllm:request_success_total{finished_reason="abort",model_name="served",engine="1"} 4
vllm:request_success_total{finished_reason="stop",model_name="served"} 999
vllm:request_success_total{finished_reason="abort",model_name="other"} 999
'''


class ClientChecksTest(unittest.TestCase):
    def test_metrics_filter_model_reason_and_sum_workers(self):
        self.assertEqual(vllm_snapshot(METRICS, "served"), (7, 3))

    def test_missing_metrics_are_not_evidence(self):
        for payload in ["404 page not found", METRICS.replace('finished_reason="abort"', 'finished_reason="stop"')]:
            with self.assertRaises(ValueError):
                vllm_snapshot(payload, "served")
        with self.assertRaises(ValueError):
            vllm_snapshot(METRICS, "missing-model")

    def test_invalid_metric_values(self):
        for value in ["NaN", "Inf", "-1"]:
            with self.assertRaises(ValueError):
                vllm_snapshot(METRICS.replace('model_name="served"} 2', f'model_name="served"}} {value}'), "served")

    @patch.dict(os.environ, {"OLR_REQUIRE_CANCEL_EVIDENCE": "1"}, clear=True)
    def test_required_evidence_is_not_silently_skipped(self):
        with self.assertRaisesRegex(ValueError, "evidence URL"):
            CancellationProbe()

    @patch.dict(os.environ, {"OLR_CANCEL_STATS_URLS": "http://mock/_mock/stats"}, clear=True)
    def test_abort_and_idle_are_both_required(self):
        # Closing the client and an idle backend alone are not proof of abort.
        with patch.object(CancellationProbe, "snapshot", side_effect=[(5, 0), (5, 0), (6, 1), (6, 0)]), patch("client_checks.time.sleep") as sleep:
            self.assertIn("abort counter increased", CancellationProbe().verify())
            self.assertEqual(sleep.call_count, 2)

    @patch.dict(os.environ, {"OLR_CANCEL_STATS_URLS": "http://mock/_mock/stats"}, clear=True)
    def test_busy_backend_and_counter_reset_fail(self):
        with patch.object(CancellationProbe, "snapshot", return_value=(5, 1)):
            with self.assertRaisesRegex(RuntimeError, "idle backend"):
                CancellationProbe()
        with patch.object(CancellationProbe, "snapshot", side_effect=[(5, 0), (0, 0)]):
            with self.assertRaisesRegex(RuntimeError, "counters reset"):
                CancellationProbe().verify()

    @patch.dict(os.environ, {"OLR_CANCEL_STATS_URLS": "http://mock/_mock/stats"}, clear=True)
    def test_idle_without_abort_times_out(self):
        with patch.object(CancellationProbe, "snapshot", return_value=(5, 0)), patch("client_checks.time.monotonic", side_effect=[0, 31]):
            with self.assertRaisesRegex(RuntimeError, "did not confirm"):
                CancellationProbe().verify()

    @patch.dict(os.environ, {"OLR_VLLM_METRICS_URL": "http://vllm/metrics", "OLR_UPSTREAM_MODEL": "served"}, clear=True)
    def test_log_evidence_matches_response_and_requires_idle(self):
        with tempfile.TemporaryDirectory() as directory:
            log = Path(directory) / "vllm.log"
            log.write_text("")
            with patch.dict(os.environ, {"OLR_VLLM_LOG_PATH": str(log)}), patch.object(
                CancellationProbe, "snapshot", side_effect=[(0, 0), (0, 0), (0, 1), (0, 0)]
            ):
                probe = CancellationProbe()
                log.write_text("Aborted request(s) chatcmpl-other.\n")
                def append_match():
                    with log.open("a") as output:
                        output.write("Aborted request(s) chatcmpl-target-0.\n")
                with patch("client_checks.time.sleep", side_effect=lambda _: append_match()) as sleep:
                    self.assertIn("matched this response ID", probe.verify("chatcmpl-target"))
                    self.assertEqual(sleep.call_count, 2)

    @patch.dict(os.environ, {"OLR_VLLM_METRICS_URL": "http://vllm/metrics", "OLR_UPSTREAM_MODEL": "served"}, clear=True)
    def test_stale_and_other_request_logs_are_not_evidence(self):
        with tempfile.TemporaryDirectory() as directory:
            log = Path(directory) / "vllm.log"
            log.write_text("Aborted request(s) chatcmpl-target.\n")
            with patch.dict(os.environ, {"OLR_VLLM_LOG_PATH": str(log)}), patch.object(
                CancellationProbe, "snapshot", return_value=(0, 0)
            ):
                probe = CancellationProbe()
                with log.open("a") as output:
                    output.write("Aborted request(s) chatcmpl-target2.\n")
                with patch("client_checks.time.monotonic", side_effect=[0, 31]):
                    with self.assertRaisesRegex(RuntimeError, "did not confirm"):
                        probe.verify("chatcmpl-target")

    @patch.dict(os.environ, {"OLR_VLLM_METRICS_URL": "http://vllm/metrics", "OLR_UPSTREAM_MODEL": "served"}, clear=True)
    def test_log_rotation_and_missing_response_id_fail(self):
        with tempfile.TemporaryDirectory() as directory:
            log = Path(directory) / "vllm.log"
            log.write_text("initial log\n")
            with patch.dict(os.environ, {"OLR_VLLM_LOG_PATH": str(log)}), patch.object(
                CancellationProbe, "snapshot", return_value=(0, 0)
            ):
                probe = CancellationProbe()
                with self.assertRaisesRegex(ValueError, "response ID"):
                    probe.verify()
                replacement = Path(directory) / "replacement.log"
                replacement.write_text("Aborted request(s) chatcmpl-target.\n")
                replacement.replace(log)
                with self.assertRaisesRegex(RuntimeError, "replaced or truncated"):
                    probe.verify("chatcmpl-target")

    @patch.dict(os.environ, {"OLR_VLLM_LOG_PATH": "/unused"}, clear=True)
    def test_abort_log_alone_does_not_prove_engine_idle(self):
        with self.assertRaisesRegex(ValueError, "also requires"):
            CancellationProbe()

    @patch.dict(os.environ, {"OLR_LONG_MIN_TOKENS": "600"}, clear=True)
    def test_long_output_bounds(self):
        with self.assertRaisesRegex(ValueError, "exceeds"):
            long_limits()

    def test_performance_gate_uses_medians_and_rejects_regressions(self):
        samples = {"direct": [{"ttft_ms": 80, "tokens_per_second": 50}] * 3,
                   "router": [{"ttft_ms": 81, "tokens_per_second": 50},
                              {"ttft_ms": 81, "tokens_per_second": 50},
                              {"ttft_ms": 999, "tokens_per_second": 1}]}
        self.assertTrue(comparison(samples)["passed"])
        samples["router"] = [{"ttft_ms": 80, "tokens_per_second": 49}]
        self.assertTrue(comparison(samples)["passed"])
        samples["router"] = [{"ttft_ms": 85, "tokens_per_second": 50}]
        self.assertFalse(comparison(samples)["passed"])
        samples["router"] = [{"ttft_ms": 80, "tokens_per_second": 46}]
        self.assertFalse(comparison(samples)["passed"])


if __name__ == "__main__":
    unittest.main()
