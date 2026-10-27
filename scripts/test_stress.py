import contextlib
import io
import unittest

import stress


class TerminalReportTests(unittest.TestCase):
    def result(self, samples):
        names = [name for name, _ in stress.COMPARABLE_METRICS]
        names += ["clone_current_checkout", "clone_depth_1", "pr_create", "pr_comment",
                  "pr_diff_first", "pr_approve", "pr_show", "pr_merge_wait"]
        return {
            "parameters": {"files": 20, "commits": 21, "prs": 8, "workers": 4},
            "metrics": {tool: {name: stress.distribution(samples) for name in names}
                        for tool in ("crease", "git")},
            "status": "passed", "commands": 100, "elapsed_seconds": 1.5,
        }

    def test_inclusive_boundaries_count_every_sample_once(self):
        self.assertEqual(stress.latency_histogram([10, 0, 5, 0, 1, 10, 9], bins=2),
                         [(0, 2), (5, 2), (10, 3)])

    def test_identical_samples_and_empty_input(self):
        self.assertEqual(stress.latency_histogram([0, 0, 0]), [(0, 3)])
        self.assertEqual(stress.latency_histogram([7]), [(7, 1)])
        self.assertEqual(stress.latency_histogram([]), [])

    def test_round_increments_and_aligned_bounds(self):
        for samples, expected in (
            ([8.2, 74.6], list(range(10, 81, 10))),
            ([12.1, 34.7], list(range(15, 36, 5))),
            ([2.1, 8.9], list(range(3, 10))),
            ([100, 7600], list(range(1000, 8001, 1000))),
        ):
            with self.subTest(samples=samples):
                buckets = stress.latency_histogram(samples)
                self.assertEqual([bound for bound, _ in buckets], expected)
                self.assertEqual(sum(count for _, count in buckets), len(samples))

    def test_fractional_samples_fit_their_displayed_bucket_bounds(self):
        self.assertEqual(stress.latency_histogram([120.635, 120.635]), [(121, 2)])
        self.assertEqual(stress.latency_histogram([9.999, 10, 10.001, 74]),
                         [(10, 2), (20, 1), (30, 0), (40, 0),
                          (50, 0), (60, 0), (70, 0), (80, 1)])

    def test_histogram_bars_encode_counts_instead_of_latency(self):
        output = stress.terminal_report(self.result([1, 1, 1, 1, 10]),
                                       width=80, ascii_only=True)
        histogram = output.split("Response time histogram, ms:\n", 1)[1]
        histogram = histogram.split("Latency distribution, ms:", 1)[0]
        rows = [line for line in histogram.splitlines() if "|" in line]
        self.assertEqual(rows[0].count("#"), 40)
        self.assertEqual(rows[-1].count("#"), 10)
        self.assertTrue(all("#" not in line for line in rows[1:-1]))
        self.assertIn("95% in 10.000 ms", output)

    def test_narrow_output_and_ascii_fallback(self):
        result = self.result([0, 1, 10000])
        for width in (24, 32, 40, 60, 80, 120):
            for ascii_only in (True, False):
                output = stress.terminal_report(result, width=width, ascii_only=ascii_only)
                self.assertTrue(all(len(line) <= width for line in output.splitlines()))
        stream = io.TextIOWrapper(io.BytesIO(), encoding="ascii")
        with contextlib.redirect_stdout(stream):
            output = stress.terminal_report(result)
        output.encode("ascii")
        self.assertIn("#", output)


if __name__ == "__main__":
    unittest.main()
