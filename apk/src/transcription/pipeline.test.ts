import { describe, expect, it, vi } from "vitest";
import type { TranscriptionAPI, TranscriptionJob } from "@tgcontrol/shared";
import { transcribeAudio } from "./pipeline";

const id = "0123456789abcdef0123456789abcdef";
const file = () => new File(["synthetic audio"], "voice.webm", { type: "audio/webm" });
function fixture(): TranscriptionAPI {
  return { status: vi.fn(), connect: vi.fn(), action: vi.fn(), upload: vi.fn(async () => ({ path: "C:\\uploads\\voice.webm" })),
    start: vi.fn(async (): Promise<TranscriptionJob> => ({ id, state: "running" })), job: vi.fn(async (): Promise<TranscriptionJob> => ({ id, state: "done", text: "Проверь терминал" })), cancel: vi.fn(async () => ({})) };
}
const options = (signal: AbortSignal) => ({ signal, id, model: "small", language: "ru", phase: vi.fn(), pollMs: 1 });

describe("local transcription pipeline", () => {
  it("uses only the completed upload, then returns text for explicit review", async () => {
    const api = fixture(), config = options(new AbortController().signal);
    const result = await transcribeAudio(file(), api, config);
    expect(result.text).toBe("Проверь терминал");
    expect(api.start).toHaveBeenCalledWith({ id, path: "C:\\uploads\\voice.webm", model: "small", language: "ru" }, config.signal);
    expect(config.phase.mock.calls.map(call => call[0])).toEqual(["uploading", "recognizing"]);
    expect(api.cancel).not.toHaveBeenCalled();
  });
  it("never starts recognition for a partial chunk response", async () => {
    const api = fixture(); api.upload = vi.fn(async () => ({ path: "" }));
    await expect(transcribeAudio(file(), api, options(new AbortController().signal))).rejects.toThrow("upload_incomplete");
    expect(api.start).not.toHaveBeenCalled();
  });
  it("cancels by its known ID even when the start response is late", async () => {
    const api = fixture(), abort = new AbortController();
    let complete!: () => void;
    api.start = vi.fn(() => new Promise<TranscriptionJob>(resolve => { complete = () => resolve({ id, state: "running" }); }));
    const result = transcribeAudio(file(), api, options(abort.signal));
    await vi.waitFor(() => expect(api.start).toHaveBeenCalled());
    abort.abort(); complete();
    await expect(result).rejects.toMatchObject({ name: "AbortError" });
    expect(api.cancel).toHaveBeenCalledWith(id);
    expect(api.job).not.toHaveBeenCalled();
  });
  it("discards a result that arrives after cancellation", async () => {
    const api = fixture(), abort = new AbortController();
    let complete!: () => void;
    api.job = vi.fn(() => new Promise<TranscriptionJob>(resolve => { complete = () => resolve({ id, state: "done", text: "late" }); }));
    const result = transcribeAudio(file(), api, options(abort.signal));
    await vi.waitFor(() => expect(api.job).toHaveBeenCalled());
    abort.abort(); complete();
    await expect(result).rejects.toMatchObject({ name: "AbortError" });
    expect(api.cancel).toHaveBeenCalledWith(id);
  });
  it("rejects a result belonging to another recording", async () => {
    const api = fixture(); api.job = vi.fn(async (): Promise<TranscriptionJob> => ({ id: "other", state: "done", text: "wrong" }));
    await expect(transcribeAudio(file(), api, options(new AbortController().signal))).rejects.toThrow("invalid_recognition_result");
  });
});
