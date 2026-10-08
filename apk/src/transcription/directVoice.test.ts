import { describe, expect, it, vi } from "vitest";
import type { TranscriptionAPI, TranscriptionJob, TranscriptionStatus } from "@tgcontrol/shared";
import { DirectVoice } from "./directVoice";

function fixture() {
  let job = "";
  const api: TranscriptionAPI = { status: vi.fn(async () => ({ available:true, platform:"windows", max_bytes:1 })), connect: vi.fn(), updateAgent: vi.fn(),
    action: vi.fn(async (input): Promise<TranscriptionJob> => ({ id:input.id, state:"done", result:{ management:true, module_version:"1", models:[{id:"small",label:"Small",disk_gb:1,vram_gb:1,ram_gb:1,cached:true,engine:"whisper",russian_only:false,gpu_only:false,ready:true,supported:true}], settings:{model:"small",language:"ru",dictionary:[]}, hardware:{name:"CPU",has_nvidia:false,memory_mb:0,recommended_model:"small"} } })),
    upload:vi.fn(async () => ({path:"C:/own/audio.webm"})), start:vi.fn(async (input): Promise<TranscriptionJob> => { job=input.id; return {id:job,state:"done",text:"Новый текст"}; }),
    job:vi.fn(), cancel:vi.fn(async () => ({})) };
  const recording = { stop:vi.fn(async () => new File(["own audio"], "own.webm")), cancel:vi.fn() };
  const capture = vi.fn(async () => recording), insert = vi.fn();
  return {api, recording, capture, insert, voice:new DirectVoice(api, insert, capture, 30)};
}
describe("direct terminal dictation", () => {
  it("cancel releases a recorder whose stop event never arrives", async () => {
    const f=fixture(); f.recording.stop=vi.fn(() => new Promise<File>(() => {}));
    await f.voice.start(); const stopping=f.voice.finish(); f.voice.cancel(); await stopping;
    expect(f.insert).not.toHaveBeenCalled(); expect(f.voice.snapshot().phase).toBe("idle");
    expect(f.recording.cancel).toHaveBeenCalled();
  });
  it("opens the microphone inside the tap before waiting for the computer", async () => {
    const f=fixture(); f.api.status=vi.fn(() => new Promise<TranscriptionStatus>(() => {}));
    await f.voice.start();
    expect(f.capture).toHaveBeenCalled();
    expect(f.capture.mock.invocationCallOrder[0]).toBeLessThan(vi.mocked(f.api.status).mock.invocationCallOrder[0]);
    expect(f.voice.snapshot().phase).toBe("recording");
    await vi.waitFor(() => expect(f.voice.snapshot().phase).toBe("error"));
    expect(f.recording.cancel).toHaveBeenCalled();
    expect((f.voice.snapshot().error as Error).message).toBe("connection_timeout");
    f.voice.cancel();
  });
  it("the second tap stops, uses saved preferences and inserts exactly once without approval", async () => {
    const f=fixture(); await f.voice.start(); await f.voice.finish();
    expect(f.recording.stop).toHaveBeenCalledTimes(1);
    expect(f.api.start).toHaveBeenCalledWith(expect.objectContaining({model:"small",language:"ru"}), expect.any(AbortSignal));
    expect(f.insert).toHaveBeenCalledExactlyOnceWith("Новый текст");
    expect(f.voice.snapshot().phase).toBe("idle");
    await f.voice.finish(); expect(f.insert).toHaveBeenCalledTimes(1);
  });
  it("keeps failed audio for retry without asking to speak again", async () => {
    const f=fixture(); vi.mocked(f.api.upload).mockRejectedValueOnce(new Error("offline"));
    await f.voice.start(); await f.voice.finish();
    expect(f.voice.snapshot().retry).toBe(true);
    await f.voice.retry();
    expect(f.capture).toHaveBeenCalledTimes(1);
    expect(f.api.upload).toHaveBeenCalledTimes(2);
    expect(f.insert).toHaveBeenCalledExactlyOnceWith("Новый текст");
  });
  it("cancels an old terminal's result even if the transport completes late", async () => {
    const f=fixture(); let resolve!: (job:TranscriptionJob) => void;
    f.api.start=vi.fn(() => new Promise<TranscriptionJob>(done => { resolve=done; }));
    await f.voice.start(); const stop=f.voice.finish();
    await vi.waitFor(() => expect(f.api.start).toHaveBeenCalled());
    f.voice.cancel(); resolve({id:"late",state:"done",text:"Wrong draft"}); await stop;
    expect(f.insert).not.toHaveBeenCalled(); expect(f.voice.snapshot().phase).toBe("idle");
  });
});
