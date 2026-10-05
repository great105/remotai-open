import { describe, expect, it, vi } from "vitest";
import { androidWebglNeedsDom } from "./androidWebgl";

function canvas(renderer: string): HTMLCanvasElement {
  return {
    getContext: () => ({
      getExtension: () => ({ UNMASKED_RENDERER_WEBGL: 0x9246 }),
      getParameter: () => renderer,
    }),
  } as unknown as HTMLCanvasElement;
}

describe("Android WebView renderer choice", () => {
  it("uses DOM for native Android SwiftShader", () => {
    expect(androidWebglNeedsDom(true, "Android WebView", () => canvas("Android Emulator OpenGL ES Translator (Google SwiftShader)"))).toBe(true);
  });

  it("keeps WebGL for native Android hardware GPU", () => {
    expect(androidWebglNeedsDom(true, "Android WebView", () => canvas("Android Emulator OpenGL ES Translator (NVIDIA GeForce RTX 3070 Ti)"))).toBe(false);
  });

  it("does not probe software WebGL in a browser or on iOS", () => {
    const createCanvas = vi.fn(() => canvas("Google SwiftShader"));
    expect(androidWebglNeedsDom(false, "Android Chrome", createCanvas)).toBe(false);
    expect(androidWebglNeedsDom(true, "iPhone", createCanvas)).toBe(false);
    expect(createCanvas).not.toHaveBeenCalled();
  });
});
