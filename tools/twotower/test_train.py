import json
import math
from pathlib import Path
import random
import subprocess
import sys
import tempfile
import unittest

import train


HERE = Path(__file__).resolve().parent


class TrainingTests(unittest.TestCase):
    def setUp(self):
        self.videos = train.load_videos(HERE / "demo" / "videos.jsonl")
        self.events = train.load_events(HERE / "demo" / "events.jsonl", self.videos)
        self.cutoff = train.parse_time("2026-01-06T00:00:00Z")

    def test_history_is_strictly_before_training_target(self):
        positives, _ = train.select_positives(self.events, 3000)
        before = [event for event in positives if event.at <= self.cutoff]
        samples, histories = train.prepare_samples(before, self.videos)
        first, second = [sample for sample in samples if sample.user_id == 1][:2]
        self.assertEqual(first.features, {})
        self.assertFalse(any(first.has_prior_positive(video_id) for video_id in self.videos))
        self.assertTrue(second.has_prior_positive(1))
        self.assertFalse(second.has_prior_positive(2))
        self.assertFalse(second.has_prior_positive(3))
        self.assertIs(first.positive_positions, second.positive_positions)
        self.assertEqual(set(second.features), set(self.videos[1].features))
        for key, value in second.features.items():
            self.assertAlmostEqual(value, self.videos[1].features[key])
        self.assertEqual(histories[1].seen, {1, 2, 3, 4})
        self.assertNotIn(5, histories[1].seen)

    def test_negative_sampling_uses_only_available_past(self):
        positives, _ = train.select_positives(self.events, 3000)
        samples, _ = train.prepare_samples(positives[:2], self.videos)
        future = train.Video(99, 999, "future", "future", train.parse_time("2027-01-01T00:00:00Z"), {1: 1.0})
        videos = dict(self.videos)
        videos[99] = future
        sampler = train.NegativeSampler(videos)
        rng = random.Random(42)
        candidates = [sampler.draw(samples[1], rng) for _ in range(100)]
        self.assertNotIn(99, candidates)
        self.assertNotIn(samples[1].positive_id, candidates)
        self.assertFalse(any(samples[1].has_prior_positive(video_id) for video_id in candidates))

    def test_future_positive_can_be_an_earlier_negative(self):
        positives, _ = train.select_positives(self.events, 3000)
        samples, _ = train.prepare_samples([event for event in positives if event.at <= self.cutoff], self.videos)
        first = next(sample for sample in samples if sample.user_id == 1)

        class ChooseSecondVideo:
            def randrange(self, count):
                return 1

        self.assertIn(2, first.positive_positions)
        self.assertFalse(first.has_prior_positive(2))
        self.assertEqual(train.NegativeSampler(self.videos).draw(first, ChooseSecondVideo()), 2)

    def test_saturated_catalog_skips_without_infinite_loop(self):
        sample = train.Sample(1, 1, {}, {video_id: index for index, video_id in enumerate(self.videos)}, len(self.videos), self.cutoff)
        self.assertIsNone(train.NegativeSampler(self.videos).draw(sample, random.Random(42)))
        with self.assertRaisesRegex(ValueError, "no trainable examples"):
            train.TwoTower().train([sample], self.videos, 1, 0.025, 0.2, 42)

    def test_first_qualifying_play_is_one_equal_target(self):
        events = [
            train.Event(1, 1, "play", 500, False, train.parse_time("2026-01-02T00:00:00Z")),
            train.Event(1, 1, "play", 5000, False, train.parse_time("2026-01-03T00:00:00Z")),
            train.Event(1, 1, "complete", 30000, True, train.parse_time("2026-01-07T00:00:00Z")),
        ]
        positives, ignored = train.select_positives(events, 3000)
        self.assertEqual(len(positives), 1)
        self.assertEqual(positives[0].at, events[1].at)
        self.assertEqual(ignored, {"not_positive": 1, "repeat_user_video": 1})

    def test_validation_rejects_bad_identifiers_dates_and_unknown_video(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "data.jsonl"
            path.write_text('{"video_id":true,"author_id":1,"title":"x","description":"","published_at":"2026-01-01T00:00:00Z"}\n')
            with self.assertRaisesRegex(ValueError, "video_id"):
                train.load_videos(path)
            path.write_text('{"user_id":1,"video_id":999,"event_type":"complete","watch_ms":1000,"completed":true,"created_at":"2026-01-02T00:00:00Z"}\n')
            with self.assertRaisesRegex(ValueError, "unknown video"):
                train.load_events(path, self.videos)
        with self.assertRaisesRegex(ValueError, "timezone"):
            train.parse_time("2026-01-01 08:00:00")
        self.assertEqual(train.parse_time("2026-01-01 08:00:00", train.parse_timezone("+08:00")), train.parse_time("2026-01-01T00:00:00Z"))

    def test_evaluation_excludes_future_catalog(self):
        artifact, _ = train.fit(self.videos, self.events, self.cutoff, epochs=1, k=3)
        event = train.Event(7, 5, "complete", 30000, True, train.parse_time("2026-01-07T00:00:00Z"))
        videos = dict(self.videos)
        videos[99] = train.Video(99, 999, "future", "future", train.parse_time("2027-01-01T00:00:00Z"), {1: 1.0})
        # No exported vector for 99: evaluating an unavailable video would fail.
        report = train.evaluate(artifact, [event], {}, videos, train.Counter(), 3)
        self.assertEqual(report["all"]["mean_candidates"], 18)

    def test_future_interactions_do_not_change_trained_vectors(self):
        first, _ = train.fit(self.videos, self.events, self.cutoff, epochs=2, seed=42, k=3)
        second, _ = train.fit(self.videos, [event for event in self.events if event.at <= self.cutoff], self.cutoff, epochs=2, seed=42, k=3)
        for field in ("users", "videos", "default_user"):
            self.assertEqual(first[field], second[field])

    def test_both_towers_learn_and_export_unit_vectors(self):
        positives, _ = train.select_positives(self.events, 3000)
        samples, histories = train.prepare_samples([event for event in positives if event.at <= self.cutoff], self.videos)
        model = train.TwoTower(seed=42)
        initial_user = [list(row) for row in model.user.weights]
        initial_video = [list(row) for row in model.video.weights]
        initial_default = list(model.user.bias)
        losses = model.train(samples, self.videos, epochs=12, learning_rate=0.025, temperature=0.2, seed=42)
        self.assertNotEqual(initial_default, model.user.bias)
        self.assertNotEqual(initial_user, model.user.weights)
        self.assertNotEqual(initial_video, model.video.weights)
        self.assertLess(losses[-1], losses[0])
        artifact = train.make_artifact(model, histories, self.videos)
        self.assertEqual(set(artifact), {"version", "dimension", "default_user", "users", "videos"})
        self.assertEqual(artifact["dimension"], 128)
        self.assertNotEqual(artifact["version"], train.make_artifact(model, histories, self.videos)["version"])
        for vector in [artifact["default_user"], *artifact["users"].values(), *artifact["videos"].values()]:
            self.assertEqual(len(vector), 128)
            self.assertTrue(all(math.isfinite(value) for value in vector))
            self.assertAlmostEqual(sum(value * value for value in vector), 1.0, places=8)

    def test_projection_backprop_matches_numerical_gradient(self):
        tower = train.Tower(random.Random(42))
        features = {3: 0.6, 17: 0.8}
        gradient = [math.sin(index) for index in range(128)]
        epsilon = 1e-6
        before = tower.weights[3][7]
        tower.weights[3][7] = before + epsilon
        above = train.dot(tower.encode(features)[0], gradient)
        tower.weights[3][7] = before - epsilon
        below = train.dot(tower.encode(features)[0], gradient)
        tower.weights[3][7] = before
        numerical = (above - below) / (2 * epsilon)
        _, cache = tower.encode(features)
        tower.backward(features, cache, gradient, learning_rate=0.001)
        actual = (before - tower.weights[3][7]) / 0.001
        self.assertAlmostEqual(actual, numerical, places=6)

    def test_demo_cli_and_temporal_evaluation(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "model.json"
            completed = subprocess.run([sys.executable, str(HERE / "train.py"), "--demo", "--epochs", "12", "--k", "3", "--output", str(path)], check=True, capture_output=True, text=True)
            artifact = json.loads(path.read_text())
            report = json.loads(Path(str(path) + ".metrics.json").read_text())
            self.assertEqual(report["training"]["positive_events"], 24)
            self.assertEqual(report["evaluation"]["all"]["count"], 13)
            self.assertEqual(report["evaluation"]["cold_users"]["count"], 1)
            self.assertNotIn("7", artifact["users"])
            self.assertGreater(report["evaluation"]["all"]["model"]["recall_at_k"], report["evaluation"]["all"]["popularity"]["recall_at_k"])
            self.assertIn("synthetic", report["data_kind"])
            self.assertIn("model.json", completed.stdout)

    def test_full_train_cli_uses_all_events_without_claiming_evaluation(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "serving.json"
            subprocess.run([sys.executable, str(HERE / "train.py"), "--videos", str(HERE / "demo" / "videos.jsonl"), "--events", str(HERE / "demo" / "events.jsonl"), "--full-train", "--epochs", "1", "--output", str(path)], check=True, capture_output=True, text=True)
            artifact = json.loads(path.read_text())
            report = json.loads(Path(str(path) + ".metrics.json").read_text())
            self.assertEqual(report["mode"], "full_train_no_held_out_evaluation")
            self.assertEqual(report["training"]["positive_events"], 37)
            self.assertIn("7", artifact["users"])
            self.assertEqual(report["evaluation"]["all"]["count"], 0)
            self.assertIsNone(report["evaluation"]["all"]["model"]["recall_at_k"])


if __name__ == "__main__":
    unittest.main()
