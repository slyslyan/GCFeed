import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import vm from "node:vm";
import { transformSync } from "esbuild";

const source = readFileSync(new URL("../src/App.jsx", import.meta.url), "utf8");
const { code } = transformSync(`${source}\nexport { createVideoViewState, sampleVideoViewState, sendViewEventReport, VideoStage };`, {
  loader: "jsx", jsx: "automatic", format: "cjs"
});

function loadApp() {
  let now = 0;
  let video = null;
  let effects = [];
  const documentListeners = new Map();
  const windowListeners = new Map();
  const context = vm.createContext({
    module: { exports: {} },
    atob,
    require(name) {
      if (name === "react/jsx-runtime") return { jsx: () => null, jsxs: () => null };
      if (name === "react") return {
        useRef: (value) => ({ current: value === null ? video : value }),
        useEffect: (effect) => effects.push(effect),
        useCallback: (callback) => callback
      };
      throw new Error(`unexpected import ${name}`);
    },
    performance: { now: () => now },
    document: {
      hidden: false,
      addEventListener: (type, listener) => documentListeners.set(type, listener),
      removeEventListener: (type) => documentListeners.delete(type)
    },
    window: {
      addEventListener: (type, listener) => windowListeners.set(type, listener),
      removeEventListener: (type) => windowListeners.delete(type),
      setTimeout: (callback) => callback()
    }
  });
  vm.runInContext(code, context);
  return {
    ...context.module.exports,
    context,
    setTime: (value) => { now = value; },
    renderVideo(item, element, onViewEvent, active = true) {
      video = element;
      effects = [];
      context.module.exports.VideoStage({ item, active, onViewEvent });
      const cleanups = effects.map((effect) => effect());
      return () => cleanups.forEach((cleanup) => cleanup?.());
    },
    visibility(hidden) {
      context.document.hidden = hidden;
      documentListeners.get("visibilitychange")?.();
    }
  };
}

const item = { video_id: 11, feed_scene: "recommend", request_id: "feed-1", media_url: "/11.mp4" };
const plain = (value) => JSON.parse(JSON.stringify(value));

test("autoplay and quick skips produce no positive signal; three visible seconds produce one play", () => {
  const app = loadApp();
  const state = app.createVideoViewState(item);
  const sample = (position, time, eligible = true) => plain(app.sampleVideoViewState(state, position, 10, time, eligible));
  assert.deepEqual(sample(0, 0), []);
  assert.deepEqual(sample(0.2, 200), []);
  assert.deepEqual(sample(2.9, 2900), []);
  assert.deepEqual(sample(3, 3000), [{ event_type: "play", watch_ms: 3000, completed: false }]);
  assert.deepEqual(sample(3, 3000), []);
  assert.deepEqual(sample(4, 4000), []);
});

test("pause, buffering, hidden playback, and seeking do not inflate watched time or completion", () => {
  const app = loadApp();
  const state = app.createVideoViewState(item);
  const sample = (position, time, eligible = true) => plain(app.sampleVideoViewState(state, position, 10, time, eligible));
  sample(0, 0);
  sample(1, 1000);
  sample(1, 1000, false);
  sample(7, 7000, false); // tab stayed hidden while media advanced
  sample(7, 7000);
  assert.deepEqual(sample(8, 8000), []);
  sample(8, 8000, false); // a seek resets the baseline
  sample(9.9, 8001, false);
  sample(9.9, 8001);
  assert.deepEqual(sample(10, 8101), []);
  assert.ok(state.watchMs < 2200);
  assert.equal(state.completeReported, false);
});

test("looped short videos report real completion without ended and never repeat samples", () => {
  const app = loadApp();
  const state = app.createVideoViewState(item);
  const sample = (position, time) => plain(app.sampleVideoViewState(state, position, 2, time, true));
  sample(0, 0);
  assert.deepEqual(sample(1.8, 1800), []);
  assert.deepEqual(sample(0.1, 2100), [
    { event_type: "play", watch_ms: 2100, completed: false },
    { event_type: "complete", watch_ms: 2100, completed: true }
  ]);
  assert.deepEqual(sample(1, 3000), []);
  assert.deepEqual(sample(0.1, 4100), []);
});

test("a seek-sized media jump cannot create a positive signal", () => {
  const app = loadApp();
  const state = app.createVideoViewState(item);
  app.sampleVideoViewState(state, 0, 10, 0, true);
  assert.deepEqual(plain(app.sampleVideoViewState(state, 9.9, 10, 100, true)), []);
  assert.equal(state.watchMs, 0);
});

function fakeVideo() {
  const listeners = new Map();
  return {
    currentTime: 0, duration: 10, playbackRate: 1, paused: true, readyState: 4,
    addEventListener(type, listener) { listeners.set(type, listener); },
    removeEventListener(type) { listeners.delete(type); },
    emit(type) { listeners.get(type)?.(); },
    play() { this.paused = false; this.emit("playing"); return Promise.resolve(); },
    pause() { this.paused = true; this.emit("pause"); }
  };
}

test("VideoStage flushes the departing video's own time and keeps the next video independent", () => {
  const app = loadApp();
  const reports = [];
  const first = fakeVideo();
  const cleanup = app.renderVideo(item, first, (video, event) => reports.push(plain({ video, event })));
  app.setTime(2900);
  first.currentTime = 2.9;
  first.emit("timeupdate");
  assert.equal(reports.length, 0);
  app.setTime(3100);
  first.currentTime = 3.1;
  cleanup();
  assert.equal(reports.length, 1);
  assert.equal(reports[0].video.video_id, 11);
  assert.equal(reports[0].video.request_id, "feed-1");
  assert.equal(reports[0].event.watch_ms, 3100);

  const second = fakeVideo();
  const cleanupSecond = app.renderVideo({ ...item, video_id: 12 }, second, (video, event) => reports.push(plain({ video, event })));
  app.setTime(3400);
  second.currentTime = 0.3;
  second.emit("timeupdate");
  cleanupSecond();
  assert.equal(reports.length, 1);
});

test("inactive preview stages and background playback do not train positive feedback", () => {
  const app = loadApp();
  const reports = [];
  const preview = fakeVideo();
  const cleanupPreview = app.renderVideo(item, preview, (...args) => reports.push(args), false);
  preview.play();
  app.setTime(4000);
  preview.currentTime = 4;
  preview.emit("timeupdate");
  cleanupPreview();
  assert.equal(reports.length, 0);

  const active = fakeVideo();
  const cleanupActive = app.renderVideo(item, active, (...args) => reports.push(args));
  app.visibility(true);
  app.setTime(9000);
  active.currentTime = 5;
  active.emit("timeupdate");
  app.visibility(false);
  app.setTime(10000);
  active.currentTime = 6;
  active.emit("timeupdate");
  cleanupActive();
  assert.equal(reports.length, 0);
});

test("network retries preserve the event payload and request ID and keep navigation delivery enabled", async () => {
  const app = loadApp();
  const calls = [];
  app.context.apiRequest = async (path, options) => {
    calls.push(plain({ path, options }));
    if (calls.length === 1) throw new TypeError("connection lost after write");
    return { event: { id: 7 } };
  };
  const body = { video_id: 11, scene: "recommend", request_id: "feed-1", event_type: "complete", watch_ms: 9500, completed: true };
  await app.sendViewEventReport(body, "access-token");
  assert.equal(calls.length, 2);
  assert.deepEqual(calls[0], calls[1]);
  assert.equal(calls[0].options.keepalive, true);
});

const accessToken = (uid, signature) => `header.${Buffer.from(JSON.stringify({ uid })).toString("base64url")}.${signature}`;
const jsonResponse = (status, body) => ({ status, ok: status >= 200 && status < 300, json: async () => body });

test("an expired viewing token cannot be retried under a different account's refresh cookie", async () => {
  const app = loadApp();
  const paths = [];
  const refreshed = [];
  app.context.registerSessionRefreshHandler((token) => refreshed.push(token));
  app.context.fetch = async (path) => {
    paths.push(path);
    return path === "/api/sessions/refresh"
      ? jsonResponse(200, { access_token: accessToken(43, "new-account") })
      : jsonResponse(401, { error: "expired" });
  };
  await assert.rejects(app.sendViewEventReport({ ...item, event_type: "play", watch_ms: 3000 }, accessToken(42, "old-account")), { status: 401 });
  assert.deepEqual(paths, ["/api/video-view-events", "/api/sessions/refresh"]);
  assert.deepEqual(refreshed, []);
});

test("refreshing the same viewer retries the original event with the new token", async () => {
  const app = loadApp();
  const calls = [];
  const nextToken = accessToken(42, "refreshed");
  app.context.fetch = async (path, options) => {
    calls.push(plain({ path, options }));
    if (path === "/api/sessions/refresh") return jsonResponse(200, { access_token: nextToken });
    return calls.length === 1 ? jsonResponse(401, { error: "expired" }) : jsonResponse(201, { event: { id: 7 } });
  };
  await app.sendViewEventReport({ ...item, event_type: "play", watch_ms: 3000 }, accessToken(42, "expired"));
  assert.equal(calls.length, 3);
  assert.equal(calls[2].options.headers.Authorization, `Bearer ${nextToken}`);
  assert.equal(calls[0].options.body, calls[2].options.body);
});
