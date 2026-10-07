import test from "node:test";
import assert from "node:assert/strict";
import { startPhoneRecording, transcribePhoneRecording } from "./phoneAudio.ts";

class FakeRecorder extends EventTarget {
  static isTypeSupported(mime) { return mime.startsWith("audio/ogg"); }
  state = "inactive";
  constructor(stream, options) { super(); this.stream = stream; this.mimeType = options?.mimeType ?? "audio/webm"; }
  start() { this.state = "recording"; }
  stop() {
    this.state = "inactive";
    const event = new Event("dataavailable");
    event.data = new Blob(["phone voice"], { type: this.mimeType });
    this.dispatchEvent(event);
    this.dispatchEvent(new Event("stop"));
  }
}

function fixture() {
  const state = { closed: 0 };
  const stream = { getTracks: () => [{ stop: () => state.closed++ }] };
  return { state, stream, mediaDevices: { getUserMedia: async () => stream }, Recorder: FakeRecorder };
}

test("format, repeated stop, upload order and microphone release", async () => {
  const fake = fixture();
  const recording = await startPhoneRecording(fake);
  const first = recording.stop();
  assert.equal(first, recording.stop());
  const file = await first;
  assert.match(file.name, /\.ogg$/);
  assert.match(file.type, /audio\/ogg/);
  assert.equal(fake.state.closed, 1);
  const calls = [];
  const result = await transcribePhoneRecording(recording, {
    upload: async actual => { assert.equal(actual, file); calls.push("upload"); return { path: "local/voice.ogg" }; },
    transcribe: async params => { calls.push("transcribe"); assert.deepEqual(params, { path: "local/voice.ogg", task_id: "task" }); return { text: "расшифровка" }; },
  }, "task");
  assert.deepEqual(calls, ["upload", "transcribe"]);
  assert.equal(result.text, "расшифровка");
});

test("cancel releases recording and prevents upload", async () => {
  const fake = fixture();
  const signal = new AbortController();
  const recording = await startPhoneRecording({ ...fake, signal: signal.signal });
  signal.abort();
  await assert.rejects(recording.stop(), { name: "AbortError" });
  recording.cancel();
  assert.equal(fake.state.closed, 1);
});

test("cancel during permission wait closes a stream granted later", async () => {
  const fake = fixture();
  const signal = new AbortController();
  let grant;
  const pending = startPhoneRecording({ ...fake, signal: signal.signal,
    mediaDevices: { getUserMedia: () => new Promise(resolve => { grant = resolve; }) },
  });
  signal.abort();
  await assert.rejects(pending, { name: "AbortError" });
  grant(fake.stream);
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(fake.state.closed, 1);
});

test("incomplete upload cannot be transcribed", async () => {
  const recording = await startPhoneRecording(fixture());
  await assert.rejects(transcribePhoneRecording(recording, {
    upload: async () => ({}),
    transcribe: async () => { assert.fail("must wait for upload completion"); },
  }), /Загрузка ещё не завершена/);
});

test("abort between permission resolution and recorder creation closes stream", async () => {
  const fake = fixture();
  const signal = new AbortController();
  const pending = startPhoneRecording({ ...fake, signal: signal.signal });
  queueMicrotask(() => signal.abort());
  await assert.rejects(pending, { name: "AbortError" });
  assert.equal(fake.state.closed, 1);
});

