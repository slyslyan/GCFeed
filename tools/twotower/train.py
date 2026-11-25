#!/usr/bin/env python3
"""Small CPU two-tower retrieval trainer; Python standard library only."""

import argparse
from bisect import bisect_right
from collections import Counter
from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
import hashlib
import json
import math
import os
from pathlib import Path
import random
import re
import tempfile
from typing import Dict
import uuid


DIMENSION = 128
FEATURE_BUCKETS = 256
MAX_FEATURES = 64
Sparse = Dict[int, float]


def parse_timezone(value):
    if value == "UTC":
        return timezone.utc
    if not re.fullmatch(r"[+-]\d{2}:\d{2}", value):
        raise ValueError("source timezone must be UTC or an offset such as +08:00")
    hours, minutes = map(int, value[1:].split(":"))
    if hours > 23 or minutes > 59:
        raise ValueError("invalid source timezone offset")
    return timezone((1 if value[0] == "+" else -1) * timedelta(hours=hours, minutes=minutes))


def parse_time(value, source_timezone=None):
    if not isinstance(value, str) or not ("T" in value or " " in value):
        raise ValueError("timestamp must include date, time and timezone")
    result = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if result.tzinfo is None:
        if source_timezone is None:
            raise ValueError("timestamp has no timezone; pass --source-timezone after checking MySQL DATETIME storage")
        result = result.replace(tzinfo=source_timezone)
    return result.astimezone(timezone.utc)


def positive_id(value, name):
    if isinstance(value, bool) or not isinstance(value, int) or not (0 < value <= 2 ** 63 - 1):
        raise ValueError(name + " must be a positive signed 64-bit integer")
    return value


def normalized(features):
    # Bound per-example work without fitting a vocabulary on held-out data.
    chosen = sorted(features.items(), key=lambda item: (-abs(item[1]), item[0]))[:MAX_FEATURES]
    norm = math.sqrt(sum(value * value for _, value in chosen))
    return {key: value / norm for key, value in sorted(chosen) if value} if norm else {}


def content_features(author_id, title, description):
    counts = Counter()

    def add(token, weight):
        key = int.from_bytes(hashlib.blake2b(token.encode("utf-8"), digest_size=8).digest(), "little") % FEATURE_BUCKETS
        counts[key] += weight

    add("author:" + str(author_id), 3.0)
    for text, weight in ((title, 2.0), (description, 1.0)):
        text = text.casefold()
        for token in re.findall(r"[^\W_]+", text, re.UNICODE):
            add("word:" + token, weight)
        compact = "".join(char for char in text if char.isalnum())[:240]
        for size in (2, 3):
            for index in range(len(compact) - size + 1):
                add("gram:" + compact[index:index + size], weight * 0.5)
    return normalized(counts)


@dataclass
class Video:
    video_id: int
    author_id: int
    title: str
    description: str
    published_at: datetime
    features: Sparse


@dataclass
class Event:
    user_id: int
    video_id: int
    event_type: str
    watch_ms: int
    completed: bool
    at: datetime
    event_id: int = 0


@dataclass
class History:
    totals: Sparse = field(default_factory=dict)
    positive_positions: Dict[int, int] = field(default_factory=dict)

    @property
    def seen(self):
        return self.positive_positions.keys()

    def add(self, video):
        self.positive_positions[video.video_id] = len(self.positive_positions)
        for key, value in video.features.items():
            self.totals[key] = self.totals.get(key, 0.0) + value


@dataclass
class Sample:
    user_id: int
    positive_id: int
    features: Sparse
    positive_positions: Dict[int, int]
    position: int
    at: datetime

    def has_prior_positive(self, video_id):
        position = self.positive_positions.get(video_id)
        return position is not None and position < self.position


def json_lines(path):
    with Path(path).open(encoding="utf-8") as source:
        for line_number, line in enumerate(source, 1):
            if not line.strip():
                continue
            try:
                value = json.loads(line)
                if not isinstance(value, dict):
                    raise ValueError("each line must be a JSON object")
                yield value
            except (ValueError, TypeError) as error:
                raise ValueError("{}:{}: {}".format(path, line_number, error)) from error


def load_videos(path, source_timezone=None):
    videos = {}
    for row in json_lines(path):
        video_id = positive_id(row.get("video_id"), "video_id")
        author_id = positive_id(row.get("author_id"), "author_id")
        title, description = row.get("title"), row.get("description", "")
        if not isinstance(title, str) or not title.strip() or not isinstance(description, str):
            raise ValueError("video {}: title must be nonempty text and description must be text".format(video_id))
        if video_id in videos:
            raise ValueError("duplicate video_id {}".format(video_id))
        at = parse_time(row.get("published_at"), source_timezone)
        videos[video_id] = Video(video_id, author_id, title, description, at, content_features(author_id, title, description))
    if not videos:
        raise ValueError("no videos in dataset")
    return videos


def load_events(path, videos, source_timezone=None):
    events = []
    identifiers = set()
    for row in json_lines(path):
        user_id = positive_id(row.get("user_id"), "user_id")
        video_id = positive_id(row.get("video_id"), "video_id")
        if video_id not in videos:
            raise ValueError("event refers to unknown video {}".format(video_id))
        event_type = row.get("event_type")
        if event_type not in {"exposed", "play", "complete", "skip"}:
            raise ValueError("unsupported event_type {!r}".format(event_type))
        watch_ms = row.get("watch_ms", 0)
        completed = row.get("completed", False)
        if isinstance(watch_ms, bool) or not isinstance(watch_ms, int) or watch_ms < 0:
            raise ValueError("watch_ms must be a nonnegative integer")
        if not isinstance(completed, (bool, int)) or completed not in (False, True, 0, 1):
            raise ValueError("completed must be a boolean or 0/1")
        event_id = row.get("event_id", 0)
        if "event_id" in row:
            positive_id(event_id, "event_id")
            if event_id in identifiers:
                raise ValueError("duplicate event_id {}".format(event_id))
            identifiers.add(event_id)
        at = parse_time(row.get("created_at"), source_timezone)
        if at < videos[video_id].published_at:
            raise ValueError("event predates video {} publication".format(video_id))
        events.append(Event(user_id, video_id, event_type, watch_ms, bool(completed), at, event_id))
    return sorted(events, key=lambda event: (event.at, event.event_id, event.user_id, event.video_id))


def select_positives(events, min_watch_ms):
    positives, seen = [], set()
    ignored = Counter()
    for event in events:
        if event.event_type != "complete" and not (event.event_type == "play" and (event.completed or event.watch_ms >= min_watch_ms)):
            ignored["not_positive"] += 1
            continue
        key = (event.user_id, event.video_id)
        if key in seen:
            ignored["repeat_user_video"] += 1
            continue
        seen.add(key)
        positives.append(event)
    return positives, dict(ignored)


def prepare_samples(events, videos):
    samples, histories = [], {}
    for event in events:
        history = histories.setdefault(event.user_id, History())
        samples.append(Sample(event.user_id, event.video_id, normalized(history.totals), history.positive_positions, len(history.positive_positions), event.at))
        history.add(videos[event.video_id])
    return samples, histories


class NegativeSampler:
    """O(videos) storage, bounded work per draw, no future-positive knowledge."""

    def __init__(self, videos):
        ordered = sorted(videos.values(), key=lambda video: (video.published_at, video.video_id))
        self.ids = [video.video_id for video in ordered]
        self.times = [video.published_at for video in ordered]

    def draw(self, sample, rng):
        count = bisect_right(self.times, sample.at)
        if not count:
            return None
        for _ in range(32):
            video_id = self.ids[rng.randrange(count)]
            if video_id != sample.positive_id and not sample.has_prior_positive(video_id):
                return video_id
        start = rng.randrange(count)
        for offset in range(min(count, 256)):
            video_id = self.ids[(start + offset) % count]
            if video_id != sample.positive_id and not sample.has_prior_positive(video_id):
                return video_id
        return None


def dot(left, right):
    return sum(a * b for a, b in zip(left, right))


class Tower:
    """Independent sparse linear projection, trainable bias, tanh and L2."""

    def __init__(self, rng):
        self.weights = [[rng.gauss(0, 0.06) for _ in range(DIMENSION)] for _ in range(FEATURE_BUCKETS)]
        self.bias = [rng.gauss(0, 0.02) for _ in range(DIMENSION)]

    def encode(self, features):
        values = list(self.bias)
        for key, value in features.items():
            row = self.weights[key]
            for index in range(DIMENSION):
                values[index] += value * row[index]
        activated = [math.tanh(value) for value in values]
        norm = math.sqrt(dot(activated, activated))
        if norm <= 1e-12 or not math.isfinite(norm):
            raise ValueError("tower produced a zero/nonfinite vector; reduce learning rate or inspect input")
        vector = [value / norm for value in activated]
        return vector, (activated, norm, vector)

    def backward(self, features, cache, gradient, learning_rate):
        activated, norm, vector = cache
        radial = dot(gradient, vector)
        gradient = [(value - vector[index] * radial) / norm * (1 - activated[index] ** 2) for index, value in enumerate(gradient)]
        for index, value in enumerate(gradient):
            self.bias[index] -= learning_rate * value
        for key, value in features.items():
            row = self.weights[key]
            for index, derivative in enumerate(gradient):
                row[index] -= learning_rate * value * derivative


class TwoTower:
    def __init__(self, seed=42):
        rng = random.Random(seed)
        self.user = Tower(rng)
        self.video = Tower(rng)

    def train(self, samples, videos, epochs, learning_rate, temperature, seed):
        rng = random.Random(seed)
        sampler = NegativeSampler(videos)
        eligible = list(samples)
        losses = []
        self.skipped_updates = []
        for _ in range(epochs):
            rng.shuffle(eligible)
            loss, updates = 0.0, 0
            for sample in eligible:
                negative_id = sampler.draw(sample, rng)
                if negative_id is None:
                    continue
                updates += 1
                positive = videos[sample.positive_id].features
                negative = videos[negative_id].features
                user, user_cache = self.user.encode(sample.features)
                target, target_cache = self.video.encode(positive)
                other, other_cache = self.video.encode(negative)
                margin = (dot(user, target) - dot(user, other)) / temperature
                loss += max(-margin, 0.0) + math.log1p(math.exp(-abs(margin)))
                factor = -1.0 / (1.0 + math.exp(margin)) / temperature
                user_gradient = [(left - right) * factor for left, right in zip(target, other)]
                positive_gradient = [value * factor for value in user]
                negative_gradient = [-value for value in positive_gradient]
                self.user.backward(sample.features, user_cache, user_gradient, learning_rate)
                self.video.backward(positive, target_cache, positive_gradient, learning_rate)
                self.video.backward(negative, other_cache, negative_gradient, learning_rate)
            if not updates:
                raise ValueError("no trainable examples: each positive needs another published, not-yet-positive video")
            losses.append(loss / updates)
            self.skipped_updates.append(len(eligible) - updates)
        return losses


def make_artifact(model, histories, videos):
    version = "two-tower-{}-{}".format(datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S%fZ"), uuid.uuid4().hex[:12])
    return {
        "version": version,
        "dimension": DIMENSION,
        "default_user": model.user.encode({})[0],
        "users": {str(user_id): model.user.encode(normalized(history.totals))[0] for user_id, history in sorted(histories.items())},
        "videos": {str(video_id): model.video.encode(video.features)[0] for video_id, video in sorted(videos.items())},
    }


def evaluate(artifact, events, histories, videos, popularity, k):
    groups = {name: [] for name in ("all", "warm_users", "cold_users")}
    seen = {user_id: set(history.seen) for user_id, history in histories.items()}
    for event in events:
        user_seen = seen.setdefault(event.user_id, set())
        candidates = [video_id for video_id, video in videos.items() if video.published_at <= event.at and video_id not in user_seen]
        user = artifact["users"].get(str(event.user_id), artifact["default_user"])
        ranking = sorted(candidates, key=lambda video_id: (-dot(user, artifact["videos"][str(video_id)]), video_id))
        baseline = sorted(candidates, key=lambda video_id: (-popularity[video_id], video_id))
        ranks = [ranking.index(event.video_id) + 1, baseline.index(event.video_id) + 1]
        result = (ranks, len(candidates))
        groups["all"].append(result)
        groups["warm_users" if str(event.user_id) in artifact["users"] else "cold_users"].append(result)
        user_seen.add(event.video_id)

    result = {}
    for name, rows in groups.items():
        metrics = {"count": len(rows), "mean_candidates": sum(count for _, count in rows) / len(rows) if rows else None}
        for index, algorithm in enumerate(("model", "popularity")):
            metrics[algorithm] = {
                "recall_at_k": sum(ranks[index] <= k for ranks, _ in rows) / len(rows) if rows else None,
                "mrr_at_k": sum(1.0 / ranks[index] if ranks[index] <= k else 0 for ranks, _ in rows) / len(rows) if rows else None,
                "ndcg_at_k": sum(1.0 / math.log2(ranks[index] + 1) if ranks[index] <= k else 0 for ranks, _ in rows) / len(rows) if rows else None,
            }
        result[name] = metrics
    return result


def fit(videos, events, cutoff, epochs=20, learning_rate=0.025, temperature=0.2, seed=42, k=10, min_watch_ms=3000):
    positives, ignored = select_positives(events, min_watch_ms)
    training = [event for event in positives if event.at <= cutoff]
    held_out = [event for event in positives if event.at > cutoff]
    if not training:
        raise ValueError("no positive training events at/before cutoff")
    samples, histories = prepare_samples(training, videos)
    model = TwoTower(seed)
    losses = model.train(samples, videos, epochs, learning_rate, temperature, seed)
    artifact = make_artifact(model, histories, videos)
    report = {
        "version": artifact["version"],
        "data_kind": "user supplied; effectiveness has not been verified online",
        "mode": "temporal_evaluation",
        "cutoff": cutoff.isoformat(),
        "dimension": DIMENSION,
        "feature_buckets": FEATURE_BUCKETS,
        "seed": seed,
        "k": k,
        "training": {"positive_events": len(training), "users": len(histories), "positive_videos": len({event.video_id for event in training}), "catalog_videos": len(videos), "available_videos_at_cutoff": sum(video.published_at <= cutoff for video in videos.values()), "epochs": epochs, "learning_rate": learning_rate, "temperature": temperature, "mean_bpr_loss_by_epoch": losses, "skipped_negative_samples_by_epoch": model.skipped_updates},
        "ignored_events": ignored,
        "evaluation": evaluate(artifact, held_out, histories, videos, Counter(event.video_id for event in training), k),
        "evaluation_protocol": "global time cutoff; frozen training-only user vectors and parameters; full available catalog; prior positive items excluded; candidate publication <= target time; popularity uses training events only",
        "limitations": ["Unobserved videos are sampled negatives, not confirmed dislikes.", "First qualifying play/complete per user/video is one equally weighted target; later completion does not add extra weight.", "Negative sampling uses 32 random attempts plus at most 256 fallback probes; saturated histories may skip an update.", "Metadata is a supplied snapshot: strict historical evaluation needs point-in-time titles/descriptions/authors and eligibility.", "Current published-only SQL exports have survivorship bias; deleted videos and former eligibility are not reconstructed.", "One positive target per event: Recall@K is also the hit rate; no online uplift is established."],
    }
    return artifact, report


def atomic_json(path, value):
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=path.parent, prefix=path.name + ".", suffix=".tmp", delete=False) as output:
            temporary = output.name
            json.dump(value, output, ensure_ascii=False, allow_nan=False, separators=(",", ":"))
            output.write("\n")
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
    finally:
        if temporary and os.path.exists(temporary):
            os.unlink(temporary)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--videos", type=Path)
    parser.add_argument("--events", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--cutoff", help="training events <= this ISO timestamp, later events evaluation only")
    parser.add_argument("--full-train", action="store_true", help="after evaluation, train on all supplied events; no hold-out metrics")
    parser.add_argument("--source-timezone", help="explicit timezone of naive MySQL DATETIME values, e.g. UTC or +08:00")
    parser.add_argument("--epochs", type=int, default=20)
    parser.add_argument("--learning-rate", type=float, default=0.025)
    parser.add_argument("--temperature", type=float, default=0.2)
    parser.add_argument("--min-watch-ms", type=int, default=3000)
    parser.add_argument("--seed", type=int, default=42)
    parser.add_argument("--k", type=int, default=10)
    parser.add_argument("--demo", action="store_true", help="use checked-in synthetic data; never an online-effect claim")
    args = parser.parse_args()
    try:
        if args.epochs <= 0 or args.k <= 0 or args.min_watch_ms < 0 or not (0 < args.learning_rate <= 1) or not (0.01 <= args.temperature <= 10):
            raise ValueError("epochs/k must be positive, min-watch-ms >= 0, learning-rate in (0,1], temperature in [0.01,10]")
        if args.full_train and (args.cutoff or args.demo):
            raise ValueError("--full-train cannot be combined with --cutoff or --demo")
        if args.demo:
            if args.videos or args.events or args.cutoff:
                raise ValueError("--demo cannot be combined with --videos/--events/--cutoff")
            args.videos = Path(__file__).parent / "demo" / "videos.jsonl"
            args.events = Path(__file__).parent / "demo" / "events.jsonl"
            args.cutoff = "2026-01-06T00:00:00Z"
        if not args.videos or not args.events or not (args.cutoff or args.full_train):
            raise ValueError("supply --videos, --events and --cutoff/--full-train, or use --demo")
        source_timezone = parse_timezone(args.source_timezone) if args.source_timezone else None
        videos = load_videos(args.videos, source_timezone)
        events = load_events(args.events, videos, source_timezone)
        if args.full_train and not events:
            raise ValueError("--full-train requires nonempty events")
        cutoff = max(event.at for event in events) if args.full_train else parse_time(args.cutoff, source_timezone)
        artifact, report = fit(videos, events, cutoff, args.epochs, args.learning_rate, args.temperature, args.seed, args.k, args.min_watch_ms)
        if args.full_train:
            report["mode"] = "full_train_no_held_out_evaluation"
        if args.demo:
            report["data_kind"] = "synthetic demonstration only; not real user or online-effect evidence"
        # Publish artifact last so a failed metrics write cannot replace a working model.
        atomic_json(str(args.output) + ".metrics.json", report)
        atomic_json(args.output, artifact)
        print("saved {} (version {}, {} users, {} videos)".format(args.output, artifact["version"], len(artifact["users"]), len(artifact["videos"])))
        print(json.dumps(report["evaluation"], ensure_ascii=False, indent=2))
    except (OSError, ValueError, KeyError, OverflowError) as error:
        parser.exit(2, "training failed: {}\n".format(error))


if __name__ == "__main__":
    main()
