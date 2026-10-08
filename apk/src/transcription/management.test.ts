import { describe, expect, it, vi } from "vitest";
import type { LocalDetails, TranscriptionAPI, TranscriptionJob } from "@tgcontrol/shared";
import { dictionaryText, parseDictionary, runLocalAction } from "./management";

const id = "0123456789abcdef0123456789abcdef";
const result = { models: [{ id: "new-model" }] } as LocalDetails;
function fixture() {
  return { action: vi.fn(async () => ({ id, state: "running" } as TranscriptionJob)),
    job: vi.fn(async () => ({ id, state: "done", result } as TranscriptionJob)), cancel: vi.fn(async () => ({})) };
}
describe("local module actions", () => {
  it("returns the module catalog without assuming model IDs", async () => {
    const api = fixture();
    expect(await runLocalAction(api as unknown as TranscriptionAPI, { operation: "inspect" }, new AbortController().signal, 1, { id })).toBe(result);
    expect(api.cancel).not.toHaveBeenCalled();
  });
  it("rejects a start response from another task", async () => {
    const api = fixture(); api.action.mockResolvedValue({ id: "other", state: "done", result });
    await expect(runLocalAction(api as unknown as TranscriptionAPI, { operation: "inspect" }, new AbortController().signal, 1, { id })).rejects.toThrow("invalid_recognition_result");
    expect(api.cancel).toHaveBeenCalledWith(id); expect(api.job).not.toHaveBeenCalled();
  });
  it("rejects another running job before polling it again", async () => {
    const api = fixture(); api.job.mockResolvedValue({ id: "other", state: "running" });
    await expect(runLocalAction(api as unknown as TranscriptionAPI, { operation: "inspect" }, new AbortController().signal, 1, { id })).rejects.toThrow("invalid_recognition_result");
    expect(api.job).toHaveBeenCalledTimes(1);
  });
  it.each([true, false])("navigation cancels polling, cancellation policy %s", async cancelOnAbort => {
    const api = fixture(), abort = new AbortController();
    let resolve!: (job: TranscriptionJob) => void;
    api.action.mockImplementation(() => new Promise(done => { resolve = done; }));
    const pending = runLocalAction(api as unknown as TranscriptionAPI, { operation: "install", model: "new-model" }, abort.signal, 1, { id, cancelOnAbort });
    abort.abort(); resolve({ id, state: "done", result });
    await expect(pending).rejects.toMatchObject({ name: "AbortError" });
    expect(api.cancel.mock.calls.length).toBe(cancelOnAbort ? 1 : 0);
  });
  it("round trips replacements and rejects malformed or excessive dictionaries", () => {
    const rows = [{ heard: "ремотай", written: "Remotai" }];
    expect(parseDictionary(dictionaryText(rows))).toEqual(rows);
    for (const bad of ["no separator", "x =>", "=> y", `${"x".repeat(201)} => y`, Array(101).fill("x => y").join("\n")]) expect(() => parseDictionary(bad)).toThrow();
  });
});
